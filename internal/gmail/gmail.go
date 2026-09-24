// Package gmail sends from and reads replies in Google Workspace/Gmail
// mailboxes through the Gmail API, one OAuth token per mailbox.
package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
)

// Scopes: send mail, and read the mailbox to find replies and bounces.
var Scopes = []string{"https://www.googleapis.com/auth/gmail.send", "https://www.googleapis.com/auth/gmail.readonly"}

const apiBase = "https://gmail.googleapis.com/gmail/v1/users/me"

// Accounts finds mailbox tokens in Dir (one <address>.json per mailbox).
type Accounts struct {
	Dir    string
	OAuth  *oauth2.Config
	APIURL string // tests override the Gmail API base URL

	mu    sync.Mutex
	boxes map[string]*Mailbox
}

// OAuthConfig builds the OAuth client from the Google Cloud "Desktop app"
// client ID and secret.
func OAuthConfig(clientID, clientSecret string) *oauth2.Config {
	return &oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, Endpoint: google.Endpoint, Scopes: Scopes}
}

func (a *Accounts) tokenPath(address string) string {
	return filepath.Join(a.Dir, strings.ToLower(address)+".json")
}

// SaveToken stores a mailbox's token with owner-only permissions.
func (a *Accounts) SaveToken(address string, tok *oauth2.Token) error {
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	return os.WriteFile(a.tokenPath(address), b, 0o600)
}

// Addresses lists mailboxes that have a token.
func (a *Accounts) Addresses() []string {
	files, _ := filepath.Glob(filepath.Join(a.Dir, "*@*.json"))
	var out []string
	for _, f := range files {
		out = append(out, strings.TrimSuffix(filepath.Base(f), ".json"))
	}
	return out
}

// Mailbox returns the mailbox for an address, or an error if it was never
// authorised with `astrolabe gmail auth`.
func (a *Accounts) Mailbox(address string) (*Mailbox, error) {
	address = strings.ToLower(address)
	a.mu.Lock()
	defer a.mu.Unlock()
	if m, ok := a.boxes[address]; ok {
		return m, nil
	}
	if a.OAuth == nil || a.OAuth.ClientID == "" {
		return nil, errors.New("Google OAuth client is not configured (ASTROLABE_GOOGLE_CLIENT_ID/SECRET)")
	}
	b, err := os.ReadFile(a.tokenPath(address))
	if err != nil {
		return nil, fmt.Errorf("mailbox %s is not authorised; run `astrolabe gmail auth %s`", address, address)
	}
	tok := &oauth2.Token{}
	if err := json.Unmarshal(b, tok); err != nil {
		return nil, err
	}
	base := a.APIURL
	if base == "" {
		base = apiBase
	}
	m := &Mailbox{Address: address, base: base,
		HTTP: oauth2.NewClient(context.Background(), a.OAuth.TokenSource(context.Background(), tok))}
	if a.boxes == nil {
		a.boxes = map[string]*Mailbox{}
	}
	a.boxes[address] = m
	return m, nil
}

type Mailbox struct {
	Address string
	HTTP    *http.Client
	base    string
}

// NewMailbox is for tests and tools that already have an HTTP client.
func NewMailbox(address, base string, client *http.Client) *Mailbox {
	return &Mailbox{Address: strings.ToLower(address), HTTP: client, base: base}
}

// Send posts one message. Errors wrapping channel.ErrNotSent mean Gmail
// certainly did not send it; any other error means the outcome is unknown.
func (m *Mailbox) Send(ctx context.Context, msg channel.Message) (channel.Result, error) {
	raw, err := channel.BuildMIME(msg)
	if err != nil {
		return channel.Result{}, channel.NotSent("build message: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"raw": base64.URLEncoding.EncodeToString(raw), "threadId": msg.ThreadID})
	req, err := http.NewRequestWithContext(ctx, "POST", m.base+"/messages/send", bytes.NewReader(body))
	if err != nil {
		return channel.Result{}, channel.NotSent("%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			// Token refresh failed before anything was sent.
			return channel.Result{}, channel.NotSent("mailbox %s token: %v", m.Address, err)
		}
		return channel.Result{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == 200:
		var r struct{ ID, ThreadID string }
		if err := json.Unmarshal(respBody, &r); err != nil || r.ID == "" {
			return channel.Result{}, fmt.Errorf("sent, but the response was unreadable: %s", respBody)
		}
		return channel.Result{ProviderID: r.ID, ThreadID: r.ThreadID}, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408:
		return channel.Result{}, channel.NotSent("gmail %d: %s", resp.StatusCode, apiError(respBody))
	default:
		return channel.Result{}, fmt.Errorf("gmail %d: %s", resp.StatusCode, apiError(respBody))
	}
}

func apiError(b []byte) string {
	var e struct{ Error struct{ Message string } }
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return strings.TrimSpace(string(b))
}

// BuildMIME is kept for callers of this package; see channel.BuildMIME.
func BuildMIME(msg channel.Message) ([]byte, error) { return channel.BuildMIME(msg) }

// FindSent looks for a message we sent by its Message-ID, to settle an
// unknown send. It returns the Gmail message and thread ids, or empty strings
// if no message with that id is in Sent mail.
func (m *Mailbox) FindSent(ctx context.Context, messageID string) (channel.Result, error) {
	var r struct {
		Messages []struct{ ID, ThreadID string }
	}
	v := url.Values{"q": {"in:sent rfc822msgid:" + strings.Trim(messageID, "<>")}}
	if err := m.get(ctx, "/messages?"+v.Encode(), &r); err != nil || len(r.Messages) == 0 {
		return channel.Result{}, err
	}
	return channel.Result{ProviderID: r.Messages[0].ID, ThreadID: r.Messages[0].ThreadID}, nil
}

// SentTo lists subjects of mail sent to an address since a time. It is the
// fallback when a Message-ID lookup finds nothing (Gmail may not keep ours).
func (m *Mailbox) SentTo(ctx context.Context, recipient string, since time.Time) ([]string, error) {
	ids, _, err := m.list(ctx, fmt.Sprintf("in:sent to:%s after:%d", recipient, since.Unix()), "")
	if err != nil {
		return nil, err
	}
	var subjects []string
	for i, id := range ids {
		if i == 5 {
			break
		}
		var msg message
		if err := m.get(ctx, "/messages/"+id+"?format=metadata&metadataHeaders=Subject", &msg); err != nil {
			return nil, err
		}
		subjects = append(subjects, msg.Payload.header("Subject"))
	}
	return subjects, nil
}

func (m *Mailbox) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", m.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("gmail %s: %d %s", path, resp.StatusCode, apiError(b))
	}
	return json.Unmarshal(b, out)
}

func (m *Mailbox) list(ctx context.Context, query, pageToken string) ([]string, string, error) {
	v := url.Values{"q": {query}, "maxResults": {"100"}}
	if pageToken != "" {
		v.Set("pageToken", pageToken)
	}
	var r struct {
		Messages      []struct{ ID string }
		NextPageToken string
	}
	if err := m.get(ctx, "/messages?"+v.Encode(), &r); err != nil {
		return nil, "", err
	}
	ids := make([]string, len(r.Messages))
	for i, msg := range r.Messages {
		ids[i] = msg.ID
	}
	return ids, r.NextPageToken, nil
}

type part struct {
	MimeType string
	Headers  []struct{ Name, Value string }
	Body     struct{ Data string }
	Parts    []part
}

type message struct {
	ID, ThreadID, Snippet string
	InternalDate          string
	Payload               part
}

func (p *part) header(name string) string {
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// text concatenates the decoded text/plain and delivery-status parts.
func (p *part) text() string {
	var b strings.Builder
	var walk func(p *part)
	walk = func(p *part) {
		if strings.HasPrefix(p.MimeType, "text/plain") || strings.HasPrefix(p.MimeType, "message/delivery-status") {
			if d, err := base64.URLEncoding.DecodeString(p.Body.Data); err == nil {
				b.Write(d)
				b.WriteString("\n")
			} else if d, err := base64.RawURLEncoding.DecodeString(p.Body.Data); err == nil {
				b.Write(d)
				b.WriteString("\n")
			}
		}
		for i := range p.Parts {
			walk(&p.Parts[i])
		}
	}
	walk(p)
	return b.String()
}

var finalRecipient = regexp.MustCompile(`(?im)^(?:Final|Original)-Recipient:\s*rfc822;\s*<?([^>\s]+@[^>\s]+)>?`)

// Poll returns messages received since a time, skipping our own. It searches
// everywhere, not just the inbox: a reply I read and archived in Gmail, or one
// filed as spam, must still stop the sequence.
func (m *Mailbox) Poll(ctx context.Context, since time.Time) ([]outreach.Inbound, error) {
	query := fmt.Sprintf("in:anywhere -in:sent -in:drafts -in:chats after:%d", since.Unix())
	var ids []string
	page := ""
	for {
		got, next, err := m.list(ctx, query, page)
		if err != nil {
			return nil, err
		}
		ids = append(ids, got...)
		if next == "" || len(ids) >= 1000 {
			break
		}
		page = next
	}
	var out []outreach.Inbound
	for _, id := range ids {
		var msg message
		if err := m.get(ctx, "/messages/"+id+"?format=full", &msg); err != nil {
			return out, err
		}
		in := parse(m.Address, &msg)
		if in.From == m.Address {
			continue
		}
		out = append(out, in)
	}
	return out, nil
}

func parse(mailbox string, msg *message) outreach.Inbound {
	from := msg.Payload.header("From")
	if a, err := parseAddress(from); err == nil {
		from = a
	}
	ms, _ := strconv.ParseInt(msg.InternalDate, 10, 64)
	body := msg.Payload.text()
	in := outreach.Inbound{
		Mailbox: mailbox, ProviderID: msg.ID, ThreadID: msg.ThreadID, From: strings.ToLower(from), MessageID: msg.Payload.header("Message-ID"),
		Subject: msg.Payload.header("Subject"), Snippet: msg.Snippet, Body: body, ReceivedAt: time.UnixMilli(ms).UTC(),
	}
	local := strings.SplitN(in.From, "@", 2)[0]
	ct := strings.ToLower(msg.Payload.header("Content-Type"))
	if local == "mailer-daemon" || local == "postmaster" || strings.Contains(ct, "report-type=delivery-status") {
		in.Bounce = true
		for _, r := range strings.Split(msg.Payload.header("X-Failed-Recipients"), ",") {
			if r = strings.TrimSpace(strings.ToLower(r)); r != "" {
				in.FailedRecipients = append(in.FailedRecipients, r)
			}
		}
		for _, m := range finalRecipient.FindAllStringSubmatch(body, -1) {
			in.FailedRecipients = append(in.FailedRecipients, strings.ToLower(m[1]))
		}
	}
	return in
}

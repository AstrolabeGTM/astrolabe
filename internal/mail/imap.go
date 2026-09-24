package mail

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
)

func dialIMAP(a *Account) (*imapclient.Client, error) {
	var c *imapclient.Client
	var err error
	opts := &imapclient.Options{}
	if a.Insecure {
		c, err = imapclient.DialInsecure(a.imapAddr(), opts)
	} else {
		opts.TLSConfig = a.tlsConfig(a.IMAPHost)
		c, err = imapclient.DialTLS(a.imapAddr(), opts)
	}
	if err != nil {
		return nil, fmt.Errorf("imap %s: %w", a.IMAPHost, err)
	}
	user := a.Username
	if a.IMAPUsername != "" {
		user = a.IMAPUsername
	}
	if err := c.Login(user, a.Password).Wait(); err != nil {
		c.Close()
		return nil, fmt.Errorf("imap login failed (check the app password): %w", err)
	}
	return c, nil
}

// TestIMAP logs in and opens the inbox.
func TestIMAP(a *Account) error {
	c, err := dialIMAP(a)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return err
	}
	return c.Logout().Wait()
}

func appendSent(_ context.Context, a *Account, raw []byte) error {
	c, err := dialIMAP(a)
	if err != nil {
		return err
	}
	defer c.Close()
	cmd := c.Append(sentFolder(c, a), int64(len(raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen}, Time: time.Now()})
	if _, err := cmd.Write(raw); err != nil {
		return err
	}
	if err := cmd.Close(); err != nil {
		return err
	}
	if _, err := cmd.Wait(); err != nil {
		return err
	}
	return c.Logout().Wait()
}

// FindSent reports whether a message with this Message-ID is in the Sent
// folder. On providers that don't file SMTP sends there, a copy exists only
// if the append after sending succeeded.
func FindSent(a *Account, messageID string) (bool, error) {
	c, err := dialIMAP(a)
	if err != nil {
		return false, err
	}
	defer c.Close()
	sent := sentFolder(c, a)
	if _, err := c.Select(sent, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return false, fmt.Errorf("open %s: %w", sent, err)
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: messageID}}}, nil).Wait()
	if err != nil {
		return false, err
	}
	return len(data.AllUIDs()) > 0, nil
}

// sentFolder finds the Sent mailbox: the one flagged \Sent (names are
// localised, e.g. Gmail's "[Gmail]/Gesendet"), else the configured name.
func sentFolder(c *imapclient.Client, a *Account) string {
	boxes, err := c.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err == nil {
		for _, b := range boxes {
			for _, attr := range b.Attrs {
				if attr == imap.MailboxAttrSent {
					return b.Mailbox
				}
			}
		}
	}
	return a.SentFolder
}

// Cursor is the polling position in the inbox.
type Cursor struct {
	UIDValidity uint32
	LastUID     uint32
}

// Poll returns new inbox messages after the cursor, and the new cursor.
// On the first poll (or if the server reset UIDVALIDITY) it reads the last
// two days only.
func Poll(a *Account, cur Cursor) ([]outreach.Inbound, Cursor, error) {
	c, err := dialIMAP(a)
	if err != nil {
		return nil, cur, err
	}
	defer c.Close()
	sel, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return nil, cur, err
	}
	crit := &imap.SearchCriteria{}
	if cur.UIDValidity == sel.UIDValidity && cur.LastUID > 0 {
		var set imap.UIDSet
		set.AddRange(imap.UID(cur.LastUID+1), 0) // 0 = "*"
		crit.UID = []imap.UIDSet{set}
	} else {
		crit.Since = time.Now().Add(-48 * time.Hour)
	}
	data, err := c.UIDSearch(crit, nil).Wait()
	if err != nil {
		return nil, cur, err
	}
	next := Cursor{UIDValidity: sel.UIDValidity, LastUID: cur.LastUID}
	if cur.UIDValidity != sel.UIDValidity {
		next.LastUID = 0
	}
	uids := data.AllUIDs()
	var out []outreach.Inbound
	if len(uids) == 0 {
		return nil, next, c.Logout().Wait()
	}
	section := &imap.FetchItemBodySection{Peek: true}
	msgs, err := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, InternalDate: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		return nil, cur, err
	}
	for _, m := range msgs {
		if uint32(m.UID) <= next.LastUID && cur.UIDValidity == sel.UIDValidity {
			continue // "N:*" always returns the last message
		}
		in, err := Parse(a.Address, m.FindBodySection(section))
		if err == nil && in.From != a.Address {
			in.ProviderID = fmt.Sprintf("imap:%s:%d:%d", a.Address, sel.UIDValidity, m.UID)
			if in.ReceivedAt.IsZero() {
				in.ReceivedAt = m.InternalDate
			}
			out = append(out, in)
		}
		if uint32(m.UID) > next.LastUID {
			next.LastUID = uint32(m.UID)
		}
	}
	return out, next, c.Logout().Wait()
}

var (
	msgIDRE       = regexp.MustCompile(`<[^<>\s]+>`)
	finalRecipRE  = regexp.MustCompile(`(?im)^(?:Final|Original)-Recipient:\s*rfc822;\s*<?([^>\s]+@[^>\s]+)>?`)
	quotedReplyRE = regexp.MustCompile(`(?m)^On .+wrote:\s*$`)
)

// Parse turns a raw RFC 822 message into an inbound reply or bounce.
func Parse(mailbox string, raw []byte) (outreach.Inbound, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return outreach.Inbound{}, err
	}
	h := msg.Header
	dec := new(mime.WordDecoder)
	subject, _ := dec.DecodeHeader(h.Get("Subject"))
	from := strings.ToLower(h.Get("From"))
	if a, err := mail.ParseAddress(h.Get("From")); err == nil {
		from = strings.ToLower(a.Address)
	}
	in := outreach.Inbound{Channel: "email", Mailbox: mailbox, From: from, Subject: subject, MessageID: h.Get("Message-ID")}
	if d, err := h.Date(); err == nil {
		in.ReceivedAt = d.UTC()
	}
	// References lists the thread from its first message; In-Reply-To is
	// the message answered. The first ID is the thread key SMTP sends record.
	seen := map[string]bool{}
	for _, id := range msgIDRE.FindAllString(h.Get("References")+" "+h.Get("In-Reply-To"), -1) {
		if !seen[id] {
			seen[id] = true
			in.References = append(in.References, id)
		}
	}
	if len(in.References) > 0 {
		in.ThreadID = in.References[0]
	}
	body, report := textParts(h.Get("Content-Type"), h.Get("Content-Transfer-Encoding"), msg.Body)
	in.Body = strings.TrimSpace(body)
	snippet := in.Body
	if loc := quotedReplyRE.FindStringIndex(snippet); loc != nil {
		snippet = snippet[:loc[0]]
	}
	in.Snippet = excerpt(snippet, 200)

	local := strings.SplitN(from, "@", 2)[0]
	ct := strings.ToLower(h.Get("Content-Type"))
	if local == "mailer-daemon" || local == "postmaster" || strings.Contains(ct, "report-type=delivery-status") {
		in.Bounce = true
		for _, r := range strings.Split(h.Get("X-Failed-Recipients"), ",") {
			if r = strings.ToLower(strings.TrimSpace(r)); r != "" {
				in.FailedRecipients = append(in.FailedRecipients, r)
			}
		}
		for _, m := range finalRecipRE.FindAllStringSubmatch(report+"\n"+body, -1) {
			in.FailedRecipients = append(in.FailedRecipients, strings.ToLower(m[1]))
		}
	}
	return in, nil
}

// textParts returns the text/plain content and any delivery-status report.
func textParts(contentType, encoding string, r io.Reader) (text, report string) {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || contentType == "" {
		mt = "text/plain"
	}
	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(r, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			t, rep := textParts(p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p)
			if text == "" {
				text = t
			}
			report += rep
		}
		return text, report
	}
	b, _ := io.ReadAll(io.LimitReader(decode(encoding, r), 1<<20))
	switch {
	case mt == "text/plain":
		return string(b), ""
	case mt == "message/delivery-status":
		return "", string(b)
	}
	return "", ""
}

func excerpt(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

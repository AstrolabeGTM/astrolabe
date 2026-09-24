package channel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func client(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// ---- Resend (email to existing users) ----

type Resend struct {
	APIKey string
	Base   string // default https://api.resend.com
	HTTP   *http.Client
}

func (r *Resend) Send(ctx context.Context, m Message) (Result, error) {
	base := r.Base
	if base == "" {
		base = "https://api.resend.com"
	}
	body, _ := json.Marshal(map[string]any{"from": m.From, "to": []string{m.To}, "subject": m.Subject, "text": m.Body,
		"headers": map[string]string{"List-Unsubscribe": "<mailto:" + m.From + "?subject=unsubscribe>"}})
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/emails", bytes.NewReader(body))
	if err != nil {
		return Result{}, NotSent("%v", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	req.Header.Set("Content-Type", "application/json")
	// Resend drops a repeat of the same key for 24 hours.
	req.Header.Set("Idempotency-Key", m.IdempotencyKey)
	resp, err := client(r.HTTP).Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Result{}, classify("resend", resp)
	}
	var out struct{ ID string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.ID == "" {
		return Result{}, fmt.Errorf("resend: accepted but unreadable response")
	}
	return Result{ProviderID: out.ID}, nil
}

// ---- WhatsApp Cloud API (template messages) ----

type WhatsApp struct {
	Token       string
	PhoneNumber string // the WhatsApp phone number id messages are sent from
	Version     string // Graph API version, e.g. v23.0
	Base        string // default https://graph.facebook.com
	HTTP        *http.Client
}

func (w *WhatsApp) Send(ctx context.Context, m Message) (Result, error) {
	base := w.Base
	if base == "" {
		base = "https://graph.facebook.com"
	}
	var params []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(m.Body), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			params = append(params, map[string]string{"type": "text", "text": line})
		}
	}
	tmpl := map[string]any{"name": m.Subject, "language": map[string]string{"code": m.Language}}
	if len(params) > 0 {
		tmpl["components"] = []map[string]any{{"type": "body", "parameters": params}}
	}
	body, _ := json.Marshal(map[string]any{"messaging_product": "whatsapp", "recipient_type": "individual",
		"to": strings.TrimPrefix(m.To, "+"), "type": "template", "template": tmpl})
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/%s/%s/messages", base, w.Version, w.PhoneNumber), bytes.NewReader(body))
	if err != nil {
		return Result{}, NotSent("%v", err)
	}
	req.Header.Set("Authorization", "Bearer "+w.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client(w.HTTP).Do(req)
	if err != nil {
		return Result{}, err // WhatsApp has no idempotency key: unknown
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Result{}, classify("whatsapp", resp)
	}
	var out struct{ Messages []struct{ ID string } }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Messages) == 0 {
		return Result{}, fmt.Errorf("whatsapp: accepted but unreadable response")
	}
	return Result{ProviderID: out.Messages[0].ID}, nil
}

// ---- Product webhook (push, in-app, anything the product delivers) ----

// Notify posts a signed JSON message to the product's own notification
// endpoint, which delivers it and must dedupe on "id".
type Notify struct {
	URL    string
	Secret string
	HTTP   *http.Client
}

// SignBody is the Astrolabe/Stripe-style signature header for a body.
func SignBody(body []byte, secret string, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (n *Notify) Send(ctx context.Context, m Message) (Result, error) {
	body, _ := json.Marshal(map[string]string{"id": m.IdempotencyKey, "channel": m.Channel, "user_id": m.To,
		"title": m.Subject, "body": m.Body})
	req, err := http.NewRequestWithContext(ctx, "POST", n.URL, bytes.NewReader(body))
	if err != nil {
		return Result{}, NotSent("%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", m.IdempotencyKey)
	req.Header.Set("Astrolabe-Signature", SignBody(body, n.Secret, time.Now()))
	resp, err := client(n.HTTP).Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, classify("product", resp)
	}
	var out struct{ ID string }
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	json.Unmarshal(b, &out)
	return Result{ProviderID: out.ID}, nil
}

// Package channel defines how an approved message leaves the system, and
// the providers for each channel.
package channel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"strings"
	"time"
)

type Message struct {
	Channel string // email, whatsapp, push, in_app, webhook
	Kind    string // cold or users (the sequence's kind)
	Product string
	From    string // email address, or the channel's sender id
	To      string // email address, phone number or product user id
	Subject string // email subject; WhatsApp template name; push title
	Body    string // email/push text; WhatsApp template parameters, one per line
	// MessageID is the RFC 822 Message-ID we set on email, so an uncertain
	// send can be looked up in the mailbox later.
	MessageID string
	// ThreadID and InReplyTo keep follow-ups in the first message's thread.
	ThreadID, InReplyTo string
	// IdempotencyKey is stable per action; providers that support it drop
	// a repeated request with the same key.
	IdempotencyKey string
	Language       string // WhatsApp template language
}

type Result struct {
	ProviderID, ThreadID string
}

type Sender interface {
	Send(ctx context.Context, m Message) (Result, error)
}

// ErrNotSent marks failures where the provider certainly did not send
// (bad request, auth failure before the request). Any other error from Send
// means delivery is unknown and must not be retried automatically.
var ErrNotSent = errors.New("not sent")

func NotSent(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrNotSent, fmt.Sprintf(format, a...))
}

// Senders resolves the provider for a message (by channel, kind, sender).
type Senders func(m Message) (Sender, error)

// classify turns an HTTP response into a result error: 2xx is success;
// 4xx other than timeouts and conflicts means the provider refused it (not
// sent); anything else is unknown.
func classify(provider string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	msg := strings.TrimSpace(string(b))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && resp.StatusCode != 409:
		return NotSent("%s %d: %s", provider, resp.StatusCode, msg)
	default:
		return fmt.Errorf("%s %d: %s", provider, resp.StatusCode, msg)
	}
}

// BuildMIME renders a plain-text email with our Message-ID and threading headers.
func BuildMIME(msg Message) ([]byte, error) {
	for _, v := range []string{msg.From, msg.To, msg.Subject, msg.MessageID, msg.InReplyTo} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, errors.New("header values must not contain line breaks")
		}
	}
	var b bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	h("From", msg.From)
	h("To", msg.To)
	h("Subject", mime.QEncoding.Encode("utf-8", msg.Subject))
	h("Message-ID", msg.MessageID)
	if msg.InReplyTo != "" {
		h("In-Reply-To", msg.InReplyTo)
		// The whole chain: the thread's first message (when the thread id is
		// a Message-ID, as with SMTP) and the one being answered.
		refs := msg.InReplyTo
		if strings.HasPrefix(msg.ThreadID, "<") && msg.ThreadID != msg.InReplyTo {
			refs = msg.ThreadID + " " + refs
		}
		h("References", refs)
	}
	h("Date", time.Now().UTC().Format(time.RFC1123Z))
	h("List-Unsubscribe", fmt.Sprintf("<mailto:%s?subject=unsubscribe>", msg.From))
	h("MIME-Version", "1.0")
	h("Content-Type", `text/plain; charset="utf-8"`)
	h("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	if _, err := qp.Write([]byte(strings.ReplaceAll(msg.Body, "\n", "\r\n"))); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Package sandbox lets someone try the whole loop without any accounts:
// every channel "sends" into a local outbox, and replies, bounces and
// stage events can be simulated so the real matching and stop logic runs.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/astrolabe-gtm/astrolabe/internal/channel"
	"github.com/astrolabe-gtm/astrolabe/internal/outreach"
	"github.com/astrolabe-gtm/astrolabe/internal/store"
)

const (
	Sandbox = "sandbox"
	Live    = "live"
)

// ErrModeMismatch: the database was first used in the other mode.
var ErrModeMismatch = errors.New("mode mismatch")

// EnsureMode records the mode on first start and refuses a different one
// later, so sandbox data and real sends never share a database.
func EnsureMode(ctx context.Context, q store.Q, want string) error {
	if want != Sandbox && want != Live {
		return fmt.Errorf("ASTROLABE_MODE must be sandbox or live, not %q", want)
	}
	if _, err := q.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('mode', $1) ON CONFLICT DO NOTHING`, want); err != nil {
		return err
	}
	got, err := Mode(ctx, q)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: this database was set up in %s mode but ASTROLABE_MODE=%s. Use a separate database for %s, or run `astrolabe mode set %s` to switch this one on purpose",
			ErrModeMismatch, got, want, want, want)
	}
	return nil
}

func Mode(ctx context.Context, q store.Q) (string, error) {
	var m string
	err := q.QueryRow(ctx, `SELECT value FROM settings WHERE key = 'mode'`).Scan(&m)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return m, err
}

// SetMode switches the database's mode deliberately.
func SetMode(ctx context.Context, q store.Q, mode string) error {
	if mode != Sandbox && mode != Live {
		return fmt.Errorf("mode must be sandbox or live")
	}
	_, err := q.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('mode', $1) ON CONFLICT (key) DO UPDATE SET value = $1, updated_at = now()`, mode)
	return err
}

// Sender records messages in the outbox. It stands in for every channel.
type Sender struct {
	Pool *pgxpool.Pool
}

func (s *Sender) Send(ctx context.Context, m channel.Message) (channel.Result, error) {
	thread := m.ThreadID
	if thread == "" {
		thread = m.MessageID // email threads are keyed by their first Message-ID
	}
	if thread == "" {
		thread = m.IdempotencyKey
	}
	var id int64
	var actionID *int64
	var n int64
	if _, err := fmt.Sscanf(m.IdempotencyKey, "astrolabe-action-%d", &n); err == nil {
		actionID = &n
	}
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO sandbox_outbox (action_id, channel, sender, recipient, subject, body, message_id, thread_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		actionID, m.Channel, m.From, m.To, m.Subject, m.Body, m.MessageID, thread).Scan(&id); err != nil {
		return channel.Result{}, channel.NotSent("sandbox outbox: %v", err)
	}
	return channel.Result{ProviderID: fmt.Sprintf("sandbox:%d", id), ThreadID: thread}, nil
}

type Message struct {
	ID        int64     `json:"id"`
	ActionID  *int64    `json:"action_id,omitempty"`
	Channel   string    `json:"channel"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Subject   string    `json:"subject,omitempty"`
	Body      string    `json:"body"`
	MessageID string    `json:"-"`
	ThreadID  string    `json:"-"`
	SentAt    time.Time `json:"sent_at"`
	PersonID  *int64    `json:"person_id,omitempty"`
	Product   string    `json:"product,omitempty"`
}

// Outbox lists sandbox messages, newest first.
func Outbox(ctx context.Context, q store.Q, limit int) ([]Message, error) {
	rows, err := q.Query(ctx, `
		SELECT o.id, o.action_id, o.channel, o.sender, o.recipient, o.subject, o.body, o.message_id, o.thread_id, o.sent_at,
			a.person_id, COALESCE(a.product_id, '')
		FROM sandbox_outbox o LEFT JOIN actions a ON a.id = o.action_id ORDER BY o.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Message, error) {
		var m Message
		return m, r.Scan(&m.ID, &m.ActionID, &m.Channel, &m.From, &m.To, &m.Subject, &m.Body, &m.MessageID, &m.ThreadID, &m.SentAt, &m.PersonID, &m.Product)
	})
}

func get(ctx context.Context, q store.Q, id int64) (*Message, error) {
	all, err := q.Query(ctx, `SELECT id, channel, sender, recipient, subject, message_id, thread_id FROM sandbox_outbox WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	m, err := pgx.CollectExactlyOneRow(all, func(r pgx.CollectableRow) (Message, error) {
		var m Message
		return m, r.Scan(&m.ID, &m.Channel, &m.From, &m.To, &m.Subject, &m.MessageID, &m.ThreadID)
	})
	if err != nil {
		return nil, fmt.Errorf("no sandbox message %d", id)
	}
	return &m, nil
}

// SimulateReply delivers a reply to an outbox message through the normal
// reply path (thread and In-Reply-To matching, sequence stop, labelling).
func SimulateReply(ctx context.Context, out *outreach.Service, outboxID int64, body string) error {
	m, err := get(ctx, out.Pool, outboxID)
	if err != nil {
		return err
	}
	ch := "email"
	if m.Channel == "whatsapp" {
		ch = "whatsapp"
	} else if m.Channel != "email" {
		return fmt.Errorf("%s messages can't be replied to; simulate a stage instead", m.Channel)
	}
	refs := []string{}
	if m.MessageID != "" {
		refs = append(refs, m.MessageID)
	}
	_, err = out.RecordReply(ctx, outreach.Inbound{Channel: ch, Mailbox: m.From, ProviderID: fmt.Sprintf("sandbox-reply:%d:%d", outboxID, time.Now().UnixNano()),
		ThreadID: m.ThreadID, From: m.To, Subject: "Re: " + strings.TrimPrefix(m.Subject, "Re: "), Snippet: body, Body: body,
		MessageID: fmt.Sprintf("<sandbox.%d@reply.test>", time.Now().UnixNano()), References: refs, ReceivedAt: time.Now()})
	return err
}

// SimulateBounce makes an outbox email bounce (the person goes on do-not-contact).
func SimulateBounce(ctx context.Context, out *outreach.Service, outboxID int64) error {
	m, err := get(ctx, out.Pool, outboxID)
	if err != nil {
		return err
	}
	if m.Channel != "email" {
		return fmt.Errorf("only email bounces")
	}
	_, err = out.RecordReply(ctx, outreach.Inbound{Channel: "email", Mailbox: m.From, ProviderID: fmt.Sprintf("sandbox-bounce:%d", outboxID),
		ThreadID: m.ThreadID, From: "mailer-daemon@bounce.test", Subject: "Delivery Status Notification (Failure)",
		Body: "Address not found", Bounce: true, FailedRecipients: []string{m.To}, ReceivedAt: time.Now()})
	return err
}

// Package updates is the updates list: an opt-in email list for a product
// (sales terms: newsletter, mailing list, nurture list).
//
// A sign-up is a pending request, not a subscriber. It becomes one only
// when the person confirms: by replying to the confirmation email (works
// with no public address; the reply is read from the sending mailbox), or by
// opening a signed link when ASTROLABE_PUBLIC_URL is set. Unsubscribing
// works the same two ways, and every message carries both. Broadcasts go
// only to confirmed subscribers, once each, in batches under a daily cap.
//
// Everything is plain functions on Service so the web app, MCP tools and
// automations behave the same way.
package updates

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

type Service struct {
	Pool    *pgxpool.Pool
	Senders channel.Senders
	Now     func() time.Time
	// PublicURL, when set, adds confirmation and one-click unsubscribe links.
	PublicURL string
	// Kick runs inside the transaction that queues a message, to wake the
	// sender (a background job). Optional: a periodic job also sends.
	Kick func(ctx context.Context, tx pgx.Tx) error

	secretMu sync.Mutex
	secretV  []byte
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ErrRefused is returned when a rule says no; the message says which.
var ErrRefused = errors.New("refused")

func refused(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, a...))
}

// ErrExpired: the confirmation request is too old.
var ErrExpired = fmt.Errorf("%w: the confirmation request has expired; sign up again", ErrRefused)

type Subscriber struct {
	ID             int64      `json:"id"`
	Product        string     `json:"product"`
	PersonID       int64      `json:"person_id"`
	Name           string     `json:"name,omitempty"`
	Email          string     `json:"email"`
	Status         string     `json:"status"`
	Source         string     `json:"source"`
	RequestedAt    time.Time  `json:"requested_at"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	ConfirmedAt    *time.Time `json:"confirmed_at,omitempty"`
	ConfirmedVia   string     `json:"confirmed_via,omitempty"`
	UnsubscribedAt *time.Time `json:"unsubscribed_at,omitempty"`
}

const subscriberSelect = `
	SELECT s.id, s.product_id, s.person_id, p.name, s.email, s.status, s.source, s.requested_at, s.expires_at,
		s.confirmed_at, s.confirmed_via, s.unsubscribed_at
	FROM updates_subscribers s JOIN people p ON p.id = s.person_id`

func scanSubscriber(r pgx.Row) (*Subscriber, error) {
	x := &Subscriber{}
	err := r.Scan(&x.ID, &x.Product, &x.PersonID, &x.Name, &x.Email, &x.Status, &x.Source, &x.RequestedAt, &x.ExpiresAt,
		&x.ConfirmedAt, &x.ConfirmedVia, &x.UnsubscribedAt)
	return x, err
}

// settings returns the product's list settings.
func settings(ctx context.Context, q store.Q, productID string) (*product.Product, product.Updates, error) {
	p, err := product.Get(ctx, q, productID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, product.Updates{}, refused("no product %s", productID)
	} else if err != nil {
		return nil, product.Updates{}, err
	}
	u, ok := p.UpdatesSettings()
	if !ok {
		return nil, u, refused("%s has no updates list; add an updates: section to its product.yaml", productID)
	}
	return p, u, nil
}

// ---- sign-up ----

type SubscribeResult struct {
	Subscriber *Subscriber `json:"subscriber"`
	// State: "pending" (a confirmation was queued), "already pending",
	// "already subscribed".
	State string `json:"state"`
}

// AddByEmail asks an address to join the list (someone who asked in
// person, say): finds or creates the person and queues a confirmation. They
// are not subscribed until they confirm.
func (s *Service) AddByEmail(ctx context.Context, productID, email, name, source string) (SubscribeResult, error) {
	var res SubscribeResult
	addr := people.NormalizeEmail(email)
	if addr == "" {
		return res, refused("%q is not an email address", email)
	}
	if _, _, err := settings(ctx, s.Pool, productID); err != nil {
		return res, err
	}
	if _, err := people.FindByEmail(ctx, s.Pool, addr); errors.Is(err, pgx.ErrNoRows) {
		r, err := people.Import(ctx, s.Pool, productID, []people.Row{{Email: addr, Name: name}})
		if err != nil {
			return res, err
		}
		if len(r.Skipped) > 0 {
			return res, refused("%s", r.Skipped[0])
		}
	} else if err != nil {
		return res, err
	}
	p, err := people.FindByEmail(ctx, s.Pool, addr)
	if err != nil {
		return res, err
	}
	if source == "" {
		source = "manual"
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// They gave the address to the operator themselves.
		if err := people.LinkIdentity(ctx, tx, "email", addr, p.ID, people.SourceManual, true); err != nil {
			return err
		}
		var err error
		res, err = s.subscribeTx(ctx, tx, productID, p.ID, addr, source)
		return err
	})
	return res, err
}

// SubscribeTx asks a known person to join the list (the automation
// action). Their best address is used: one they gave through a form first.
func (s *Service) SubscribeTx(ctx context.Context, tx pgx.Tx, productID string, personID int64, source string) (SubscribeResult, error) {
	var email string
	err := tx.QueryRow(ctx, `
		SELECT value FROM identities WHERE person_id = $1 AND kind = 'email'
		ORDER BY (source = 'form') DESC, (source = 'github'), value LIMIT 1`, personID).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return SubscribeResult{}, refused("person %d has no email address", personID)
	} else if err != nil {
		return SubscribeResult{}, err
	}
	return s.subscribeTx(ctx, tx, productID, personID, email, source)
}

func (s *Service) subscribeTx(ctx context.Context, tx pgx.Tx, productID string, personID int64, email, source string) (SubscribeResult, error) {
	var res SubscribeResult
	_, u, err := settings(ctx, tx, productID)
	if err != nil {
		return res, err
	}
	var suppressed string
	if err := tx.QueryRow(ctx, `SELECT reason FROM suppressions WHERE person_id = $1`, personID).Scan(&suppressed); err == nil {
		return res, refused("person %d is on the do-not-contact list (%s)", personID, suppressed)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return res, err
	}
	// The same guard as every other email: addresses found only through
	// GitHub are never mailed. A sign-up gave the address, so this passes.
	if ok, reason, err := outreach.MayEmail(ctx, tx, productID, personID); err != nil {
		return res, err
	} else if !ok {
		return res, refused("not emailed, %s", reason)
	}
	window, _ := product.AfterDuration(u.ConfirmWithin)
	now := s.now()

	var id int64
	var status string
	var expires *time.Time
	var requests int
	err = tx.QueryRow(ctx, `SELECT id, status, expires_at, requests FROM updates_subscribers WHERE product_id = $1 AND person_id = $2 FOR UPDATE`,
		productID, personID).Scan(&id, &status, &expires, &requests)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		requests = 1
		err = tx.QueryRow(ctx, `
			INSERT INTO updates_subscribers (product_id, person_id, email, source, requested_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, productID, personID, email, source, now, now.Add(window)).Scan(&id)
		if err != nil {
			return res, err
		}
		res.State = "pending"
	case err != nil:
		return res, err
	case status == "confirmed":
		res.State = "already subscribed"
	case status == "pending" && expires != nil && expires.After(now):
		res.State = "already pending"
	default:
		// Unsubscribed, expired or bounced: they may ask again, and must
		// confirm again.
		requests++
		if _, err := tx.Exec(ctx, `
			UPDATE updates_subscribers SET status = 'pending', email = $2, source = $3, requests = $4, requested_at = $5, expires_at = $6,
				confirmed_at = NULL, confirmed_via = '', unsubscribed_at = NULL, unsubscribed_via = ''
			WHERE id = $1`, id, email, source, requests, now, now.Add(window)); err != nil {
			return res, err
		}
		res.State = "pending"
	}
	if res.State == "pending" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO updates_messages (product_id, subscriber_id, kind, request_no) VALUES ($1, $2, 'confirm', $3)
			ON CONFLICT DO NOTHING`, productID, id, requests); err != nil {
			return res, err
		}
		if s.Kick != nil {
			if err := s.Kick(ctx, tx); err != nil {
				return res, err
			}
		}
	}
	res.Subscriber, err = scanSubscriber(tx.QueryRow(ctx, subscriberSelect+` WHERE s.id = $1`, id))
	return res, err
}

// ---- confirm and unsubscribe ----

// Confirm turns a pending sign-up into a subscriber. via is reply, link or
// (for demo data) another label. It reports false if they were not pending.
func (s *Service) Confirm(ctx context.Context, q store.Q, subscriberID int64, via string) (bool, error) {
	var productID string
	var requests int
	err := q.QueryRow(ctx, `
		UPDATE updates_subscribers SET status = 'confirmed', confirmed_at = $2, confirmed_via = $3, expires_at = NULL
		WHERE id = $1 AND status = 'pending' AND expires_at > $2 RETURNING product_id, requests`, subscriberID, s.now(), via).Scan(&productID, &requests)
	if errors.Is(err, pgx.ErrNoRows) {
		var status string
		var expires *time.Time
		if err := q.QueryRow(ctx, `SELECT status, expires_at FROM updates_subscribers WHERE id = $1`, subscriberID).Scan(&status, &expires); err != nil {
			return false, refused("no subscriber %d", subscriberID)
		}
		if status == "pending" {
			return false, ErrExpired
		}
		return false, nil
	} else if err != nil {
		return false, err
	}
	_, err = q.Exec(ctx, `INSERT INTO updates_messages (product_id, subscriber_id, kind, request_no) VALUES ($1, $2, 'welcome', $3) ON CONFLICT DO NOTHING`,
		productID, subscriberID, requests)
	return true, err
}

// Unsubscribe ends a subscription (or a pending request) at once: anything
// queued for them is dropped, so it is honoured before the next send. all
// also puts the person on the global do-not-contact list; use it only when
// they asked to stop everything.
func (s *Service) Unsubscribe(ctx context.Context, q store.Q, subscriberID int64, via string, all bool) (bool, error) {
	var personID int64
	err := q.QueryRow(ctx, `
		UPDATE updates_subscribers SET status = 'unsubscribed', unsubscribed_at = $2, unsubscribed_via = $3, expires_at = NULL
		WHERE id = $1 AND status IN ('pending', 'confirmed') RETURNING person_id`, subscriberID, s.now(), via).Scan(&personID)
	changed := true
	if errors.Is(err, pgx.ErrNoRows) {
		changed = false
		if err := q.QueryRow(ctx, `SELECT person_id FROM updates_subscribers WHERE id = $1`, subscriberID).Scan(&personID); err != nil {
			return false, refused("no subscriber %d", subscriberID)
		}
	} else if err != nil {
		return false, err
	}
	if _, err := q.Exec(ctx, `UPDATE updates_messages SET status = 'skipped', error = 'unsubscribed' WHERE subscriber_id = $1 AND status = 'queued'`, subscriberID); err != nil {
		return false, err
	}
	if all {
		if err := people.Suppress(ctx, q, personID, "unsubscribed", "asked to stop everything ("+via+")"); err != nil {
			return false, err
		}
	}
	return changed, nil
}

// UnsubscribeByEmail is the operator's unsubscribe, for someone who asked
// you directly.
func (s *Service) UnsubscribeByEmail(ctx context.Context, productID, email string) (bool, error) {
	var id int64
	if err := s.Pool.QueryRow(ctx, `SELECT id FROM updates_subscribers WHERE product_id = $1 AND email = $2`, productID, people.NormalizeEmail(email)).Scan(&id); err != nil {
		return false, refused("%s is not on the %s list", email, productID)
	}
	return s.Unsubscribe(ctx, s.Pool, id, "manual", false)
}

// expire marks pending sign-ups that were never confirmed.
func (s *Service) expire(ctx context.Context) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE updates_subscribers SET status = 'expired' WHERE status = 'pending' AND expires_at <= $1`, s.now())
	if err != nil {
		return 0, err
	}
	_, err = s.Pool.Exec(ctx, `
		UPDATE updates_messages m SET status = 'skipped', error = 'request expired'
		FROM updates_subscribers x WHERE x.id = m.subscriber_id AND x.status = 'expired' AND m.status = 'queued'`)
	return tag.RowsAffected(), err
}

// ---- tokens ----

// Links are signed with a random key kept in the database: unguessable,
// stateless, and a confirm token cannot be used to unsubscribe.
func (s *Service) secret(ctx context.Context) ([]byte, error) {
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	if s.secretV != nil {
		return s.secretV, nil
	}
	b := make([]byte, 32)
	rand.Read(b)
	if _, err := s.Pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('updates_secret', $1) ON CONFLICT DO NOTHING`, hex.EncodeToString(b)); err != nil {
		return nil, err
	}
	var v string
	if err := s.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = 'updates_secret'`).Scan(&v); err != nil {
		return nil, err
	}
	s.secretV, _ = hex.DecodeString(v)
	return s.secretV, nil
}

func (s *Service) token(ctx context.Context, purpose string, id int64) (string, error) {
	key, err := s.secret(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d.%s", id, mac(key, purpose, id)), nil
}

func mac(key []byte, purpose string, id int64) string {
	h := hmac.New(sha256.New, key)
	fmt.Fprintf(h, "updates:%s:%d", purpose, id)
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// parseToken checks a token for a purpose and returns the subscriber id.
func (s *Service) parseToken(ctx context.Context, purpose, tok string) (int64, bool) {
	idStr, sig, ok := strings.Cut(strings.TrimSpace(tok), ".")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if !ok || err != nil || id <= 0 {
		return 0, false
	}
	key, err := s.secret(ctx)
	if err != nil {
		return 0, false
	}
	return id, hmac.Equal([]byte(sig), []byte(mac(key, purpose, id)))
}

// ---- reading ----

type Counts struct {
	Pending      int `json:"pending"`
	Confirmed    int `json:"confirmed"`
	Unsubscribed int `json:"unsubscribed"`
	Expired      int `json:"expired"`
	Bounced      int `json:"bounced"`
}

func (s *Service) Counts(ctx context.Context, productID string) (Counts, error) {
	var c Counts
	err := s.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'pending'), count(*) FILTER (WHERE status = 'confirmed'),
			count(*) FILTER (WHERE status = 'unsubscribed'), count(*) FILTER (WHERE status = 'expired'),
			count(*) FILTER (WHERE status = 'bounced')
		FROM updates_subscribers WHERE product_id = $1`, productID).Scan(&c.Pending, &c.Confirmed, &c.Unsubscribed, &c.Expired, &c.Bounced)
	return c, err
}

// List returns subscribers, newest request first; status filters ("" for all).
func (s *Service) List(ctx context.Context, productID, status string, limit int) ([]*Subscriber, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, subscriberSelect+`
		WHERE s.product_id = $1 AND ($2 = '' OR s.status = $2) ORDER BY s.requested_at DESC, s.id DESC LIMIT $3`, productID, status, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Subscriber, error) { return scanSubscriber(r) })
}

// ExportCSV writes confirmed subscribers as CSV (email, name,
// confirmed_at, confirmed_via, source), for moving the list elsewhere.
func (s *Service) ExportCSV(ctx context.Context, productID string) (string, error) {
	rows, err := s.Pool.Query(ctx, subscriberSelect+` WHERE s.product_id = $1 AND s.status = 'confirmed' ORDER BY s.confirmed_at, s.id`, productID)
	if err != nil {
		return "", err
	}
	subs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Subscriber, error) { return scanSubscriber(r) })
	if err != nil {
		return "", err
	}
	var b strings.Builder
	w := csv.NewWriter(&b)
	w.Write([]string{"email", "name", "confirmed_at", "confirmed_via", "source"})
	for _, x := range subs {
		at := ""
		if x.ConfirmedAt != nil {
			at = x.ConfirmedAt.UTC().Format(time.RFC3339)
		}
		w.Write([]string{safeCell(x.Email), safeCell(x.Name), at, x.ConfirmedVia, safeCell(x.Source)})
	}
	w.Flush()
	return b.String(), w.Error()
}

// safeCell stops spreadsheets from running a name that starts like a formula.
func safeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

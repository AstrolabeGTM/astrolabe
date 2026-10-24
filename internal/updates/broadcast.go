package updates

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/draft"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

// Broadcast is one email to the whole list. It is drafted (from the content
// published in a period, or by hand), edited, and approved by the
// operator; approval covers the exact subject and body.
type Broadcast struct {
	ID         int64      `json:"id"`
	Product    string     `json:"product"`
	Subject    string     `json:"subject"`
	Body       string     `json:"body"`
	Status     string     `json:"status"` // draft, approved, cancelled
	State      string     `json:"state"`  // draft, approved, sending, sent, cancelled
	Flags      []string   `json:"flags,omitempty"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
	PeriodFrom *time.Time `json:"period_from,omitempty"`
	PeriodTo   *time.Time `json:"period_to,omitempty"`
	ContentIDs []int64    `json:"content_ids,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	// Delivery counts, one per subscriber who was confirmed at approval.
	Queued  int `json:"queued"`
	Sending int `json:"sending"`
	Sent    int `json:"sent"`
	Unknown int `json:"unknown"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	// Note says what is holding it, when something is.
	Note string `json:"note,omitempty"`
}

const broadcastSelect = `
	SELECT b.id, b.product_id, b.subject, b.body, b.status, b.flags, b.approved_at, b.period_from, b.period_to, b.content_ids, b.created_at,
		count(m.id) FILTER (WHERE m.status = 'queued'), count(m.id) FILTER (WHERE m.status = 'sending'),
		count(m.id) FILTER (WHERE m.status = 'sent'), count(m.id) FILTER (WHERE m.status = 'unknown'),
		count(m.id) FILTER (WHERE m.status = 'failed'), count(m.id) FILTER (WHERE m.status = 'skipped')
	FROM updates_broadcasts b LEFT JOIN updates_messages m ON m.broadcast_id = b.id`

func scanBroadcast(r pgx.Row) (*Broadcast, error) {
	b := &Broadcast{}
	err := r.Scan(&b.ID, &b.Product, &b.Subject, &b.Body, &b.Status, &b.Flags, &b.ApprovedAt, &b.PeriodFrom, &b.PeriodTo, &b.ContentIDs, &b.CreatedAt,
		&b.Queued, &b.Sending, &b.Sent, &b.Unknown, &b.Failed, &b.Skipped)
	switch {
	case b.Status != "approved":
		b.State = b.Status
	case b.Queued > 0 || b.Sending > 0:
		b.State = "sending"
		if b.Sent+b.Unknown+b.Failed == 0 && b.Sending == 0 {
			b.State = "approved"
		}
	case b.Sent+b.Unknown+b.Failed+b.Skipped == 0:
		b.State = "approved"
	default:
		b.State = "sent"
	}
	return b, err
}

// Broadcast returns one broadcast with its delivery counts.
func (s *Service) Broadcast(ctx context.Context, id int64) (*Broadcast, error) {
	b, err := scanBroadcast(s.Pool.QueryRow(ctx, broadcastSelect+` WHERE b.id = $1 GROUP BY b.id`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refused("no broadcast %d", id)
	}
	if err == nil {
		s.annotate(ctx, b)
	}
	return b, err
}

// Broadcasts lists a product's broadcasts, newest first.
func (s *Service) Broadcasts(ctx context.Context, productID string, limit int) ([]*Broadcast, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx, broadcastSelect+` WHERE b.product_id = $1 GROUP BY b.id ORDER BY b.id DESC LIMIT $2`, productID, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Broadcast, error) { return scanBroadcast(r) })
	for _, b := range out {
		s.annotate(ctx, b)
	}
	return out, err
}

// annotate says why a sending broadcast is waiting.
func (s *Service) annotate(ctx context.Context, b *Broadcast) {
	if b.State != "sending" && b.State != "approved" || b.Queued == 0 {
		return
	}
	_, u, err := settings(ctx, s.Pool, b.Product)
	if err != nil {
		return
	}
	var used int
	s.Pool.QueryRow(ctx, `SELECT count(*) FROM updates_messages WHERE product_id = $1 AND kind = 'broadcast' AND status IN ('sending', 'sent', 'unknown') AND claimed_at > $2`,
		b.Product, s.now().Add(-24*time.Hour)).Scan(&used)
	var erased int
	s.Pool.QueryRow(ctx, `SELECT count(*) FROM erased_sends WHERE product_id = $1 AND counter = 'broadcast' AND counted_at > $2`,
		b.Product, s.now().Add(-24*time.Hour)).Scan(&erased)
	used += erased
	switch scope, _ := pausedFor(ctx, s, b.Product); {
	case u.PostalAddress == "":
		b.Note = "waiting: updates.postal_address is not set"
	case scope != "":
		b.Note = "waiting: sending is paused (" + scope + ")"
	case used >= u.PerDay:
		b.Note = fmt.Sprintf("waiting: the daily cap of %d is reached; the rest goes out as the last 24 hours free room", u.PerDay)
	}
}

// Flags are the problems found in a broadcast's text: a forbidden claim or
// a leftover placeholder. (Length is not one: a newsletter is long.)
func Flags(p *product.Product, subject, body string) []string {
	flags := []string{}
	for _, f := range draft.Check(p, subject+"\n"+body, nil) {
		if !strings.HasPrefix(f, "long (") {
			flags = append(flags, f)
		}
	}
	return flags
}

type CreateOpts struct {
	Product string
	// Subject and Body write it by hand. With Body empty and a Period, the
	// body is built from the content published in that period.
	Subject, Body string
	Period        string // this-month, last-month, YYYY-MM, or 30d
}

// CreateBroadcast saves a draft. Nothing is sent until it is approved.
func (s *Service) CreateBroadcast(ctx context.Context, o CreateOpts) (*Broadcast, error) {
	p, u, err := settings(ctx, s.Pool, o.Product)
	if err != nil {
		return nil, err
	}
	var from, to *time.Time
	var ids []int64
	subject, body := strings.TrimSpace(o.Subject), strings.TrimSpace(o.Body)
	if o.Period != "" {
		f, t, label, err := ParsePeriod(o.Period, s.now())
		if err != nil {
			return nil, err
		}
		from, to = &f, &t
		if body == "" {
			items, text, err := s.fromContent(ctx, o.Product, f, t)
			if err != nil {
				return nil, err
			}
			if len(items) == 0 {
				return nil, refused("no content was posted in %s; write the text yourself", label)
			}
			ids, body = items, text
		}
		if subject == "" {
			subject = fmt.Sprintf("%s: %s", u.Name, label)
		}
	}
	if subject == "" || body == "" {
		return nil, refused("a broadcast needs a subject and text, or a period to build them from")
	}
	if ids == nil {
		ids = []int64{}
	}
	var id int64
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO updates_broadcasts (product_id, subject, body, flags, period_from, period_to, content_ids, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8) RETURNING id`,
		o.Product, subject, body, Flags(p, subject, body), from, to, ids, s.now()).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.Broadcast(ctx, id)
}

// fromContent builds a body from the content posted in [from, to): the
// newsletter items if there are any, else every posted item.
func (s *Service) fromContent(ctx context.Context, productID string, from, to time.Time) ([]int64, string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, platform, body FROM content_items
		WHERE product_id = $1 AND status = 'posted' AND posted_at >= $2 AND posted_at < $3 ORDER BY posted_at, id`, productID, from, to)
	if err != nil {
		return nil, "", err
	}
	type item struct {
		id             int64
		platform, body string
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (item, error) {
		var it item
		return it, r.Scan(&it.id, &it.platform, &it.body)
	})
	if err != nil {
		return nil, "", err
	}
	news := false
	for _, it := range all {
		news = news || it.platform == "newsletter"
	}
	var ids []int64
	var parts []string
	for _, it := range all {
		if news && it.platform != "newsletter" {
			continue
		}
		ids = append(ids, it.id)
		parts = append(parts, strings.TrimSpace(it.body))
	}
	return ids, strings.Join(parts, "\n\n"), nil
}

// ParsePeriod reads this-month, last-month, YYYY-MM or a trailing window
// like 30d, as [from, to) with a label for the subject.
func ParsePeriod(s string, now time.Time) (from, to time.Time, label string, err error) {
	s = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(s, " ", "-")))
	month := func(t time.Time) (time.Time, time.Time, string) {
		f := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
		return f, f.AddDate(0, 1, 0), f.Format("January 2006")
	}
	switch {
	case s == "this-month":
		from, to, label = month(now)
	case s == "last-month":
		from, to, label = month(now.AddDate(0, -1, 0))
	default:
		if t, e := time.ParseInLocation("2006-01", s, now.Location()); e == nil {
			from, to, label = month(t)
		} else if d, e := product.AfterDuration(s); e == nil {
			from, to, label = now.Add(-d), now.Add(time.Second), "the last "+s
		} else {
			err = refused("period must be this-month, last-month, YYYY-MM or a window like 30d")
		}
	}
	return
}

// EditBroadcast changes a draft's subject or text (nil leaves one as it is).
// Editing an approved broadcast withdraws the approval and drops what was
// queued for it. Once sending has started it can no longer be edited.
func (s *Service) EditBroadcast(ctx context.Context, id int64, subject, body *string) (*Broadcast, error) {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var pid, subj, text, status string
		if err := tx.QueryRow(ctx, `SELECT product_id, subject, body, status FROM updates_broadcasts WHERE id = $1 FOR UPDATE`, id).
			Scan(&pid, &subj, &text, &status); err != nil {
			return refused("no broadcast %d", id)
		}
		if status == "cancelled" {
			return refused("broadcast %d is cancelled", id)
		}
		var claimed int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM updates_messages WHERE broadcast_id = $1 AND status IN ('sending', 'sent', 'unknown', 'failed')`, id).Scan(&claimed); err != nil {
			return err
		}
		if claimed > 0 {
			return refused("broadcast %d has started sending and can no longer be edited; write a new one", id)
		}
		if subject != nil {
			subj = strings.TrimSpace(*subject)
		}
		if body != nil {
			text = strings.TrimSpace(*body)
		}
		if subj == "" || text == "" {
			return refused("a broadcast needs a subject and text")
		}
		p, _, err := settings(ctx, tx, pid)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE updates_broadcasts SET subject = $2, body = $3, flags = $4, status = 'draft', approved_hash = '', approved_at = NULL, updated_at = $5 WHERE id = $1`,
			id, subj, text, Flags(p, subj, text), s.now()); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM updates_messages WHERE broadcast_id = $1 AND status IN ('queued', 'skipped')`, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Broadcast(ctx, id)
}

// ApproveBroadcast approves the broadcast exactly as it stands and queues
// it for everyone confirmed right now. Copy with flags (a forbidden claim, a
// placeholder) and a missing postal address are refused.
func (s *Service) ApproveBroadcast(ctx context.Context, id int64) (*Broadcast, error) {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var pid, subject, body, status string
		if err := tx.QueryRow(ctx, `SELECT product_id, subject, body, status FROM updates_broadcasts WHERE id = $1 FOR UPDATE`, id).
			Scan(&pid, &subject, &body, &status); err != nil {
			return refused("no broadcast %d", id)
		}
		if status == "approved" {
			return nil
		}
		if status != "draft" {
			return refused("broadcast %d is %s, not a draft", id, status)
		}
		p, u, err := settings(ctx, tx, pid)
		if err != nil {
			return err
		}
		if u.PostalAddress == "" {
			return refused("set updates.postal_address in %s's product.yaml first: commercial email law requires your postal address in every message, and no broadcast is sent without it", pid)
		}
		if flags := Flags(p, subject, body); len(flags) > 0 {
			return refused("broadcast %d has problems and cannot be approved until they are fixed: %s", id, strings.Join(flags, "; "))
		}
		if _, err := tx.Exec(ctx, `UPDATE updates_broadcasts SET status = 'approved', approved_hash = $2, approved_at = $3, updated_at = $3, flags = '{}' WHERE id = $1`,
			id, broadcastHash(u, subject, body), s.now()); err != nil {
			return err
		}
		// One row per confirmed subscriber: the unique index is what makes
		// a second delivery impossible.
		if _, err := tx.Exec(ctx, `
			INSERT INTO updates_messages (product_id, subscriber_id, kind, broadcast_id)
			SELECT product_id, id, 'broadcast', $2 FROM updates_subscribers WHERE product_id = $1 AND status = 'confirmed'
			ON CONFLICT DO NOTHING`, pid, id); err != nil {
			return err
		}
		if s.Kick != nil {
			return s.Kick(ctx, tx)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Broadcast(ctx, id)
}

// CancelBroadcast stops a broadcast: nothing queued for it is sent. What
// already went out stays recorded.
func (s *Service) CancelBroadcast(ctx context.Context, id int64) (*Broadcast, error) {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM updates_broadcasts WHERE id = $1 FOR UPDATE`, id).Scan(&status); err != nil {
			return refused("no broadcast %d", id)
		}
		if _, err := tx.Exec(ctx, `UPDATE updates_broadcasts SET status = 'cancelled', approved_hash = '', updated_at = $2 WHERE id = $1`, id, s.now()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE updates_messages SET status = 'skipped', error = 'broadcast cancelled' WHERE broadcast_id = $1 AND status = 'queued'`, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Broadcast(ctx, id)
}

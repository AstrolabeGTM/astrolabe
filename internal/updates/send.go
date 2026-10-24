package updates

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

// A claimed send that has no recorded result after this long is assumed
// interrupted and marked unknown.
const sendingTimeout = 10 * time.Minute

// A message the provider certainly did not take is tried again this many
// times, then marked failed.
const maxAttempts = 3

// perRun bounds one Process call.
const perRun = 500

type Result struct {
	Sent    int `json:"sent"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
	Expired int `json:"expired"`
}

// Process expires old requests, marks interrupted sends unknown, and sends
// what is queued: confirmations and welcomes first, then broadcasts up to
// each product's per_day. It is safe to run repeatedly and concurrently.
func (s *Service) Process(ctx context.Context) (Result, error) {
	var res Result
	n, err := s.expire(ctx)
	if err != nil {
		return res, err
	}
	res.Expired = int(n)
	if _, err := s.Pool.Exec(ctx, `
		UPDATE updates_messages SET status = 'unknown', error = 'no result recorded; the process may have stopped mid-send. Check the mailbox.'
		WHERE status = 'sending' AND claimed_at < $1`, s.now().Add(-sendingTimeout)); err != nil {
		return res, err
	}
	for range perRun {
		c, progressed, err := s.claimNext(ctx)
		if err != nil {
			return res, err
		}
		if c == nil {
			if progressed {
				res.Skipped++
				continue
			}
			break
		}
		switch out := s.deliver(ctx, c); out {
		case "sent", "unknown":
			res.Sent++
		case "retry":
			// The provider refused before accepting anything (bad
			// credentials, say); trying the rest now would only repeat it.
			res.Failed++
			return res, nil
		default:
			res.Failed++
		}
	}
	return res, nil
}

type outbound struct {
	id    int64
	msg   channel.Message
	kind  string
	tries int
}

// claimNext finds the next message that may go out and, in one transaction
// under the shared send lock, rechecks everything and claims it as
// "sending". progressed is true when it only skipped a message that could
// no longer be sent.
func (s *Service) claimNext(ctx context.Context) (out *outbound, progressed bool, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// The same lock as outreach sends: one sender at a time, so the
		// counts below are exact.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('astrolabe:send'))`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT DISTINCT product_id FROM updates_messages WHERE status = 'queued' ORDER BY 1`)
		if err != nil {
			return err
		}
		pids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, pid := range pids {
			p, u, err := settings(ctx, tx, pid)
			if err != nil {
				if errors.Is(err, ErrRefused) {
					continue // the list was removed from product.yaml; messages wait
				}
				return err
			}
			if scope, err := outreach.PausedFor(ctx, tx, pid); err != nil {
				return err
			} else if scope != "" {
				continue
			}
			var id int64
			err = tx.QueryRow(ctx, `
				SELECT id FROM updates_messages WHERE product_id = $1 AND status = 'queued' AND kind <> 'broadcast'
				ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`, pid).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				// Broadcasts need the postal address and room under per_day.
				if u.PostalAddress == "" {
					continue
				}
				var used int
				if err := tx.QueryRow(ctx, `
					SELECT count(*) FROM updates_messages WHERE product_id = $1 AND kind = 'broadcast'
					AND status IN ('sending', 'sent', 'unknown') AND claimed_at > $2`, pid, s.now().Add(-24*time.Hour)).Scan(&used); err != nil {
					return err
				}
				if used >= u.PerDay {
					continue
				}
				err = tx.QueryRow(ctx, `
					SELECT id FROM updates_messages WHERE product_id = $1 AND status = 'queued' AND kind = 'broadcast'
					ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`, pid).Scan(&id)
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
			}
			if err != nil {
				return err
			}
			out, progressed, err = s.claimOne(ctx, tx, p, u, id)
			return err
		}
		return nil
	})
	return out, progressed, err
}

func (s *Service) claimOne(ctx context.Context, tx pgx.Tx, p *product.Product, u product.Updates, id int64) (*outbound, bool, error) {
	var (
		kind, subStatus, email string
		subID, personID        int64
		bcID                   *int64
		expires                *time.Time
		requests, attempts     int
	)
	if err := tx.QueryRow(ctx, `
		SELECT m.kind, m.subscriber_id, m.broadcast_id, m.attempts, x.status, x.email, x.person_id, x.expires_at, x.requests
		FROM updates_messages m JOIN updates_subscribers x ON x.id = m.subscriber_id WHERE m.id = $1`, id).
		Scan(&kind, &subID, &bcID, &attempts, &subStatus, &email, &personID, &expires, &requests); err != nil {
		return nil, false, err
	}
	skip := func(reason string) (*outbound, bool, error) {
		_, err := tx.Exec(ctx, `UPDATE updates_messages SET status = 'skipped', error = $2 WHERE id = $1`, id, reason)
		return nil, true, err
	}
	var suppressed string
	if err := tx.QueryRow(ctx, `SELECT reason FROM suppressions WHERE person_id = $1`, personID).Scan(&suppressed); err == nil {
		return skip("do-not-contact: " + suppressed)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	if ok, reason, err := outreach.MayEmail(ctx, tx, p.ID, personID); err != nil {
		return nil, false, err
	} else if !ok {
		return skip("not sent, " + reason)
	}
	switch kind {
	case "confirm":
		if subStatus != "pending" || expires == nil || !expires.After(s.now()) {
			return skip("no longer waiting for confirmation (" + subStatus + ")")
		}
	default:
		if subStatus != "confirmed" {
			return skip("not subscribed (" + subStatus + ")")
		}
	}
	var subject, body string
	data := product.UpdatesTemplateData{List: u.Name, Product: p.Name, Email: email, Expires: expiresText(u.ConfirmWithin)}
	switch kind {
	case "confirm":
		subject, _ = product.RenderUpdates(u.ConfirmSubject, data)
		body, _ = product.RenderUpdates(u.ConfirmBody, data)
	case "welcome":
		subject, _ = product.RenderUpdates(u.WelcomeSubject, data)
		body, _ = product.RenderUpdates(u.WelcomeBody, data)
	case "broadcast":
		var bstatus, hash string
		if err := tx.QueryRow(ctx, `SELECT subject, body, status, approved_hash FROM updates_broadcasts WHERE id = $1 FOR UPDATE`, *bcID).
			Scan(&subject, &body, &bstatus, &hash); err != nil {
			return nil, false, err
		}
		if bstatus != "approved" {
			return skip("broadcast is " + bstatus)
		}
		if broadcastHash(u, subject, body) != hash {
			// Changed (or the postal address did) since it was approved.
			if _, err := tx.Exec(ctx, `UPDATE updates_broadcasts SET status = 'draft', approved_hash = '', approved_at = NULL, updated_at = $2 WHERE id = $1`, *bcID, s.now()); err != nil {
				return nil, false, err
			}
			return skip("changed after approval; approve it again")
		}
		if u.PostalAddress == "" {
			return nil, false, refused("postal_address is required to send a broadcast")
		}
	}
	unsubTok, err := s.token(ctx, "unsubscribe", subID)
	if err != nil {
		return nil, false, err
	}
	msgID := newMessageID(u.From)
	m := channel.Message{
		Channel: "email", Kind: "updates", Via: u.Via, Product: p.ID, From: u.From, To: email,
		Subject: strings.Join(strings.Fields(subject), " "), MessageID: msgID,
		IdempotencyKey: fmt.Sprintf("astrolabe-updates-%d-%d", id, attempts+1),
		Headers:        map[string]string{},
	}
	unsubLine := `reply "unsubscribe" to this email`
	listUnsub := fmt.Sprintf("<mailto:%s?subject=%s>", u.From, url.PathEscape("unsubscribe "+unsubTok))
	if s.PublicURL != "" {
		link := s.PublicURL + "/updates/unsubscribe/" + unsubTok
		unsubLine += ", or open " + link
		listUnsub += ", <" + link + ">"
		m.Headers["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"
	}
	m.Headers["List-Unsubscribe"] = listUnsub
	switch kind {
	case "confirm":
		m.Headers["Auto-Submitted"] = "auto-generated" // an out-of-office reply must not confirm
		how := "To confirm, reply to this email with any message (even just \"yes\")."
		if s.PublicURL != "" {
			tok, err := s.token(ctx, "confirm", subID)
			if err != nil {
				return nil, false, err
			}
			how += "\nOr open this link: " + s.PublicURL + "/updates/confirm/" + tok
		}
		body = strings.TrimSpace(body) + "\n\n" + how + "\n\nNot you? Ignore this email, or " + unsubLine + "."
		if u.PostalAddress != "" {
			body += "\n\n-- \n" + u.PostalAddress
		}
	default:
		body = strings.TrimSpace(body) + "\n\n" + footer(u, unsubLine)
	}
	m.Body = body + "\n"
	if _, err := tx.Exec(ctx, `
		UPDATE updates_messages SET status = 'sending', claimed_at = $2, message_id = $3, attempts = attempts + 1, error = '' WHERE id = $1`,
		id, s.now(), msgID); err != nil {
		return nil, false, err
	}
	return &outbound{id: id, msg: m, kind: kind, tries: attempts + 1}, false, nil
}

func expiresText(w string) string {
	d, err := product.AfterDuration(w)
	if err != nil {
		return w
	}
	if d%(24*time.Hour) == 0 {
		n := int(d / (24 * time.Hour))
		if n == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", n)
	}
	return d.String()
}

// footerTemplate is the fixed end of every broadcast and welcome; approval
// covers it through the hash.
func footer(u product.Updates, unsubLine string) string {
	return fmt.Sprintf("-- \nYou're getting this because you subscribed to %s.\nTo unsubscribe, %s.\n%s", u.Name, unsubLine, u.PostalAddress)
}

// broadcastHash covers exactly what is approved: sender, subject, body and
// the footer's fixed parts. Changing any of them requires approval again.
func broadcastHash(u product.Updates, subject, body string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{"broadcast", u.From, u.Name, u.PostalAddress, subject, body}, "\x00")))
	return hex.EncodeToString(h[:])
}

func newMessageID(sender string) string {
	b := make([]byte, 12)
	rand.Read(b)
	domain := sender[strings.LastIndex(sender, "@")+1:]
	return fmt.Sprintf("<astrolabe.list.%s@%s>", hex.EncodeToString(b), domain)
}

// deliver calls the provider for a claimed message and records the result:
// sent, failed (certainly not sent: queued again, up to maxAttempts) or
// unknown (maybe sent: never retried).
func (s *Service) deliver(ctx context.Context, c *outbound) string {
	sender, err := s.Senders(c.msg)
	var res channel.Result
	if err == nil {
		res, err = sender.Send(ctx, c.msg)
	} else {
		err = channel.NotSent("no sender for %s: %v", c.msg.From, err)
	}
	// Record the result even if the job's context was cancelled mid-send.
	ctx = context.WithoutCancel(ctx)
	switch {
	case err == nil:
		s.Pool.Exec(ctx, `UPDATE updates_messages SET status = 'sent', sent_at = $2, provider_id = $3, error = '' WHERE id = $1 AND status IN ('sending', 'unknown')`,
			c.id, s.now(), res.ProviderID)
		return "sent"
	case errors.Is(err, channel.ErrNotSent):
		status := "queued"
		if c.tries >= maxAttempts {
			status = "failed"
		}
		s.Pool.Exec(ctx, `UPDATE updates_messages SET status = $2, error = $3 WHERE id = $1 AND status = 'sending'`, c.id, status, err.Error())
		if status == "failed" {
			return "failed"
		}
		return "retry"
	default:
		s.Pool.Exec(ctx, `UPDATE updates_messages SET status = 'unknown', error = $2 WHERE id = $1 AND status = 'sending'`,
			c.id, "delivery uncertain: "+err.Error())
		return "unknown"
	}
}

// ---- background jobs ----

type TickArgs struct{}

func (TickArgs) Kind() string { return "updates_tick" }

// TickWorker runs Process.
type TickWorker struct {
	river.WorkerDefaults[TickArgs]
	S *Service
}

func (w *TickWorker) Work(ctx context.Context, _ *river.Job[TickArgs]) error {
	_, err := w.S.Process(ctx)
	return err
}

func (w *TickWorker) Timeout(*river.Job[TickArgs]) time.Duration { return 5 * time.Minute }

func pausedFor(ctx context.Context, s *Service, productID string) (string, error) {
	return outreach.PausedFor(ctx, s.Pool, productID)
}

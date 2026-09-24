package outreach

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

// How long a claimed send may go without a recorded result before it is
// assumed interrupted and marked unknown.
const sendingTimeout = 10 * time.Minute

// Retry delays when a send must wait rather than fail.
const (
	pausedRetry  = 15 * time.Minute
	limitedRetry = time.Hour
)

// SendOutcome says what SendAction did.
type SendOutcome struct {
	Status string        // final action status, or "" if nothing happened
	Retry  time.Duration // >0 when the send should be retried later (paused or over a limit)
	Reason string
}

// SendAction delivers one approved email at most once:
//  1. In one transaction (serialised by an advisory lock), recheck that it is
//     still approved and unchanged, the enrollment is active and on this step,
//     the person is not suppressed, nothing is paused and limits allow it;
//     then claim it as "sending" and commit.
//  2. Call the provider.
//  3. Record sent, failed (certainly not sent) or unknown (maybe sent).
//
// A crash between 2 and 3 leaves "sending", which the sweep turns into
// "unknown". Unknown is never retried automatically.
func (s *Service) SendAction(ctx context.Context, id int64) (SendOutcome, error) {
	var a *Action
	var out SendOutcome
	var kind, lang string
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('astrolabe:send'))`); err != nil {
			return err
		}
		var err error
		a, err = getAction(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if a.Status != "approved" {
			out.Reason = "action is " + a.Status
			return nil
		}
		now := s.now()
		setStatus := func(status, reason string) error {
			out.Status, out.Reason = status, reason
			_, err := tx.Exec(ctx, `UPDATE actions SET status = $2, error = $3, updated_at = $4 WHERE id = $1`, id, status, reason, now)
			return err
		}

		if approvalHash(a.Channel, a.Sender, a.Recipient, a.Subject, a.Body) != a.ApprovedHash {
			if _, err := tx.Exec(ctx, `UPDATE actions SET approved_hash = '', approved_at = NULL WHERE id = $1`, id); err != nil {
				return err
			}
			return setStatus("draft", "changed after approval; approve again")
		}
		var enrollStatus, stopReason string
		var nextStep int
		if err := tx.QueryRow(ctx, `SELECT status, stop_reason, next_step FROM enrollments WHERE id = $1`, a.EnrollmentID).
			Scan(&enrollStatus, &stopReason, &nextStep); err != nil {
			return err
		}
		// Answers to replies go out even though the reply stopped the sequence.
		if a.Kind == "step" && (enrollStatus != "active" || nextStep != a.Step) {
			return setStatus("skipped", fmt.Sprintf("enrollment is %s %s", enrollStatus, stopReason))
		}
		var suppressed string
		err = tx.QueryRow(ctx, `SELECT reason FROM suppressions WHERE person_id = $1`, a.PersonID).Scan(&suppressed)
		if err == nil {
			if err := stopEnrollment(ctx, tx, a.EnrollmentID, "suppressed", now); err != nil {
				return err
			}
			return setStatus("blocked", "person is suppressed: "+suppressed)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if scope, err := pausedScope(ctx, tx, a.ProductID, a.SequenceID); err != nil {
			return err
		} else if scope != "" {
			out.Retry, out.Reason = pausedRetry, "paused: "+scope
			return nil
		}
		prod, err := product.Get(ctx, tx, a.ProductID)
		if err != nil {
			return err
		}
		seq, err := product.GetSequence(ctx, tx, a.ProductID, a.SequenceID)
		if err != nil {
			return err
		}
		kind = seq.Kind
		if a.Step >= 0 && a.Step < len(seq.Steps) {
			lang = seq.Steps[a.Step].Language
		}
		if lang == "" {
			lang = "en"
		}
		if kind == "users" && a.Kind == "step" {
			// Lifecycle messages need the user's permission for the channel:
			// WhatsApp requires an explicit opt-in; others are allowed unless
			// the product said no.
			var allowed *bool
			tx.QueryRow(ctx, `SELECT allowed FROM channel_permissions WHERE product_id = $1 AND person_id = $2 AND channel = $3`,
				a.ProductID, a.PersonID, a.Channel).Scan(&allowed)
			if (allowed != nil && !*allowed) || (a.Channel == "whatsapp" && allowed == nil) {
				return setStatus("blocked", "no permission to message this user on "+a.Channel)
			}
		}
		// The daily limit is for cold outbound volume; the weekly touch cap
		// protects everyone from every sequence.
		var today, week int
		if err := tx.QueryRow(ctx, `
			SELECT
				count(*) FILTER (WHERE a.product_id = $1 AND COALESCE(s.config->>'kind', 'cold') = 'cold' AND a.kind = 'step'
					AND a.claimed_at >= $3::timestamptz - interval '24 hours'),
				count(*) FILTER (WHERE a.person_id = $2 AND COALESCE(a.sent_at, a.claimed_at) >= $3::timestamptz - interval '7 days')
			FROM actions a
			JOIN enrollments e ON e.id = a.enrollment_id
			JOIN sequences s ON s.product_id = e.product_id AND s.id = e.sequence_id
			WHERE a.status IN ('sending', 'sent', 'unknown') AND NOT (a.channel LIKE '%\_task')`,
			a.ProductID, a.PersonID, now).Scan(&today, &week); err != nil {
			return err
		}
		if a.Kind == "reply" {
			today, week = 0, 0 // a conversation they started is not outreach volume
		}
		if kind == "users" {
			today = 0
		}
		if today >= prod.Limits.ColdPerDay {
			out.Retry, out.Reason = limitedRetry, fmt.Sprintf("daily limit of %d reached", prod.Limits.ColdPerDay)
			return nil
		}
		if week >= prod.Limits.TouchesPerPersonPerWeek {
			out.Retry, out.Reason = limitedRetry, fmt.Sprintf("weekly touch limit of %d for this person reached", prod.Limits.TouchesPerPersonPerWeek)
			return nil
		}

		if a.Channel == "email" {
			a.MessageID = newMessageID(a.Sender)
		}
		_, err = tx.Exec(ctx, `
			UPDATE actions SET status = 'sending', claimed_at = $2, message_id = $3, updated_at = $2 WHERE id = $1`,
			id, now, a.MessageID)
		out.Status = "sending"
		return err
	})
	if err != nil || out.Status != "sending" {
		return out, err
	}

	// Past this point the action is claimed: every path records a final state.
	var res channel.Result
	msg := channel.Message{
		Channel: a.Channel, Kind: kind, Product: a.ProductID,
		From: a.Sender, To: a.Recipient, Subject: a.Subject, Body: a.Body,
		MessageID: a.MessageID, ThreadID: a.ThreadID, InReplyTo: a.InReplyTo,
		IdempotencyKey: fmt.Sprintf("astrolabe-action-%d", a.ID), Language: lang,
	}
	sender, err := s.Senders(msg)
	if err == nil {
		res, err = sender.Send(ctx, msg)
	} else {
		err = channel.NotSent("no %s sender for %s: %v", a.Channel, a.Sender, err)
	}
	// Record the result even if the job's context was cancelled mid-send.
	ctx = context.WithoutCancel(ctx)
	switch {
	case err == nil:
		out.Status, out.Reason = "sent", ""
		err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				UPDATE actions SET status = 'sent', sent_at = $2, provider_id = $3,
					thread_id = CASE WHEN $4 = '' THEN thread_id ELSE $4 END, error = '', updated_at = $2
				WHERE id = $1 AND status IN ('sending', 'unknown')`, id, s.now(), res.ProviderID, res.ThreadID); err != nil {
				return err
			}
			return s.afterSent(ctx, tx, a)
		})
	case errors.Is(err, channel.ErrNotSent):
		out.Status, out.Reason = "failed", err.Error()
		_, err = s.Pool.Exec(ctx, `UPDATE actions SET status = 'failed', error = $2, updated_at = $3 WHERE id = $1 AND status = 'sending'`,
			id, out.Reason, s.now())
	default:
		out.Status, out.Reason = "unknown", "delivery uncertain: "+err.Error()
		_, err = s.Pool.Exec(ctx, `UPDATE actions SET status = 'unknown', error = $2, updated_at = $3 WHERE id = $1 AND status = 'sending'`,
			id, out.Reason, s.now())
	}
	return out, err
}

func newMessageID(sender string) string {
	b := make([]byte, 12)
	rand.Read(b)
	domain := sender[strings.LastIndex(sender, "@")+1:]
	return fmt.Sprintf("<astrolabe.%s@%s>", hex.EncodeToString(b), domain)
}

// sweep marks sends that were claimed but never finished as unknown.
func (s *Service) sweep(ctx context.Context) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE actions SET status = 'unknown', error = 'no result recorded; the process may have stopped mid-send. Check the mailbox.',
			updated_at = $1
		WHERE status = 'sending' AND claimed_at < $2`, s.now(), s.now().Add(-sendingTimeout))
	return tag.RowsAffected(), err
}

// SendWorker runs SendAction for queued jobs.
type SendWorker struct {
	river.WorkerDefaults[SendArgs]
	S *Service
}

func (w *SendWorker) Work(ctx context.Context, job *river.Job[SendArgs]) error {
	out, err := w.S.SendAction(ctx, job.Args.ActionID)
	if err != nil {
		return err
	}
	if out.Retry > 0 {
		return river.JobSnooze(out.Retry)
	}
	return nil
}

func (w *SendWorker) Timeout(*river.Job[SendArgs]) time.Duration { return 2 * time.Minute }

type TickArgs struct{}

func (TickArgs) Kind() string { return "tick" }

// TickWorker drafts due steps, sweeps stale sends and re-queues approved
// messages whose job was lost.
type TickWorker struct {
	river.WorkerDefaults[TickArgs]
	S *Service
}

func (w *TickWorker) Work(ctx context.Context, _ *river.Job[TickArgs]) error {
	if _, err := w.S.Tick(ctx); err != nil {
		return err
	}
	rows, err := w.S.Pool.Query(ctx, `SELECT id FROM actions WHERE status = 'approved' AND approved_at < $1`, w.S.now().Add(-5*time.Minute))
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := w.S.River.Insert(ctx, SendArgs{ActionID: id}, nil); err != nil {
			return err
		}
	}
	return nil
}

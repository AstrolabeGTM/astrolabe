package outreach

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/funnel"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

type Action struct {
	ID           int64      `json:"id"`
	EnrollmentID int64      `json:"enrollment_id"`
	Step         int        `json:"step"`
	SequenceID   string     `json:"sequence"`
	ProductID    string     `json:"product"`
	PersonID     int64      `json:"person_id"`
	PersonName   string     `json:"person_name"`
	Company      string     `json:"company,omitempty"`
	Channel      string     `json:"channel"`
	Sender       string     `json:"sender,omitempty"`
	Recipient    string     `json:"recipient"`
	Subject      string     `json:"subject,omitempty"`
	Body         string     `json:"body"`
	Goal         string     `json:"goal"`
	Status       string     `json:"status"`
	Error        string     `json:"error,omitempty"`
	ThreadID     string     `json:"-"`
	InReplyTo    string     `json:"-"`
	MessageID    string     `json:"-"`
	ApprovedHash string     `json:"-"`
	Kind         string     `json:"kind"` // step or reply
	ReplyID      *int64     `json:"reply_id,omitempty"`
	AIState      string     `json:"ai_state,omitempty"` // pending while Claude drafts it
	AIBody       string     `json:"-"`
	Flags        []string   `json:"flags,omitempty"` // problems found in an AI draft
	Variant      string     `json:"variant,omitempty"`
	LinkCode     *string    `json:"link_code,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	SentAt       *time.Time `json:"sent_at,omitempty"`
}

const actionSelect = `
	SELECT a.id, a.enrollment_id, a.step, e.sequence_id, a.product_id, a.person_id,
		COALESCE(NULLIF(p.name, ''), a.recipient), COALESCE(NULLIF(c.name, ''), c.domain, ''),
		a.channel, a.sender, a.recipient, a.subject, a.body, a.goal, a.status, a.error,
		a.thread_id, a.in_reply_to, a.message_id, a.approved_hash, a.kind, a.reply_id, a.ai_state, a.ai_body, a.flags,
		a.variant, a.link_code, a.created_at, a.sent_at
	FROM actions a
	JOIN enrollments e ON e.id = a.enrollment_id
	JOIN people p ON p.id = a.person_id
	LEFT JOIN companies c ON c.id = p.company_id`

func scanAction(row pgx.Row) (*Action, error) {
	a := &Action{}
	err := row.Scan(&a.ID, &a.EnrollmentID, &a.Step, &a.SequenceID, &a.ProductID, &a.PersonID,
		&a.PersonName, &a.Company, &a.Channel, &a.Sender, &a.Recipient, &a.Subject, &a.Body, &a.Goal,
		&a.Status, &a.Error, &a.ThreadID, &a.InReplyTo, &a.MessageID, &a.ApprovedHash, &a.Kind, &a.ReplyID, &a.AIState, &a.AIBody, &a.Flags,
		&a.Variant, &a.LinkCode, &a.CreatedAt, &a.SentAt)
	return a, err
}

func getAction(ctx context.Context, q store.Q, id int64, lock bool) (*Action, error) {
	sql := actionSelect + ` WHERE a.id = $1`
	if lock {
		sql += ` FOR UPDATE OF a`
	}
	a, err := scanAction(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, invalid("no action %d", id)
	}
	return a, err
}

func (s *Service) Action(ctx context.Context, id int64) (*Action, error) {
	return getAction(ctx, s.Pool, id, false)
}

// Actions lists actions in the given statuses, oldest first.
func (s *Service) Actions(ctx context.Context, productID string, statuses ...string) ([]*Action, error) {
	rows, err := s.Pool.Query(ctx, actionSelect+`
		WHERE a.status = ANY($1) AND ($2 = '' OR a.product_id = $2)
		ORDER BY a.created_at, a.id LIMIT 200`, statuses, productID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Action
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Edit replaces a draft's text. Editing an approved message withdraws the
// approval; it must be approved again.
func (s *Service) Edit(ctx context.Context, id int64, subject, body string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		a, err := getAction(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if a.Status != "draft" && a.Status != "approved" {
			return invalid("action %d is %s and can no longer be edited", id, a.Status)
		}
		if a.Channel == "email" && a.Step > 0 && a.ThreadID != "" && subject != a.Subject {
			return invalid("follow-ups keep the thread subject %q", a.Subject)
		}
		// My edit of an AI draft becomes an example of the product's voice.
		if a.AIBody != "" && body != a.AIBody && body != a.Body {
			if _, err := tx.Exec(ctx, `
				INSERT INTO voice_examples (product_id, action_id, original, edited) VALUES ($1, $2, $3, $4)`,
				a.ProductID, id, a.AIBody, body); err != nil {
				return err
			}
		}
		// An edit made while Claude is drafting wins: the draft job only fills
		// actions still marked pending.
		_, err = tx.Exec(ctx, `
			UPDATE actions SET subject = $2, body = $3, status = 'draft', approved_hash = '', approved_at = NULL, updated_at = $4,
				ai_state = CASE WHEN ai_state = 'pending' THEN '' ELSE ai_state END
			WHERE id = $1`, id, subject, body, s.now())
		return err
	})
}

// FillDraft stores an AI-written draft if the action is still an untouched
// pending draft. It returns false if I edited or resolved it meanwhile.
func (s *Service) FillDraft(ctx context.Context, id int64, subject, body string, flags []string) (bool, error) {
	if flags == nil {
		flags = []string{}
	}
	tag, err := s.Pool.Exec(ctx, `
		UPDATE actions SET subject = CASE WHEN $2 = '' THEN subject ELSE $2 END, body = $3, ai_body = $3, flags = $4,
			ai_state = 'done', updated_at = $5
		WHERE id = $1 AND status = 'draft' AND ai_state = 'pending'`, id, subject, body, flags, s.now())
	return tag.RowsAffected() == 1, err
}

// DraftFailed marks AI drafting as failed so the draft shows as needing me.
func (s *Service) DraftFailed(ctx context.Context, id int64, reason string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE actions SET ai_state = 'failed', error = $2 WHERE id = $1 AND ai_state = 'pending'`, id, reason)
	return err
}

// ApproveIfAllowed approves an AI draft when the product's auto_approve allows
// it (auto_approve): none never; follow_ups for steps after the first;
// all for every step. Drafts with flags always wait for me. It goes
// through Approve, so every approval check still applies.
func (s *Service) ApproveIfAllowed(ctx context.Context, id int64) (bool, error) {
	a, err := s.Action(ctx, id)
	if err != nil {
		return false, err
	}
	if a.Status != "draft" || a.AIState != "done" || len(a.Flags) > 0 || a.Kind != "step" || product.ManualChannel(a.Channel) {
		return false, nil
	}
	prod, err := product.Get(ctx, s.Pool, a.ProductID)
	if err != nil {
		return false, err
	}
	seq, err := product.GetSequence(ctx, s.Pool, a.ProductID, a.SequenceID)
	if err != nil {
		return false, err
	}
	mode := prod.AutoApprove.Cold
	if seq.Kind == "users" {
		mode = prod.AutoApprove.Users
	}
	switch {
	case mode == "all":
	case mode == "follow_ups" && a.Step > 0:
	default:
		return false, nil
	}
	if err := s.Approve(ctx, id); err != nil {
		if errors.Is(err, ErrInvalid) {
			return false, nil // e.g. a never-claim: leave it for me
		}
		return false, err
	}
	return true, nil
}

// AnswerReply creates (or returns) a draft email answering a reply, in the
// same thread. It is approved and sent like any other message.
func (s *Service) AnswerReply(ctx context.Context, replyID int64, body string) (int64, error) {
	var id int64
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var enrollmentID, personID, actionID *int64
		var productID *string
		var subject, threadID, msgID, mailbox string
		if err := tx.QueryRow(ctx, `
			SELECT enrollment_id, person_id, product_id, action_id, subject, thread_id, message_id, mailbox
			FROM replies WHERE id = $1`, replyID).Scan(&enrollmentID, &personID, &productID, &actionID, &subject, &threadID, &msgID, &mailbox); err != nil {
			return invalid("no reply %d", replyID)
		}
		if enrollmentID == nil || personID == nil {
			return invalid("reply %d is not from someone I contacted; answer it in the mailbox", replyID)
		}
		var recipient string
		tx.QueryRow(ctx, `SELECT recipient FROM actions WHERE id = $1`, actionID).Scan(&recipient)
		if recipient == "" {
			return invalid("reply %d has no address to answer", replyID)
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO actions (enrollment_id, step, product_id, person_id, channel, sender, recipient, subject, body, goal,
				thread_id, in_reply_to, kind, reply_id)
			VALUES ($1, $2, $3, $4, 'email', $5, $6, $7, $8, 'answer their reply', $9, $10, 'reply', $11)
			ON CONFLICT (enrollment_id, step) DO UPDATE SET body = CASE WHEN actions.status = 'draft' THEN EXCLUDED.body ELSE actions.body END
			RETURNING id`,
			*enrollmentID, -replyID, *productID, *personID, mailbox, recipient, "Re: "+strings.TrimPrefix(subject, "Re: "), body,
			threadID, msgID, replyID).Scan(&id)
		return err
	})
	return id, err
}

type SendArgs struct {
	ActionID int64 `json:"action_id"`
}

func (SendArgs) Kind() string { return "send_action" }

func (SendArgs) InsertOpts() river.InsertOpts {
	// Unique only while a job is pending: after "return to draft" and a fresh
	// approval, a new job must be insertable. The action row, not the job,
	// is what prevents a second send.
	return river.InsertOpts{Queue: "send", MaxAttempts: 25, UniqueOpts: river.UniqueOpts{
		ByArgs: true,
		ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending,
			rivertype.JobStateRetryable, rivertype.JobStateRunning, rivertype.JobStateScheduled},
	}}
}

// Approve approves exactly the message as it stands. Email is queued for
// sending in the same transaction; a manual task is marked done.
func (s *Service) Approve(ctx context.Context, id int64) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		a, err := getAction(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if a.Status == "approved" {
			return nil
		}
		if a.Status != "draft" {
			return invalid("action %d is %s, not a draft", id, a.Status)
		}
		if strings.TrimSpace(a.Body) == "" {
			return invalid("action %d has no text yet", id)
		}
		prod, err := product.Get(ctx, tx, a.ProductID)
		if err != nil {
			return err
		}
		lower := strings.ToLower(a.Subject + "\n" + a.Body)
		for _, never := range prod.Claims.Never {
			if strings.Contains(lower, strings.ToLower(never)) {
				return invalid("the message makes a claim %s never allows: %q", a.ProductID, never)
			}
		}

		if product.ManualChannel(a.Channel) {
			// I send these by hand; approving means it is done.
			if _, err := tx.Exec(ctx, `
				UPDATE actions SET status = 'sent', approved_at = $2, sent_at = $2, updated_at = $2 WHERE id = $1`,
				id, s.now()); err != nil {
				return err
			}
			return s.afterSent(ctx, tx, a)
		}

		if a.Recipient == "" || a.Sender == "" {
			return invalid("action %d has no recipient or sender", id)
		}
		if a.Step == 0 || a.ThreadID == "" {
			if strings.TrimSpace(a.Subject) == "" {
				return invalid("action %d needs a subject", id)
			}
		}
		hash := approvalHash(a.Channel, a.Sender, a.Recipient, a.Subject, a.Body)
		if _, err := tx.Exec(ctx, `
			UPDATE actions SET status = 'approved', approved_hash = $2, approved_at = $3, error = '', updated_at = $3
			WHERE id = $1`, id, hash, s.now()); err != nil {
			return err
		}
		_, err = s.River.InsertTx(ctx, tx, SendArgs{ActionID: id}, nil)
		return err
	})
}

// Skip drops this step and moves the enrollment on.
func (s *Service) Skip(ctx context.Context, id int64) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		a, err := getAction(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if a.Status != "draft" && a.Status != "approved" && a.Status != "blocked" && a.Status != "failed" {
			return invalid("action %d is %s and cannot be skipped", id, a.Status)
		}
		if _, err := tx.Exec(ctx, `UPDATE actions SET status = 'skipped', updated_at = $2 WHERE id = $1`, id, s.now()); err != nil {
			return err
		}
		return s.advance(ctx, tx, a.EnrollmentID, a.Step)
	})
}

// Resolve settles an unknown or failed send after I have checked the mailbox:
// sent=true records it as sent (with Gmail's ids when the lookup found it, so
// follow-ups stay in its thread); sent=false returns it to draft for a fresh
// approval. Nothing is ever resent without that approval.
func (s *Service) Resolve(ctx context.Context, id int64, sent bool, found *channel.Result) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		a, err := getAction(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if a.Status != "unknown" && a.Status != "failed" {
			return invalid("action %d is %s; only unknown or failed sends need resolving", id, a.Status)
		}
		if !sent {
			_, err := tx.Exec(ctx, `
				UPDATE actions SET status = 'draft', approved_hash = '', approved_at = NULL, message_id = '',
					error = 'returned to draft after ' || status, updated_at = $2 WHERE id = $1`, id, s.now())
			return err
		}
		if found == nil {
			found = &channel.Result{}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE actions SET status = 'sent', sent_at = COALESCE(claimed_at, $2), error = '', updated_at = $2,
				provider_id = CASE WHEN $3 = '' THEN provider_id ELSE $3 END,
				thread_id = CASE WHEN $4 = '' THEN thread_id ELSE $4 END
			WHERE id = $1`, id, s.now(), found.ProviderID, found.ThreadID); err != nil {
			return err
		}
		return s.afterSent(ctx, tx, a)
	})
}

// afterSent advances the sequence and records the first touch in the funnel.
func (s *Service) afterSent(ctx context.Context, tx pgx.Tx, a *Action) error {
	if a.Kind == "reply" {
		return nil
	}
	if err := s.advance(ctx, tx, a.EnrollmentID, a.Step); err != nil {
		return err
	}
	prod, err := product.Get(ctx, tx, a.ProductID)
	if err != nil {
		return err
	}
	for _, st := range prod.Funnel {
		if st == "reached" {
			_, err := funnel.Record(ctx, tx, a.ProductID, a.PersonID, "reached", s.now(), "outreach", fmt.Sprintf("reached:%d", a.PersonID))
			return err
		}
	}
	return nil
}

package outreach

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/funnel"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// ---- Pauses ----

// Pause scopes: "global", "product:<id>", "sequence:<product>/<id>".
func (s *Service) Pause(ctx context.Context, scope string) error {
	if err := checkScope(scope); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO pauses (scope) VALUES ($1) ON CONFLICT DO NOTHING`, scope)
	return err
}

func (s *Service) Resume(ctx context.Context, scope string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM pauses WHERE scope = $1`, scope)
	return err
}

func (s *Service) Pauses(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT scope FROM pauses ORDER BY scope`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func checkScope(scope string) error {
	if scope == "global" || strings.HasPrefix(scope, "product:") && len(scope) > 8 ||
		strings.HasPrefix(scope, "sequence:") && strings.Count(scope, "/") == 1 {
		return nil
	}
	return invalid("pause scope must be global, product:<id> or sequence:<product>/<id>")
}

func pausedScope(ctx context.Context, q store.Q, productID, sequenceID string) (string, error) {
	var scope string
	err := q.QueryRow(ctx, `SELECT scope FROM pauses WHERE scope = ANY($1) ORDER BY scope LIMIT 1`,
		[]string{"global", "product:" + productID, "sequence:" + productID + "/" + sequenceID}).Scan(&scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return scope, err
}

// ---- Replies ----

// Inbound is one message found in a sending mailbox.
type Inbound struct {
	Channel    string // email (default) or whatsapp
	Mailbox    string // email address, or "whatsapp:<phone number id>"
	ProviderID string
	ThreadID   string
	MessageID  string   // RFC 822 Message-ID, for threading an answer
	References []string // In-Reply-To and References: our Message-IDs it answers
	From       string   // bare address
	Subject    string
	Snippet    string
	Body       string
	ReceivedAt time.Time
	// Bounce is set for delivery failure notices; FailedRecipients lists the
	// addresses that bounced when the notice says so.
	Bounce           bool
	FailedRecipients []string
}

type Reply struct {
	ID             int64     `json:"id"`
	ProductID      string    `json:"product,omitempty"`
	PersonID       *int64    `json:"person_id,omitempty"`
	PersonName     string    `json:"person_name,omitempty"`
	ActionID       *int64    `json:"action_id,omitempty"`
	From           string    `json:"from"`
	Subject        string    `json:"subject"`
	Snippet        string    `json:"snippet"`
	Body           string    `json:"body"`
	ReceivedAt     time.Time `json:"received_at"`
	Classification string    `json:"classification,omitempty"`
	Suggested      string    `json:"suggested_class,omitempty"`
}

// RecordReply stores an inbound message once and, if it belongs to an
// outreach thread, stops that enrollment. Bounces also suppress the person.
// It returns false if the message was already recorded.
func (s *Service) RecordReply(ctx context.Context, in Inbound) (bool, error) {
	recorded := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var replyID int64
		err := tx.QueryRow(ctx, `
			INSERT INTO replies (provider_message_id, mailbox, thread_id, from_addr, subject, snippet, body, is_bounce, received_at, classification, message_id, refs)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, CASE WHEN $8 THEN 'bounce' ELSE '' END, $10, $11)
			ON CONFLICT (provider_message_id) DO NOTHING RETURNING id`,
			in.ProviderID, in.Mailbox, in.ThreadID, strings.ToLower(in.From), in.Subject, in.Snippet, in.Body, in.Bounce, in.ReceivedAt, in.MessageID, refsOrEmpty(in.References)).Scan(&replyID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		recorded = true

		a, err := s.matchInbound(ctx, tx, in)
		if err != nil || a == nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE replies SET product_id = $2, person_id = $3, enrollment_id = $4, action_id = $5 WHERE id = $1`,
			replyID, a.ProductID, a.PersonID, a.EnrollmentID, a.ID); err != nil {
			return err
		}
		if in.Bounce {
			if err := people.Suppress(ctx, tx, a.PersonID, "bounced", in.Subject); err != nil {
				return err
			}
			return stopEnrollment(ctx, tx, a.EnrollmentID, "bounced", s.now())
		}
		if err := stopEnrollment(ctx, tx, a.EnrollmentID, "replied", s.now()); err != nil {
			return err
		}
		// Messaging opt-out keywords unsubscribe at once.
		switch strings.ToUpper(strings.TrimSpace(in.Body)) {
		case "STOP", "UNSUBSCRIBE", "STOP ALL":
			if _, err := tx.Exec(ctx, `UPDATE replies SET classification = 'unsubscribe', classified_at = now() WHERE id = $1`, replyID); err != nil {
				return err
			}
			return people.Suppress(ctx, tx, a.PersonID, "unsubscribed", in.Channel+" "+in.Body)
		}
		if err := s.recordStage(ctx, tx, a.ProductID, a.PersonID, "replied", in.ReceivedAt, "reply", fmt.Sprintf("replied:%d", a.PersonID)); err != nil {
			return err
		}
		if s.OnReply != nil {
			return s.OnReply(ctx, tx, replyID)
		}
		return nil
	})
	return recorded, err
}

func refsOrEmpty(r []string) []string {
	if r == nil {
		return []string{}
	}
	return r
}

// matchInbound finds the sent action a message answers: same thread first,
// then (for bounces) the failed recipient, then the sender's address.
func (s *Service) matchInbound(ctx context.Context, tx pgx.Tx, in Inbound) (*Action, error) {
	type match struct {
		where string
		arg   any
	}
	ch := in.Channel
	if ch == "" {
		ch = "email"
	}
	var queries []match
	switch {
	case ch == "whatsapp":
		queries = append(queries, match{`a.person_id = (SELECT person_id FROM identities WHERE kind = 'phone' AND value = $1)`, in.From})
	default:
		// Our Message-IDs in In-Reply-To/References, then the provider thread.
		if len(in.References) > 0 {
			queries = append(queries, match{`a.message_id = ANY($1) AND a.message_id <> ''`, in.References})
		}
		queries = append(queries, match{`a.thread_id = $1 AND a.thread_id <> ''`, in.ThreadID})
		if in.Bounce {
			for _, r := range in.FailedRecipients {
				queries = append(queries, match{`a.recipient = $1`, strings.ToLower(r)})
			}
		} else {
			queries = append(queries, match{`a.person_id = (SELECT person_id FROM identities WHERE kind = 'email' AND value = $1)`, strings.ToLower(in.From)})
		}
	}
	for _, q := range queries {
		a, err := scanAction(tx.QueryRow(ctx, actionSelect+`
			WHERE `+q.where+` AND a.channel = $3 AND a.status IN ('sent', 'unknown') AND lower(a.sender) = $2
			ORDER BY a.sent_at DESC NULLS LAST, a.id DESC LIMIT 1`, q.arg, strings.ToLower(in.Mailbox), ch))
		if err == nil {
			return a, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	return nil, nil
}

var Classifications = []string{"interested", "question", "not_now", "no", "out_of_office", "unsubscribe"}

// Classify labels a reply and applies its consequence: "no" and
// "unsubscribe" suppress the person; "interested" and "question" open a
// task due now; "not_now" opens a reminder task (default 30 days).
func (s *Service) Classify(ctx context.Context, replyID int64, class string, remindAt *time.Time) error {
	if !slices.Contains(Classifications, class) {
		return invalid("classification must be one of %v", Classifications)
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var productID *string
		var personID, enrollmentID *int64
		var subject string
		if err := tx.QueryRow(ctx, `
			UPDATE replies SET classification = $2, classified_at = $3 WHERE id = $1
			RETURNING product_id, person_id, enrollment_id, subject`, replyID, class, s.now()).
			Scan(&productID, &personID, &enrollmentID, &subject); errors.Is(err, pgx.ErrNoRows) {
			return invalid("no reply %d", replyID)
		} else if err != nil {
			return err
		}
		if personID == nil || productID == nil {
			return nil // not from anyone we contacted; labelled only
		}
		var next string
		due := s.now()
		switch class {
		case "no":
			return people.Suppress(ctx, tx, *personID, "replied_no", subject)
		case "unsubscribe":
			return people.Suppress(ctx, tx, *personID, "unsubscribed", subject)
		case "interested":
			next = "Reply and agree one next step: " + subject
		case "question":
			next = "Answer their question: " + subject
		case "not_now":
			next = "Check back in (they said not now): " + subject
			due = s.now().AddDate(0, 0, 30)
			if remindAt != nil {
				due = *remindAt
			}
		default:
			return nil
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO tasks (product_id, person_id, enrollment_id, reply_id, next_action, due_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, *productID, *personID, enrollmentID, replyID, next, due)
		return err
	})
}

// Replies lists replies; unclassifiedOnly limits to ones I haven't labelled.
func (s *Service) Replies(ctx context.Context, unclassifiedOnly bool, limit int) ([]*Reply, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT r.id, COALESCE(r.product_id, ''), r.person_id, COALESCE(p.name, ''), r.action_id, r.from_addr,
			r.subject, r.snippet, r.body, r.received_at, r.classification, r.suggested_class
		FROM replies r LEFT JOIN people p ON p.id = r.person_id
		WHERE NOT $1 OR r.classification = ''
		ORDER BY r.received_at DESC LIMIT $2`, unclassifiedOnly, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Reply
	for rows.Next() {
		r := &Reply{}
		if err := rows.Scan(&r.ID, &r.ProductID, &r.PersonID, &r.PersonName, &r.ActionID, &r.From,
			&r.Subject, &r.Snippet, &r.Body, &r.ReceivedAt, &r.Classification, &r.Suggested); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- Tasks ----

type Task struct {
	ID         int64     `json:"id"`
	ProductID  string    `json:"product"`
	PersonID   *int64    `json:"person_id,omitempty"`
	PersonName string    `json:"person_name,omitempty"`
	NextAction string    `json:"next_action"`
	DueAt      time.Time `json:"due_at"`
	Status     string    `json:"status"`
	Outcome    string    `json:"outcome,omitempty"`
	ReplyID    *int64    `json:"reply_id,omitempty"`
	Draft      string    `json:"draft,omitempty"`
	PlayID     string    `json:"play,omitempty"`
	Evidence   string    `json:"evidence_url,omitempty"`
	// AnswerID is the draft email answering the reply, if one exists.
	AnswerID *int64 `json:"answer_action_id,omitempty"`
}

var Outcomes = []string{"activated", "paid", "wrong_fit", "unclear_value", "setup_blocked", "price", "later", "other"}

func (s *Service) CreateTask(ctx context.Context, productID string, personID *int64, nextAction string, due time.Time) (int64, error) {
	return s.CreateTaskTx(ctx, s.Pool, TaskOpts{Product: productID, PersonID: personID, NextAction: nextAction, Due: due})
}

type TaskOpts struct {
	Product    string
	PersonID   *int64
	NextAction string
	Due        time.Time
	PlayID     string
	SignalID   *int64
}

func (s *Service) CreateTaskTx(ctx context.Context, q store.Q, o TaskOpts) (int64, error) {
	if strings.TrimSpace(o.NextAction) == "" {
		return 0, invalid("a task needs a next action")
	}
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO tasks (product_id, person_id, next_action, due_at, play_id, signal_id) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		o.Product, o.PersonID, o.NextAction, o.Due, o.PlayID, o.SignalID).Scan(&id)
	return id, err
}

// SetTaskDraft stores suggested text for a task (e.g. a community reply).
func (s *Service) SetTaskDraft(ctx context.Context, id int64, draft string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE tasks SET draft = $2 WHERE id = $1`, id, draft)
	return err
}

// SetSuggestion records the classifier's guess for a reply I haven't sorted.
func (s *Service) SetSuggestion(ctx context.Context, replyID int64, class string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE replies SET suggested_class = $2 WHERE id = $1`, replyID, class)
	return err
}

// AutoClassify applies a confident classification as if I had chosen it.
func (s *Service) AutoClassify(ctx context.Context, replyID int64, class string, remindAt *time.Time) error {
	if err := s.Classify(ctx, replyID, class, remindAt); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `UPDATE replies SET auto_classified = true WHERE id = $1`, replyID)
	return err
}

// CloseTask records the outcome. "activated" and "paid" also enter the
// funnel (the product's activation stage, and "paid" if the funnel has it).
func (s *Service) CloseTask(ctx context.Context, id int64, outcome, note string, minutes *int) error {
	if !slices.Contains(Outcomes, outcome) {
		return invalid("outcome must be one of %v", Outcomes)
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var productID string
		var personID *int64
		err := tx.QueryRow(ctx, `
			UPDATE tasks SET status = 'done', outcome = $2, outcome_note = $3, minutes_spent = $4, closed_at = $5
			WHERE id = $1 AND status = 'open' RETURNING product_id, person_id`, id, outcome, note, minutes, s.now()).Scan(&productID, &personID)
		if errors.Is(err, pgx.ErrNoRows) {
			return invalid("no open task %d", id)
		} else if err != nil {
			return err
		}
		if personID == nil {
			return nil
		}
		prod, err := product.Get(ctx, tx, productID)
		if err != nil {
			return err
		}
		stages := map[string]string{"activated": prod.Activation, "paid": "paid"}
		if st, ok := stages[outcome]; ok && slices.Contains(prod.Funnel, st) {
			return s.recordStage(ctx, tx, productID, *personID, st, s.now(), "task", fmt.Sprintf("%s:%d", st, *personID))
		}
		return nil
	})
}

func (s *Service) Tasks(ctx context.Context, openOnly bool, limit int) ([]*Task, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT t.id, t.product_id, t.person_id, COALESCE(p.name, ''), t.next_action, t.due_at, t.status, t.outcome, t.reply_id,
			t.draft, t.play_id, COALESCE(sg.evidence_url, ''),
			(SELECT a.id FROM actions a WHERE a.reply_id = t.reply_id AND a.kind = 'reply' AND t.reply_id IS NOT NULL LIMIT 1)
		FROM tasks t LEFT JOIN people p ON p.id = t.person_id LEFT JOIN signals sg ON sg.id = t.signal_id
		WHERE NOT $1 OR t.status = 'open'
		ORDER BY t.status, t.due_at LIMIT $2`, openOnly, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t := &Task{}
		if err := rows.Scan(&t.ID, &t.ProductID, &t.PersonID, &t.PersonName, &t.NextAction, &t.DueAt, &t.Status, &t.Outcome, &t.ReplyID,
			&t.Draft, &t.PlayID, &t.Evidence, &t.AnswerID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- Funnel stages ----

// MarkStage records that a person reached a funnel stage (by hand in M1;
// product events later). Enrollments that stop on that stage are stopped.
func (s *Service) MarkStage(ctx context.Context, productID string, personID int64, stage string, at time.Time) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		prod, err := product.Get(ctx, tx, productID)
		if errors.Is(err, pgx.ErrNoRows) {
			return invalid("no product %s", productID)
		} else if err != nil {
			return err
		}
		if !slices.Contains(prod.Funnel, stage) {
			return invalid("%s is not a stage of %s (have %v)", stage, productID, prod.Funnel)
		}
		return s.recordStage(ctx, tx, productID, personID, stage, at, "manual", fmt.Sprintf("%s:%d", stage, personID))
	})
}

// RecordStage records a stage from an automatic source (product events,
// Stripe) once per dedupe key, with the same stops as MarkStage.
func (s *Service) RecordStage(ctx context.Context, productID string, personID int64, stage string, at time.Time, source, key string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		return s.recordStage(ctx, tx, productID, personID, stage, at, source, key)
	})
}

func (s *Service) recordStage(ctx context.Context, tx pgx.Tx, productID string, personID int64, stage string, at time.Time, source, key string) error {
	prod, err := product.Get(ctx, tx, productID)
	if err != nil {
		return err
	}
	if !slices.Contains(prod.Funnel, stage) {
		return nil
	}
	if _, err := funnel.Record(ctx, tx, productID, personID, stage, at, source, key); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT e.id, s.config->'stop_on' ? $3 OR e.goal_stage = $3
		FROM enrollments e JOIN sequences s ON s.product_id = e.product_id AND s.id = e.sequence_id
		WHERE e.product_id = $1 AND e.person_id = $2 AND e.status = 'active'`, productID, personID, stage)
	if err != nil {
		return err
	}
	type hit struct {
		id   int64
		stop bool
	}
	hits, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (hit, error) {
		var h hit
		return h, r.Scan(&h.id, &h.stop)
	})
	if err != nil {
		return err
	}
	for _, h := range hits {
		if h.stop {
			if err := stopEnrollment(ctx, tx, h.id, stage, s.now()); err != nil {
				return err
			}
		}
	}
	return nil
}

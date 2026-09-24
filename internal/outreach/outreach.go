// Package outreach runs sequences: enrollments, drafts, approvals, sends,
// replies, tasks and stops. Every state change goes through here so the web
// app and the MCP tools behave the same way.
package outreach

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/experiment"
	"github.com/AstrolabeGTM/astrolabe/internal/links"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

type Service struct {
	Pool    *pgxpool.Pool
	River   *river.Client[pgx.Tx] // used to enqueue sends in the approving transaction
	Senders channel.Senders
	Now     func() time.Time
	// PublicURL is where /l/<code> links resolve; without it drafts link
	// straight to the destination (no click tracking).
	PublicURL string

	// Optional hooks, all run inside the caller's transaction.
	// OnDraftCreated runs for a new empty draft (to queue AI drafting); it
	// returns true if drafting was queued.
	OnDraftCreated func(ctx context.Context, tx pgx.Tx, actionID int64) (bool, error)
	// Recheck runs before each step of an enrollment started by a play and
	// returns a stop reason if the person no longer qualifies.
	Recheck func(ctx context.Context, tx pgx.Tx, enrollmentID int64) (string, error)
	// OnReply runs for a new reply matched to outreach (to queue classification).
	OnReply func(ctx context.Context, tx pgx.Tx, replyID int64) error
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ErrInvalid is returned for requests that break a rule; the message says which.
var ErrInvalid = errors.New("invalid")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

type EnrollResult struct {
	Enrolled []int64  `json:"started"`
	Skipped  []string `json:"skipped,omitempty"`
}

// Enroll puts people into a sequence by hand. The first step is drafted by
// the next tick.
func (s *Service) Enroll(ctx context.Context, productID, sequenceID string, personIDs []int64) (EnrollResult, error) {
	var res EnrollResult
	for _, id := range personIDs {
		var eid int64
		var skip string
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			var err error
			eid, skip, err = s.EnrollTx(ctx, tx, EnrollOpts{Product: productID, Sequence: sequenceID, PersonID: id, Source: "enroll"})
			return err
		})
		if err != nil {
			return res, err
		}
		if skip != "" {
			res.Skipped = append(res.Skipped, fmt.Sprintf("person %d: %s", id, skip))
			continue
		}
		res.Enrolled = append(res.Enrolled, eid)
	}
	return res, nil
}

type EnrollOpts struct {
	Product, Sequence string
	PersonID          int64
	PlayID            string
	SignalID          *int64
	Delay             time.Duration // before the first step
	GoalStage         string        // reaching it stops the sequence
	Source            string        // product_people source if new
}

// EnrollTx enrolls one person inside a transaction. It returns a skip reason
// instead of an error when the person cannot be enrolled.
func (s *Service) EnrollTx(ctx context.Context, tx pgx.Tx, o EnrollOpts) (int64, string, error) {
	seq, err := product.GetSequence(ctx, tx, o.Product, o.Sequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", invalid("no sequence %s/%s", o.Product, o.Sequence)
	} else if err != nil {
		return 0, "", err
	}
	p, err := people.Get(ctx, tx, o.PersonID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "not found", nil
	} else if err != nil {
		return 0, "", err
	}
	if p.Suppressed != "" {
		return 0, "suppressed (" + p.Suppressed + ")", nil
	}
	first, _ := product.AfterDuration(seq.Steps[0].After)
	var eid int64
	err = tx.QueryRow(ctx, `
		INSERT INTO enrollments (product_id, person_id, sequence_id, next_due_at, started_at, play_id, signal_id, goal_stage)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING RETURNING id`,
		o.Product, o.PersonID, o.Sequence, s.now().Add(o.Delay+first), s.now().Add(o.Delay), o.PlayID, o.SignalID, o.GoalStage).Scan(&eid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "already in an active " + o.Product + " sequence", nil
	} else if err != nil {
		return 0, "", err
	}
	if o.Source == "" {
		o.Source = "enroll"
	}
	_, err = tx.Exec(ctx, `INSERT INTO product_people (product_id, person_id, source) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, o.Product, o.PersonID, o.Source)
	return eid, "", err
}

// Tick drafts every due step that has no action yet and marks stale sends
// unknown. It is safe to run concurrently and repeatedly.
func (s *Service) Tick(ctx context.Context) (int, error) {
	if _, err := s.sweep(ctx); err != nil {
		return 0, err
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT e.id FROM enrollments e
		WHERE e.status = 'active' AND e.next_due_at <= $1
		  AND NOT EXISTS (SELECT 1 FROM actions a WHERE a.enrollment_id = e.id AND a.step = e.next_step)
		ORDER BY e.next_due_at LIMIT 500`, s.now())
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	// One broken enrollment must not hold up drafting for the others.
	n := 0
	var errs []error
	for _, id := range ids {
		created, err := s.draftNextStep(ctx, id)
		if err != nil {
			slog.Error("drafting failed", "enrollment", id, "err", err)
			errs = append(errs, fmt.Errorf("enrollment %d: %w", id, err))
			continue
		}
		if created {
			n++
		}
	}
	return n, errors.Join(errs...)
}

func (s *Service) draftNextStep(ctx context.Context, enrollmentID int64) (bool, error) {
	created := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var productID, sequenceID, status string
		var personID int64
		var step int
		if err := tx.QueryRow(ctx, `
			SELECT product_id, sequence_id, person_id, next_step, status FROM enrollments WHERE id = $1 FOR UPDATE`,
			enrollmentID).Scan(&productID, &sequenceID, &personID, &step, &status); err != nil {
			return err
		}
		if status != "active" {
			return nil
		}
		prod, err := product.Get(ctx, tx, productID)
		if err != nil {
			return err
		}
		seq, err := product.GetSequence(ctx, tx, productID, sequenceID)
		if err != nil {
			return err
		}
		if step >= len(seq.Steps) {
			_, err := tx.Exec(ctx, `UPDATE enrollments SET status = 'completed', ended_at = $2, next_due_at = NULL WHERE id = $1`, enrollmentID, s.now())
			return err
		}
		person, err := people.Get(ctx, tx, personID)
		if err != nil {
			return err
		}
		if person.Suppressed != "" {
			return stopEnrollment(ctx, tx, enrollmentID, "suppressed", s.now())
		}
		if s.Recheck != nil {
			reason, err := s.Recheck(ctx, tx, enrollmentID)
			if err != nil {
				return err
			}
			if reason != "" {
				return stopEnrollment(ctx, tx, enrollmentID, reason, s.now())
			}
		}
		st := seq.Steps[step]
		var playID string
		var companyID *int64
		if err := tx.QueryRow(ctx, `SELECT e.play_id, p.company_id FROM enrollments e JOIN people p ON p.id = e.person_id WHERE e.id = $1`,
			enrollmentID).Scan(&playID, &companyID); err != nil {
			return err
		}
		// Experiments: a fixed variant per person (or per company for b2b).
		variant := ""
		if len(st.Variants) > 0 {
			unit := fmt.Sprintf("person:%d", personID)
			if seq.ExperimentUnit == "company" && companyID != nil {
				unit = fmt.Sprintf("company:%d", *companyID)
			}
			ids := make([]string, len(st.Variants))
			for i, v := range st.Variants {
				ids[i] = v.ID
			}
			if variant, err = experiment.Assign(ctx, tx, experiment.Key(productID, sequenceID, step), unit, ids); err != nil {
				return err
			}
			for _, v := range st.Variants {
				if v.ID == variant {
					if v.Subject != "" {
						st.Subject = v.Subject
					}
					if v.Body != "" {
						st.Body = v.Body
					}
					if v.Goal != "" {
						st.Goal = v.Goal
					}
				}
			}
		}

		d := product.TemplateData{
			FirstName: person.FirstName, Name: person.Name, Company: person.Company, Domain: person.Domain,
			Product: prod.Name, Promise: prod.Offer.Promise, CTA: prod.Offer.CTA, Destination: prod.Offer.Destination,
		}
		if d.FirstName == "" {
			d.FirstName = "there"
		}
		if d.Company == "" {
			d.Company = strings.Split(d.Domain, ".")[0] // acmepay.io -> acmepay
		}
		// A tracked link per message, created before approval so the
		// approved text already contains it.
		var linkCode *string
		d.Link = prod.Offer.Destination
		if s.PublicURL != "" && prod.Offer.Destination != "" && st.Channel != "whatsapp" {
			campaign := sequenceID
			if playID != "" {
				campaign = playID
			}
			code, err := links.Create(ctx, tx, links.Link{Product: productID, Destination: prod.Offer.Destination,
				Label: "sequence:" + sequenceID, PersonID: &personID,
				UTM: map[string]string{"utm_source": "astrolabe", "utm_medium": st.Channel, "utm_campaign": campaign, "utm_content": variant}})
			if err != nil {
				return err
			}
			linkCode = &code
			d.Link = links.URL(s.PublicURL, code)
		}
		subject, err := product.Render(st.Subject, d)
		if err != nil {
			return fmt.Errorf("step %d subject: %w", step+1, err)
		}
		body, err := product.Render(st.Body, d)
		if err != nil {
			return fmt.Errorf("step %d body: %w", step+1, err)
		}

		a := struct {
			channel, sender, recipient, threadID, inReplyTo, status, errMsg string
		}{channel: st.Channel, status: "draft"}
		if st.Channel == "email" {
			a.sender, a.recipient = prod.Sender.Cold, person.Email
			if seq.Kind == "users" {
				a.sender = prod.Sender.Users
			}
			if a.recipient == "" {
				a.status, a.errMsg = "blocked", "no email address for this person"
			}
			// Follow-ups reply in the thread of the last email that went out.
			var prevSubject string
			err := tx.QueryRow(ctx, `
				SELECT subject, thread_id, message_id FROM actions
				WHERE enrollment_id = $1 AND channel = 'email' AND status = 'sent'
				ORDER BY step DESC LIMIT 1`, enrollmentID).Scan(&prevSubject, &a.threadID, &a.inReplyTo)
			if err == nil {
				subject = "Re: " + strings.TrimPrefix(prevSubject, "Re: ")
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		} else if st.Channel == "whatsapp" {
			a.sender = "whatsapp:" + prod.WhatsApp.PhoneNumberID
			a.recipient = identity(ctx, tx, personID, "phone", "")
			// The template is the subject; its parameters, one per line, the body.
			subject = st.Template
			var params []string
			for _, prm := range st.Params {
				v, err := product.Render(prm, d)
				if err != nil {
					return fmt.Errorf("step %d param: %w", step+1, err)
				}
				params = append(params, strings.ReplaceAll(v, "\n", " "))
			}
			body = strings.Join(params, "\n")
			if a.recipient == "" {
				a.status, a.errMsg = "blocked", "no phone number for this person"
			}
		} else if product.LifecycleOnly(st.Channel) {
			a.sender = "product:" + productID
			a.recipient = strings.TrimPrefix(identity(ctx, tx, personID, "user", productID+":"), productID+":")
			if a.recipient == "" {
				a.status, a.errMsg = "blocked", "no product user id for this person"
			}
		} else {
			a.recipient = person.Name
			if person.GitHub != "" {
				a.recipient += " (github.com/" + person.GitHub + ")"
			}
		}
		var actionID int64
		err = tx.QueryRow(ctx, `
			INSERT INTO actions (enrollment_id, step, product_id, person_id, channel, sender, recipient,
				subject, body, goal, status, error, thread_id, in_reply_to, variant, link_code)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			ON CONFLICT (enrollment_id, step) DO NOTHING RETURNING id`,
			enrollmentID, step, productID, personID, a.channel, a.sender, a.recipient,
			subject, body, st.Goal, a.status, a.errMsg, a.threadID, a.inReplyTo, variant, linkCode).Scan(&actionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		created = true
		if linkCode != nil {
			if _, err := tx.Exec(ctx, `UPDATE links SET action_id = $2 WHERE code = $1`, *linkCode, actionID); err != nil {
				return err
			}
		}
		if body == "" && a.status == "draft" && st.Channel != "whatsapp" && s.OnDraftCreated != nil {
			queued, err := s.OnDraftCreated(ctx, tx, actionID)
			if err != nil {
				return err
			}
			if queued {
				_, err = tx.Exec(ctx, `UPDATE actions SET ai_state = 'pending' WHERE id = $1`, actionID)
			}
			return err
		}
		return nil
	})
	return created, err
}

// identity returns a person's identity of a kind (optionally with a value
// prefix), or "".
func identity(ctx context.Context, q store.Q, personID int64, kind, prefix string) string {
	var v string
	q.QueryRow(ctx, `SELECT value FROM identities WHERE person_id = $1 AND kind = $2 AND value LIKE $3 || '%' ORDER BY value LIMIT 1`,
		personID, kind, prefix).Scan(&v)
	return v
}

func stopEnrollment(ctx context.Context, q store.Q, enrollmentID int64, reason string, at time.Time) error {
	if _, err := q.Exec(ctx, `
		UPDATE enrollments SET status = 'stopped', stop_reason = $2, ended_at = $3, next_due_at = NULL
		WHERE id = $1 AND status = 'active'`, enrollmentID, reason, at); err != nil {
		return err
	}
	// Unsent work for a stopped enrollment must not go out.
	_, err := q.Exec(ctx, `
		UPDATE actions SET status = 'skipped', error = 'enrollment stopped: ' || $2, updated_at = $3
		WHERE enrollment_id = $1 AND status IN ('draft', 'approved', 'blocked')`, enrollmentID, reason, at)
	return err
}

// advance moves an enrollment past a finished step and schedules the next.
func (s *Service) advance(ctx context.Context, tx pgx.Tx, enrollmentID int64, finishedStep int) error {
	var productID, sequenceID string
	var next int
	var started time.Time
	err := tx.QueryRow(ctx, `
		SELECT product_id, sequence_id, next_step, started_at FROM enrollments
		WHERE id = $1 AND status = 'active' FOR UPDATE`, enrollmentID).Scan(&productID, &sequenceID, &next, &started)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if next != finishedStep {
		return nil
	}
	seq, err := product.GetSequence(ctx, tx, productID, sequenceID)
	if err != nil {
		return err
	}
	next++
	if next >= len(seq.Steps) {
		_, err := tx.Exec(ctx, `UPDATE enrollments SET status = 'completed', next_step = $2, ended_at = $3, next_due_at = NULL WHERE id = $1`, enrollmentID, next, s.now())
		return err
	}
	after, _ := product.AfterDuration(seq.Steps[next].After)
	due := started.Add(after)
	if due.Before(s.now()) {
		due = s.now()
	}
	_, err = tx.Exec(ctx, `UPDATE enrollments SET next_step = $2, next_due_at = $3 WHERE id = $1`, enrollmentID, next, due)
	return err
}

// approvalHash covers exactly what is approved: changing any part of it
// requires approval again.
func approvalHash(channel, sender, recipient, subject, body string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{channel, sender, recipient, subject, body}, "\x00")))
	return hex.EncodeToString(h[:])
}

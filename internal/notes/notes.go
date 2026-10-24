// Package notes holds conversation notes: what was said in a call, email
// thread or chat with a person, the product-market-fit survey answer, pricing
// feedback, and the weekly themes drawn from them. Plain Go functions; the MCP
// tools and the browser app both call them.
package notes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/pipeline"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// ErrInvalid is returned for requests that break a rule; the message says which.
var ErrInvalid = errors.New("invalid")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// DB is a pool or a transaction.
type DB interface {
	store.Q
	Begin(ctx context.Context) (pgx.Tx, error)
}

var (
	Channels = []string{"call", "email", "chat", "in_person", "other"}
	// Answers to "How would you feel if you could no longer use <product>?"
	PMFAnswers      = []string{"very_disappointed", "somewhat_disappointed", "not_disappointed"}
	BuyingProcesses = []string{"card", "invoice", "procurement"}
)

const maxText = 8000

// PMF is the product-market-fit survey answer (sales: Sean Ellis test).
type PMF struct {
	Answer string `json:"answer" jsonschema:"very_disappointed, somewhat_disappointed or not_disappointed: how they would feel if they could no longer use the product" yaml:"answer"`
	// UsedTwoWeeks: they used the product two weeks or more. Only those
	// people are asked; recorded as given and counted separately if false.
	UsedTwoWeeks bool   `json:"used_two_weeks" jsonschema:"true if they used the product for two weeks or more (only those people are asked)" yaml:"used_two_weeks"`
	Alternative  string `json:"alternative,omitempty" jsonschema:"what would you use instead? word for word" yaml:"alternative"`
	WhoElse      string `json:"who_else,omitempty" jsonschema:"who else would benefit? word for word" yaml:"who_else"`
}

// Pricing is one pricing statement: what they said, not a commitment (sales:
// willingness to pay, budget).
type Pricing struct {
	Offer         string `json:"offer" jsonschema:"the offer named, e.g. 'Team plan $29/month'" yaml:"offer"`
	Replaces      string `json:"replaces,omitempty" jsonschema:"what it would replace (the comparison)" yaml:"replaces"`
	Approver      string `json:"approver,omitempty" jsonschema:"who would approve it" yaml:"approver"`
	BuyingProcess string `json:"buying_process,omitempty" jsonschema:"card, invoice or procurement" yaml:"buying_process"`
	Reaction      string `json:"reaction,omitempty" jsonschema:"their reaction, in their words" yaml:"reaction"`
}

// LogOpts is everything one logged conversation can carry.
type LogOpts struct {
	PersonID int64
	// AccountID is optional; zero means the note is about the person only.
	AccountID int64
	// Date is the day it happened (default: the day of now).
	Date     time.Time
	Channel  string
	Duration int // minutes, optional
	Assisted bool

	Summary         string
	TheirWords      string
	Problem         string
	CurrentApproach string
	NextStep        string

	// Answers go to pipeline.Qualify on AccountID, with the note as context.
	Answers []pipeline.Answer
	// NextAction and NextDue replace the account's next action, or become a
	// task for the person when there is no account.
	NextAction string
	NextDue    time.Time
	PMF        *PMF
	Pricing    []Pricing
}

// Note is a logged conversation.
type Note struct {
	ID              int64     `json:"id"`
	Product         string    `json:"product"`
	PersonID        int64     `json:"person_id"`
	Person          string    `json:"person,omitempty"`
	AccountID       *int64    `json:"account_id,omitempty"`
	Company         string    `json:"company,omitempty"`
	Date            string    `json:"date"`
	Channel         string    `json:"channel"`
	DurationMinutes *int      `json:"duration_minutes,omitempty"`
	Assisted        bool      `json:"assisted"`
	Summary         string    `json:"summary,omitempty"`
	TheirWords      string    `json:"their_words,omitempty"`
	Problem         string    `json:"problem,omitempty"`
	CurrentApproach string    `json:"current_approach,omitempty"`
	NextStep        string    `json:"next_step,omitempty"`
	PMF             *PMF      `json:"pmf_survey,omitempty"`
	Pricing         []Pricing `json:"pricing_feedback,omitempty"`
	LoggedAt        time.Time `json:"logged_at"`
}

func clean(field, s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) > maxText {
		return "", invalid("%s is too long (over %d characters)", field, maxText)
	}
	return s, nil
}

func day(t time.Time) string { return t.Format("2006-01-02") }

// Log records a conversation, and applies its optional extras (qualification
// answers, a new next action, the survey answer, pricing statements) in one
// transaction: if any part is refused, nothing is saved.
func Log(ctx context.Context, db DB, p *product.Product, o LogOpts, now time.Time) (int64, error) {
	if o.PersonID == 0 {
		return 0, invalid("a note is about a person: give person")
	}
	if !slices.Contains(Channels, o.Channel) {
		return 0, invalid("channel must be one of %v", Channels)
	}
	if o.Duration < 0 {
		return 0, invalid("duration must be a number of minutes")
	}
	if o.Date.IsZero() {
		o.Date = now
	}
	if o.Date.After(now.AddDate(0, 0, 1)) {
		return 0, invalid("date %s is in the future", day(o.Date))
	}
	var err error
	for _, f := range []struct {
		name string
		v    *string
	}{{"summary", &o.Summary}, {"their_words", &o.TheirWords}, {"problem", &o.Problem}, {"current_approach", &o.CurrentApproach}, {"next_step", &o.NextStep}} {
		if *f.v, err = clean(f.name, *f.v); err != nil {
			return 0, err
		}
	}
	if o.Summary == "" && o.TheirWords == "" && o.Problem == "" && o.CurrentApproach == "" && o.NextStep == "" {
		return 0, invalid("a note needs at least one of summary, their_words, problem, current_approach, next_step")
	}
	if len(o.Answers) > 0 && o.AccountID == 0 {
		return 0, invalid("qualification answers belong to an account: give account")
	}
	if len(o.Answers) > 0 {
		if p.Pipeline == nil {
			return 0, invalid("%s has no account pipeline, so there is nothing to qualify", p.ID)
		}
	}
	if o.NextAction = strings.TrimSpace(o.NextAction); o.NextAction != "" && o.NextDue.IsZero() {
		o.NextDue = now
	}
	if o.NextAction == "" && !o.NextDue.IsZero() {
		return 0, invalid("a due date needs a next action")
	}
	if o.AccountID != 0 && o.NextAction != "" && p.Pipeline == nil {
		return 0, invalid("%s has no account pipeline", p.ID)
	}
	if o.PMF != nil {
		if !slices.Contains(PMFAnswers, o.PMF.Answer) {
			return 0, invalid("pmf_survey answer must be one of %v", PMFAnswers)
		}
		if o.PMF.Alternative, err = clean("alternative", o.PMF.Alternative); err != nil {
			return 0, err
		}
		if o.PMF.WhoElse, err = clean("who_else", o.PMF.WhoElse); err != nil {
			return 0, err
		}
	}
	prices := slices.Clone(o.Pricing)
	for i, pr := range prices {
		if prices[i].Offer = strings.TrimSpace(pr.Offer); prices[i].Offer == "" {
			return 0, invalid("pricing feedback %d: name the offer", i+1)
		}
		if pr.BuyingProcess != "" && !slices.Contains(BuyingProcesses, pr.BuyingProcess) {
			return 0, invalid("pricing feedback %d: buying_process must be one of %v", i+1, BuyingProcesses)
		}
		for _, f := range []*string{&prices[i].Replaces, &prices[i].Approver, &prices[i].Reaction} {
			if *f, err = clean("pricing feedback", *f); err != nil {
				return 0, err
			}
		}
	}

	var id int64
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var account any
		if o.AccountID != 0 {
			var prod string
			if err := tx.QueryRow(ctx, `SELECT product_id FROM accounts WHERE id = $1`, o.AccountID).Scan(&prod); err != nil || prod != p.ID {
				return invalid("no account %d in %s", o.AccountID, p.ID)
			}
			account = o.AccountID
		}
		var dur any
		if o.Duration > 0 {
			dur = o.Duration
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO conversations (product_id, person_id, account_id, occurred_on, channel, duration_minutes, assisted,
				summary, their_words, problem, current_approach, next_step)
			VALUES ($1, $2, $3, $4::date, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING id`,
			p.ID, o.PersonID, account, day(o.Date), o.Channel, dur, o.Assisted,
			o.Summary, o.TheirWords, o.Problem, o.CurrentApproach, o.NextStep).Scan(&id); err != nil {
			return err
		}
		if len(o.Answers) > 0 {
			ctxNote := fmt.Sprintf(" [%s %s, note #%d]", strings.ReplaceAll(o.Channel, "_", " "), day(o.Date), id)
			answers := slices.Clone(o.Answers)
			for i, a := range answers {
				if ev := strings.Join(strings.Fields(a.Evidence), " "); ev != "" && len(ev)+len(ctxNote) <= 500 {
					answers[i].Evidence = ev + ctxNote
				}
			}
			if err := pipeline.Qualify(ctx, tx, p, o.AccountID, answers, now); err != nil {
				return err
			}
		}
		if o.NextAction != "" {
			if o.AccountID != 0 {
				if err := pipeline.SetNextAction(ctx, tx, p, o.AccountID, o.NextAction, o.NextDue); err != nil {
					return err
				}
			} else if _, err := tx.Exec(ctx, `INSERT INTO tasks (product_id, person_id, next_action, due_at) VALUES ($1, $2, $3, $4)`,
				p.ID, o.PersonID, o.NextAction, o.NextDue.Truncate(24*time.Hour).Add(9*time.Hour)); err != nil {
				return err
			}
		}
		if o.PMF != nil {
			if _, err := tx.Exec(ctx, `
				INSERT INTO pmf_answers (conversation_id, product_id, person_id, answered_on, answer, used_two_weeks, alternative, who_else)
				VALUES ($1, $2, $3, $4::date, $5, $6, $7, $8)`,
				id, p.ID, o.PersonID, day(o.Date), o.PMF.Answer, o.PMF.UsedTwoWeeks, o.PMF.Alternative, o.PMF.WhoElse); err != nil {
				return err
			}
		}
		for _, pr := range prices {
			if _, err := tx.Exec(ctx, `
				INSERT INTO pricing_feedback (conversation_id, product_id, person_id, stated_on, offer, replaces, approver, buying_process, reaction)
				VALUES ($1, $2, $3, $4::date, $5, $6, $7, $8, $9)`,
				id, p.ID, o.PersonID, day(o.Date), pr.Offer, pr.Replaces, pr.Approver, pr.BuyingProcess, pr.Reaction); err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

// ListOpts filters List. Zero values mean no filter.
type ListOpts struct {
	Product   string
	PersonID  int64
	AccountID int64
	// Days: notes dated within the last Days days (today included).
	Days int
	// Assisted: only assisted (true) or only unassisted (false) notes.
	Assisted *bool
	// From and To bound the date (To exclusive), used for ISO weeks.
	From, To time.Time
	Limit    int
}

const noteSelect = `
	SELECT c.id, c.product_id, c.person_id, pe.name, c.account_id, COALESCE(NULLIF(co.name, ''), co.domain, ''), to_char(c.occurred_on, 'YYYY-MM-DD'),
		c.channel, c.duration_minutes, c.assisted, c.summary, c.their_words, c.problem, c.current_approach, c.next_step, c.created_at,
		pa.answer, pa.used_two_weeks, pa.alternative, pa.who_else
	FROM conversations c
	JOIN people pe ON pe.id = c.person_id
	LEFT JOIN accounts a ON a.id = c.account_id
	LEFT JOIN companies co ON co.id = a.company_id
	LEFT JOIN pmf_answers pa ON pa.conversation_id = c.id`

// filter builds the WHERE clause for the filters in o.
func filter(o ListOpts, now time.Time) (string, []any) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args))))
	}
	if o.Product != "" {
		add("c.product_id = ?", o.Product)
	}
	if o.PersonID != 0 {
		add("c.person_id = ?", o.PersonID)
	}
	if o.AccountID != 0 {
		add("c.account_id = ?", o.AccountID)
	}
	if o.Days > 0 {
		add("c.occurred_on > ?::date", day(now.AddDate(0, 0, -o.Days)))
	}
	if !o.From.IsZero() {
		add("c.occurred_on >= ?::date", day(o.From))
	}
	if !o.To.IsZero() {
		add("c.occurred_on < ?::date", day(o.To))
	}
	if o.Assisted != nil {
		add("c.assisted = ?", *o.Assisted)
	}
	if len(where) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// List returns notes, newest first, with their survey answer and pricing
// statements.
func List(ctx context.Context, q store.Q, o ListOpts, now time.Time) ([]*Note, error) {
	w, args := filter(o, now)
	sql := noteSelect + w + " ORDER BY c.occurred_on DESC, c.id DESC"
	if o.Limit > 0 {
		sql += fmt.Sprintf(" LIMIT %d", o.Limit)
	}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Note, error) {
		n := &Note{}
		var ans, alt, who *string
		var used *bool
		if err := r.Scan(&n.ID, &n.Product, &n.PersonID, &n.Person, &n.AccountID, &n.Company, &n.Date, &n.Channel, &n.DurationMinutes,
			&n.Assisted, &n.Summary, &n.TheirWords, &n.Problem, &n.CurrentApproach, &n.NextStep, &n.LoggedAt, &ans, &used, &alt, &who); err != nil {
			return nil, err
		}
		if ans != nil {
			n.PMF = &PMF{Answer: *ans, UsedTwoWeeks: *used, Alternative: *alt, WhoElse: *who}
		}
		return n, nil
	})
	if err != nil || len(list) == 0 {
		return list, err
	}
	ids := make([]int64, len(list))
	byID := map[int64]*Note{}
	for i, n := range list {
		ids[i] = n.ID
		byID[n.ID] = n
	}
	prows, err := q.Query(ctx, `SELECT conversation_id, offer, replaces, approver, buying_process, reaction FROM pricing_feedback
		WHERE conversation_id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return nil, err
	}
	defer prows.Close()
	for prows.Next() {
		var cid int64
		var pr Pricing
		if err := prows.Scan(&cid, &pr.Offer, &pr.Replaces, &pr.Approver, &pr.BuyingProcess, &pr.Reaction); err != nil {
			return nil, err
		}
		byID[cid].Pricing = append(byID[cid].Pricing, pr)
	}
	return list, prows.Err()
}

// Counts is how many notes there are, assisted and unassisted apart.
type Counts struct {
	Total      int `json:"total"`
	Assisted   int `json:"assisted"`
	Unassisted int `json:"unassisted"`
}

// Count counts notes by the same filters as List (Assisted and Limit are
// ignored: both kinds are always counted apart).
func Count(ctx context.Context, q store.Q, o ListOpts, now time.Time) (Counts, error) {
	o.Limit, o.Assisted = 0, nil
	w, args := filter(o, now)
	var c Counts
	err := q.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE c.assisted), count(*) FILTER (WHERE NOT c.assisted) FROM conversations c`+w, args...).
		Scan(&c.Total, &c.Assisted, &c.Unassisted)
	return c, err
}

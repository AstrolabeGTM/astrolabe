package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/notes"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

// Decisions an operator can record for a running experiment or campaign.
var Decisions = []string{"keep", "change", "stop"}

// Kinds of things a decision is about.
var DecisionKinds = []string{"campaign", "experiment"}

// Answer is the operator's answer to one question. When saving, give the
// question's number (1-based) or its exact text.
type Answer struct {
	Number   int    `json:"number,omitempty" jsonschema:"the question's number in the review (1-based); or give question"`
	Question string `json:"question,omitempty" jsonschema:"the question's exact text, if no number"`
	Answer   string `json:"answer" jsonschema:"the answer; empty clears it"`
}

// Decision is keep, change or stop for one running experiment or campaign.
type Decision struct {
	Kind     string `json:"kind" jsonschema:"campaign or experiment"`
	ID       string `json:"id" jsonschema:"campaign id or code, or the experiment key shown in the review (product/sequence/step)"`
	Decision string `json:"decision" jsonschema:"keep, change or stop"`
	Note     string `json:"note,omitempty"`
}

// Saved is a week's saved review: the answers and decisions, and the
// review as built when it was first saved.
type Saved struct {
	Product    string     `json:"product"`
	Week       string     `json:"week"`
	CreatedAt  time.Time  `json:"first_saved_at"`
	SavedAt    time.Time  `json:"saved_at"`
	Answers    []Answer   `json:"answers"`
	Decisions  []Decision `json:"decisions"`
	Unanswered int        `json:"unanswered_questions"`
	// Review is the frozen snapshot, with the questions showing the
	// answers saved now. Its numbers never change after the first save.
	Review *Review `json:"review"`
}

const maxAnswer = 8000

func get(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, productID, week string) (*Saved, error) {
	s := &Saved{Product: productID, Week: week}
	var answers, decisions, snap []byte
	err := q.QueryRow(ctx, `SELECT answers, decisions, snapshot, created_at, saved_at FROM weekly_reviews WHERE product_id = $1 AND iso_week = $2`,
		productID, week).Scan(&answers, &decisions, &snap, &s.CreatedAt, &s.SavedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	s.Answers, s.Decisions = []Answer{}, []Decision{}
	if err := json.Unmarshal(answers, &s.Answers); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(decisions, &s.Decisions); err != nil {
		return nil, err
	}
	s.Review = &Review{}
	return s, json.Unmarshal(snap, s.Review)
}

// Get returns the saved review for a product and ISO week, or nil.
func Get(ctx context.Context, pool *pgxpool.Pool, p *product.Product, week string) (*Saved, error) {
	s, err := get(ctx, pool, p.ID, week)
	if s != nil && err == nil {
		s.finish(p)
	}
	return s, err
}

// finish overlays the current questions and answers on the snapshot.
func (s *Saved) finish(p *product.Product) {
	qs := p.ReviewSettings().Questions
	s.Review.Questions = questionsWith(qs, s)
	s.Unanswered = 0
	for _, q := range s.Review.Questions {
		if !q.Answered {
			s.Unanswered++
		}
	}
}

// SaveInput is what the operator saves. Answers and decisions are merged
// into what was saved before (by question, and by kind and id); the
// snapshot stays as first built unless Refresh is set.
type SaveInput struct {
	Answers   []Answer   `json:"answers,omitempty"`
	Decisions []Decision `json:"decisions,omitempty"`
	Refresh   bool       `json:"refresh_snapshot,omitempty"`
}

// Save records answers and decisions for a week (default: this one). The
// first save freezes the review as built then.
func Save(ctx context.Context, pool *pgxpool.Pool, p *product.Product, week string, in SaveInput, now time.Time) (*Saved, error) {
	if week == "" {
		week = notes.ISOWeek(now)
	}
	if _, _, err := notes.WeekBounds(week); err != nil {
		return nil, err
	}
	existing, err := get(ctx, pool, p.ID, week)
	if err != nil {
		return nil, err
	}
	var snap *Review
	switch {
	case existing != nil && !in.Refresh:
		snap = existing.Review
	default:
		if snap, err = BuildAt(ctx, pool, p, week, now); err != nil {
			return nil, err
		}
	}
	snap.Questions = nil // the answers live beside the snapshot, not in it
	qs := p.ReviewSettings().Questions

	answers := []Answer{}
	if existing != nil {
		answers = existing.Answers
	}
	for i, a := range in.Answers {
		text := strings.TrimSpace(a.Question)
		switch {
		case a.Number > 0:
			if a.Number > len(qs) {
				return nil, fmt.Errorf("%w: answer %d: there are %d questions", notes.ErrInvalid, i+1, len(qs))
			}
			text = qs[a.Number-1]
		case text == "":
			return nil, fmt.Errorf("%w: answer %d needs a question number or text", notes.ErrInvalid, i+1)
		default:
			found := false
			for _, q := range qs {
				found = found || q == text
			}
			if !found {
				return nil, fmt.Errorf("%w: answer %d: %q is not one of the review's questions", notes.ErrInvalid, i+1, text)
			}
		}
		ans := strings.TrimSpace(a.Answer)
		if len(ans) > maxAnswer {
			return nil, fmt.Errorf("%w: answer %d is too long", notes.ErrInvalid, i+1)
		}
		answers = setAnswer(answers, text, ans)
	}

	decisions := []Decision{}
	if existing != nil {
		decisions = existing.Decisions
	}
	for i, d := range in.Decisions {
		d.Kind, d.Decision, d.Note = strings.ToLower(strings.TrimSpace(d.Kind)), strings.ToLower(strings.TrimSpace(d.Decision)), strings.TrimSpace(d.Note)
		if !contains(DecisionKinds, d.Kind) {
			return nil, fmt.Errorf("%w: decision %d: kind must be one of %v", notes.ErrInvalid, i+1, DecisionKinds)
		}
		if !contains(Decisions, d.Decision) {
			return nil, fmt.Errorf("%w: decision %d: decision must be one of %v", notes.ErrInvalid, i+1, Decisions)
		}
		if len(d.Note) > maxAnswer {
			return nil, fmt.Errorf("%w: decision %d: note is too long", notes.ErrInvalid, i+1)
		}
		id, ok := decisionTarget(snap, d.Kind, strings.TrimSpace(d.ID))
		if !ok {
			return nil, fmt.Errorf("%w: decision %d: no running %s %q in this review", notes.ErrInvalid, i+1, d.Kind, d.ID)
		}
		d.ID = id
		decisions = setDecision(decisions, d)
	}

	raw, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	aj, _ := json.Marshal(answers)
	dj, _ := json.Marshal(decisions)
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// The first save wins the snapshot, even with two at once.
		if _, err := tx.Exec(ctx, `INSERT INTO weekly_reviews (product_id, iso_week, snapshot, created_at, saved_at) VALUES ($1, $2, $3, $4, $4)
			ON CONFLICT DO NOTHING`, p.ID, week, raw, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM weekly_reviews WHERE product_id = $1 AND iso_week = $2 FOR UPDATE`, p.ID, week); err != nil {
			return err
		}
		if in.Refresh {
			_, err = tx.Exec(ctx, `UPDATE weekly_reviews SET answers = $3, decisions = $4, snapshot = $5, saved_at = $6 WHERE product_id = $1 AND iso_week = $2`,
				p.ID, week, aj, dj, raw, now)
		} else {
			_, err = tx.Exec(ctx, `UPDATE weekly_reviews SET answers = $3, decisions = $4, saved_at = $5 WHERE product_id = $1 AND iso_week = $2`,
				p.ID, week, aj, dj, now)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return Get(ctx, pool, p, week)
}

func decisionTarget(r *Review, kind, id string) (string, bool) {
	switch kind {
	case "campaign":
		for _, c := range r.Campaigns {
			if c.ID == id || c.Code == id {
				return c.ID, true
			}
		}
	case "experiment":
		for _, e := range r.Experiments {
			if e.Key == id {
				return e.Key, true
			}
		}
	}
	return "", false
}

func setAnswer(list []Answer, q, ans string) []Answer {
	out := list[:0:0]
	replaced := false
	for _, a := range list {
		if a.Question == q {
			replaced = true
			if ans == "" {
				continue
			}
			a.Answer = ans
		}
		out = append(out, a)
	}
	if !replaced && ans != "" {
		out = append(out, Answer{Question: q, Answer: ans})
	}
	return out
}

func setDecision(list []Decision, d Decision) []Decision {
	for i := range list {
		if list[i].Kind == d.Kind && list[i].ID == d.ID {
			list[i] = d
			return list
		}
	}
	return append(list, d)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Listed is one past week in the list of saved reviews.
type Listed struct {
	Week      string    `json:"week"`
	SavedAt   time.Time `json:"saved_at"`
	Answered  int       `json:"answered_questions"`
	Questions int       `json:"questions"`
	Decisions int       `json:"decisions"`
	Window    Window    `json:"window"`
}

// List returns saved reviews, newest week first.
func List(ctx context.Context, pool *pgxpool.Pool, p *product.Product, limit int) ([]Listed, error) {
	if limit <= 0 || limit > 200 {
		limit = 26
	}
	rows, err := pool.Query(ctx, `SELECT iso_week FROM weekly_reviews WHERE product_id = $1 ORDER BY iso_week DESC LIMIT $2`, p.ID, limit)
	if err != nil {
		return nil, err
	}
	var weeks []string
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			rows.Close()
			return nil, err
		}
		weeks = append(weeks, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []Listed{}
	for _, w := range weeks {
		s, err := Get(ctx, pool, p, w)
		if err != nil {
			return nil, err
		}
		n := len(s.Review.Questions)
		out = append(out, Listed{Week: w, SavedAt: s.SavedAt, Questions: n, Answered: n - s.Unanswered, Decisions: len(s.Decisions), Window: s.Review.Window})
	}
	return out, nil
}

// Status says whether a week's review is saved and how many questions are
// still unanswered. The digest shows it.
type Status struct {
	Week       string `json:"week"`
	Day        string `json:"review_day"`
	Saved      bool   `json:"saved"`
	Questions  int    `json:"questions"`
	Unanswered int    `json:"unanswered_questions"`
}

// WeekStatus reports the review of the ISO week containing now.
func WeekStatus(ctx context.Context, pool *pgxpool.Pool, p *product.Product, now time.Time) (*Status, error) {
	set := p.ReviewSettings()
	st := &Status{Week: notes.ISOWeek(now), Day: strings.ToLower(set.Weekday.String()), Questions: len(set.Questions), Unanswered: len(set.Questions)}
	s, err := Get(ctx, pool, p, st.Week)
	if err != nil || s == nil {
		return st, err
	}
	st.Saved, st.Unanswered = true, s.Unanswered
	return st, nil
}

// Text is the one digest line.
func (s *Status) Text() string {
	if !s.Saved {
		return fmt.Sprintf("Weekly review %s: not saved yet (%d questions unanswered; review day is %s). Ask for weekly_review.\n", s.Week, s.Unanswered, capital(s.Day))
	}
	return fmt.Sprintf("Weekly review %s: saved, %d of %d questions unanswered.\n", s.Week, s.Unanswered, s.Questions)
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

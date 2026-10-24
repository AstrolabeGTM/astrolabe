package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

type StageCount struct {
	Stage    string `json:"stage"`
	Accounts int    `json:"accounts"`
}

// Board is the pipeline at a glance: open accounts per stage, paused and
// lost counts, and what needs attention.
type Board struct {
	Product    string       `json:"product"`
	StuckAfter string       `json:"stuck_after"`
	Stages     []StageCount `json:"stages"`
	Paused     int          `json:"paused"`
	Lost       int          `json:"lost"`
	// Stuck: open accounts in one stage longer than stuck_after. Overdue:
	// next actions due before today.
	Stuck   []*Account `json:"stuck"`
	Overdue []*Account `json:"overdue"`
	// RevisitDue: paused or lost accounts whose revisit date has come.
	RevisitDue []*Account `json:"revisit_due"`
	Open       []*Account `json:"accounts,omitempty"`
	Closed     []*Account `json:"closed,omitempty"`
	// The qualification setup: the gate stage, its gating checks, the
	// record-only checks, and the reasons for pausing or losing an account.
	Gate    string   `json:"gate,omitempty"`
	Checks  []string `json:"checks"`
	Record  []string `json:"record_checks,omitempty"`
	Reasons []string `json:"reasons"`
}

// BuildBoard summarises a product's pipeline at now.
func BuildBoard(ctx context.Context, q store.Q, p *product.Product, now time.Time) (*Board, error) {
	if err := needPipeline(p); err != nil {
		return nil, err
	}
	list, err := List(ctx, q, p, now)
	if err != nil {
		return nil, err
	}
	b := &Board{Product: p.ID, StuckAfter: p.Pipeline.StuckAfter, Stuck: []*Account{}, Overdue: []*Account{}, RevisitDue: []*Account{},
		Checks: []string{}, Reasons: p.PipelineReasons()}
	if qa := p.Pipeline.Qualify; qa != nil {
		b.Gate, b.Checks, b.Record = qa.Gate, qa.Checks, qa.Record
	}
	if err := fillAnswers(ctx, q, list); err != nil {
		return nil, err
	}
	if b.StuckAfter == "" {
		b.StuckAfter = "14d"
	}
	count := map[string]int{}
	for _, a := range list {
		switch a.Status {
		case Open:
			count[a.Stage]++
			b.Open = append(b.Open, a)
			if a.Stuck {
				b.Stuck = append(b.Stuck, a)
			}
			if a.Overdue {
				b.Overdue = append(b.Overdue, a)
			}
		default:
			if a.Status == "paused" {
				b.Paused++
			} else {
				b.Lost++
			}
			b.Closed = append(b.Closed, a)
			if a.RevisitOn != "" && a.RevisitOn <= day(now) {
				b.RevisitDue = append(b.RevisitDue, a)
			}
		}
	}
	for _, s := range p.Pipeline.Stages {
		b.Stages = append(b.Stages, StageCount{Stage: s, Accounts: count[s]})
	}
	return b, nil
}

// fillAnswers sets each account's check answers (for chips on a board).
func fillAnswers(ctx context.Context, q store.Q, list []*Account) error {
	byID := map[int64]*Account{}
	ids := make([]int64, 0, len(list))
	for _, a := range list {
		byID[a.ID], a.Answers = a, map[string]string{}
		ids = append(ids, a.ID)
	}
	rows, err := q.Query(ctx, `SELECT account_id, name, answer FROM account_checks WHERE account_id = ANY($1)`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n, ans string
		if err := rows.Scan(&id, &n, &ans); err != nil {
			return err
		}
		byID[id].Answers[n] = ans
	}
	return rows.Err()
}

// Summary drops the per-account lists, keeping counts and what needs
// attention (for the digest).
func (b *Board) Summary() *Board {
	s := *b
	s.Open, s.Closed = nil, nil
	strip := func(l []*Account) []*Account {
		out := make([]*Account, len(l))
		for i, a := range l {
			c := *a
			c.Answers = nil
			out[i] = &c
		}
		return out
	}
	s.Stuck, s.Overdue, s.RevisitDue = strip(s.Stuck), strip(s.Overdue), strip(s.RevisitDue)
	return &s
}

// Text renders counts by stage, stuck accounts and overdue next actions.
func (b *Board) Text() string {
	var w strings.Builder
	w.WriteString("Pipeline (accounts):")
	for i, s := range b.Stages {
		sep := ","
		if i == 0 {
			sep = ""
		}
		fmt.Fprintf(&w, "%s %s %d", sep, s.Stage, s.Accounts)
	}
	fmt.Fprintf(&w, "; paused %d, lost %d\n", b.Paused, b.Lost)
	if len(b.Overdue) > 0 {
		fmt.Fprintf(&w, "Overdue next actions (%d):", len(b.Overdue))
		for i, a := range b.Overdue {
			if i > 0 {
				w.WriteString(";")
			}
			fmt.Fprintf(&w, " %s: %s (due %s)", a.Company, a.NextAction, a.NextDue)
		}
		w.WriteString("\n")
	}
	if len(b.Stuck) > 0 {
		fmt.Fprintf(&w, "Stuck accounts (over %s in one stage, %d):", b.StuckAfter, len(b.Stuck))
		for i, a := range b.Stuck {
			if i > 0 {
				w.WriteString(";")
			}
			fmt.Fprintf(&w, " %s (%s, %dd)", a.Company, a.Stage, a.DaysInStage)
		}
		w.WriteString("\n")
	}
	if len(b.RevisitDue) > 0 {
		fmt.Fprintf(&w, "Revisit now (%d):", len(b.RevisitDue))
		for i, a := range b.RevisitDue {
			if i > 0 {
				w.WriteString(";")
			}
			fmt.Fprintf(&w, " %s (%s: %s)", a.Company, a.Status, a.Reason)
		}
		w.WriteString("\n")
	}
	return w.String()
}

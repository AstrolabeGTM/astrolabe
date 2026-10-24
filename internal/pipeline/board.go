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
	b := &Board{Product: p.ID, StuckAfter: p.Pipeline.StuckAfter, Stuck: []*Account{}, Overdue: []*Account{}, RevisitDue: []*Account{}}
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

// Summary drops the per-account lists, keeping counts and what needs
// attention (for the digest).
func (b *Board) Summary() *Board {
	s := *b
	s.Open, s.Closed = nil, nil
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

// Package views builds what the operator looks at and does from more than
// one place: the MCP tools and the browser app's JSON API both call these
// functions, so chat and the screen always agree. Each function composes the
// domain packages (outreach, people, score, signal, ...); none of them holds
// rules of its own.
package views

import (
	"context"
	"slices"
	"strings"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/sandbox"
)

// SequenceSummary is one sequence of a product, enough to pick it.
type SequenceSummary struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Steps    int      `json:"steps"`
	Channels []string `json:"channels"`
	// Email is true when a step sends email, so the outreach guard applies.
	Email bool `json:"email"`
}

// ProductSummary is a loaded product as the operator sees it.
type ProductSummary struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	OneLiner    string            `json:"one_liner"`
	Funnel      []string          `json:"funnel"`
	Activation  string            `json:"activation"`
	Sequences   []SequenceSummary `json:"sequences"`
	Monitors    int               `json:"monitors"`
	Automations int               `json:"automations"`
	Demo        bool              `json:"demo,omitempty"`
}

// Summarize lists products with their sequences, sorted by id.
func Summarize(products []*product.Product) []ProductSummary {
	out := make([]ProductSummary, 0, len(products))
	for _, p := range products {
		ps := ProductSummary{ID: p.ID, Name: p.Name, OneLiner: p.OneLiner, Funnel: p.Funnel, Activation: p.Activation,
			Monitors: len(p.Monitors), Automations: len(p.Plays), Demo: p.Demo, Sequences: []SequenceSummary{}}
		for id, sq := range p.Sequences {
			s := SequenceSummary{ID: id, Kind: sq.Kind, Steps: len(sq.Steps), Email: outreach.HasEmailStep(sq), Channels: []string{}}
			for _, st := range sq.Steps {
				if !slices.Contains(s.Channels, st.Channel) {
					s.Channels = append(s.Channels, st.Channel)
				}
			}
			ps.Sequences = append(ps.Sequences, s)
		}
		slices.SortFunc(ps.Sequences, func(a, b SequenceSummary) int { return strings.Compare(a.ID, b.ID) })
		out = append(out, ps)
	}
	slices.SortFunc(out, func(a, b ProductSummary) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Status is the instance at a glance: the mode, what is loaded and what is
// paused. The browser app shows it on every screen.
type Status struct {
	Mode     string           `json:"mode"`
	ModeNote string           `json:"mode_note"`
	Products []ProductSummary `json:"products"`
	// NotLoaded counts product folders that failed to load (see setup).
	NotLoaded int      `json:"products_not_loaded"`
	Pauses    []string `json:"pauses"`
	// AIDrafting is true when the Claude API key is set.
	AIDrafting bool `json:"ai_drafting"`
}

// GetStatus reports the mode, products, load problems and pauses.
func GetStatus(ctx context.Context, out *outreach.Service, mode string, products []*product.Product, loadErrors []string, ai bool) (*Status, error) {
	pauses, err := out.Pauses(ctx)
	if err != nil {
		return nil, err
	}
	if pauses == nil {
		pauses = []string{}
	}
	st := &Status{Mode: mode, Products: Summarize(products), NotLoaded: len(loadErrors), Pauses: pauses, AIDrafting: ai}
	if mode == sandbox.Sandbox {
		st.ModeNote = "Sandbox: nothing leaves this machine. Messages go to the local outbox, where you can simulate replies."
	} else {
		st.Mode = "live"
		st.ModeNote = "Live: approved messages are sent to real people."
	}
	return st, nil
}

// LoadProblems turns the error from loading product folders into one line
// per problem folder, as the operator should read it.
func LoadProblems(err error) []string {
	out := []string{}
	var walk func(error)
	walk = func(err error) {
		if err == nil {
			return
		}
		if j, ok := err.(interface{ Unwrap() []error }); ok {
			for _, e := range j.Unwrap() {
				walk(e)
			}
			return
		}
		out = append(out, strings.TrimSpace(err.Error()))
	}
	walk(err)
	return out
}

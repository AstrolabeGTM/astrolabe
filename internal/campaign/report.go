package campaign

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// SpendTotals are sums over a campaign's spend entries.
type SpendTotals struct {
	Total      product.Money `json:"total"`
	Entries    int           `json:"entries"`
	BeforeGate product.Money `json:"before_gate_amount"`
	BeforeN    int           `json:"before_gate_entries"`
	OutsideAmt product.Money `json:"outside_window_amount"`
	OutsideN   int           `json:"outside_window_entries"`
	// Clicks are the platform's clicks as entered, on ClickEntries of Entries.
	Clicks       int64 `json:"platform_clicks"`
	ClickEntries int   `json:"entries_with_clicks"`
}

// Events counts success events attributed to the campaign.
//
// Attribution contract (one place, so form intake and product events can
// plug in): a signal whose data has campaign=<code> and whose type is the
// success event (or product.<success event>, which product events create),
// or a funnel event of the success stage whose source is campaign:<code>.
// A person counts once however many of these they have; a signal with no
// person counts once per signal.
type Events struct {
	Success   int `json:"success"`
	BySignal  int `json:"from_signals"`
	ByFunnel  int `json:"from_funnel_events"`
	TaggedAll int `json:"tagged_signals"` // all signals carrying campaign=<code>, any type
}

// window returns the campaign's report window as instants. to is nil when the
// campaign has no end date (counted up to now).
func (s Settings) window(loc *time.Location) (from time.Time, to *time.Time) {
	from, _ = time.ParseInLocation(dateLayout, s.Start, loc)
	if s.End != "" {
		e, _ := time.ParseInLocation(dateLayout, s.End, loc)
		e = e.AddDate(0, 0, 1)
		to = &e
	}
	return from, to
}

// CountEvents counts success events attributed to a campaign in its window.
func CountEvents(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign, s Settings, now time.Time) (Events, error) {
	from, to := s.window(now.Location())
	types := []string{c.SuccessEvent, "product." + c.SuccessEvent}
	stage := ""
	if slices.Contains(p.Funnel, c.SuccessEvent) {
		stage = c.SuccessEvent
	}
	var e Events
	err := q.QueryRow(ctx, `
		WITH hits AS (
			SELECT COALESCE('p' || person_id::text, 's' || id::text) AS k, 'signal' AS src FROM signals
			WHERE product_id = $1 AND data->>'campaign' = $2 AND type = ANY($3) AND occurred_at >= $4 AND ($5::timestamptz IS NULL OR occurred_at < $5)
			UNION ALL
			SELECT 'p' || person_id::text, 'funnel' FROM funnel_events
			WHERE product_id = $1 AND source = 'campaign:' || $2 AND stage = $6 AND occurred_at >= $4 AND ($5::timestamptz IS NULL OR occurred_at < $5))
		SELECT count(DISTINCT k), count(DISTINCT k) FILTER (WHERE src = 'signal'), count(DISTINCT k) FILTER (WHERE src = 'funnel') FROM hits`,
		p.ID, c.Code, types, from, to, stage).Scan(&e.Success, &e.BySignal, &e.ByFunnel)
	if err != nil {
		return e, err
	}
	err = q.QueryRow(ctx, `
		SELECT count(*) FROM signals WHERE product_id = $1 AND data->>'campaign' = $2 AND occurred_at >= $3 AND ($4::timestamptz IS NULL OR occurred_at < $4)`,
		p.ID, c.Code, from, to).Scan(&e.TaggedAll)
	return e, err
}

// Totals sums a campaign's spend. Spend dated outside the window is counted separately.
func Totals(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign, s Settings) (SpendTotals, error) {
	var t SpendTotals
	var total, before, outside int64
	var end any
	if s.End != "" {
		end = s.End
	}
	err := q.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(amount), 0),
			COALESCE(sum(amount) FILTER (WHERE NOT gate_complete), 0), count(*) FILTER (WHERE NOT gate_complete),
			COALESCE(sum(amount) FILTER (WHERE spent_on < $3::date OR ($4::date IS NOT NULL AND spent_on > $4::date)), 0),
			count(*) FILTER (WHERE spent_on < $3::date OR ($4::date IS NOT NULL AND spent_on > $4::date)),
			COALESCE(sum(clicks), 0), count(*) FILTER (WHERE clicks IS NOT NULL)
		FROM campaign_spend WHERE product_id = $1 AND campaign_id = $2`, p.ID, c.ID, s.Start, end).
		Scan(&t.Entries, &total, &before, &t.BeforeN, &outside, &t.OutsideN, &t.Clicks, &t.ClickEntries)
	t.Total, t.BeforeGate, t.OutsideAmt = product.Money(total), product.Money(before), product.Money(outside)
	return t, err
}

// Status is a campaign's settings, gate, spend and warnings right now.
type Status struct {
	Product  string      `json:"product"`
	ID       string      `json:"id"`
	Code     string      `json:"code"`
	Label    string      `json:"label"`
	Settings Settings    `json:"settings"`
	Active   bool        `json:"active"`
	Gate     []GateItem  `json:"gate"`
	GateOK   bool        `json:"gate_complete"`
	Spend    SpendTotals `json:"spend"`
	Events   Events      `json:"events"`
	PctOfCap *int        `json:"percent_of_cap,omitempty"`
	Warnings []string    `json:"warnings"`
}

// Check gathers the settings, gate, spend and success events and the
// warnings they imply. Warnings are only ever warnings: nothing is stopped,
// paused or raised by this.
func Check(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign, now time.Time) (*Status, error) {
	s, err := Effective(ctx, q, p, c)
	if err != nil {
		return nil, err
	}
	gate, ok, err := Gate(ctx, q, p, c)
	if err != nil {
		return nil, err
	}
	tot, err := Totals(ctx, q, p, c, s)
	if err != nil {
		return nil, err
	}
	ev, err := CountEvents(ctx, q, p, c, s, now)
	if err != nil {
		return nil, err
	}
	st := &Status{Product: p.ID, ID: c.ID, Code: c.Code, Label: c.Label, Settings: s, Active: s.Running(now), Gate: gate, GateOK: ok, Spend: tot, Events: ev}
	if s.Cap > 0 {
		pct := int(int64(tot.Total) * 100 / int64(s.Cap))
		st.PctOfCap = &pct
	}
	st.Warnings = st.warnings()
	return st, nil
}

func (st *Status) warnings() []string {
	var w []string
	s, t := st.Settings, st.Spend
	switch {
	case t.Total > s.Cap:
		w = append(w, fmt.Sprintf("OVER THE CAP: %s %s spent against a cap of %s %s. Nothing was stopped; decide whether to stop or to raise the cap on purpose.", t.Total, s.Currency, s.Cap, s.Currency))
	case s.Cap > 0 && int64(t.Total)*100 >= int64(s.Cap)*80:
		w = append(w, fmt.Sprintf("Near the cap: %s of %s %s spent (%d%%).", t.Total, s.Cap, s.Currency, *st.PctOfCap))
	}
	if t.BeforeN > 0 {
		w = append(w, fmt.Sprintf("%d spend entries (%s %s) were recorded before the gate was fully checked.", t.BeforeN, t.BeforeGate, s.Currency))
	}
	if !st.GateOK && t.Entries > 0 {
		var open []string
		for _, g := range st.Gate {
			if !g.Checked {
				open = append(open, g.Item)
			}
		}
		w = append(w, fmt.Sprintf("The gate is not fully checked and money has been spent. Unchecked: %s.", strings.Join(open, "; ")))
	}
	if t.OutsideN > 0 {
		w = append(w, fmt.Sprintf("%d spend entries (%s %s) are dated outside the campaign dates (%s to %s).", t.OutsideN, t.OutsideAmt, s.Currency, s.Start, orOpen(s.End)))
	}
	if r := s.StopWhen; r != nil && s.Cap > 0 && int64(t.Total)*100 >= int64(s.Cap)*int64(r.SpendPct) && st.Events.Success <= r.MaxEvents {
		w = append(w, fmt.Sprintf("STOP RULE MET: %s %s (%d%% of the cap) spent with %d success events (rule: at %d%% with at most %d). Rule: %s", t.Total, s.Currency, *st.PctOfCap, st.Events.Success, r.SpendPct, r.MaxEvents, s.StopText))
	}
	return w
}

func orOpen(end string) string {
	if end == "" {
		return "no end date"
	}
	return end
}

// Count is a number with what it is out of. Of is nil when there is nothing
// natural to divide by; no rates are ever computed from these.
type Count struct {
	Label  string `json:"label"`
	N      int64  `json:"n"`
	Of     *int64 `json:"of,omitempty"`
	OfWhat string `json:"of_what,omitempty"`
}

// Report is the campaign report for its window.
type Report struct {
	*Status
	WindowFrom   string   `json:"window_from"`
	WindowTo     string   `json:"window_to"`
	SoFar        bool     `json:"window_still_open"`
	Counts       []Count  `json:"counts"`
	CostPerEvent string   `json:"cost_per_success_event,omitempty"`
	NotMeasured  []string `json:"not_measured"`
}

func ptr(n int64) *int64 { return &n }

// BuildReport assembles the report. Zero results are shown as zero; there are
// no rates, only counts with their denominators; cost per success event
// appears only when there is at least one event.
func BuildReport(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign, now time.Time) (*Report, error) {
	st, err := Check(ctx, q, p, c, now)
	if err != nil {
		return nil, err
	}
	r := &Report{Status: st, WindowFrom: st.Settings.Start, WindowTo: st.Settings.End}
	r.SoFar = st.Settings.End == "" || st.Settings.End >= now.Format(dateLayout)
	if r.WindowTo == "" {
		r.WindowTo = now.Format(dateLayout) + " (open-ended, so far)"
	} else if r.SoFar {
		r.WindowTo += " (not over yet, so far)"
	}
	var linkClicks, posts, posted int64
	from, to := st.Settings.window(now.Location())
	if err := q.QueryRow(ctx, `
		SELECT count(*) FROM clicks k JOIN links l ON l.code = k.code
		WHERE l.product_id = $1 AND l.label LIKE 'campaign:' || $2 || ':%' AND k.clicked_at >= $3 AND ($4::timestamptz IS NULL OR k.clicked_at < $4)`,
		p.ID, c.Code, from, to).Scan(&linkClicks); err != nil {
		return nil, err
	}
	if err := q.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE status = 'posted') FROM content_items WHERE product_id = $1 AND campaign_id = $2 AND campaign_task IS NOT NULL`,
		p.ID, c.ID).Scan(&posts, &posted); err != nil {
		return nil, err
	}
	succ := Count{Label: fmt.Sprintf("success events (%s) attributed to campaign=%s", c.SuccessEvent, c.Code), N: int64(st.Events.Success)}
	if st.Spend.Clicks > 0 {
		succ.Of, succ.OfWhat = ptr(st.Spend.Clicks), "platform clicks as entered"
	} else {
		succ.OfWhat = "no platform clicks entered"
	}
	r.Counts = []Count{
		succ,
		{Label: "platform clicks as entered", N: st.Spend.Clicks, Of: ptr(int64(st.Spend.Entries)), OfWhat: fmt.Sprintf("spend entries (%d of them carry a click count)", st.Spend.ClickEntries)},
		{Label: "tracked-link clicks (Astrolabe short links made for this campaign)", N: linkClicks},
		{Label: "signals carrying campaign=" + c.Code + " (any type)", N: int64(st.Events.TaggedAll)},
		{Label: "posting tasks posted", N: posted, Of: ptr(posts), OfWhat: "posting tasks made"},
		{Label: "gate items checked", N: int64(countChecked(st.Gate)), Of: ptr(int64(len(st.Gate))), OfWhat: "gate items"},
	}
	if st.Events.Success > 0 && st.Spend.Total > 0 {
		r.CostPerEvent = fmt.Sprintf("%s %s per success event (%s %s over %d events)", product.Money(int64(st.Spend.Total)/int64(st.Events.Success)), st.Settings.Currency, st.Spend.Total, st.Settings.Currency, st.Events.Success)
	}
	r.NotMeasured = []string{
		"Downloads per campaign: a download is not tied to a campaign or a person; release download counts are shown separately in counters.",
		"Anyone who visited the landing page and did nothing: there is no tracking pixel and no page-view counting.",
		"Impressions and reach, except what you typed in or imported from the platform.",
		"Success events that do not carry campaign=" + c.Code + ": a signup without it is not credited to this campaign, even if it came from it. Ask people where they heard of you and record it.",
		"Whether the campaign caused a result; these are counts of events that carried the code.",
	}
	return r, nil
}

func countChecked(g []GateItem) int {
	n := 0
	for _, x := range g {
		if x.Checked {
			n++
		}
	}
	return n
}

// Text renders the report for chat.
func (r *Report) Text() string {
	var b strings.Builder
	s := r.Settings
	fmt.Fprintf(&b, "Campaign %s (%s), %s to %s\n", r.Label, r.Code, r.WindowFrom, r.WindowTo)
	if s.Cap == 0 && r.Spend.Total == 0 {
		fmt.Fprintf(&b, "Spend: 0.00 %s, cap 0.00 (nothing is planned to be spent)\n", s.Currency)
	} else {
		fmt.Fprintf(&b, "Spend: %s of %s %s cap", r.Spend.Total, s.Cap, s.Currency)
		if r.PctOfCap != nil {
			fmt.Fprintf(&b, " (%d%%)", *r.PctOfCap)
		}
		fmt.Fprintf(&b, ", %d entries\n", r.Spend.Entries)
	}
	for _, c := range r.Counts {
		fmt.Fprintf(&b, "  %s: %d", c.Label, c.N)
		if c.Of != nil {
			fmt.Fprintf(&b, " of %d %s", *c.Of, c.OfWhat)
		} else if c.OfWhat != "" {
			fmt.Fprintf(&b, " (%s)", c.OfWhat)
		}
		b.WriteString("\n")
	}
	if r.CostPerEvent != "" {
		fmt.Fprintf(&b, "  Cost: %s\n", r.CostPerEvent)
	} else if r.Spend.Total > 0 {
		fmt.Fprintf(&b, "  Cost per success event: not computed (0 success events for %s %s spent)\n", r.Spend.Total, s.Currency)
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "WARNING: %s\n", w)
	}
	b.WriteString("Not measured:\n")
	for _, n := range r.NotMeasured {
		fmt.Fprintf(&b, "  - %s\n", n)
	}
	return b.String()
}

// ---- listing ----

// Summary is one line per campaign.
type Summary struct {
	Product  string   `json:"product"`
	ID       string   `json:"id"`
	Code     string   `json:"code"`
	Label    string   `json:"label"`
	Start    string   `json:"start"`
	End      string   `json:"end,omitempty"`
	Active   bool     `json:"active"`
	Cap      string   `json:"budget_cap"`
	Spent    string   `json:"spent"`
	Currency string   `json:"currency"`
	Gate     string   `json:"gate"` // "2 of 3 checked"
	Warnings []string `json:"warnings"`
}

// Summaries lists a product's campaigns with gate status and warnings. With
// onlyRunning, only campaigns that have started and not ended.
func Summaries(ctx context.Context, q store.Q, p *product.Product, now time.Time, onlyRunning bool) ([]Summary, error) {
	out := []Summary{}
	for _, c := range p.Campaigns {
		st, err := Check(ctx, q, p, c, now)
		if err != nil {
			return nil, err
		}
		if onlyRunning && !st.Active {
			continue
		}
		out = append(out, Summary{Product: p.ID, ID: c.ID, Code: c.Code, Label: c.Label, Start: st.Settings.Start, End: st.Settings.End,
			Active: st.Active, Cap: st.Settings.Cap.String(), Spent: st.Spend.Total.String(), Currency: st.Settings.Currency,
			Gate: fmt.Sprintf("%d of %d checked", countChecked(st.Gate), len(st.Gate)), Warnings: st.Warnings})
	}
	return out, nil
}

// Variant is one entry of campaigns.json.
type Variant struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// Variants lists the landing-page variants of campaigns that have not ended
// (so a page exists before the first day), as a static site wants them:
// exactly code and label, nothing else.
func Variants(ctx context.Context, q store.Q, products []*product.Product, now time.Time) ([]Variant, error) {
	out := []Variant{}
	seen := map[string]string{}
	for _, p := range products {
		for _, c := range p.Campaigns {
			s, err := Effective(ctx, q, p, c)
			if err != nil {
				return nil, err
			}
			if !s.NotEnded(now) {
				continue
			}
			if other, dup := seen[c.Code]; dup {
				return nil, fmt.Errorf("campaign code %q is used by both %s and %s; export one product at a time", c.Code, other, p.ID)
			}
			seen[c.Code] = p.ID
			out = append(out, Variant{Code: c.Code, Label: c.Label})
		}
	}
	slices.SortFunc(out, func(a, b Variant) int { return strings.Compare(a.Code, b.Code) })
	return out, nil
}

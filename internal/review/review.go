// Package review builds the weekly review: one structured page of numbers
// for an ISO week that answers the Friday questions (what reached
// activation, what changed in reach, how accounts moved, how fast we
// answered people, what people said). It reuses the summaries the digest
// uses; it computes no number twice. Every figure is a count, with its
// denominator and window beside it.
//
// Plain functions, no HTML: the MCP tools and the browser app both call
// them. Saved reviews (see saved.go) keep the operator's answers and a
// frozen copy of the built review.
package review

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/campaign"
	"github.com/AstrolabeGTM/astrolabe/internal/counters"
	"github.com/AstrolabeGTM/astrolabe/internal/experiment"
	"github.com/AstrolabeGTM/astrolabe/internal/funnel"
	"github.com/AstrolabeGTM/astrolabe/internal/notes"
	"github.com/AstrolabeGTM/astrolabe/internal/pipeline"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/reception"
	"github.com/AstrolabeGTM/astrolabe/internal/updates"
)

// Window is the span a number covers. From is inclusive; To is the last
// day counted. Partial is true while the week is still going: the numbers
// cover the days so far.
type Window struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Partial bool   `json:"partial,omitempty"`
}

// Review is one week for one product.
type Review struct {
	Product string    `json:"product"`
	Name    string    `json:"name"`
	Week    string    `json:"week"` // ISO week, like 2026-W43
	Window  Window    `json:"window"`
	BuiltAt time.Time `json:"built_at"`
	// Weekday is the configured review day, e.g. "friday".
	Weekday string `json:"review_day"`

	Spread      Spread             `json:"spread"`
	Users       Users              `json:"users"`
	Accounts    *Accounts          `json:"accounts,omitempty"` // products with a pipeline
	Response    ResponseTimes      `json:"response_times"`
	Themes      Themes             `json:"themes"`
	Campaigns   []campaign.Summary `json:"campaigns"`
	Experiments []Experiment       `json:"experiments"`
	Questions   []Question         `json:"questions"`
}

// Tally is a count with the total it is part of.
type Tally struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Spread is reach: what changed in how many people can find the product.
type Spread struct {
	// Counters: public counters' change over the 7 days up to the end of
	// the window against the 7 before (nil without a counter monitor).
	Counters *counters.Report `json:"public_counters,omitempty"`
	// Updates: the updates list's size and its changes (nil without a list).
	Updates *updates.Summary `json:"updates_list,omitempty"`
	// Mentions: public mentions by tone; the 7 days up to the end of the window.
	Mentions Mentions `json:"mentions"`
	// Requests: form requests this week by the source people reported.
	Requests funnel.Requests `json:"requests_by_reported_source"`
	// Signals: new signals this week by type.
	Signals Signals `json:"new_signals"`
}

type Mentions struct {
	Window    reception.Window    `json:"window"`
	Total     int                 `json:"total"`
	ByTone    map[string]int      `json:"by_tone"`
	Questions int                 `json:"asking_a_question"`
	Latest    []reception.Mention `json:"latest"`
}

type Signals struct {
	Total  int     `json:"total"`
	ByType []Tally `json:"by_type"`
}

// Users: people moving through the funnel.
type Users struct {
	Stages []StageEntries `json:"stages"`
	// Cohort: people whose first funnel event fell in the four weeks up to
	// the end of this one, and how far each got. Counts as of now.
	Cohort Cohort `json:"four_week_cohort"`
	// Weekly cohorts in that span, from the funnel report.
	WeeklyCohorts []funnel.Cohort  `json:"weekly_cohorts"`
	PMF           *notes.PMFCounts `json:"pmf_survey_so_far"`
	Activation    Activation       `json:"activation"`
}

// StageEntries: people who reached a stage for the first time this week.
type StageEntries struct {
	Stage        string `json:"stage"`
	Entered      int    `json:"entered_this_week"`
	PreviousWeek int    `json:"entered_previous_week"`
	Activation   bool   `json:"activation,omitempty"`
}

type Cohort struct {
	From     string        `json:"from"`
	To       string        `json:"to"`
	People   int           `json:"people"`
	Complete bool          `json:"complete"` // false while the newest people are inside the offer window
	Stages   []CohortStage `json:"stages"`
}

// CohortStage: Reached of Previous, where Previous is the people who
// reached the stage before (the cohort size for the first stage).
type CohortStage struct {
	Stage    string `json:"stage"`
	Reached  int    `json:"reached"`
	Previous int    `json:"of_previous_stage"`
	Percent  int    `json:"percent_of_previous"`
}

// Activation: who activated this week, and whether we coached them.
// Assisted means a note marks a session with that person as assisted.
type Activation struct {
	Stage      string `json:"stage"`
	Activated  int    `json:"activated_this_week"`
	Assisted   int    `json:"assisted"`
	Unassisted int    `json:"unassisted"`
	// Sessions are the week's conversation notes, assisted apart.
	Sessions notes.Counts `json:"conversation_notes_this_week"`
}

// Accounts: the pipeline this week.
type Accounts struct {
	OpenAccounts   int             `json:"open_accounts"`
	Entries        []StageEntries2 `json:"entries_this_week"`
	MedianDays     []StageDays     `json:"median_days_in_stage"`
	Board          *pipeline.Board `json:"board"`
	ClosedThisWeek []Reason        `json:"paused_and_lost_this_week"`
	ClosedNow      []Reason        `json:"paused_and_lost_now"`
	Advocates      []Advocate      `json:"advocates"`
}

// StageEntries2: accounts entering a stage (or paused, lost) this week.
type StageEntries2 struct {
	Stage    string `json:"stage"`
	Accounts int    `json:"accounts"`
}

// StageDays: the median time accounts spent in a stage, over the visits
// that ended before the window's end.
type StageDays struct {
	Stage      string  `json:"stage"`
	MedianDays float64 `json:"median_days"`
	Visits     int     `json:"visits"`
}

type Reason struct {
	Status   string `json:"status"` // paused or lost
	Reason   string `json:"reason"`
	Accounts int    `json:"accounts"`
}

type Advocate struct {
	Company string `json:"company"`
	Person  string `json:"person"`
	Account int64  `json:"account_id"`
}

// Themes: what people said.
type Themes struct {
	Saved   *notes.Themes         `json:"saved,omitempty"`
	Pricing *notes.PricingSummary `json:"pricing_feedback"`
}

// Experiment: a running A/B test with its verdict.
type Experiment struct {
	experiment.Result
}

// Question is one of the review's questions with its saved answer.
type Question struct {
	Number   int    `json:"number"`
	Text     string `json:"question"`
	Answer   string `json:"answer,omitempty"`
	Answered bool   `json:"answered"`
}

// Build builds the review for a product and ISO week (default: the week
// containing now).
func Build(ctx context.Context, pool *pgxpool.Pool, p *product.Product, week string) (*Review, error) {
	return BuildAt(ctx, pool, p, week, time.Now())
}

// BuildAt is Build as of a given time.
func BuildAt(ctx context.Context, pool *pgxpool.Pool, p *product.Product, week string, now time.Time) (*Review, error) {
	now = now.UTC()
	if week == "" {
		week = notes.ISOWeek(now)
	}
	from, to, err := notes.WeekBounds(week)
	if err != nil {
		return nil, err
	}
	if from.After(now) {
		return nil, fmt.Errorf("%w: %s has not started", notes.ErrInvalid, week)
	}
	// end: where the window stops. Inside the current week it is now.
	end, partial := to, false
	if now.Before(to) {
		end, partial = now, true
	}
	lastDay := end.Add(-time.Nanosecond).Truncate(24 * time.Hour)
	set := p.ReviewSettings()
	r := &Review{Product: p.ID, Name: p.Name, Week: week, BuiltAt: now, Weekday: strings.ToLower(set.Weekday.String()),
		Window: Window{From: from.Format("2006-01-02"), To: lastDay.Format("2006-01-02"), Partial: partial}}

	if err := r.spread(ctx, pool, p, from, to, end); err != nil {
		return nil, fmt.Errorf("spread: %w", err)
	}
	if err := r.users(ctx, pool, p, from, to, end, lastDay); err != nil {
		return nil, fmt.Errorf("users: %w", err)
	}
	if p.Pipeline != nil {
		if r.Accounts, err = accounts(ctx, pool, p, from, to, end); err != nil {
			return nil, fmt.Errorf("accounts: %w", err)
		}
	}
	if r.Response, err = responseTimes(ctx, pool, p, from, to, now); err != nil {
		return nil, fmt.Errorf("response times: %w", err)
	}
	days := int(lastDay.Sub(from).Hours()/24) + 1
	r.Themes.Pricing, err = notes.PricingFeedback(ctx, pool, p.ID, days, lastDay)
	if err != nil {
		return nil, fmt.Errorf("pricing: %w", err)
	}
	if r.Themes.Saved, err = notes.GetThemes(ctx, pool, p.ID, week); err != nil {
		return nil, fmt.Errorf("themes: %w", err)
	}
	endMinus := end.Add(-time.Second)
	if r.Campaigns, err = campaign.Summaries(ctx, pool, p, endMinus, true); err != nil {
		return nil, fmt.Errorf("campaigns: %w", err)
	}
	if r.Experiments, err = runningExperiments(ctx, pool, p); err != nil {
		return nil, fmt.Errorf("experiments: %w", err)
	}
	saved, err := Get(ctx, pool, p, week)
	if err != nil {
		return nil, err
	}
	r.Questions = questionsWith(set.Questions, saved)
	return r, nil
}

func (r *Review) spread(ctx context.Context, pool *pgxpool.Pool, p *product.Product, from, to, end time.Time) error {
	var err error
	if r.Spread.Counters, err = counters.Weekly(ctx, pool, p.ID, end); err != nil {
		return err
	}
	if len(r.Spread.Counters.Repos) == 0 {
		r.Spread.Counters = nil
	}
	if u, ok := p.UpdatesSettings(); ok {
		sum, err := updates.WeekSummary(ctx, pool, p.ID, u.Name, end)
		if err != nil {
			return err
		}
		if !sum.Empty() {
			r.Spread.Updates = sum
		}
	}
	rec, err := reception.Week(ctx, pool, p.ID, end, 5)
	if err != nil {
		return err
	}
	r.Spread.Mentions = Mentions{Window: rec.Window, Total: rec.Total, ByTone: rec.ByTone, Questions: rec.Questions, Latest: rec.Mentions}
	if r.Spread.Requests, err = funnel.RequestsBetween(ctx, pool, p.ID, from, to); err != nil {
		return err
	}
	rows, err := pool.Query(ctx, `
		SELECT type, count(*) FROM signals WHERE product_id = $1 AND occurred_at >= $2 AND occurred_at < $3
		GROUP BY 1 ORDER BY 2 DESC, 1`, p.ID, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	r.Spread.Signals.ByType = []Tally{}
	for rows.Next() {
		var t Tally
		if err := rows.Scan(&t.Name, &t.Count); err != nil {
			return err
		}
		r.Spread.Signals.Total += t.Count
		r.Spread.Signals.ByType = append(r.Spread.Signals.ByType, t)
	}
	return rows.Err()
}

func (r *Review) users(ctx context.Context, pool *pgxpool.Pool, p *product.Product, from, to, end, lastDay time.Time) error {
	prev := from.AddDate(0, 0, -7)
	for _, st := range p.Funnel {
		e := StageEntries{Stage: st, Activation: st == p.Activation}
		if err := pool.QueryRow(ctx, `
			WITH first AS (SELECT person_id, min(occurred_at) t FROM funnel_events WHERE product_id = $1 AND stage = $2 GROUP BY 1)
			SELECT count(*) FILTER (WHERE t >= $3 AND t < $4), count(*) FILTER (WHERE t >= $5 AND t < $3) FROM first`,
			p.ID, st, from, to, prev).Scan(&e.Entered, &e.PreviousWeek); err != nil {
			return err
		}
		r.Users.Stages = append(r.Users.Stages, e)
	}

	// Fixed four-week cohort: first funnel event in the four weeks ending with this one.
	cFrom := from.AddDate(0, 0, -21)
	rows, err := pool.Query(ctx, `
		WITH entry AS (SELECT person_id FROM funnel_events WHERE product_id = $1 GROUP BY 1 HAVING min(occurred_at) >= $2 AND min(occurred_at) < $3)
		SELECT f.person_id, f.stage FROM funnel_events f JOIN entry e ON e.person_id = f.person_id WHERE f.product_id = $1`, p.ID, cFrom, to)
	if err != nil {
		return err
	}
	furthest := map[int64]int{}
	for rows.Next() {
		var id int64
		var stage string
		if err := rows.Scan(&id, &stage); err != nil {
			rows.Close()
			return err
		}
		i := slices.Index(p.Funnel, stage)
		if cur, ok := furthest[id]; !ok || i > cur {
			furthest[id] = i
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	window, _ := product.AfterDuration(p.Offer.Window)
	c := Cohort{From: cFrom.Format("2006-01-02"), To: lastDay.Format("2006-01-02"), People: len(furthest),
		Complete: !end.Before(to) && to.Add(window).Before(r.BuiltAt)}
	prevN := c.People
	for i, st := range p.Funnel {
		n := 0
		for _, f := range furthest {
			if f >= i {
				n++
			}
		}
		cs := CohortStage{Stage: st, Reached: n, Previous: prevN}
		if prevN > 0 {
			cs.Percent = 100 * n / prevN
		}
		c.Stages = append(c.Stages, cs)
		prevN = n
	}
	r.Users.Cohort = c

	rep, err := funnel.BuildReport(ctx, pool, p, cFrom, end)
	if err != nil {
		return err
	}
	r.Users.WeeklyCohorts = []funnel.Cohort{}
	for _, wc := range rep.Cohorts {
		if !wc.Week.Before(cFrom) && wc.Week.Before(to) {
			r.Users.WeeklyCohorts = append(r.Users.WeeklyCohorts, wc)
		}
	}
	sort.Slice(r.Users.WeeklyCohorts, func(i, j int) bool { return r.Users.WeeklyCohorts[i].Week.Before(r.Users.WeeklyCohorts[j].Week) })

	if r.Users.PMF, err = notes.PMFSurvey(ctx, pool, p.ID, 0, lastDay); err != nil {
		return err
	}
	a := Activation{Stage: p.Activation}
	if err := pool.QueryRow(ctx, `
		WITH act AS (SELECT person_id FROM funnel_events WHERE product_id = $1 AND stage = $2 GROUP BY 1 HAVING min(occurred_at) >= $3 AND min(occurred_at) < $4)
		SELECT count(*), count(*) FILTER (WHERE EXISTS (SELECT 1 FROM conversations c WHERE c.product_id = $1 AND c.person_id = act.person_id AND c.assisted AND c.occurred_on <= $5::date))
		FROM act`, p.ID, p.Activation, from, to, lastDay).Scan(&a.Activated, &a.Assisted); err != nil {
		return err
	}
	a.Unassisted = a.Activated - a.Assisted
	if a.Sessions, err = notes.Count(ctx, pool, notes.ListOpts{Product: p.ID, From: from, To: to}, end); err != nil {
		return err
	}
	r.Users.Activation = a
	return nil
}

func accounts(ctx context.Context, pool *pgxpool.Pool, p *product.Product, from, to, end time.Time) (*Accounts, error) {
	a := &Accounts{}
	board, err := pipeline.BuildBoard(ctx, pool, p, end)
	if err != nil {
		return nil, err
	}
	a.Board = board.Summary()
	for _, s := range board.Stages {
		a.OpenAccounts += s.Accounts
	}
	// Entries this week, in stage order, then paused and lost.
	counts := map[string]int{}
	rows, err := pool.Query(ctx, `
		SELECT s.stage, count(*) FROM account_stages s JOIN accounts a ON a.id = s.account_id
		WHERE a.product_id = $1 AND s.entered_at >= $2 AND s.entered_at < $3 GROUP BY 1`, p.ID, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			rows.Close()
			return nil, err
		}
		counts[s] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, s := range append(slices.Clone(p.Pipeline.Stages), product.ClosedStates...) {
		a.Entries = append(a.Entries, StageEntries2{Stage: s, Accounts: counts[s]})
	}
	// Median days per stage over visits that ended before the window's end.
	rows, err = pool.Query(ctx, `
		WITH v AS (SELECT s.stage, s.entered_at, lead(s.entered_at) OVER (PARTITION BY s.account_id ORDER BY s.id) AS left_at
			FROM account_stages s JOIN accounts a ON a.id = s.account_id WHERE a.product_id = $1)
		SELECT stage, count(*), percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM left_at - entered_at) / 86400)
		FROM v WHERE left_at IS NOT NULL AND left_at < $2 GROUP BY 1`, p.ID, end)
	if err != nil {
		return nil, err
	}
	med := map[string]StageDays{}
	for rows.Next() {
		var d StageDays
		if err := rows.Scan(&d.Stage, &d.Visits, &d.MedianDays); err != nil {
			rows.Close()
			return nil, err
		}
		d.MedianDays = float64(int(d.MedianDays*10+0.5)) / 10
		med[d.Stage] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, s := range p.Pipeline.Stages {
		d, ok := med[s]
		if !ok {
			d = StageDays{Stage: s}
		}
		a.MedianDays = append(a.MedianDays, d)
	}

	if a.ClosedThisWeek, err = reasons(ctx, pool, `
		SELECT s.stage, s.reason, count(*) FROM account_stages s JOIN accounts a ON a.id = s.account_id
		WHERE a.product_id = $1 AND s.stage IN ('paused', 'lost') AND s.entered_at >= $2 AND s.entered_at < $3 GROUP BY 1, 2 ORDER BY 3 DESC, 1, 2`, p.ID, from, to); err != nil {
		return nil, err
	}
	if a.ClosedNow, err = reasons(ctx, pool, `
		SELECT status, reason, count(*) FROM accounts WHERE product_id = $1 AND status <> 'open' GROUP BY 1, 2 ORDER BY 3 DESC, 1, 2`, p.ID); err != nil {
		return nil, err
	}
	a.Advocates = []Advocate{}
	rows, err = pool.Query(ctx, `
		SELECT COALESCE(NULLIF(c.name, ''), c.domain, '#' || c.id), COALESCE(NULLIF(pe.name, ''), '#' || pe.id), a.id
		FROM account_people ap JOIN accounts a ON a.id = ap.account_id JOIN companies c ON c.id = a.company_id JOIN people pe ON pe.id = ap.person_id
		WHERE a.product_id = $1 AND ap.role = 'advocate' ORDER BY 1, 2`, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x Advocate
		if err := rows.Scan(&x.Company, &x.Person, &x.Account); err != nil {
			return nil, err
		}
		a.Advocates = append(a.Advocates, x)
	}
	return a, rows.Err()
}

func reasons(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) ([]Reason, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reason{}
	for rows.Next() {
		var x Reason
		if err := rows.Scan(&x.Status, &x.Reason, &x.Accounts); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// runningExperiments: experiments on steps that still have variants, with
// their verdict. An experiment with too little data says so in its verdict.
func runningExperiments(ctx context.Context, pool *pgxpool.Pool, p *product.Product) ([]Experiment, error) {
	res, err := experiment.Report(ctx, pool, p)
	if err != nil {
		return nil, err
	}
	out := []Experiment{}
	for _, x := range res {
		parts := strings.Split(x.Key, "/")
		if len(parts) != 3 {
			continue
		}
		seq := p.Sequences[parts[1]]
		var step int
		fmt.Sscanf(parts[2], "%d", &step)
		if seq == nil || step < 0 || step >= len(seq.Steps) || len(seq.Steps[step].Variants) < 2 {
			continue
		}
		out = append(out, Experiment{x})
	}
	return out, nil
}

// questionsWith pairs the questions with the saved answers (matched by text).
func questionsWith(qs []string, saved *Saved) []Question {
	out := make([]Question, len(qs))
	for i, q := range qs {
		out[i] = Question{Number: i + 1, Text: q}
		if saved != nil {
			for _, a := range saved.Answers {
				if a.Question == q && strings.TrimSpace(a.Answer) != "" {
					out[i].Answer, out[i].Answered = a.Answer, true
				}
			}
		}
	}
	return out
}

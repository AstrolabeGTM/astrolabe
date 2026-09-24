package funnel

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/llm"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

type Report struct {
	Since    time.Time    `json:"since"`
	Stages   []Stage      `json:"stages"`
	Times    []StageTime  `json:"time_between_stages"`
	Stuck    []StuckStage `json:"stuck"`
	Cohorts  []Cohort     `json:"weekly_cohorts"`
	Sources  []Attrib     `json:"by_source"`
	Plays    []Attrib     `json:"by_play"`
	Revenue  []Money      `json:"revenue"`
	Spend    Spend        `json:"spend"`
	Outcomes []Outcome    `json:"task_outcomes"`
}

type StageTime struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	MedianDays float64 `json:"median_days"`
	People     int     `json:"people"`
}

type StuckStage struct {
	Stage  string        `json:"stage"`
	Days   int           `json:"inactive_days"`
	Count  int           `json:"count"`
	People []StuckPerson `json:"people"`
}

type StuckPerson struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Days int    `json:"days_inactive"`
}

type Cohort struct {
	Week      time.Time `json:"week"`
	People    int       `json:"people"`
	Activated int       `json:"activated"`
	Paid      int       `json:"paid"`
	// Complete is false while the cohort is still inside the offer's
	// observation window: its numbers will still grow.
	Complete bool `json:"complete"`
}

// Attrib counts people from a source or play and how far they got.
type Attrib struct {
	Key       string           `json:"key"`
	People    int              `json:"people"`
	Replied   int              `json:"replied"`
	Activated int              `json:"activated"`
	Paid      int              `json:"paid"`
	Revenue   map[string]int64 `json:"revenue_cents,omitempty"`
}

type Money struct {
	Currency string `json:"currency"`
	Payments int64  `json:"payments_cents"`
	Refunds  int64  `json:"refunds_cents"`
	Net      int64  `json:"net_cents"`
	Count    int    `json:"payments"`
}

type Spend struct {
	LLMCalls     int      `json:"llm_calls"`
	InTokens     int64    `json:"input_tokens"`
	OutTokens    int64    `json:"output_tokens"`
	LLMUSD       *float64 `json:"llm_usd,omitempty"` // only when ASTROLABE_LLM_PRICES covers every model used
	Minutes      int      `json:"operator_minutes"`
	Activated    int      `json:"activated"`
	Paying       int      `json:"paying"`
	USDPerActive *float64 `json:"llm_usd_per_activated,omitempty"`
	USDPerPaying *float64 `json:"llm_usd_per_paying,omitempty"`
	MinPerActive *float64 `json:"minutes_per_activated,omitempty"`
}

type Outcome struct {
	Outcome string `json:"outcome"`
	N       int    `json:"count"`
	Minutes int    `json:"minutes"`
}

const defaultStuckDays = 7

// BuildReport computes the full funnel picture for a product since a time.
func BuildReport(ctx context.Context, q store.Q, p *product.Product, since, now time.Time) (*Report, error) {
	r := &Report{Since: since}
	var err error
	if r.Stages, err = Counts(ctx, q, p, since); err != nil {
		return nil, err
	}
	steps := []func() error{
		func() error { return r.times(ctx, q, p, since) },
		func() error { return r.stuck(ctx, q, p, now) },
		func() error { return r.cohorts(ctx, q, p, since, now) },
		func() error { return r.attribution(ctx, q, p, since) },
		func() error { return r.money(ctx, q, p, since) },
		func() error { return r.spend(ctx, q, p, since) },
	}
	for _, f := range steps {
		if err := f(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Report) times(ctx context.Context, q store.Q, p *product.Product, since time.Time) error {
	for i := 0; i+1 < len(p.Funnel); i++ {
		st := StageTime{From: p.Funnel[i], To: p.Funnel[i+1]}
		var median *float64
		if err := q.QueryRow(ctx, `
			WITH a AS (SELECT person_id, min(occurred_at) t FROM funnel_events WHERE product_id = $1 AND stage = $2 GROUP BY 1),
			     b AS (SELECT person_id, min(occurred_at) t FROM funnel_events WHERE product_id = $1 AND stage = $3 GROUP BY 1)
			SELECT count(*), percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM b.t - a.t) / 86400)
			FROM a JOIN b USING (person_id) WHERE b.t >= a.t AND b.t >= $4`, p.ID, st.From, st.To, since).Scan(&st.People, &median); err != nil {
			return err
		}
		if median != nil {
			st.MedianDays = *median
		}
		r.Times = append(r.Times, st)
	}
	return nil
}

// stuckDays is the stall length a stage_stuck play uses for a stage, else 7.
func stuckDays(p *product.Product, stage string) int {
	for _, pl := range p.Plays {
		if st := pl.Trigger.StageStuck; st != nil && st.Stage == stage {
			if d, err := product.AfterDuration(st.For); err == nil {
				return int(d.Hours() / 24)
			}
		}
	}
	return defaultStuckDays
}

func (r *Report) stuck(ctx context.Context, q store.Q, p *product.Product, now time.Time) error {
	for i, stage := range p.Funnel {
		if i == len(p.Funnel)-1 {
			break
		}
		days := stuckDays(p, stage)
		rows, err := q.Query(ctx, `
			WITH furthest AS (
				SELECT person_id, max(array_position($2::text[], stage)) idx FROM funnel_events WHERE product_id = $1 GROUP BY 1),
			activity AS (
				SELECT person_id, max(t) last FROM (
					SELECT person_id, occurred_at t FROM funnel_events WHERE product_id = $1
					UNION ALL SELECT person_id, occurred_at FROM signals WHERE product_id = $1 AND person_id IS NOT NULL AND type LIKE 'product.%'
				) x GROUP BY 1)
			SELECT f.person_id, COALESCE(NULLIF(p.name, ''), (SELECT value FROM identities WHERE person_id = p.id LIMIT 1), ''),
				floor(extract(epoch FROM $4::timestamptz - a.last) / 86400)::int
			FROM furthest f JOIN activity a USING (person_id) JOIN people p ON p.id = f.person_id
			WHERE f.idx = $3 AND a.last < $4::timestamptz - make_interval(days => $5)
			ORDER BY a.last`, p.ID, p.Funnel, i+1, now, days)
		if err != nil {
			return err
		}
		people, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (StuckPerson, error) {
			var sp StuckPerson
			return sp, row.Scan(&sp.ID, &sp.Name, &sp.Days)
		})
		if err != nil {
			return err
		}
		s := StuckStage{Stage: stage, Days: days, Count: len(people)}
		if len(people) > 20 {
			people = people[:20]
		}
		s.People = people
		r.Stuck = append(r.Stuck, s)
	}
	return nil
}

func (r *Report) cohorts(ctx context.Context, q store.Q, p *product.Product, since, now time.Time) error {
	window, _ := product.AfterDuration(p.Offer.Window)
	rows, err := q.Query(ctx, `
		WITH entry AS (SELECT person_id, date_trunc('week', min(occurred_at)) wk FROM funnel_events WHERE product_id = $1 GROUP BY 1)
		SELECT e.wk, count(*),
			count(*) FILTER (WHERE EXISTS (SELECT 1 FROM funnel_events f WHERE f.product_id = $1 AND f.person_id = e.person_id AND f.stage = $2)),
			count(*) FILTER (WHERE EXISTS (SELECT 1 FROM funnel_events f WHERE f.product_id = $1 AND f.person_id = e.person_id AND f.stage = 'paid'))
		FROM entry e WHERE e.wk >= date_trunc('week', $3::timestamptz) GROUP BY 1 ORDER BY 1 DESC LIMIT 26`, p.ID, p.Activation, since)
	if err != nil {
		return err
	}
	r.Cohorts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Cohort, error) {
		var c Cohort
		err := row.Scan(&c.Week, &c.People, &c.Activated, &c.Paid)
		c.Complete = c.Week.Add(7*24*time.Hour + window).Before(now)
		return c, err
	})
	return err
}

func (r *Report) attribution(ctx context.Context, q store.Q, p *product.Product, since time.Time) error {
	// One row per (person, key) with the time counting starts from; stages
	// and revenue count only from then on.
	collect := func(sql string) ([]Attrib, error) {
		rows, err := q.Query(ctx, `
			WITH k AS (`+sql+`)
			SELECT k.key, k.person_id,
				EXISTS (SELECT 1 FROM funnel_events f WHERE f.product_id = $1 AND f.person_id = k.person_id AND f.stage = 'replied' AND f.occurred_at >= k.t0),
				EXISTS (SELECT 1 FROM funnel_events f WHERE f.product_id = $1 AND f.person_id = k.person_id AND f.stage = $2 AND f.occurred_at >= k.t0),
				EXISTS (SELECT 1 FROM funnel_events f WHERE f.product_id = $1 AND f.person_id = k.person_id AND f.stage = 'paid' AND f.occurred_at >= k.t0),
				COALESCE((SELECT array_agg(currency) FROM payments pay WHERE pay.product_id = $1 AND pay.person_id = k.person_id AND pay.occurred_at >= k.t0), '{}'),
				COALESCE((SELECT array_agg(amount_cents) FROM payments pay WHERE pay.product_id = $1 AND pay.person_id = k.person_id AND pay.occurred_at >= k.t0), '{}')
			FROM k`, p.ID, p.Activation, since)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		byKey := map[string]*Attrib{}
		var order []string
		for rows.Next() {
			var key string
			var person int64
			var replied, activated, paid bool
			var curs []string
			var amts []int64
			if err := rows.Scan(&key, &person, &replied, &activated, &paid, &curs, &amts); err != nil {
				return nil, err
			}
			a := byKey[key]
			if a == nil {
				a = &Attrib{Key: key}
				byKey[key] = a
				order = append(order, key)
			}
			a.People++
			if replied {
				a.Replied++
			}
			if activated {
				a.Activated++
			}
			if paid {
				a.Paid++
			}
			for i := range curs {
				if a.Revenue == nil {
					a.Revenue = map[string]int64{}
				}
				a.Revenue[curs[i]] += amts[i]
			}
		}
		out := make([]Attrib, 0, len(order))
		for _, k := range order {
			out = append(out, *byKey[k])
		}
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Paid != out[j].Paid {
				return out[i].Paid > out[j].Paid
			}
			if out[i].Activated != out[j].Activated {
				return out[i].Activated > out[j].Activated
			}
			return out[i].People > out[j].People
		})
		return out, rows.Err()
	}
	var err error
	// Source: the first touch that brought each person in.
	if r.Sources, err = collect(`SELECT source AS key, person_id, '-infinity'::timestamptz AS t0 FROM product_people WHERE product_id = $1 AND created_at >= $3`); err != nil {
		return err
	}
	// Influence: every play that enrolled them, counting what happened after.
	r.Plays, err = collect(`SELECT DISTINCT ON (person_id, play_id) play_id AS key, person_id, started_at AS t0
		FROM enrollments WHERE product_id = $1 AND play_id <> '' AND started_at >= $3 ORDER BY person_id, play_id, started_at`)
	return err
}

func (r *Report) money(ctx context.Context, q store.Q, p *product.Product, since time.Time) error {
	rows, err := q.Query(ctx, `
		SELECT currency, COALESCE(sum(amount_cents) FILTER (WHERE kind = 'payment'), 0), COALESCE(-sum(amount_cents) FILTER (WHERE kind = 'refund'), 0),
			sum(amount_cents), count(*) FILTER (WHERE kind = 'payment')
		FROM payments WHERE product_id = $1 AND occurred_at >= $2 GROUP BY 1 ORDER BY 1`, p.ID, since)
	if err != nil {
		return err
	}
	r.Revenue, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Money, error) {
		var m Money
		return m, row.Scan(&m.Currency, &m.Payments, &m.Refunds, &m.Net, &m.Count)
	})
	return err
}

func (r *Report) spend(ctx context.Context, q store.Q, p *product.Product, since time.Time) error {
	rows, err := q.Query(ctx, `
		SELECT model, count(*), sum(input_tokens), sum(output_tokens) FROM llm_calls
		WHERE product_id = $1 AND created_at >= $2 GROUP BY 1`, p.ID, since)
	if err != nil {
		return err
	}
	prices := llm.Prices()
	usd, priced := 0.0, true
	for rows.Next() {
		var model string
		var n int
		var in, out int64
		if err := rows.Scan(&model, &n, &in, &out); err != nil {
			rows.Close()
			return err
		}
		r.Spend.LLMCalls += n
		r.Spend.InTokens += in
		r.Spend.OutTokens += out
		if pr, ok := prices[model]; ok {
			usd += float64(in)/1e6*pr[0] + float64(out)/1e6*pr[1]
		} else {
			priced = false
		}
	}
	rows.Close()
	if priced && r.Spend.LLMCalls > 0 {
		r.Spend.LLMUSD = &usd
	}
	rows, err = q.Query(ctx, `
		SELECT outcome, count(*), COALESCE(sum(minutes_spent), 0)::int FROM tasks
		WHERE product_id = $1 AND status = 'done' AND closed_at >= $2 GROUP BY 1 ORDER BY 2 DESC`, p.ID, since)
	if err != nil {
		return err
	}
	if r.Outcomes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Outcome, error) {
		var o Outcome
		return o, row.Scan(&o.Outcome, &o.N, &o.Minutes)
	}); err != nil {
		return err
	}
	for _, o := range r.Outcomes {
		r.Spend.Minutes += o.Minutes
	}
	for _, s := range r.Stages {
		if s.Activation {
			r.Spend.Activated = s.People
		}
		if s.Name == "paid" {
			r.Spend.Paying = s.People
		}
	}
	per := func(total float64, n int) *float64 {
		if n == 0 {
			return nil // zero conversions: show spend and zero, never a ratio
		}
		v := total / float64(n)
		return &v
	}
	if r.Spend.LLMUSD != nil {
		r.Spend.USDPerActive = per(*r.Spend.LLMUSD, r.Spend.Activated)
		r.Spend.USDPerPaying = per(*r.Spend.LLMUSD, r.Spend.Paying)
	}
	if r.Spend.Minutes > 0 {
		r.Spend.MinPerActive = per(float64(r.Spend.Minutes), r.Spend.Activated)
	}
	return nil
}

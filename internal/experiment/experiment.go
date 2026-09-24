// Package experiment assigns variants (random, fixed once made) and reports
// how many people in each variant reached the next stage, with a simple
// confidence interval and a plain verdict.
package experiment

import (
	"context"
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// Key names an experiment: one sequence step.
func Key(productID, sequenceID string, step int) string {
	return fmt.Sprintf("%s/%s/%d", productID, sequenceID, step)
}

// Assign returns the unit's variant, choosing one at random the first time.
func Assign(ctx context.Context, q store.Q, key, unit string, variants []string) (string, error) {
	if len(variants) == 0 {
		return "", nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(variants))))
	if err != nil {
		return "", err
	}
	if _, err := q.Exec(ctx, `INSERT INTO assignments (experiment, unit, variant) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		key, unit, variants[n.Int64()]); err != nil {
		return "", err
	}
	var v string
	err = q.QueryRow(ctx, `SELECT variant FROM assignments WHERE experiment = $1 AND unit = $2`, key, unit).Scan(&v)
	return v, err
}

type VariantStat struct {
	ID      string  `json:"variant"`
	Sent    int     `json:"sent"`
	Replied int     `json:"replied"`
	Success int     `json:"reached_goal"`
	Rate    float64 `json:"goal_rate"`
	Low     float64 `json:"ci_low"`
	High    float64 `json:"ci_high"`
	RepRate float64 `json:"reply_rate"`
}

type Result struct {
	Key      string        `json:"experiment"`
	Goal     string        `json:"goal_stage"`
	Variants []VariantStat `json:"variants"`
	Verdict  string        `json:"verdict"`
}

// minPerVariant: below this many sends per variant, call nothing.
const minPerVariant = 30

// Report compares every experiment in a product: people who were sent the
// step in each variant, how many replied, and how many then reached the
// goal stage (the offer's success event).
func Report(ctx context.Context, q store.Q, p *product.Product) ([]Result, error) {
	rows, err := q.Query(ctx, `
		SELECT e.sequence_id, a.step, a.variant, count(DISTINCT a.person_id),
			count(DISTINCT a.person_id) FILTER (WHERE EXISTS (SELECT 1 FROM funnel_events f WHERE f.product_id = $1 AND f.person_id = a.person_id AND f.stage = 'replied' AND f.occurred_at >= a.sent_at)),
			count(DISTINCT a.person_id) FILTER (WHERE EXISTS (SELECT 1 FROM funnel_events f WHERE f.product_id = $1 AND f.person_id = a.person_id AND f.stage = $2 AND f.occurred_at >= a.sent_at))
		FROM actions a JOIN enrollments e ON e.id = a.enrollment_id
		WHERE a.product_id = $1 AND a.variant <> '' AND a.status = 'sent'
		GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`, p.ID, p.Offer.SuccessEvent)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*Result{}
	var keys []string
	err = forEach(rows, func(r pgx.Rows) error {
		var seq, variant string
		var step int
		var v VariantStat
		if err := r.Scan(&seq, &step, &variant, &v.Sent, &v.Replied, &v.Success); err != nil {
			return err
		}
		v.ID = variant
		k := Key(p.ID, seq, step)
		if byKey[k] == nil {
			byKey[k] = &Result{Key: k, Goal: p.Offer.SuccessEvent}
			keys = append(keys, k)
		}
		v.Rate, v.Low, v.High = wilson(v.Success, v.Sent)
		if v.Sent > 0 {
			v.RepRate = float64(v.Replied) / float64(v.Sent)
		}
		byKey[k].Variants = append(byKey[k].Variants, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	var out []Result
	for _, k := range keys {
		r := byKey[k]
		r.Verdict = verdict(r.Variants)
		out = append(out, *r)
	}
	return out, nil
}

// wilson returns the rate and its 95% Wilson score interval.
func wilson(k, n int) (rate, low, high float64) {
	if n == 0 {
		return 0, 0, 0
	}
	const z = 1.96
	p := float64(k) / float64(n)
	nf := float64(n)
	den := 1 + z*z/nf
	centre := (p + z*z/(2*nf)) / den
	margin := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / den
	return p, math.Max(0, centre-margin), math.Min(1, centre+margin)
}

func verdict(vs []VariantStat) string {
	if len(vs) < 2 {
		return "only one variant has been sent so far"
	}
	for _, v := range vs {
		if v.Sent < minPerVariant {
			return fmt.Sprintf("too little data: every variant needs %d sends (have %s)", minPerVariant, sentList(vs))
		}
	}
	best := vs[0]
	for _, v := range vs[1:] {
		if v.Rate > best.Rate {
			best = v
		}
	}
	for _, v := range vs {
		if v.ID != best.ID && v.High >= best.Low {
			return fmt.Sprintf("no clear winner yet: %s leads at %.0f%% but the intervals overlap", best.ID, best.Rate*100)
		}
	}
	return fmt.Sprintf("%s wins: %.0f%% reach the goal (95%% CI %.0f–%.0f%%)", best.ID, best.Rate*100, best.Low*100, best.High*100)
}

func sentList(vs []VariantStat) string {
	var parts []string
	for _, v := range vs {
		parts = append(parts, fmt.Sprintf("%s %d", v.ID, v.Sent))
	}
	return strings.Join(parts, ", ")
}

func forEach(rows pgx.Rows, f func(pgx.Rows) error) error {
	defer rows.Close()
	for rows.Next() {
		if err := f(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

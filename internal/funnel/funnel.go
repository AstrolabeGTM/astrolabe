// Package funnel records stage events and counts people per stage.
package funnel

import (
	"context"
	"slices"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// Record adds a stage event once per dedupe key. It returns false if the
// event was already recorded.
func Record(ctx context.Context, q store.Q, productID string, personID int64, stage string, at time.Time, source, dedupeKey string) (bool, error) {
	tag, err := q.Exec(ctx, `
		INSERT INTO funnel_events (product_id, person_id, stage, occurred_at, source, dedupe_key)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (product_id, dedupe_key) DO NOTHING`,
		productID, personID, stage, at, source, dedupeKey)
	return tag.RowsAffected() == 1, err
}

type Stage struct {
	Name string `json:"stage"`
	// People who reached this stage or any later one.
	People int `json:"people"`
	// Conversion from the previous stage, 0–100.
	FromPrevious float64 `json:"from_previous_pct"`
	Activation   bool    `json:"activation,omitempty"`
}

// Counts returns the product's funnel since a time. A person counts for a
// stage if they reached it or any later stage, so the funnel never widens.
func Counts(ctx context.Context, q store.Q, p *product.Product, since time.Time) ([]Stage, error) {
	rows, err := q.Query(ctx, `
		SELECT person_id, stage FROM funnel_events
		WHERE product_id = $1 AND occurred_at >= $2`, p.ID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	furthest := map[int64]int{}
	for rows.Next() {
		var person int64
		var stage string
		if err := rows.Scan(&person, &stage); err != nil {
			return nil, err
		}
		i := slices.Index(p.Funnel, stage)
		if cur, ok := furthest[person]; i >= 0 && (!ok || i > cur) {
			furthest[person] = i
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Stage, len(p.Funnel))
	for i, name := range p.Funnel {
		out[i] = Stage{Name: name, Activation: name == p.Activation}
		for _, f := range furthest {
			if f >= i {
				out[i].People++
			}
		}
		if i > 0 && out[i-1].People > 0 {
			out[i].FromPrevious = 100 * float64(out[i].People) / float64(out[i-1].People)
		}
	}
	return out, nil
}

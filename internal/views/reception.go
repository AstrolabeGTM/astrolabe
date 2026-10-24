package views

import (
	"context"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/counters"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/reception"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// Counters reports public counters per product. With all false, products
// without a counter monitor are left out.
func Counters(ctx context.Context, q store.Q, products []*product.Product, all bool, now time.Time) ([]*counters.Report, error) {
	out := []*counters.Report{}
	for _, p := range products {
		r, err := counters.Weekly(ctx, q, p.ID, now)
		if err != nil {
			return nil, err
		}
		if len(r.Repos) > 0 || all {
			out = append(out, r)
		}
	}
	return out, nil
}

// Reception reports each product's public mentions in the last days days
// (7 if 0), with counts by tone; limit caps the listed mentions.
func Reception(ctx context.Context, q store.Q, products []*product.Product, days int, now time.Time, limit int) ([]*reception.Report, error) {
	if days <= 0 {
		days = 7
	}
	out := []*reception.Report{}
	for _, p := range products {
		r, err := reception.Days(ctx, q, p.ID, days, now, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

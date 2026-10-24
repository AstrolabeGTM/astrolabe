package funnel

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// BoardPerson is one person on the people board, in the furthest funnel
// stage they have reached.
type BoardPerson struct {
	ID      int64     `json:"id"`
	Name    string    `json:"name"`
	Company string    `json:"company,omitempty"`
	Since   time.Time `json:"since"`
	Days    int       `json:"days_in_stage"`
}

// BoardStage is one column: everyone whose furthest stage it is.
type BoardStage struct {
	Stage      string        `json:"stage"` // "" for people with no stage yet
	Activation bool          `json:"activation,omitempty"`
	People     int           `json:"people"`
	Shown      []BoardPerson `json:"shown"`
}

// PeopleBoard is a product's people by the furthest funnel stage they
// reached. Unlike Counts it puts each person in exactly one column.
type PeopleBoard struct {
	Product string       `json:"product"`
	Total   int          `json:"total"`
	Limit   int          `json:"shown_per_stage"`
	Stages  []BoardStage `json:"stages"`
}

// Board builds the people board, showing up to limit people per stage,
// longest in their stage first.
func Board(ctx context.Context, q store.Q, p *product.Product, now time.Time, limit int) (*PeopleBoard, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := q.Query(ctx, `
		WITH f AS (
			SELECT person_id, stage, occurred_at,
				row_number() OVER (PARTITION BY person_id ORDER BY array_position($2::text[], stage) DESC, occurred_at) AS rn
			FROM funnel_events WHERE product_id = $1 AND stage = ANY($2)
		)
		SELECT pp.person_id, COALESCE(NULLIF(p.name, ''), (SELECT value FROM identities WHERE person_id = p.id ORDER BY kind = 'email' DESC, value LIMIT 1), ''),
			COALESCE(NULLIF(c.name, ''), c.domain, ''), COALESCE(f.stage, ''), COALESCE(f.occurred_at, pp.created_at)
		FROM product_people pp JOIN people p ON p.id = pp.person_id LEFT JOIN companies c ON c.id = p.company_id
		LEFT JOIN f ON f.person_id = pp.person_id AND f.rn = 1
		WHERE pp.product_id = $1
		ORDER BY 5, 1`, p.ID, p.Funnel)
	if err != nil {
		return nil, err
	}
	type row struct {
		BoardPerson
		stage string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.ID, &x.Name, &x.Company, &x.stage, &x.Since)
	})
	if err != nil {
		return nil, err
	}
	b := &PeopleBoard{Product: p.ID, Total: len(list), Limit: limit}
	col := map[string]*BoardStage{}
	b.Stages = append(b.Stages, BoardStage{Stage: "", Shown: []BoardPerson{}})
	for _, s := range p.Funnel {
		b.Stages = append(b.Stages, BoardStage{Stage: s, Activation: s == p.Activation, Shown: []BoardPerson{}})
	}
	for i := range b.Stages {
		col[b.Stages[i].Stage] = &b.Stages[i]
	}
	for _, x := range list {
		c := col[x.stage]
		if c == nil {
			continue
		}
		c.People++
		if len(c.Shown) < limit {
			x.Days = max(0, int(now.Sub(x.Since).Hours()/24))
			c.Shown = append(c.Shown, x.BoardPerson)
		}
	}
	return b, nil
}

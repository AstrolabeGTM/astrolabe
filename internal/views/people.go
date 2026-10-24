package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/score"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// PersonRow is one person in a product's list, with their score, their
// sequences and whether the outreach guard lets the product email them.
type PersonRow struct {
	*people.Person
	Tier     string `json:"tier,omitempty"`
	Priority *int   `json:"priority,omitempty"`
	Fit      int    `json:"fit"`
	Intent   int    `json:"intent"`
	WhyNow   int    `json:"why_now"`
	Eligible bool   `json:"eligible"`
	Excluded string `json:"excluded,omitempty"`
	// Sequences sums up their sequences in this product, e.g.
	// "intro (step 2)" or "intro (stopped: replied)".
	Sequences string `json:"sequences,omitempty"`
	Active    bool   `json:"in_active_sequence"`
	// EmailRefused is the outreach guard's reason this product may not
	// email them, if it may not.
	EmailRefused string `json:"email_refused,omitempty"`
}

// PeopleList is a product's people, best priority first.
type PeopleList struct {
	Product string      `json:"product"`
	Search  string      `json:"search,omitempty"`
	Total   int         `json:"total"` // people matching, beyond the limit too
	People  []PersonRow `json:"people"`
}

// GetPeople lists a product's people (matching search), best score first.
func GetPeople(ctx context.Context, q store.Q, productID, search string, limit int) (*PeopleList, error) {
	if limit <= 0 {
		limit = 100
	}
	list, total, err := people.ListRanked(ctx, q, productID, strings.TrimSpace(search), limit)
	if err != nil {
		return nil, err
	}
	out := &PeopleList{Product: productID, Search: search, Total: total, People: make([]PersonRow, 0, len(list))}
	ids := make([]int64, 0, len(list))
	for _, p := range list {
		ids = append(ids, p.ID)
	}
	refused, err := outreach.EmailRefusals(ctx, q, productID, ids)
	if err != nil {
		return nil, err
	}
	type seq struct {
		summary string
		active  bool
	}
	seqs := map[int64]seq{}
	rows, err := q.Query(ctx, `
		SELECT person_id, string_agg(sequence_id || ' (' ||
			CASE WHEN status = 'active' THEN 'step ' || (next_step + 1)
			ELSE status || CASE WHEN stop_reason <> '' THEN ': ' || replace(stop_reason, '_', ' ') ELSE '' END END || ')', ', ' ORDER BY id DESC),
			bool_or(status = 'active')
		FROM enrollments WHERE product_id = $1 AND person_id = ANY($2) GROUP BY person_id`, productID, ids)
	if err != nil {
		return nil, err
	}
	err = forRows(rows, func(r pgx.Rows) error {
		var id int64
		var s seq
		if err := r.Scan(&id, &s.summary, &s.active); err != nil {
			return err
		}
		seqs[id] = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, p := range list {
		row := PersonRow{Person: p, Eligible: true, EmailRefused: refused[p.ID], Sequences: seqs[p.ID].summary, Active: seqs[p.ID].active}
		if sc, ok, err := score.Get(ctx, q, productID, p.ID); err != nil {
			return nil, err
		} else if ok {
			pr := sc.Priority
			row.Tier, row.Priority, row.Fit, row.Intent, row.WhyNow, row.Eligible, row.Excluded = sc.Tier, &pr, sc.Fit, sc.Intent, sc.WhyNow, sc.Eligible, sc.Breakdown.Excluded
		}
		out.People = append(out.People, row)
	}
	return out, nil
}

func forRows(rows pgx.Rows, f func(pgx.Rows) error) error {
	defer rows.Close()
	for rows.Next() {
		if err := f(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ImportPeople reads CSV text (email, github, name, first_name, title,
// company, company_domain, notes, facts) into a product and rescores it.
func ImportPeople(ctx context.Context, pool *pgxpool.Pool, productID, csv string) (people.ImportResult, error) {
	rows, err := people.ReadCSV(strings.NewReader(csv))
	if err != nil {
		return people.ImportResult{}, fmt.Errorf("%w: %v", outreach.ErrInvalid, err)
	}
	res, err := people.Import(ctx, pool, productID, rows)
	if err != nil {
		return res, err
	}
	_, err = score.RecomputeAll(ctx, pool, productID, time.Now())
	if res.Skipped == nil {
		res.Skipped = []string{}
	}
	return res, err
}

// CompanyFit is a company's own facts scored against a product's fit rules.
type CompanyFit struct {
	people.Company
	Fit       int      `json:"fit"`
	Breakdown []string `json:"breakdown"`
	Eligible  bool     `json:"eligible"`
	Excluded  string   `json:"excluded,omitempty"`
}

// FitOf scores a company for a product.
func FitOf(p *product.Product, c people.Company) CompanyFit {
	f := score.CompanyFit(p, c.Facts)
	return CompanyFit{Company: c, Fit: f.Fit, Breakdown: f.Breakdown, Eligible: f.Eligible, Excluded: f.Excluded}
}

// ImportedCompany is one imported row with its fit.
type ImportedCompany struct {
	CompanyFit
	Created  bool `json:"created"`
	NewFacts int  `json:"new_facts"`
}

// CompanyImport is the result of ImportCompanies.
type CompanyImport struct {
	Created   int               `json:"created"`
	Updated   int               `json:"updated"`
	Skipped   []string          `json:"skipped"`
	Companies []ImportedCompany `json:"companies"`
}

// ImportCompanies reads CSV text (domain, company, facts, notes) without
// people, rescores everyone (company facts count toward their people) and
// returns each company's fit for the product.
func ImportCompanies(ctx context.Context, pool *pgxpool.Pool, p *product.Product, csv string) (*CompanyImport, error) {
	rows, err := people.ReadCompaniesCSV(strings.NewReader(csv))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", outreach.ErrInvalid, err)
	}
	res, err := people.ImportCompanies(ctx, pool, rows)
	if err != nil {
		return nil, err
	}
	if _, err := score.RecomputeAll(ctx, pool, "", time.Now()); err != nil {
		return nil, err
	}
	out := &CompanyImport{Created: res.Created, Updated: res.Updated, Skipped: res.Skipped, Companies: make([]ImportedCompany, 0, len(res.Companies))}
	if out.Skipped == nil {
		out.Skipped = []string{}
	}
	for _, c := range res.Companies {
		out.Companies = append(out.Companies, ImportedCompany{FitOf(p, c.Company), c.Created, c.NewFacts})
	}
	return out, nil
}

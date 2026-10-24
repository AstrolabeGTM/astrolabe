package people

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// CompanyRow is one company to import, without any people.
type CompanyRow struct {
	Name, Domain, Notes string
	// Facts separated by ";" in CSV: "fact:agent_moves_money;team:5-50".
	Facts string
}

// Company is a company with its own facts.
type Company struct {
	ID     int64             `json:"id"`
	Name   string            `json:"name"`
	Domain string            `json:"domain"`
	Notes  string            `json:"notes,omitempty"`
	Facts  map[string]string `json:"facts"`
}

// CompanyResult is what one imported row did.
type CompanyResult struct {
	Company
	Created bool `json:"created"`
	// NewFacts counts facts added by this row; a re-import adds none.
	NewFacts int `json:"new_facts"`
}

type CompanyImport struct {
	Companies []CompanyResult `json:"companies"`
	Created   int             `json:"created"`
	Updated   int             `json:"updated"`
	Skipped   []string        `json:"skipped,omitempty"`
}

// NormalizeDomain lower-cases a domain or URL down to the bare host.
func NormalizeDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	s = strings.TrimPrefix(s, "www.")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSuffix(s, ".")
}

// ReadCompaniesCSV parses a company list with a header row. Columns: domain
// (or company_domain, website), company (or name), facts, notes. Others are
// ignored.
func ReadCompaniesCSV(r io.Reader) ([]CompanyRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF")))] = i
	}
	get := func(rec []string, names ...string) string {
		for _, n := range names {
			if i, ok := col[n]; ok && i < len(rec) && strings.TrimSpace(rec[i]) != "" {
				return strings.TrimSpace(rec[i])
			}
		}
		return ""
	}
	has := false
	for _, n := range []string{"domain", "company_domain", "website"} {
		_, ok := col[n]
		has = has || ok
	}
	if !has {
		return nil, errors.New("CSV needs a domain column")
	}
	var rows []CompanyRow
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		rows = append(rows, CompanyRow{
			Name: get(rec, "company", "name"), Domain: get(rec, "domain", "company_domain", "website"),
			Facts: get(rec, "facts"), Notes: get(rec, "notes"),
		})
	}
}

// ImportCompanies upserts companies by domain. Re-importing is idempotent:
// blank name and notes are filled, never overwritten, and facts are merged
// with source "import" without replacing a fact that is already there.
func ImportCompanies(ctx context.Context, pool *pgxpool.Pool, rows []CompanyRow) (CompanyImport, error) {
	res := CompanyImport{Companies: []CompanyResult{}}
	for i, r := range rows {
		var out CompanyResult
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			domain := NormalizeDomain(r.Domain)
			if domain == "" || !strings.Contains(domain, ".") {
				return &skipError{fmt.Sprintf("invalid domain %q", r.Domain)}
			}
			var facts []string
			if r.Facts != "" {
				facts = strings.Split(r.Facts, ";")
			}
			m, err := parseFacts(facts, "import")
			if err != nil {
				return &skipError{err.Error()}
			}
			if err := tx.QueryRow(ctx, `
				INSERT INTO companies (domain, name, notes) VALUES ($1, $2, $3)
				ON CONFLICT (domain) DO UPDATE SET
					name = CASE WHEN companies.name = '' THEN $2 ELSE companies.name END,
					notes = CASE WHEN companies.notes = '' THEN $3 ELSE companies.notes END
				RETURNING id, name, notes, (xmax = 0)`, domain, strings.TrimSpace(r.Name), r.Notes).
				Scan(&out.ID, &out.Name, &out.Notes, &out.Created); err != nil {
				return err
			}
			before, err := CompanyFacts(ctx, tx, out.ID)
			if err != nil {
				return err
			}
			if err := addCompanyFacts(ctx, tx, out.ID, m); err != nil {
				return err
			}
			out.Domain = domain
			out.Facts, err = CompanyFacts(ctx, tx, out.ID)
			out.NewFacts = len(out.Facts) - len(before)
			return err
		})
		var skip *skipError
		if errors.As(err, &skip) {
			res.Skipped = append(res.Skipped, fmt.Sprintf("row %d: %s", i+2, skip.msg))
			continue
		}
		if err != nil {
			return res, fmt.Errorf("row %d: %w", i+2, err)
		}
		if out.Created {
			res.Created++
		} else {
			res.Updated++
		}
		res.Companies = append(res.Companies, out)
	}
	return res, nil
}

func parseFacts(facts []string, source string) (map[string]string, error) {
	m := map[string]string{}
	for _, f := range facts {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			continue
		}
		if !strings.Contains(f, ":") {
			return nil, fmt.Errorf("fact %q must look like kind:value, e.g. fact:sells_to_enterprise", f)
		}
		m[f] = source
	}
	return m, nil
}

// addCompanyFacts merges facts into a company; facts already there keep their source.
func addCompanyFacts(ctx context.Context, q store.Q, companyID int64, m map[string]string) error {
	b, _ := json.Marshal(m)
	_, err := q.Exec(ctx, `UPDATE companies SET facts = $2::jsonb || facts WHERE id = $1`, companyID, b)
	return err
}

// SetCompanyFacts adds facts ("fact:agent_moves_money", "dep:bullmq") to a
// company by hand. They count toward the company's fit and toward the fit of
// everyone who works there.
func SetCompanyFacts(ctx context.Context, q store.Q, companyID int64, facts []string, source string) error {
	m, err := parseFacts(facts, source)
	if err != nil {
		return err
	}
	return addCompanyFacts(ctx, q, companyID, m)
}

// CompanyFacts returns a company's own facts.
func CompanyFacts(ctx context.Context, q store.Q, companyID int64) (map[string]string, error) {
	var b []byte
	if err := q.QueryRow(ctx, `SELECT facts FROM companies WHERE id = $1`, companyID).Scan(&b); err != nil {
		return nil, err
	}
	out := map[string]string{}
	return out, json.Unmarshal(b, &out)
}

// FindCompany looks a company up by id or domain.
func FindCompany(ctx context.Context, q store.Q, idOrDomain string) (*Company, error) {
	c := &Company{}
	var b []byte
	var err error
	const sel = `SELECT id, name, COALESCE(domain, ''), notes, facts FROM companies WHERE `
	if id, perr := strconv.ParseInt(strings.TrimSpace(idOrDomain), 10, 64); perr == nil {
		err = q.QueryRow(ctx, sel+`id = $1`, id).Scan(&c.ID, &c.Name, &c.Domain, &c.Notes, &b)
	} else {
		err = q.QueryRow(ctx, sel+`domain = $1`, NormalizeDomain(idOrDomain)).Scan(&c.ID, &c.Name, &c.Domain, &c.Notes, &b)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("no company with id or domain %q", idOrDomain)
	}
	if err != nil {
		return nil, err
	}
	c.Facts = map[string]string{}
	return c, json.Unmarshal(b, &c.Facts)
}

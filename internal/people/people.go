// Package people holds people, companies, identities and suppression.
package people

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

type Person struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	FirstName string `json:"first_name"`
	Title     string `json:"title,omitempty"`
	Company   string `json:"company,omitempty"`
	Domain    string `json:"company_domain,omitempty"`
	Email     string `json:"email,omitempty"`
	GitHub    string `json:"github,omitempty"`
	Notes     string `json:"notes,omitempty"`
	// Suppressed is the reason this person must not be contacted, if any.
	Suppressed string `json:"do_not_contact,omitempty"`
}

// Row is one person to import. Email or GitHub is required.
type Row struct {
	Email, GitHub, Name, FirstName, Title, Company, CompanyDomain, Notes string
	// Facts for fit scoring, separated by ";" in CSV: "dep:bullmq;team:5-50".
	Facts string
}

type ImportResult struct {
	Created, Updated int
	Skipped          []string
}

var freeMail = []string{"gmail.com", "googlemail.com", "outlook.com", "hotmail.com", "yahoo.com", "icloud.com", "proton.me", "protonmail.com", "live.com", "aol.com"}

// IsFreeMail reports consumer email domains that say nothing about a company.
func IsFreeMail(domain string) bool { return slices.Contains(freeMail, strings.ToLower(domain)) }

// ReadCSV parses a CSV with a header row. Recognised columns: email, github,
// name, first_name, title, company, company_domain, notes, facts. Others are ignored.
func ReadCSV(r io.Reader) ([]Row, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	if _, ok := col["email"]; !ok {
		if _, ok := col["github"]; !ok {
			return nil, errors.New("CSV needs an email or github column")
		}
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var rows []Row
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		rows = append(rows, Row{
			Email: get(rec, "email"), GitHub: get(rec, "github"), Name: get(rec, "name"),
			FirstName: get(rec, "first_name"), Title: get(rec, "title"), Company: get(rec, "company"),
			CompanyDomain: get(rec, "company_domain"), Notes: get(rec, "notes"), Facts: get(rec, "facts"),
		})
	}
}

// NormalizeEmail returns the lower-cased bare address, or "" if invalid.
func NormalizeEmail(s string) string {
	a, err := mail.ParseAddress(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	return strings.ToLower(a.Address)
}

// NormalizeGitHub accepts a login, @login or profile URL.
func NormalizeGitHub(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "github.com/")
	s = strings.TrimPrefix(s, "@")
	return strings.Trim(s, "/")
}

// Import upserts rows for a product. A row whose email and GitHub belong to two
// different existing people is skipped rather than guessed.
func Import(ctx context.Context, pool *pgxpool.Pool, productID string, rows []Row) (ImportResult, error) {
	var res ImportResult
	for i, r := range rows {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			created, err := upsert(ctx, tx, productID, r)
			if err != nil {
				return err
			}
			if created {
				res.Created++
			} else {
				res.Updated++
			}
			return nil
		})
		var skip *skipError
		if errors.As(err, &skip) {
			res.Skipped = append(res.Skipped, fmt.Sprintf("row %d: %s", i+2, skip.msg))
			continue
		}
		if err != nil {
			return res, fmt.Errorf("row %d: %w", i+2, err)
		}
	}
	return res, nil
}

type skipError struct{ msg string }

func (e *skipError) Error() string { return e.msg }

func upsert(ctx context.Context, tx pgx.Tx, productID string, r Row) (bool, error) {
	email := NormalizeEmail(r.Email)
	if r.Email != "" && email == "" {
		return false, &skipError{fmt.Sprintf("invalid email %q", r.Email)}
	}
	gh := NormalizeGitHub(r.GitHub)
	if email == "" && gh == "" {
		return false, &skipError{"no email or github"}
	}

	var ids []int64
	for kind, v := range map[string]string{"email": email, "github": gh} {
		if v == "" {
			continue
		}
		var id int64
		err := tx.QueryRow(ctx, `SELECT person_id FROM identities WHERE kind = $1 AND value = $2`, kind, v).Scan(&id)
		if err == nil && !slices.Contains(ids, id) {
			ids = append(ids, id)
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	}
	if len(ids) > 1 {
		return false, &skipError{fmt.Sprintf("%s and github %s belong to different people (%d, %d); merge by hand", email, gh, ids[0], ids[1])}
	}

	domain := strings.ToLower(strings.TrimSpace(r.CompanyDomain))
	if domain == "" && email != "" {
		if d := email[strings.LastIndex(email, "@")+1:]; !slices.Contains(freeMail, d) {
			domain = d
		}
	}
	var companyID *int64
	if domain != "" {
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO companies (domain, name) VALUES ($1, $2)
			ON CONFLICT (domain) DO UPDATE SET name = CASE WHEN companies.name = '' THEN $2 ELSE companies.name END
			RETURNING id`, domain, r.Company).Scan(&id); err != nil {
			return false, err
		}
		companyID = &id
	}

	first := r.FirstName
	if first == "" && r.Name != "" {
		first = strings.Fields(r.Name)[0]
	}
	created := len(ids) == 0
	var personID int64
	if created {
		if err := tx.QueryRow(ctx, `
			INSERT INTO people (name, first_name, title, company_id, notes) VALUES ($1, $2, $3, $4, $5)
			RETURNING id`, r.Name, first, r.Title, companyID, r.Notes).Scan(&personID); err != nil {
			return false, err
		}
	} else {
		personID = ids[0]
		// Fill blanks; never overwrite what I already know.
		if _, err := tx.Exec(ctx, `
			UPDATE people SET
				name = CASE WHEN name = '' THEN $2 ELSE name END,
				first_name = CASE WHEN first_name = '' THEN $3 ELSE first_name END,
				title = CASE WHEN title = '' THEN $4 ELSE title END,
				company_id = COALESCE(company_id, $5),
				notes = CASE WHEN notes = '' THEN $6 ELSE notes END
			WHERE id = $1`, personID, r.Name, first, r.Title, companyID, r.Notes); err != nil {
			return false, err
		}
	}
	for kind, v := range map[string]string{"email": email, "github": gh} {
		if v == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identities (kind, value, person_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, kind, v, personID); err != nil {
			return false, err
		}
	}
	if r.Facts != "" {
		if err := SetFacts(ctx, tx, personID, strings.Split(r.Facts, ";"), "import"); err != nil {
			return false, &skipError{err.Error()}
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO product_people (product_id, person_id, source) VALUES ($1, $2, 'import') ON CONFLICT DO NOTHING`, productID, personID)
	return created, err
}

const personSelect = `
	SELECT p.id, p.name, p.first_name, p.title, COALESCE(c.name, ''), COALESCE(c.domain, ''),
		COALESCE((SELECT value FROM identities WHERE person_id = p.id AND kind = 'email' ORDER BY value LIMIT 1), ''),
		COALESCE((SELECT value FROM identities WHERE person_id = p.id AND kind = 'github' ORDER BY value LIMIT 1), ''),
		p.notes, COALESCE(s.reason, '')
	FROM people p
	LEFT JOIN companies c ON c.id = p.company_id
	LEFT JOIN suppressions s ON s.person_id = p.id`

func scan(row pgx.Row) (*Person, error) {
	p := &Person{}
	err := row.Scan(&p.ID, &p.Name, &p.FirstName, &p.Title, &p.Company, &p.Domain, &p.Email, &p.GitHub, &p.Notes, &p.Suppressed)
	return p, err
}

func Get(ctx context.Context, q store.Q, id int64) (*Person, error) {
	return scan(q.QueryRow(ctx, personSelect+` WHERE p.id = $1`, id))
}

// FindByEmail returns the person owning an email identity.
func FindByEmail(ctx context.Context, q store.Q, email string) (*Person, error) {
	return scan(q.QueryRow(ctx, personSelect+` WHERE p.id = (SELECT person_id FROM identities WHERE kind = 'email' AND value = $1)`, NormalizeEmail(email)))
}

// List returns a product's people, optionally filtered by a search string.
func List(ctx context.Context, q store.Q, productID, search string, limit int) ([]*Person, error) {
	rows, err := q.Query(ctx, personSelect+`
		JOIN product_people pp ON pp.person_id = p.id AND pp.product_id = $1
		WHERE $2 = '' OR p.name ILIKE '%'||$2||'%' OR c.domain ILIKE '%'||$2||'%' OR c.name ILIKE '%'||$2||'%'
			OR EXISTS (SELECT 1 FROM identities i WHERE i.person_id = p.id AND i.value ILIKE '%'||$2||'%')
		ORDER BY p.id LIMIT $3`, productID, search, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Person
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Suppress marks a person do-not-contact across all products. The first
// reason recorded wins.
func Suppress(ctx context.Context, q store.Q, personID int64, reason, detail string) error {
	_, err := q.Exec(ctx, `INSERT INTO suppressions (person_id, reason, detail) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, personID, reason, detail)
	return err
}

func Unsuppress(ctx context.Context, q store.Q, personID int64) error {
	_, err := q.Exec(ctx, `DELETE FROM suppressions WHERE person_id = $1`, personID)
	return err
}

// SetFacts adds facts ("dep:bullmq", "team:5-50") to a person by hand; they
// count toward fit scoring like enrichment facts.
func SetFacts(ctx context.Context, q store.Q, personID int64, facts []string, source string) error {
	m := map[string]string{}
	for _, f := range facts {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			continue
		}
		if !strings.Contains(f, ":") {
			return fmt.Errorf("fact %q must look like kind:value, e.g. dep:bullmq", f)
		}
		m[f] = source
	}
	b, _ := json.Marshal(m)
	_, err := q.Exec(ctx, `UPDATE people SET facts = facts || $2 WHERE id = $1`, personID, b)
	return err
}

// Facts returns a person's own and company facts.
func Facts(ctx context.Context, q store.Q, personID int64) (map[string]string, error) {
	var pf, cf []byte
	if err := q.QueryRow(ctx, `SELECT p.facts, COALESCE(c.facts, '{}') FROM people p LEFT JOIN companies c ON c.id = p.company_id WHERE p.id = $1`,
		personID).Scan(&pf, &cf); err != nil {
		return nil, err
	}
	out := map[string]string{}
	json.Unmarshal(cf, &out)
	json.Unmarshal(pf, &out)
	return out, nil
}

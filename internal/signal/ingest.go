// Package signal stores signals from monitors, webhooks and manual entry,
// attaching each to a person and company by exact identity matches only.
package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/astrolabe-gtm/astrolabe/internal/people"
	"github.com/astrolabe-gtm/astrolabe/internal/store"
)

// Subject says who a signal is about. Any field may be empty.
type Subject struct {
	GitHub        string `json:"github,omitempty"`
	Email         string `json:"email,omitempty"`
	HN            string `json:"hn,omitempty"`
	StackOverflow string `json:"stackoverflow,omitempty"` // user id
	User          string `json:"user,omitempty"`          // product user id
	Phone         string `json:"phone,omitempty"`
	Stripe        string `json:"stripe,omitempty"` // Stripe customer id
	Name          string `json:"name,omitempty"`
	CompanyDomain string `json:"company_domain,omitempty"`
	GitHubOrg     string `json:"github_org,omitempty"`
	CompanyName   string `json:"company_name,omitempty"`
}

type Signal struct {
	Product     string         `json:"product"`
	Monitor     string         `json:"monitor,omitempty"`
	Type        string         `json:"type"`
	OccurredAt  time.Time      `json:"occurred_at"`
	Subject     Subject        `json:"subject"`
	Strength    int            `json:"strength"`
	WhyNow      bool           `json:"why_now,omitempty"`
	Title       string         `json:"title,omitempty"`
	EvidenceURL string         `json:"evidence_url,omitempty"`
	Data        map[string]any `json:"data,omitempty"`
	DedupeKey   string         `json:"dedupe_key"`
	// Repo ("owner/name") the signal came from; enrichment reads it.
	Repo string `json:"repo,omitempty"`
}

// Result of ingesting one signal.
type Result struct {
	SignalID  int64
	PersonID  *int64
	Duplicate bool
	Ambiguous bool
}

// Hooks run inside the ingest transaction for every new signal (PersonID
// may be nil), e.g. to enqueue scoring, enrichment and play evaluation.
type Hook func(ctx context.Context, tx pgx.Tx, s *Signal, r Result) error

type Service struct {
	Pool  *pgxpool.Pool
	River *river.Client[pgx.Tx]
	Hooks []Hook
	// AfterEnrich runs in the enrichment transaction, e.g. to re-evaluate
	// plays that skipped the person before their facts were known.
	AfterEnrich func(ctx context.Context, tx pgx.Tx, personID int64) error
}

func (s *Signal) identities() [][2]string {
	var ids [][2]string
	add := func(kind, v string) {
		if v = strings.TrimSpace(v); v != "" {
			ids = append(ids, [2]string{kind, v})
		}
	}
	add("github", people.NormalizeGitHub(s.Subject.GitHub))
	if s.Subject.Email != "" {
		add("email", people.NormalizeEmail(s.Subject.Email))
	}
	add("hn", s.Subject.HN)
	add("stackoverflow", s.Subject.StackOverflow)
	if s.Subject.User != "" {
		add("user", s.Product+":"+s.Subject.User)
	}
	add("phone", NormalizePhone(s.Subject.Phone))
	add("stripe", s.Subject.Stripe)
	return ids
}

// NormalizePhone returns E.164-style "+<digits>" (numbers must include the
// country code), or "" if there are no digits.
func NormalizePhone(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "+" + b.String()
}

func (s *Signal) validate() error {
	switch {
	case s.Product == "" || s.Type == "" || s.DedupeKey == "":
		return errors.New("signal needs product, type and dedupe_key")
	case s.Strength < 0 || s.Strength > 100:
		return errors.New("signal strength must be 0–100")
	case s.OccurredAt.IsZero():
		return errors.New("signal needs occurred_at")
	}
	return nil
}

// Ingest stores a signal once (by product + dedupe key) and matches it.
func (svc *Service) Ingest(ctx context.Context, s *Signal) (Result, error) {
	var res Result
	if err := s.validate(); err != nil {
		return res, err
	}
	err := pgx.BeginFunc(ctx, svc.Pool, func(tx pgx.Tx) error {
		var err error
		res, err = ingestTx(ctx, tx, s)
		if err != nil || res.Duplicate {
			return err
		}
		for _, h := range svc.Hooks {
			if err := h(ctx, tx, s, res); err != nil {
				return err
			}
		}
		return nil
	})
	return res, err
}

func ingestTx(ctx context.Context, tx pgx.Tx, s *Signal) (Result, error) {
	var res Result
	subject, _ := json.Marshal(s.Subject)
	data, _ := json.Marshal(s.Data)
	if s.Data == nil {
		data = []byte("{}")
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO signals (product_id, monitor_id, type, occurred_at, subject, strength, why_now, title, evidence_url, data, dedupe_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (product_id, dedupe_key) DO NOTHING RETURNING id`,
		s.Product, s.Monitor, s.Type, s.OccurredAt, subject, s.Strength, s.WhyNow, s.Title, s.EvidenceURL, data, s.DedupeKey).Scan(&res.SignalID)
	if errors.Is(err, pgx.ErrNoRows) {
		res.Duplicate = true
		return res, nil
	} else if err != nil {
		return res, err
	}

	sub := s.Subject
	if e := people.NormalizeEmail(sub.Email); sub.CompanyDomain == "" && e != "" {
		if d := e[strings.LastIndex(e, "@")+1:]; !people.IsFreeMail(d) {
			sub.CompanyDomain = d
		}
	}
	companyID, err := upsertCompany(ctx, tx, sub)
	if err != nil {
		return res, err
	}

	ids := s.identities()
	var found []int64
	for _, id := range ids {
		var pid int64
		err := tx.QueryRow(ctx, `SELECT person_id FROM identities WHERE kind = $1 AND value = $2`, id[0], id[1]).Scan(&pid)
		if err == nil && !slices.Contains(found, pid) {
			found = append(found, pid)
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return res, err
		}
	}
	var personID int64
	switch {
	case len(found) > 1:
		res.Ambiguous = true
		_, err := tx.Exec(ctx, `INSERT INTO identity_reviews (signal_id, candidates) VALUES ($1, $2)`, res.SignalID, found)
		if err != nil {
			return res, err
		}
		_, err = tx.Exec(ctx, `UPDATE signals SET company_id = $2 WHERE id = $1`, res.SignalID, companyID)
		return res, err
	case len(found) == 1:
		personID = found[0]
	case len(ids) > 0:
		first := ""
		if f := strings.Fields(s.Subject.Name); len(f) > 0 {
			first = f[0]
		}
		if err := tx.QueryRow(ctx, `INSERT INTO people (name, first_name, company_id) VALUES ($1, $2, $3) RETURNING id`,
			s.Subject.Name, first, companyID).Scan(&personID); err != nil {
			return res, err
		}
	default:
		// Nobody identifiable (e.g. an RSS item): keep it as an account or feed signal.
		_, err := tx.Exec(ctx, `UPDATE signals SET company_id = $2 WHERE id = $1`, res.SignalID, companyID)
		return res, err
	}
	res.PersonID = &personID
	if err := linkPerson(ctx, tx, personID, ids, companyID, s); err != nil {
		return res, err
	}
	_, err = tx.Exec(ctx, `UPDATE signals SET person_id = $2, company_id = COALESCE($3, (SELECT company_id FROM people WHERE id = $2)) WHERE id = $1`,
		res.SignalID, personID, companyID)
	return res, err
}

func linkPerson(ctx context.Context, tx pgx.Tx, personID int64, ids [][2]string, companyID *int64, s *Signal) error {
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `INSERT INTO identities (kind, value, person_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, id[0], id[1], personID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE people SET company_id = COALESCE(company_id, $2),
			name = CASE WHEN name = '' THEN $3 ELSE name END,
			first_name = CASE WHEN first_name = '' THEN split_part($3, ' ', 1) ELSE first_name END
		WHERE id = $1`, personID, companyID, s.Subject.Name); err != nil {
		return err
	}
	source := "signal:" + s.Type
	if s.Monitor != "" {
		source = "monitor:" + s.Monitor
	}
	_, err := tx.Exec(ctx, `INSERT INTO product_people (product_id, person_id, source) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		s.Product, personID, source)
	return err
}

// upsertCompany finds or creates the company by domain and/or GitHub org.
func upsertCompany(ctx context.Context, q store.Q, sub Subject) (*int64, error) {
	domain := strings.ToLower(strings.TrimSpace(sub.CompanyDomain))
	org := strings.ToLower(strings.TrimSpace(sub.GitHubOrg))
	if domain == "" && org == "" {
		return nil, nil
	}
	var id int64
	err := q.QueryRow(ctx, `SELECT id FROM companies WHERE (domain = $1 AND $1 <> '') OR (github_org = $2 AND $2 <> '') ORDER BY id LIMIT 1`, domain, org).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = q.QueryRow(ctx, `INSERT INTO companies (domain, github_org, name) VALUES (NULLIF($1, ''), NULLIF($2, ''), $3) RETURNING id`,
			domain, org, sub.CompanyName).Scan(&id)
	case err == nil:
		// Fill in whichever key is missing, unless another company already owns it.
		_, err = q.Exec(ctx, `
			UPDATE companies SET
				domain = CASE WHEN domain IS NULL AND $2 <> '' AND NOT EXISTS (SELECT 1 FROM companies WHERE domain = $2) THEN $2 ELSE domain END,
				github_org = CASE WHEN github_org IS NULL AND $3 <> '' AND NOT EXISTS (SELECT 1 FROM companies WHERE github_org = $3) THEN $3 ELSE github_org END,
				name = CASE WHEN name = '' THEN $4 ELSE name END
			WHERE id = $1`, id, domain, org, sub.CompanyName)
	}
	if err != nil {
		return nil, fmt.Errorf("company: %w", err)
	}
	return &id, nil
}

// Resolve attaches an ambiguous signal to the person I chose.
func (svc *Service) Resolve(ctx context.Context, reviewID, personID int64) error {
	return pgx.BeginFunc(ctx, svc.Pool, func(tx pgx.Tx) error {
		var signalID int64
		var candidates []int64
		if err := tx.QueryRow(ctx, `SELECT signal_id, candidates FROM identity_reviews WHERE id = $1 AND status = 'open'`, reviewID).
			Scan(&signalID, &candidates); err != nil {
			return fmt.Errorf("no open review %d", reviewID)
		}
		if !slices.Contains(candidates, personID) {
			return fmt.Errorf("person %d is not a candidate for review %d", personID, reviewID)
		}
		if _, err := tx.Exec(ctx, `UPDATE identity_reviews SET status = 'resolved', resolved_to = $2 WHERE id = $1`, reviewID, personID); err != nil {
			return err
		}
		var product string
		if err := tx.QueryRow(ctx, `UPDATE signals SET person_id = $2 WHERE id = $1 RETURNING product_id`, signalID, personID).Scan(&product); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO product_people (product_id, person_id, source) VALUES ($1, $2, 'review') ON CONFLICT DO NOTHING`, product, personID)
		return err
	})
}

// Dismiss closes a review without attaching the signal.
func (svc *Service) Dismiss(ctx context.Context, reviewID int64) error {
	_, err := svc.Pool.Exec(ctx, `UPDATE identity_reviews SET status = 'dismissed' WHERE id = $1 AND status = 'open'`, reviewID)
	return err
}

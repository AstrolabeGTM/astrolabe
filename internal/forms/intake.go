// Package forms takes in form submissions, from Netlify's API (polled) or a
// signed webhook (pushed), through one path: each submission is kept once,
// finds or creates its person (the email is an address they gave
// themselves), sets the company, optionally opens a pipeline account, and
// records a signal. A reply task comes from an automation on the signal type.
package forms

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/pipeline"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
)

const (
	// NextAction is what a new account's next action says.
	NextAction = "Reply to the request"
	// defaultRespond is how soon to reply when the monitor sets no respond.
	defaultRespond = 24 * time.Hour
)

// Submission is one form submission as received.
type Submission struct {
	Source     string // netlify or webhook
	ExternalID string // unique within the source; retries reuse it
	At         time.Time
	Fields     map[string]any
}

// Outcome says what taking in a submission did.
type Outcome struct {
	New       bool // false: already taken in earlier
	PersonID  *int64
	CompanyID *int64
	AccountID *int64
	SignalID  int64
}

// Intake takes submissions in. Netlify is only needed to poll.
type Intake struct {
	Pool    *pgxpool.Pool
	Signals *signal.Service
	Netlify *Netlify
	Now     func() time.Time
}

func (in *Intake) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

// Poll implements signal.FormPoller for netlify_forms monitors. A submission
// that fails is logged and skipped so one bad row never blocks the rest; the
// error is returned after the others are in.
func (in *Intake) Poll(ctx context.Context, p *product.Product, m *product.Monitor, since, now time.Time) (int, error) {
	if in.Netlify == nil {
		return 0, errors.New("netlify is not configured in this process")
	}
	subs, fetchErr := in.Netlify.Fetch(ctx, m, since)
	n := 0
	var errs []error
	for _, s := range subs {
		o, err := in.Submit(ctx, p, m, s)
		if err != nil {
			slog.Error("form submission", "product", p.ID, "monitor", m.ID, "id", s.ExternalID, "err", err)
			errs = append(errs, fmt.Errorf("submission %s: %w", s.ExternalID, err))
			continue
		}
		if o.New {
			n++
		}
	}
	return n, errors.Join(append([]error{fetchErr}, errs...)...)
}

// BodyID is the external id of a webhook submission: its "id" field if it
// sends one, else a hash of the body (so a retried post is not taken twice).
func BodyID(body []byte, fields map[string]any) string {
	if s := text(fields["id"]); s != "" {
		return s
	}
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:12])
}

// Submit takes one submission in. Every step is safe to repeat, so a
// submission that failed half way is finished by the next try, and a
// submission already taken in changes nothing.
func (in *Intake) Submit(ctx context.Context, p *product.Product, m *product.Monitor, s Submission) (Outcome, error) {
	var o Outcome
	if s.ExternalID == "" || s.Source == "" {
		return o, errors.New("a submission needs a source and an id")
	}
	var one int
	err := in.Pool.QueryRow(ctx, `SELECT 1 FROM form_submissions WHERE product_id = $1 AND monitor_id = $2 AND source = $3 AND external_id = $4`,
		p.ID, m.ID, s.Source, s.ExternalID).Scan(&one)
	if err == nil {
		return o, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return o, err
	}
	if s.At.IsZero() {
		s.At = in.now()
	}
	f := Map(s.Fields, m.Fields)
	email := people.NormalizeEmail(f.Email)

	domain := people.NormalizeDomain(f.Domain)
	if domain != "" && !strings.Contains(domain, ".") || people.IsFreeMail(domain) {
		domain = ""
	}
	data := map[string]any{"form": formName(m)}
	for k, v := range s.Fields {
		if k == "ip" || k == "user_agent" {
			continue // collected by the host, not typed by the person
		}
		data[k] = v
	}
	if f.Campaign != "" {
		data["campaign"] = f.Campaign
	}
	if f.Source != "" {
		data["reported_source"] = f.Source
	}
	sig := &signal.Signal{
		Product: p.ID, Monitor: m.ID, Type: m.Signal, Strength: m.Strength, WhyNow: m.WhyNow, OccurredAt: s.At,
		Subject: signal.Subject{Email: email, Name: f.Name, CompanyDomain: domain, CompanyName: f.Company},
		Title:   title(f, email, m), Data: data, IdentitySource: people.SourceForm,
		DedupeKey: fmt.Sprintf("form:%s:%s:%s", m.ID, s.Source, s.ExternalID),
	}
	_, err = in.Signals.Ingest(ctx, sig)
	if err != nil {
		return o, fmt.Errorf("signal: %w", err)
	}
	o.New = true
	if err := in.Pool.QueryRow(ctx, `SELECT id, person_id, company_id FROM signals WHERE product_id = $1 AND dedupe_key = $2`,
		p.ID, sig.DedupeKey).Scan(&o.SignalID, &o.PersonID, &o.CompanyID); err != nil {
		return o, err
	}

	if o.CompanyID == nil && f.Company != "" {
		if o.CompanyID, err = in.companyByName(ctx, f.Company, o.SignalID, o.PersonID); err != nil {
			return o, err
		}
	}
	if o.PersonID != nil {
		if _, err := in.Pool.Exec(ctx, `
			UPDATE people SET title = CASE WHEN title = '' THEN $2 ELSE title END WHERE id = $1`, *o.PersonID, f.Role); err != nil {
			return o, err
		}
		if _, err := in.Pool.Exec(ctx, `
			INSERT INTO product_people (product_id, person_id, source, reported_source) VALUES ($1, $2, $3, $4)
			ON CONFLICT (product_id, person_id) DO UPDATE SET
				reported_source = CASE WHEN $4 <> '' THEN $4 ELSE product_people.reported_source END`,
			p.ID, *o.PersonID, "monitor:"+m.ID, f.Source); err != nil {
			return o, err
		}
	}
	if m.Account && p.Pipeline != nil && o.CompanyID != nil {
		if o.AccountID, err = in.account(ctx, p, m, f, email, *o.CompanyID, o.PersonID); err != nil {
			return o, fmt.Errorf("account: %w", err)
		}
	}
	fields, _ := json.Marshal(s.Fields)
	tag, err := in.Pool.Exec(ctx, `
		INSERT INTO form_submissions (product_id, monitor_id, source, external_id, submitted_at, fields, reported_source, campaign,
			person_id, company_id, account_id, signal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) ON CONFLICT DO NOTHING`,
		p.ID, m.ID, s.Source, s.ExternalID, s.At, fields, f.Source, f.Campaign, o.PersonID, o.CompanyID, o.AccountID, o.SignalID)
	if err != nil {
		return o, err
	}
	o.New = tag.RowsAffected() == 1
	return o, nil
}

func formName(m *product.Monitor) string {
	if m.Form != "" {
		return m.Form
	}
	return m.ID
}

func title(f Mapped, email string, m *product.Monitor) string {
	who := f.Name
	if who == "" {
		who = email
	}
	if who == "" {
		who = "Someone"
	}
	if f.Company != "" {
		who += " (" + f.Company + ")"
	}
	return who + " submitted " + formName(m)
}

// companyByName finds a company by name, or creates it, for a submission that
// named a company but gave no usable domain; it then sets the signal's and
// the person's company.
func (in *Intake) companyByName(ctx context.Context, name string, signalID int64, personID *int64) (*int64, error) {
	var id int64
	err := in.Pool.QueryRow(ctx, `SELECT id FROM companies WHERE lower(name) = lower($1) ORDER BY id LIMIT 1`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = in.Pool.QueryRow(ctx, `INSERT INTO companies (name) VALUES ($1) RETURNING id`, name).Scan(&id)
	}
	if err != nil {
		return nil, err
	}
	if _, err := in.Pool.Exec(ctx, `UPDATE signals SET company_id = $2 WHERE id = $1`, signalID, id); err != nil {
		return nil, err
	}
	if personID != nil {
		if _, err := in.Pool.Exec(ctx, `UPDATE people SET company_id = COALESCE(company_id, $2) WHERE id = $1`, *personID, id); err != nil {
			return nil, err
		}
	}
	return &id, nil
}

// account opens an account in the first stage with the reply as its next
// action, or, if the company already has one, attaches the person and leaves
// the stage and next action alone.
func (in *Intake) account(ctx context.Context, p *product.Product, m *product.Monitor, f Mapped, email string, companyID int64, personID *int64) (*int64, error) {
	find := func() (int64, bool, error) {
		var id int64
		err := in.Pool.QueryRow(ctx, `SELECT id FROM accounts WHERE product_id = $1 AND company_id = $2`, p.ID, companyID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return id, err == nil, err
	}
	id, ok, err := find()
	if err != nil {
		return nil, err
	}
	if !ok {
		respond := defaultRespond
		if m.Respond != "" {
			if d, err := product.AfterDuration(m.Respond); err == nil {
				respond = d
			}
		}
		now := in.now()
		who := f.Name
		if who == "" {
			who = email
		}
		evidence := strings.TrimSpace(fmt.Sprintf("%s asked via the %s form", who, formName(m)))
		if f.Message != "" {
			evidence += ": " + excerpt(f.Message, 200)
		}
		id, err = pipeline.Add(ctx, in.Pool, p, pipeline.AddOpts{CompanyID: companyID, Evidence: evidence,
			NextAction: NextAction, Due: now.Add(respond)}, now)
		if errors.Is(err, pipeline.ErrInvalid) {
			// Another submission opened it first.
			if id2, found, ferr := find(); ferr == nil && found {
				id, err = id2, nil
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if personID != nil {
		role := "contact"
		if err := pipeline.SetPerson(ctx, in.Pool, p, id, *personID, role); err != nil {
			return nil, err
		}
	}
	return &id, nil
}

func excerpt(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// Package pipeline tracks companies (accounts) through a product's deal
// stages, beside the per-person funnel. A stage move needs a short evidence
// note; the qualify gate stage and every later one need each gating check
// answered yes; every open account has exactly one next action with a due
// date; pausing or losing an account needs a reason and a revisit date.
// Chat, jobs and the web app all go through these functions, so the rules
// hold everywhere.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// ErrInvalid is returned for requests that break a rule; the message says which.
var ErrInvalid = errors.New("invalid")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// DB is a pool or a transaction. Changes run in their own transaction (a
// savepoint inside a caller's).
type DB interface {
	store.Q
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Open is the status of an account in one of the stages; the others are
// product.ClosedStates (paused, lost).
const Open = "open"

// Answers a qualification check can have.
var Answers = []string{"yes", "no", "unknown"}

const (
	defaultStuck = 14 * 24 * time.Hour
	maxNote      = 500
	maxText      = 4000
)

// StuckAfter is how long an open account may sit in one stage before it
// shows as stuck (pipeline.stuck_after, default 14 days).
func StuckAfter(p *product.Product) time.Duration {
	if p.Pipeline != nil && p.Pipeline.StuckAfter != "" {
		if d, err := product.AfterDuration(p.Pipeline.StuckAfter); err == nil {
			return d
		}
	}
	return defaultStuck
}

// Account is one company in one product's pipeline.
type Account struct {
	ID        int64  `json:"id"`
	Product   string `json:"product"`
	CompanyID int64  `json:"company_id"`
	Company   string `json:"company"`
	Domain    string `json:"domain,omitempty"`
	// Status is open, paused or lost. Stage is the current stage, or for a
	// paused or lost account the stage it left.
	Status    string    `json:"status"`
	Stage     string    `json:"stage"`
	EnteredAt time.Time `json:"entered_at"`
	// DaysInStage counts calendar days in the current stage (or since the
	// account was paused or lost).
	DaysInStage int    `json:"days_in_stage"`
	Stuck       bool   `json:"stuck,omitempty"`
	NextAction  string `json:"next_action,omitempty"`
	NextDue     string `json:"next_due,omitempty"` // YYYY-MM-DD
	Overdue     bool   `json:"overdue,omitempty"`
	Reason      string `json:"reason,omitempty"`
	RevisitOn   string `json:"revisit_on,omitempty"` // YYYY-MM-DD
	// What the account wants to achieve, what is driving the timing, and
	// how they do it today (the baseline a result is judged against).
	Goal     string `json:"goal,omitempty"`
	Timing   string `json:"timing,omitempty"`
	Baseline string `json:"baseline,omitempty"`
}

const accountSelect = `
	SELECT a.id, a.product_id, a.company_id, COALESCE(NULLIF(c.name, ''), c.domain, c.github_org, '#' || c.id), COALESCE(c.domain, ''),
		a.status, a.stage, a.entered_at, a.next_action, COALESCE(to_char(a.next_due, 'YYYY-MM-DD'), ''), a.reason,
		COALESCE(to_char(a.revisit_on, 'YYYY-MM-DD'), ''), a.goal, a.timing, a.baseline
	FROM accounts a JOIN companies c ON c.id = a.company_id`

func scanAccount(row pgx.Row) (*Account, error) {
	a := &Account{}
	err := row.Scan(&a.ID, &a.Product, &a.CompanyID, &a.Company, &a.Domain, &a.Status, &a.Stage, &a.EnteredAt,
		&a.NextAction, &a.NextDue, &a.Reason, &a.RevisitOn, &a.Goal, &a.Timing, &a.Baseline)
	return a, err
}

// derive fills the fields that depend on the time and the product config.
func (a *Account) derive(p *product.Product, now time.Time) {
	a.DaysInStage = Days(a.EnteredAt, now)
	if a.Status != Open {
		return
	}
	// The last stage is where accounts are meant to stay.
	last := p.Pipeline != nil && len(p.Pipeline.Stages) > 0 && p.Pipeline.Stages[len(p.Pipeline.Stages)-1] == a.Stage
	a.Stuck = !last && now.Sub(a.EnteredAt) > StuckAfter(p)
	a.Overdue = a.NextDue != "" && a.NextDue < day(now)
}

func day(t time.Time) string { return t.Format("2006-01-02") }

// Days counts calendar days from one time to another, in the second's
// location (so a daylight-saving change never makes 18 days read as 17).
func Days(from, to time.Time) int {
	y1, m1, d1 := from.In(to.Location()).Date()
	y2, m2, d2 := to.Date()
	n := time.Date(y2, m2, d2, 0, 0, 0, 0, time.UTC).Sub(time.Date(y1, m1, d1, 0, 0, 0, 0, time.UTC))
	return max(0, int(n.Hours()/24))
}

func needPipeline(p *product.Product) error {
	if p.Pipeline == nil {
		return invalid("%s has no pipeline (add pipeline: to product.yaml)", p.ID)
	}
	return nil
}

func validStage(p *product.Product, stage string) error {
	if !p.HasAccountStage(stage) {
		return invalid("%q is not a pipeline stage of %s (have %v)", stage, p.ID, p.Pipeline.Stages)
	}
	return nil
}

// oneLine collapses whitespace and checks a short note's length.
func oneLine(field, s string, required bool) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" && required {
		return "", invalid("%s is required: one short line saying what happened", field)
	}
	if len(s) > maxNote {
		return "", invalid("%s must be one short line (under %d characters)", field, maxNote)
	}
	return s, nil
}

// Gated reports whether entering stage needs the qualification checks: the
// qualify gate stage and every stage after it do.
func Gated(p *product.Product, stage string) bool {
	if p.Pipeline == nil || p.Pipeline.Qualify == nil {
		return false
	}
	g := slices.Index(p.Pipeline.Stages, p.Pipeline.Qualify.Gate)
	return g >= 0 && slices.Index(p.Pipeline.Stages, stage) >= g
}

// Failing lists the gating checks not answered yes, as "check is answer".
// A check never answered is unknown. Record-only checks never appear.
func Failing(p *product.Product, answers map[string]string) []string {
	if p.Pipeline == nil || p.Pipeline.Qualify == nil {
		return nil
	}
	var out []string
	for _, c := range p.Pipeline.Qualify.Checks {
		a := answers[c]
		if a == "" {
			a = "unknown"
		}
		if a != "yes" {
			out = append(out, c+" is "+a)
		}
	}
	return out
}

func answers(ctx context.Context, q store.Q, accountID int64) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT name, answer FROM account_checks WHERE account_id = $1`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, a string
		if err := rows.Scan(&n, &a); err != nil {
			return nil, err
		}
		out[n] = a
	}
	return out, rows.Err()
}

// gate refuses entering a gated stage unless every gating check is yes.
func gate(ctx context.Context, q store.Q, p *product.Product, accountID int64, stage string) error {
	if !Gated(p, stage) {
		return nil
	}
	got := map[string]string{}
	if accountID != 0 {
		var err error
		if got, err = answers(ctx, q, accountID); err != nil {
			return err
		}
	}
	if bad := Failing(p, got); len(bad) > 0 {
		return invalid("cannot enter %s: every gating check (%s) must be yes first, but %s. Record the answers with one line of evidence each",
			stage, strings.Join(p.Pipeline.Qualify.Checks, ", "), strings.Join(bad, ", "))
	}
	return nil
}

// AddOpts puts a company into a product's pipeline.
type AddOpts struct {
	// The company: by id, by domain (found or created), or by name alone.
	CompanyID int64
	Domain    string
	Name      string
	// Stage defaults to the first stage. A gated stage is refused: a new
	// account has no qualification answers yet.
	Stage string
	// Evidence: why it belongs in the pipeline (a named person and a
	// specific need or signal). Required.
	Evidence string
	// The one next action and its due date. Required.
	NextAction string
	Due        time.Time
	// Optional free text.
	Goal, Timing, Baseline string
}

// Add creates an open account and returns its id.
func Add(ctx context.Context, db DB, p *product.Product, o AddOpts, now time.Time) (int64, error) {
	if err := needPipeline(p); err != nil {
		return 0, err
	}
	if o.Stage == "" {
		o.Stage = p.Pipeline.Stages[0]
	}
	if err := validStage(p, o.Stage); err != nil {
		return 0, err
	}
	evidence, err := oneLine("evidence", o.Evidence, true)
	if err != nil {
		return 0, err
	}
	next, err := nextAction(o.NextAction, o.Due)
	if err != nil {
		return 0, err
	}
	for _, f := range []string{o.Goal, o.Timing, o.Baseline} {
		if len(f) > maxText {
			return 0, invalid("goal, timing and baseline must each be under %d characters", maxText)
		}
	}
	if err := gate(ctx, db, p, 0, o.Stage); err != nil {
		return 0, err
	}
	var id int64
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		companyID, err := company(ctx, tx, o)
		if err != nil {
			return err
		}
		var existing int64
		err = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE product_id = $1 AND company_id = $2`, p.ID, companyID).Scan(&existing)
		if err == nil {
			return invalid("that company is already in the %s pipeline (account %d)", p.ID, existing)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO accounts (product_id, company_id, stage, entered_at, next_action, next_due, goal, timing, baseline, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6::date, $7, $8, $9, $4) RETURNING id`,
			p.ID, companyID, o.Stage, now, next, day(o.Due), strings.TrimSpace(o.Goal), strings.TrimSpace(o.Timing), strings.TrimSpace(o.Baseline)).Scan(&id); err != nil {
			return err
		}
		return history(ctx, tx, id, o.Stage, now, evidence, "", "")
	})
	return id, err
}

func nextAction(text string, due time.Time) (string, error) {
	next, err := oneLine("next action", text, false)
	if err != nil {
		return "", err
	}
	if next == "" {
		return "", invalid("an open account needs one next action with a due date")
	}
	if due.IsZero() {
		return "", invalid("the next action needs a due date")
	}
	return next, nil
}

var domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// NormalizeDomain turns "https://www.Acme.example/about" into "acme.example".
func NormalizeDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, p := range []string{"https://", "http://", "www."} {
		s = strings.TrimPrefix(s, p)
	}
	s, _, _ = strings.Cut(s, "/")
	return s
}

// company finds or creates the account's company.
func company(ctx context.Context, tx pgx.Tx, o AddOpts) (int64, error) {
	name := strings.TrimSpace(o.Name)
	var id int64
	switch {
	case o.CompanyID != 0:
		err := tx.QueryRow(ctx, `SELECT id FROM companies WHERE id = $1`, o.CompanyID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, invalid("no company %d", o.CompanyID)
		}
		return id, err
	case o.Domain != "":
		d := NormalizeDomain(o.Domain)
		if !domainRE.MatchString(d) {
			return 0, invalid("%q is not a company domain (like acme.example)", o.Domain)
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO companies (domain, name) VALUES ($1, $2)
			ON CONFLICT (domain) DO UPDATE SET name = CASE WHEN companies.name = '' THEN $2 ELSE companies.name END
			RETURNING id`, d, name).Scan(&id)
		return id, err
	case name != "":
		err := tx.QueryRow(ctx, `SELECT id FROM companies WHERE lower(name) = lower($1) ORDER BY id LIMIT 1`, name).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `INSERT INTO companies (name) VALUES ($1) RETURNING id`, name).Scan(&id)
		}
		return id, err
	}
	return 0, invalid("name the company by domain (acme.example) or name")
}

func history(ctx context.Context, q store.Q, accountID int64, stage string, at time.Time, evidence, reason, revisit string) error {
	_, err := q.Exec(ctx, `INSERT INTO account_stages (account_id, stage, entered_at, evidence, reason, revisit_on)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::date)`, accountID, stage, at, evidence, reason, revisit)
	return err
}

// lock reads an account of this product for update.
func lock(ctx context.Context, tx pgx.Tx, p *product.Product, id int64) (*Account, error) {
	a, err := scanAccount(tx.QueryRow(ctx, accountSelect+` WHERE a.id = $1 AND a.product_id = $2 FOR UPDATE OF a`, id, p.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, invalid("no account %d in %s", id, p.ID)
	}
	return a, err
}

// MoveOpts moves an account to a stage, or reopens a paused or lost one.
type MoveOpts struct {
	Stage string
	// Evidence: what happened that justifies the move. Required.
	Evidence string
	// NextAction replaces the current next action (with Due). Optional for
	// an open account; required when reopening a paused or lost one.
	NextAction string
	Due        time.Time
}

// Move puts an account in a stage. Entering the gate stage or any later
// one is refused unless every gating check is yes.
func Move(ctx context.Context, db DB, p *product.Product, id int64, o MoveOpts, now time.Time) error {
	if err := needPipeline(p); err != nil {
		return err
	}
	if err := validStage(p, o.Stage); err != nil {
		return err
	}
	evidence, err := oneLine("evidence", o.Evidence, true)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		a, err := lock(ctx, tx, p, id)
		if err != nil {
			return err
		}
		if a.Status == Open && a.Stage == o.Stage {
			return invalid("%s is already in %s", a.Company, o.Stage)
		}
		next, due := a.NextAction, a.NextDue
		if o.NextAction != "" || a.Status != Open {
			if next, err = nextAction(o.NextAction, o.Due); err != nil {
				if a.Status != Open {
					return invalid("reopening %s (%s) needs one next action with a due date", a.Company, a.Status)
				}
				return err
			}
			due = day(o.Due)
		}
		if err := gate(ctx, tx, p, id, o.Stage); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE accounts SET status = 'open', stage = $2, entered_at = $3, next_action = $4, next_due = $5::date,
				reason = '', revisit_on = NULL, updated_at = $3
			WHERE id = $1`, id, o.Stage, now, next, due); err != nil {
			return err
		}
		return history(ctx, tx, id, o.Stage, now, evidence, "", "")
	})
}

// CloseOpts pauses an account or marks it lost.
type CloseOpts struct {
	Status string // paused or lost
	// Reason is one of the product's PipelineReasons().
	Reason string
	// Revisit is when to look at the account again; a task appears that day.
	Revisit time.Time
	Note    string // optional, one line
}

// Close takes an account out of the open pipeline with a reason and a
// revisit date. Its next action is dropped: the revisit task replaces it.
func Close(ctx context.Context, db DB, p *product.Product, id int64, o CloseOpts, now time.Time) error {
	if err := needPipeline(p); err != nil {
		return err
	}
	if !slices.Contains(product.ClosedStates, o.Status) {
		return invalid("status must be one of %v", product.ClosedStates)
	}
	if !slices.Contains(p.PipelineReasons(), o.Reason) {
		return invalid("reason must be one of %v", p.PipelineReasons())
	}
	if o.Revisit.IsZero() {
		return invalid("a %s account needs a revisit date", o.Status)
	}
	if day(o.Revisit) < day(now) {
		return invalid("revisit date %s is in the past", day(o.Revisit))
	}
	note, err := oneLine("note", o.Note, false)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := lock(ctx, tx, p, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE accounts SET status = $2, entered_at = $3, next_action = '', next_due = NULL, reason = $4,
				revisit_on = $5::date, revisit_task_id = NULL, updated_at = $3
			WHERE id = $1`, id, o.Status, now, o.Reason, day(o.Revisit)); err != nil {
			return err
		}
		return history(ctx, tx, id, o.Status, now, note, o.Reason, day(o.Revisit))
	})
}

// Answer is one qualification check's answer.
type Answer struct {
	Check    string `json:"check"`
	Answer   string `json:"answer" jsonschema:"yes, no or unknown"`
	Evidence string `json:"evidence,omitempty" jsonschema:"one line: what they said or did that shows it; required for yes and no"`
}

// Qualify records check answers. Every configured check (gating or
// record-only) can be answered; yes and no need one line of evidence.
func Qualify(ctx context.Context, db DB, p *product.Product, id int64, list []Answer, now time.Time) error {
	if err := needPipeline(p); err != nil {
		return err
	}
	q := p.Pipeline.Qualify
	if q == nil {
		return invalid("%s has no qualify checks (pipeline.qualify in product.yaml)", p.ID)
	}
	if len(list) == 0 {
		return invalid("give at least one answer")
	}
	all := append(slices.Clone(q.Checks), q.Record...)
	for i, a := range list {
		if !slices.Contains(all, a.Check) {
			return invalid("unknown check %q (have %v)", a.Check, all)
		}
		if !slices.Contains(Answers, a.Answer) {
			return invalid("%s: answer must be yes, no or unknown", a.Check)
		}
		ev, err := oneLine(a.Check+" evidence", a.Evidence, a.Answer != "unknown")
		if err != nil {
			return err
		}
		list[i].Evidence = ev
	}
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := lock(ctx, tx, p, id); err != nil {
			return err
		}
		for _, a := range list {
			if _, err := tx.Exec(ctx, `
				INSERT INTO account_checks (account_id, name, answer, evidence, answered_at) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (account_id, name) DO UPDATE SET answer = $3, evidence = $4, answered_at = $5`,
				id, a.Check, a.Answer, a.Evidence, now); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE accounts SET updated_at = $2 WHERE id = $1`, id, now)
		return err
	})
}

// SetNextAction replaces an open account's one next action.
func SetNextAction(ctx context.Context, db DB, p *product.Product, id int64, text string, due time.Time) error {
	next, err := nextAction(text, due)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		a, err := lock(ctx, tx, p, id)
		if err != nil {
			return err
		}
		if a.Status != Open {
			return invalid("%s is %s; reopen it by moving it to a stage (with a next action)", a.Company, a.Status)
		}
		_, err = tx.Exec(ctx, `UPDATE accounts SET next_action = $2, next_due = $3::date, updated_at = now() WHERE id = $1`, id, next, day(due))
		return err
	})
}

// UpdateOpts sets the free-text fields; nil leaves a field as it is.
type UpdateOpts struct {
	Goal, Timing, Baseline *string
}

// Update sets what the account wants to achieve, what drives the timing
// and their current approach.
func Update(ctx context.Context, db DB, p *product.Product, id int64, o UpdateOpts) error {
	for _, f := range []*string{o.Goal, o.Timing, o.Baseline} {
		if f != nil && len(*f) > maxText {
			return invalid("goal, timing and baseline must each be under %d characters", maxText)
		}
	}
	trim := func(s *string) *string {
		if s == nil {
			return nil
		}
		t := strings.TrimSpace(*s)
		return &t
	}
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := lock(ctx, tx, p, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE accounts SET goal = COALESCE($2, goal), timing = COALESCE($3, timing), baseline = COALESCE($4, baseline), updated_at = now()
			WHERE id = $1`, id, trim(o.Goal), trim(o.Timing), trim(o.Baseline))
		return err
	})
}

var roleRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// SetPerson adds a person to the account with a role (champion, buyer,
// user, ...; default contact), or changes their role. The person also
// becomes one of the product's people.
func SetPerson(ctx context.Context, db DB, p *product.Product, id, personID int64, role string) error {
	role = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(role)), " ", "_")
	if role == "" {
		role = "contact"
	}
	if !roleRE.MatchString(role) {
		return invalid("role %q must be a lowercase word like champion, buyer or user", role)
	}
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := lock(ctx, tx, p, id); err != nil {
			return err
		}
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM people WHERE id = $1)`, personID).Scan(&ok); err != nil {
			return err
		} else if !ok {
			return invalid("no person %d", personID)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO account_people (account_id, person_id, role) VALUES ($1, $2, $3)
			ON CONFLICT (account_id, person_id) DO UPDATE SET role = $3`, id, personID, role); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO product_people (product_id, person_id, source) VALUES ($1, $2, 'account') ON CONFLICT DO NOTHING`, p.ID, personID)
		return err
	})
}

// RemovePerson takes a person off the account.
func RemovePerson(ctx context.Context, db DB, p *product.Product, id, personID int64) error {
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := lock(ctx, tx, p, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM account_people WHERE account_id = $1 AND person_id = $2`, id, personID)
		return err
	})
}

// AddLink keeps an evidence link (a call note, a document, an issue) on
// the account. Adding the same URL again updates its note.
func AddLink(ctx context.Context, db DB, p *product.Product, id int64, link, note string) error {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return invalid("link %q must be an http(s) URL", link)
	}
	if note, err = oneLine("link note", note, false); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := lock(ctx, tx, p, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO account_links (account_id, url, note) VALUES ($1, $2, $3)
			ON CONFLICT (account_id, url) DO UPDATE SET note = $3`, id, u.String(), note)
		return err
	})
}

// Find resolves an account by id, company domain or company name.
func Find(ctx context.Context, q store.Q, p *product.Product, ref string) (int64, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, invalid("name the account by id, company domain or company name")
	}
	if n, err := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64); err == nil {
		var id int64
		err := q.QueryRow(ctx, `SELECT id FROM accounts WHERE id = $1 AND product_id = $2`, n, p.ID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, invalid("no account %d in %s", n, p.ID)
		}
		return id, err
	}
	rows, err := q.Query(ctx, `
		SELECT a.id, COALESCE(NULLIF(c.name, ''), c.domain, '') FROM accounts a JOIN companies c ON c.id = a.company_id
		WHERE a.product_id = $1 AND (c.domain = $2 OR lower(c.name) = lower($3)
			OR ($4 AND (c.domain ILIKE '%' || $3 || '%' OR c.name ILIKE '%' || $3 || '%')))
		ORDER BY (c.domain = $2 OR lower(c.name) = lower($3)) DESC, a.id`, p.ID, NormalizeDomain(ref), ref, len(ref) >= 3)
	if err != nil {
		return 0, err
	}
	type hit struct {
		id   int64
		name string
	}
	hits, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (hit, error) {
		var h hit
		return h, r.Scan(&h.id, &h.name)
	})
	if err != nil {
		return 0, err
	}
	switch {
	case len(hits) == 0:
		return 0, invalid("no account matching %q in %s", ref, p.ID)
	case len(hits) > 1:
		var names []string
		for _, h := range hits {
			names = append(names, fmt.Sprintf("%s (#%d)", h.name, h.id))
		}
		return 0, invalid("%q matches %d accounts: %s; use the id", ref, len(hits), strings.Join(names, ", "))
	}
	return hits[0].id, nil
}

// List returns a product's accounts: open ones by stage, then paused and lost.
func List(ctx context.Context, q store.Q, p *product.Product, now time.Time) ([]*Account, error) {
	var stages []string
	if p.Pipeline != nil {
		stages = p.Pipeline.Stages
	}
	rows, err := q.Query(ctx, accountSelect+`
		WHERE a.product_id = $1
		ORDER BY a.status <> 'open', a.status, array_position($2::text[], a.stage), a.entered_at, a.id`, p.ID, stages)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Account, error) { return scanAccount(r) })
	for _, a := range list {
		a.derive(p, now)
	}
	return list, err
}

// Check is a qualification check with its current answer.
type Check struct {
	Name       string     `json:"check"`
	Gating     bool       `json:"gating"` // false: recorded only, never blocks
	Answer     string     `json:"answer"` // unknown until answered
	Evidence   string     `json:"evidence,omitempty"`
	AnsweredAt *time.Time `json:"answered_at,omitempty"`
}

type Member struct {
	PersonID int64  `json:"person_id"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	Title    string `json:"title,omitempty"`
	Role     string `json:"role"`
}

type Link struct {
	URL     string    `json:"url"`
	Note    string    `json:"note,omitempty"`
	AddedAt time.Time `json:"added_at"`
}

// Entry is one stage entered (or a pause or loss) in an account's history.
type Entry struct {
	Stage     string    `json:"stage"`
	EnteredAt time.Time `json:"entered_at"`
	Evidence  string    `json:"evidence,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	RevisitOn string    `json:"revisit_on,omitempty"`
}

type Task struct {
	ID         int64     `json:"id"`
	NextAction string    `json:"next_action"`
	DueAt      time.Time `json:"due_at"`
	Play       string    `json:"automation,omitempty"`
}

// Detail is everything about one account.
type Detail struct {
	Account
	// Qualified: every gating check is yes, so the gate stage and later
	// ones are open to this account. Failing lists the others.
	Qualified bool     `json:"qualified"`
	Failing   []string `json:"failing_checks,omitempty"`
	Checks    []Check  `json:"checks"`
	People    []Member `json:"people"`
	Links     []Link   `json:"links"`
	History   []Entry  `json:"history"`
	Tasks     []Task   `json:"open_tasks"`
}

// Get returns one account with its checks, people, links, history and
// open tasks.
func Get(ctx context.Context, q store.Q, p *product.Product, id int64, now time.Time) (*Detail, error) {
	a, err := scanAccount(q.QueryRow(ctx, accountSelect+` WHERE a.id = $1 AND a.product_id = $2`, id, p.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, invalid("no account %d in %s", id, p.ID)
	} else if err != nil {
		return nil, err
	}
	a.derive(p, now)
	d := &Detail{Account: *a, Checks: []Check{}, People: []Member{}, Links: []Link{}, History: []Entry{}, Tasks: []Task{}}

	type answer struct {
		answer, evidence string
		at               time.Time
	}
	got := map[string]answer{}
	rows, err := q.Query(ctx, `SELECT name, answer, evidence, answered_at FROM account_checks WHERE account_id = $1`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		var x answer
		if err := rows.Scan(&n, &x.answer, &x.evidence, &x.at); err != nil {
			rows.Close()
			return nil, err
		}
		got[n] = x
	}
	rows.Close()
	if qf := p.Pipeline.Qualify; qf != nil {
		plain := map[string]string{}
		for i, name := range append(slices.Clone(qf.Checks), qf.Record...) {
			c := Check{Name: name, Gating: i < len(qf.Checks), Answer: "unknown"}
			if x, ok := got[name]; ok {
				at := x.at
				c.Answer, c.Evidence, c.AnsweredAt = x.answer, x.evidence, &at
				plain[name] = x.answer
			}
			d.Checks = append(d.Checks, c)
		}
		d.Failing = Failing(p, plain)
	}
	d.Qualified = len(d.Failing) == 0

	rows, err = q.Query(ctx, `
		SELECT ap.person_id, pe.name, COALESCE((SELECT value FROM identities WHERE person_id = pe.id AND kind = 'email' ORDER BY value LIMIT 1), ''),
			pe.title, ap.role
		FROM account_people ap JOIN people pe ON pe.id = ap.person_id
		WHERE ap.account_id = $1 ORDER BY ap.role = 'champion' DESC, ap.added_at, ap.person_id`, id)
	if err != nil {
		return nil, err
	}
	if d.People, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) {
		var m Member
		return m, r.Scan(&m.PersonID, &m.Name, &m.Email, &m.Title, &m.Role)
	}); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT url, note, created_at FROM account_links WHERE account_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	if d.Links, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Link, error) {
		var l Link
		return l, r.Scan(&l.URL, &l.Note, &l.AddedAt)
	}); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `
		SELECT stage, entered_at, evidence, reason, COALESCE(to_char(revisit_on, 'YYYY-MM-DD'), '')
		FROM account_stages WHERE account_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	if d.History, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Entry, error) {
		var e Entry
		return e, r.Scan(&e.Stage, &e.EnteredAt, &e.Evidence, &e.Reason, &e.RevisitOn)
	}); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT id, next_action, due_at, play_id FROM tasks WHERE account_id = $1 AND status = 'open' ORDER BY due_at, id`, id)
	if err != nil {
		return nil, err
	}
	if d.Tasks, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Task, error) {
		var t Task
		return t, r.Scan(&t.ID, &t.NextAction, &t.DueAt, &t.Play)
	}); err != nil {
		return nil, err
	}
	return d, nil
}

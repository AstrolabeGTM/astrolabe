package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/score"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
)

type signalsArg struct {
	Product string `json:"product,omitempty"`
	Type    string `json:"type,omitempty"`
	Days    int    `json:"days,omitempty" jsonschema:"default 7"`
	Limit   int    `json:"limit,omitempty" jsonschema:"default 50"`
}

type hotArg struct {
	Product string `json:"product"`
	MinTier string `json:"min_tier,omitempty" jsonschema:"A, B, C or D (default B)"`
	Limit   int    `json:"limit,omitempty" jsonschema:"default 20"`
}

type addSignalArg struct {
	Product  string `json:"product"`
	Who      string `json:"who" jsonschema:"email or GitHub login"`
	Type     string `json:"type" jsonschema:"short name, e.g. referral or conference_chat; stored as manual.<type>"`
	Strength int    `json:"strength" jsonschema:"0-100; 25 is a strong buying signal"`
	WhyNow   bool   `json:"why_now,omitempty" jsonschema:"a time-bound trigger (postmortem, funding, new hire)"`
	Note     string `json:"note,omitempty"`
	URL      string `json:"url,omitempty"`
}

type runMonitorArg struct {
	Product string `json:"product"`
	Monitor string `json:"monitor"`
}

type factsArg struct {
	Person string   `json:"person" jsonschema:"person id or email"`
	Facts  []string `json:"facts" jsonschema:"kind:value facts, e.g. dep:bullmq, lang:typescript, team:5-50"`
}

type reviewArg struct {
	ReviewID int64 `json:"review_id"`
	PersonID int64 `json:"person_id,omitempty" jsonschema:"the candidate to attach; omit to dismiss"`
}

func (s *Server) signals(ctx context.Context, a signalsArg) (any, error) {
	if a.Days <= 0 {
		a.Days = 7
	}
	if a.Limit <= 0 {
		a.Limit = 50
	}
	rows, err := s.S.Pool.Query(ctx, `
		SELECT s.id, s.product_id, s.type, s.occurred_at, s.title, s.evidence_url, s.strength, s.why_now, s.person_id,
			COALESCE(p.name, ''), COALESCE(NULLIF(c.name, ''), c.domain, c.github_org, ''), COALESCE(sc.tier, ''), s.data
		FROM signals s LEFT JOIN people p ON p.id = s.person_id LEFT JOIN companies c ON c.id = s.company_id
		LEFT JOIN scores sc ON sc.product_id = s.product_id AND sc.person_id = s.person_id
		WHERE s.occurred_at > now() - make_interval(days => $3) AND ($1 = '' OR s.product_id = $1) AND ($2 = '' OR s.type = $2)
		ORDER BY s.occurred_at DESC LIMIT $4`, a.Product, a.Type, a.Days, a.Limit)
	if err != nil {
		return nil, err
	}
	type item struct {
		ID         int64          `json:"id"`
		Product    string         `json:"product"`
		Type       string         `json:"type"`
		OccurredAt time.Time      `json:"occurred_at"`
		Title      string         `json:"title"`
		URL        string         `json:"url"`
		Strength   int            `json:"strength"`
		WhyNow     bool           `json:"why_now,omitempty"`
		PersonID   *int64         `json:"person_id,omitempty"`
		Person     string         `json:"person,omitempty"`
		Company    string         `json:"company,omitempty"`
		Tier       string         `json:"tier,omitempty"`
		Data       map[string]any `json:"data,omitempty"`
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (item, error) {
		var i item
		return i, r.Scan(&i.ID, &i.Product, &i.Type, &i.OccurredAt, &i.Title, &i.URL, &i.Strength, &i.WhyNow, &i.PersonID, &i.Person, &i.Company, &i.Tier, &i.Data)
	})
	return map[string]any{"signals": items}, err
}

func (s *Server) hot(ctx context.Context, a hotArg) (any, error) {
	if a.MinTier == "" {
		a.MinTier = "B"
	}
	if a.Limit <= 0 {
		a.Limit = 20
	}
	list, err := score.List(ctx, s.S.Pool, a.Product, strings.ToUpper(a.MinTier), true, a.Limit)
	return map[string]any{"people": list, "note": "Scores rank prospects; they do not authorise contact."}, err
}

func (s *Server) addSignal(ctx context.Context, a addSignalArg) (any, error) {
	sub := signal.Subject{}
	if strings.Contains(a.Who, "@") {
		sub.Email = a.Who
	} else {
		sub.GitHub = a.Who
	}
	res, err := s.Signals.Ingest(ctx, &signal.Signal{Product: a.Product, Type: "manual." + strings.TrimPrefix(a.Type, "manual."),
		OccurredAt: time.Now(), Strength: a.Strength, WhyNow: a.WhyNow, Title: a.Note, EvidenceURL: a.URL, Subject: sub,
		DedupeKey: fmt.Sprintf("manual:%d", time.Now().UnixNano())})
	if err != nil {
		return nil, err
	}
	if res.PersonID != nil {
		if _, err := score.Recompute(ctx, s.S.Pool, a.Product, *res.PersonID, time.Now()); err != nil {
			return nil, err
		}
	}
	return map[string]any{"signal_id": res.SignalID, "person_id": res.PersonID}, nil
}

func (s *Server) monitors(ctx context.Context, _ noArgs) (any, error) {
	type status struct {
		Product   string     `json:"product"`
		Monitor   string     `json:"monitor"`
		Type      string     `json:"type"`
		Every     string     `json:"every"`
		LastRun   *time.Time `json:"last_run,omitempty"`
		LastCount int        `json:"last_new_signals"`
		Error     string     `json:"error,omitempty"`
	}
	var out []status
	for _, p := range s.Products() {
		for _, m := range p.Monitors {
			st := status{Product: p.ID, Monitor: m.ID, Type: m.Type, Every: m.Every}
			s.S.Pool.QueryRow(ctx, `SELECT last_run_at, last_count, last_error FROM monitor_runs WHERE product_id = $1 AND monitor_id = $2`,
				p.ID, m.ID).Scan(&st.LastRun, &st.LastCount, &st.Error)
			out = append(out, st)
		}
	}
	return map[string]any{"monitors": out}, nil
}

func (s *Server) runMonitor(ctx context.Context, a runMonitorArg) (any, error) {
	if s.Sources == nil {
		return nil, fmt.Errorf("monitors are not configured in this process")
	}
	n, err := s.Signals.RunMonitor(ctx, s.Sources, a.Product, a.Monitor, time.Now())
	if err != nil {
		return nil, err
	}
	if _, err := score.RecomputeAll(ctx, s.S.Pool, a.Product, time.Now()); err != nil {
		return nil, err
	}
	return map[string]any{"new_signals": n, "note": "GitHub enrichment runs in the background on astrolabe serve."}, nil
}

func (s *Server) setFacts(ctx context.Context, a factsArg) (any, error) {
	p, err := s.resolvePerson(ctx, a.Person)
	if err != nil {
		return nil, err
	}
	if err := people.SetFacts(ctx, s.S.Pool, p.ID, a.Facts, "manual"); err != nil {
		return nil, err
	}
	if _, err := score.RecomputeAll(ctx, s.S.Pool, "", time.Now()); err != nil {
		return nil, err
	}
	facts, err := people.Facts(ctx, s.S.Pool, p.ID)
	return map[string]any{"facts": facts}, err
}

func (s *Server) reviews(ctx context.Context, _ noArgs) (any, error) {
	rows, err := s.S.Pool.Query(ctx, `
		SELECT r.id, s.title, s.evidence_url, s.subject, r.candidates FROM identity_reviews r JOIN signals s ON s.id = r.signal_id
		WHERE r.status = 'open' ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	type rv struct {
		ID         int64          `json:"review_id"`
		Title      string         `json:"title"`
		URL        string         `json:"url"`
		Subject    map[string]any `json:"subject"`
		Candidates []int64        `json:"candidate_person_ids"`
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (rv, error) {
		var x rv
		return x, r.Scan(&x.ID, &x.Title, &x.URL, &x.Subject, &x.Candidates)
	})
	return map[string]any{"reviews": list}, err
}

func (s *Server) resolveReview(ctx context.Context, a reviewArg) (any, error) {
	if a.PersonID == 0 {
		return ok(s.Signals.Dismiss(ctx, a.ReviewID))
	}
	return ok(s.Signals.Resolve(ctx, a.ReviewID, a.PersonID))
}

type dncArg struct {
	Person string `json:"person" jsonschema:"person id or email"`
	Reason string `json:"reason,omitempty"`
	Allow  bool   `json:"allow,omitempty" jsonschema:"true removes the person from the list"`
}

func (s *Server) doNotContact(ctx context.Context, a dncArg) (any, error) {
	p, err := s.resolvePerson(ctx, a.Person)
	if err != nil {
		return nil, err
	}
	if a.Allow {
		return ok(people.Unsuppress(ctx, s.S.Pool, p.ID))
	}
	return ok(people.Suppress(ctx, s.S.Pool, p.ID, "manual", a.Reason))
}

type importCompaniesArg struct {
	Product string `json:"product"`
	CSV     string `json:"csv" jsonschema:"CSV text including the header row; needs a domain column"`
}

type companyFactsArg struct {
	Company string   `json:"company" jsonschema:"company id or domain"`
	Facts   []string `json:"facts" jsonschema:"kind:value facts, e.g. fact:sells_to_enterprise, dep:bullmq"`
	Product string   `json:"product,omitempty" jsonschema:"show the company's fit for this product"`
}

// companyFit is a company's own facts scored against a product's fit rules.
type companyFit struct {
	people.Company
	Fit       int      `json:"fit"`
	Breakdown []string `json:"breakdown"`
	Eligible  bool     `json:"eligible"`
	Excluded  string   `json:"excluded,omitempty"`
}

func fitOf(p *product.Product, c people.Company) companyFit {
	f := score.CompanyFit(p, c.Facts)
	return companyFit{Company: c, Fit: f.Fit, Breakdown: f.Breakdown, Eligible: f.Eligible, Excluded: f.Excluded}
}

func (s *Server) importCompanies(ctx context.Context, a importCompaniesArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	rows, err := people.ReadCompaniesCSV(strings.NewReader(a.CSV))
	if err != nil {
		return nil, err
	}
	res, err := people.ImportCompanies(ctx, s.S.Pool, rows)
	if err != nil {
		return nil, err
	}
	// Facts on a company change the fit of the people who work there.
	if _, err := score.RecomputeAll(ctx, s.S.Pool, "", time.Now()); err != nil {
		return nil, err
	}
	type item struct {
		companyFit
		Created  bool `json:"created"`
		NewFacts int  `json:"new_facts"`
	}
	out := make([]item, 0, len(res.Companies))
	for _, c := range res.Companies {
		out = append(out, item{fitOf(p, c.Company), c.Created, c.NewFacts})
	}
	return map[string]any{"created": res.Created, "updated": res.Updated, "skipped": res.Skipped, "companies": out}, nil
}

func (s *Server) setCompanyFacts(ctx context.Context, a companyFactsArg) (any, error) {
	c, err := people.FindCompany(ctx, s.S.Pool, a.Company)
	if err != nil {
		return nil, err
	}
	if err := people.SetCompanyFacts(ctx, s.S.Pool, c.ID, a.Facts, "manual"); err != nil {
		return nil, err
	}
	if _, err := score.RecomputeAll(ctx, s.S.Pool, "", time.Now()); err != nil {
		return nil, err
	}
	if c, err = people.FindCompany(ctx, s.S.Pool, strconv.FormatInt(c.ID, 10)); err != nil {
		return nil, err
	}
	if a.Product == "" {
		return map[string]any{"company": c}, nil
	}
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	return map[string]any{"company": fitOf(p, *c)}, nil
}

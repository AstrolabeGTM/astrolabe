package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/score"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
	"github.com/AstrolabeGTM/astrolabe/internal/views"
)

type signalsArg struct {
	Product string `json:"product,omitempty"`
	Type    string `json:"type,omitempty"`
	MinTier string `json:"min_tier,omitempty" jsonschema:"only signals about people at this priority or better: A, B, C or D"`
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
	return signal.Recent(ctx, s.S.Pool, signal.Filter{Product: a.Product, Type: a.Type, MinTier: a.MinTier, Days: a.Days, Limit: a.Limit}, time.Now())
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
	list, err := signal.Monitors(ctx, s.S.Pool, s.Products(), time.Now())
	return map[string]any{"monitors": list}, err
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
	facts, err := views.SetFacts(ctx, s.S.Pool, p.ID, a.Facts)
	return map[string]any{"facts": facts}, err
}

func (s *Server) reviews(ctx context.Context, _ noArgs) (any, error) {
	list, total, err := signal.OpenReviews(ctx, s.S.Pool, 200)
	return map[string]any{"reviews": list, "total": total}, err
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

func (s *Server) importCompanies(ctx context.Context, a importCompaniesArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	return views.ImportCompanies(ctx, s.S.Pool, p, a.CSV)
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
	return map[string]any{"company": views.FitOf(p, *c)}, nil
}

package mcp

import (
	"context"
	"errors"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/counters"
	"github.com/AstrolabeGTM/astrolabe/internal/digest"
	"github.com/AstrolabeGTM/astrolabe/internal/experiment"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/reception"
)

type contentArg struct {
	Product   string   `json:"product"`
	Batch     string   `json:"batch" jsonschema:"what the source is, e.g. v0.4 release"`
	Source    string   `json:"source" jsonschema:"release notes, blog post or changelog text"`
	Platforms []string `json:"platforms,omitempty" jsonschema:"x, linkedin, hn, reddit, newsletter, producthunt, directories"`
	Launch    bool     `json:"launch,omitempty" jsonschema:"also make a task per platform (all launch targets if none given)"`
}

type contentListArg struct {
	Product string `json:"product,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type contentMarkArg struct {
	ID     int64  `json:"id"`
	Status string `json:"status" jsonschema:"posted or skipped"`
	URL    string `json:"url,omitempty" jsonschema:"where it was posted"`
}

func (s *Server) content(ctx context.Context, a contentArg) (any, error) {
	if s.Studio == nil {
		return nil, errors.New("content is not configured")
	}
	platforms := a.Platforms
	if a.Launch && len(platforms) == 0 {
		platforms = []string{"hn", "producthunt", "reddit", "x", "linkedin", "newsletter", "directories"}
	}
	ids, err := s.Studio.Repurpose(ctx, a.Product, a.Batch, a.Source, platforms, a.Launch)
	if err != nil {
		return nil, err
	}
	items, err := s.Studio.List(ctx, a.Product, len(ids))
	return map[string]any{"created": ids, "items": items, "note": "Nothing is posted automatically; post by hand, then content_mark."}, err
}

func (s *Server) contentList(ctx context.Context, a contentListArg) (any, error) {
	if a.Limit <= 0 {
		a.Limit = 30
	}
	items, err := s.Studio.List(ctx, a.Product, a.Limit)
	return map[string]any{"items": items}, err
}

func (s *Server) contentMark(ctx context.Context, a contentMarkArg) (any, error) {
	return ok(s.Studio.Mark(ctx, a.ID, a.Status, a.URL))
}

func (s *Server) experiments(ctx context.Context, a productArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	res, err := experiment.Report(ctx, s.S.Pool, p)
	return map[string]any{"experiments": res}, err
}

func (s *Server) digestNow(ctx context.Context, _ noArgs) (any, error) {
	d, err := digest.Build(ctx, s.S.Pool, s.Products(), time.Now())
	if err != nil {
		return nil, err
	}
	return map[string]any{"text": d.Text(), "data": d}, nil
}

type productOpt struct {
	Product string `json:"product,omitempty" jsonschema:"product id; omit for all products"`
}

type mentionArg struct {
	Product      string `json:"product"`
	URL          string `json:"url" jsonschema:"full http(s) link to the mention"`
	Title        string `json:"title,omitempty" jsonschema:"title or a short excerpt"`
	Source       string `json:"source,omitempty" jsonschema:"hn, stackoverflow, rss, github, manual, ... (default manual)"`
	Date         string `json:"date,omitempty" jsonschema:"YYYY-MM-DD, default today"`
	Tone         string `json:"tone,omitempty" jsonschema:"positive, mixed, negative or unknown (default)"`
	AsksQuestion bool   `json:"asks_question,omitempty"`
}

type toneArg struct {
	ID   int64  `json:"id" jsonschema:"mention id from reception"`
	Tone string `json:"tone" jsonschema:"positive, mixed, negative or unknown"`
}

func (s *Server) productsFor(id string) ([]*product.Product, error) {
	if id == "" {
		return s.Products(), nil
	}
	p, err := s.find(id)
	if err != nil {
		return nil, err
	}
	return []*product.Product{p}, nil
}

func (s *Server) counters(ctx context.Context, a productOpt) (any, error) {
	ps, err := s.productsFor(a.Product)
	if err != nil {
		return nil, err
	}
	var out []*counters.Report
	for _, p := range ps {
		r, err := counters.Weekly(ctx, s.S.Pool, p.ID, time.Now())
		if err != nil {
			return nil, err
		}
		if len(r.Repos) > 0 || a.Product != "" {
			out = append(out, r)
		}
	}
	return map[string]any{"counters": out}, nil
}

func (s *Server) reception(ctx context.Context, a productOpt) (any, error) {
	ps, err := s.productsFor(a.Product)
	if err != nil {
		return nil, err
	}
	var out []*reception.Report
	for _, p := range ps {
		r, err := reception.Week(ctx, s.S.Pool, p.ID, time.Now(), 50)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return map[string]any{"reception": out}, nil
}

func (s *Server) logMention(ctx context.Context, a mentionArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	at := time.Now()
	if a.Date != "" {
		if at, err = time.Parse("2006-01-02", a.Date); err != nil {
			return nil, errors.New("date must be YYYY-MM-DD")
		}
	}
	id, created, err := reception.Add(ctx, s.S.Pool, reception.Mention{Product: p.ID, URL: a.URL, Title: a.Title,
		Source: a.Source, MentionedAt: at, Tone: a.Tone, AsksQuestion: a.AsksQuestion})
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "created": created}, nil
}

func (s *Server) setTone(ctx context.Context, a toneArg) (any, error) {
	return ok(reception.SetTone(ctx, s.S.Pool, a.ID, a.Tone))
}

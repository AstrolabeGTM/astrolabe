package mcp

import (
	"context"
	"errors"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/digest"
	"github.com/AstrolabeGTM/astrolabe/internal/experiment"
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

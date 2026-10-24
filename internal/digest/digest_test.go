package digest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/digest"
	"github.com/AstrolabeGTM/astrolabe/internal/pipeline"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

func TestDigestShowsPipeline(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	start := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	p := product.TestProduct(t)
	p.Pipeline = &product.Pipeline{Stages: []string{"identified", "contacted", "qualified", "customer"}, StuckAfter: "10d"}
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	add := func(domain, next string, due time.Time) int64 {
		id, err := pipeline.Add(ctx, pool, p, pipeline.AddOpts{Domain: domain, Evidence: "Asked about flaky tests", NextAction: next, Due: due}, start)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	add("acme.example", "Write the first message", start.AddDate(0, 0, 30))
	globex := add("globex.example", "Send the trial plan", start.AddDate(0, 0, 3))
	if err := pipeline.Move(ctx, pool, p, globex, pipeline.MoveOpts{Stage: "contacted", Evidence: "Replied to the intro"}, start.AddDate(0, 0, 2)); err != nil {
		t.Fatal(err)
	}

	d, err := digest.Build(ctx, pool, []*product.Product{p}, start.AddDate(0, 0, 11))
	if err != nil {
		t.Fatal(err)
	}
	pd := d.Products[0]
	if pd.Pipeline == nil || len(pd.Pipeline.Stuck) != 1 || len(pd.Pipeline.Overdue) != 1 || pd.Pipeline.Open != nil {
		t.Fatalf("digest pipeline: %+v", pd.Pipeline)
	}
	text := d.Text()
	for _, want := range []string{
		"Pipeline (accounts): identified 1, contacted 1, qualified 0, customer 0; paused 0, lost 0",
		"Overdue next actions (1): globex.example: Send the trial plan (due 2026-10-04)",
		"Stuck accounts (over 10d in one stage, 1): acme.example (identified, 11d)",
		"1. Do the overdue next action on accounts before new outreach: globex.example: Send the trial plan (due 2026-10-04).",
		"2. Move on, pause or close the account stuck over 10d in one stage: acme.example (identified, 11d).",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("digest lacks %q:\n%s", want, text)
		}
	}

	// Products without a pipeline leave the section out.
	p.Pipeline = nil
	d, err = digest.Build(ctx, pool, []*product.Product{p}, start)
	if err != nil {
		t.Fatal(err)
	}
	if d.Products[0].Pipeline != nil || strings.Contains(d.Text(), "Pipeline (accounts)") {
		t.Fatalf("pipeline shown without one:\n%s", d.Text())
	}
}

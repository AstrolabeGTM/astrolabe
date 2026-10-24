package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

func TestPipelineTools(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	p.Pipeline = &product.Pipeline{
		Stages:  []string{"identified", "contacted", "qualified", "trial", "customer"},
		Qualify: &product.Qualify{Gate: "qualified", Checks: []string{"pain", "champion"}, Record: []string{"budget"}},
	}
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	s := &Server{S: &outreach.Service{Pool: pool, River: rc}, Products: func() []*product.Product { return []*product.Product{p} }}
	st, ct := sdk.NewInMemoryTransports()
	if _, err := s.Build("test").Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := cs.ListTools(ctx, nil)
	desc := map[string]string{}
	for _, tl := range tools.Tools {
		desc[tl.Name] = tl.Description
	}
	for name, want := range map[string]string{"pipeline": "deal pipeline", "qualify": "BANT, MEDDIC", "close_account": "close-lost",
		"move_stage": "advance a deal", "account": "opportunity", "add_account": "", "set_next_action": "", "update_account": "", "pipeline_export": ""} {
		if d, ok := desc[name]; !ok || !strings.Contains(d, want) {
			t.Errorf("tool %s: %q", name, d)
		}
	}

	call := func(name string, args any) map[string]any {
		t.Helper()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := res.Content[0].(*sdk.TextContent).Text
		if res.IsError {
			return map[string]any{"error": text}
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("%s: %s", name, text)
		}
		return out
	}

	a := call("add_account", map[string]any{"product": "demo", "company": "acme.example", "name": "Acme",
		"evidence": "Sam opened an issue about flaky checkout tests", "next_action": "Write to Sam",
		"people": []map[string]any{{"person": "sam@acme.example", "role": "champion"}}})
	if a["error"] != nil || a["stage"] != "identified" || a["company"] != "Acme" || len(a["people"].([]any)) != 1 {
		t.Fatalf("add_account: %v", a)
	}
	if r := call("move_stage", map[string]any{"product": "demo", "account": "acme", "stage": "contacted"}); r["error"] == nil {
		t.Fatal("a move without evidence was accepted")
	}
	call("move_stage", map[string]any{"product": "demo", "account": "acme", "stage": "contacted", "evidence": "Intro sent",
		"next_action": "Book a call", "due": "2026-11-02"})

	// Moving to qualified is refused with the reason until the gating checks are yes.
	r := call("move_stage", map[string]any{"product": "demo", "account": "Acme", "stage": "qualified", "evidence": "Call held"})
	if e, _ := r["error"].(string); !strings.Contains(e, "cannot enter qualified") || !strings.Contains(e, "pain is unknown") {
		t.Fatalf("gate: %v", r)
	}
	q := call("qualify", map[string]any{"product": "demo", "account": "acme.example", "answers": []map[string]any{
		{"check": "pain", "answer": "yes", "evidence": "Flaky tests block three merges a day"},
		{"check": "champion", "answer": "yes", "evidence": "Sam will run the trial"},
		{"check": "budget", "answer": "unknown"}}})
	if q["qualified"] != true {
		t.Fatalf("qualify: %v", q)
	}
	m := call("move_stage", map[string]any{"product": "demo", "account": "acme", "stage": "qualified", "evidence": "Call held",
		"next_action": "Send the trial plan", "due": "2026-11-03"})
	if m["stage"] != "qualified" || m["next_action"] != "Send the trial plan" || m["next_due"] != "2026-11-03" {
		t.Fatalf("move: %v", m)
	}
	n := call("set_next_action", map[string]any{"product": "demo", "account": "acme", "next_action": "Kickoff call", "due": "2026-11-05"})
	if n["next_action"] != "Kickoff call" {
		t.Fatalf("set_next_action: %v", n)
	}
	goal := "Merge without re-running CI"
	u := call("update_account", map[string]any{"product": "demo", "account": "acme", "goal": goal,
		"links": []map[string]any{{"url": "https://github.com/acme/api/issues/812", "note": "the issue"}}})
	if u["goal"] != goal || len(u["links"].([]any)) != 1 {
		t.Fatalf("update_account: %v", u)
	}

	b := call("pipeline", map[string]any{"product": "demo"})
	if stages := b["stages"].([]any); stages[2].(map[string]any)["accounts"] != float64(1) {
		t.Fatalf("pipeline: %v", b)
	}
	if r := call("close_account", map[string]any{"product": "demo", "account": "acme", "status": "lost", "reason": "chose_alternative"}); r["error"] == nil {
		t.Fatal("closed without a revisit date")
	}
	revisit := time.Now().AddDate(0, 3, 0).Format("2006-01-02")
	c := call("close_account", map[string]any{"product": "demo", "account": "acme", "status": "lost", "reason": "chose_alternative", "revisit": revisit})
	if c["status"] != "lost" || c["revisit_on"] != revisit || c["next_action"] != nil {
		t.Fatalf("close_account: %v", c)
	}
	csv := call("pipeline_export", map[string]any{"product": "demo"})["csv"].(string)
	if lines := strings.Split(strings.TrimSpace(csv), "\n"); len(lines) != 2 || !strings.Contains(lines[1], ",Acme,acme.example,lost,qualified,") ||
		!strings.HasSuffix(lines[0], "check_pain,check_champion,check_budget") {
		t.Fatalf("export:\n%s", csv)
	}
	if r := call("pipeline", map[string]any{"product": "nope"}); r["error"] == nil {
		t.Fatal("unknown product")
	}
}

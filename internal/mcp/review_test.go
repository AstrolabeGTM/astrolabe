package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

func TestWeeklyReviewTools(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
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

	r := call("weekly_review", map[string]any{"product": "demo"})
	rv, _ := r["review"].(map[string]any)
	if r["saved"] != false || rv == nil || len(rv["questions"].([]any)) != 5 || rv["week"] == "" {
		t.Fatalf("weekly_review: %v", r)
	}
	if e := call("weekly_review", map[string]any{"product": "demo", "week": "2099-W01"}); e["error"] == nil {
		t.Fatal("a future week was built")
	}

	sv := call("save_review", map[string]any{"product": "demo", "answers": []map[string]any{{"number": 5, "answer": "Setup docs"}}})
	if sv["unanswered_questions"] != float64(4) {
		t.Fatalf("save_review: %v", sv)
	}
	if e := call("save_review", map[string]any{"product": "demo", "answers": []map[string]any{{"number": 6, "answer": "x"}}}); e["error"] == nil {
		t.Fatal("question 6 accepted")
	}
	r = call("weekly_review", map[string]any{"product": "demo", "fresh": true})
	if r["saved"] != true || r["saved_review"] == nil || r["fresh_build"] == nil {
		t.Fatalf("saved weekly_review: %v", r)
	}
	week := sv["week"].(string)
	list := call("reviews", map[string]any{"product": "demo"})
	if rs := list["reviews"].([]any); len(rs) != 1 || rs[0].(map[string]any)["week"] != week {
		t.Fatalf("reviews: %v", list)
	}

	pr, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "friday_review", Arguments: map[string]string{"product": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	text := pr.Messages[0].Content.(*sdk.TextContent).Text
	for _, want := range []string{"save_review", "1. How many reached activation", "5. What is the single biggest blocker", week, "Setup docs"} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

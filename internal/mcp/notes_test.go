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

	"github.com/AstrolabeGTM/astrolabe/internal/notes"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

func TestConversationTools(t *testing.T) {
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

	call("add_account", map[string]any{"product": "demo", "company": "acme.example", "name": "Acme",
		"evidence": "Sam opened an issue about flaky tests", "next_action": "Write to Sam",
		"people": []map[string]any{{"person": "sam@acme.example", "role": "champion"}}})

	// Acceptance: one note updates qualification and the next action.
	due := time.Now().AddDate(0, 0, 3).Format("2006-01-02")
	n := call("log_conversation", map[string]any{"product": "demo", "person": "sam@acme.example", "account": "acme", "channel": "call",
		"duration_minutes": 25, "assisted": true,
		"summary": "First call", "their_words": "I stopped re-running CI by hand", "problem": "Flaky tests", "current_approach": "Re-runs", "next_step": "Trial plan",
		"qualification": []map[string]any{{"check": "pain", "answer": "yes", "evidence": "Blocks three merges a day"}, {"check": "champion", "answer": "yes", "evidence": "Sam runs it"}},
		"next_action":   "Send the trial plan", "due": due,
		"pmf_survey":       map[string]any{"answer": "very_disappointed", "used_two_weeks": true, "alternative": "my script"},
		"pricing_feedback": []map[string]any{{"offer": "Team $29", "replaces": "CI minutes", "buying_process": "card", "reaction": "fine"}}})
	acct, _ := n["account"].(map[string]any)
	if n["error"] != nil || acct["qualified"] != true || acct["next_action"] != "Send the trial plan" || acct["next_due"] != due {
		t.Fatalf("log_conversation: %v", n)
	}
	if r := call("log_conversation", map[string]any{"product": "demo", "person": "sam@acme.example", "channel": "fax", "summary": "x"}); r["error"] == nil {
		t.Fatal("bad channel accepted")
	}
	call("log_conversation", map[string]any{"product": "demo", "person": "sam@acme.example", "channel": "chat", "summary": "Later chat"})

	// Reading notes: account, person and the conversations tool.
	if a := call("account", map[string]any{"product": "demo", "account": "acme"}); len(a["conversations"].([]any)) != 1 || a["stage"] == nil {
		t.Fatalf("account: %v", a)
	}
	if pe := call("person", map[string]any{"person": "sam@acme.example"}); len(pe["conversations"].([]any)) != 2 {
		t.Fatalf("person: %v", pe)
	}
	c := call("conversations", map[string]any{"product": "demo", "days": 7, "assisted": true})
	counts := c["counts_all_matching"].(map[string]any)
	if len(c["conversations"].([]any)) != 1 || counts["assisted"] != float64(1) || counts["unassisted"] != float64(1) {
		t.Fatalf("conversations: %v", c)
	}

	pmf := call("pmf_survey", map[string]any{"product": "demo"})
	if pmf["total_answers"] != float64(1) || pmf["answers"].([]any)[0].(map[string]any)["count"] != float64(1) {
		t.Fatalf("pmf_survey: %v", pmf)
	}
	if pf := call("pricing_feedback", map[string]any{"product": "demo", "days": 30}); pf["statements"] != float64(1) {
		t.Fatalf("pricing_feedback: %v", pf)
	}

	// Weekly themes: the prompt gathers, save_themes stores.
	pr, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "weekly_themes", Arguments: map[string]string{"product": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	text := pr.Messages[0].Content.(*sdk.TextContent).Text
	if !strings.Contains(text, "I stopped re-running CI by hand") || !strings.Contains(text, "save_themes") {
		t.Fatalf("prompt: %s", text)
	}
	if wn := call("week_notes", map[string]any{"product": "demo"}); wn["week"] != notes.ISOWeek(time.Now()) {
		t.Fatalf("week_notes: %v", wn)
	}
	call("save_themes", map[string]any{"product": "demo", "text": "Manual re-runs", "items": []map[string]any{{"theme": "Manual re-runs", "count": 1, "quotes": []string{"I stopped re-running CI by hand"}}}})
	if th := call("themes", map[string]any{"product": "demo"}); th["text"] != "Manual re-runs" {
		t.Fatalf("themes: %v", th)
	}
	if r := call("save_themes", map[string]any{"product": "demo", "week": "soon", "text": "x"}); r["error"] == nil {
		t.Fatal("bad week accepted")
	}
	prompts, _ := cs.ListPrompts(ctx, nil)
	if len(prompts.Prompts) != 1 || prompts.Prompts[0].Name != "weekly_themes" {
		t.Fatalf("prompts: %+v", prompts.Prompts)
	}
}

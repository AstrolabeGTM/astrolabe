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

func TestReceptionAndCounterTools(t *testing.T) {
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
	tools, _ := cs.ListTools(ctx, nil)
	for _, tl := range tools.Tools {
		if tl.Name == "counters" && !strings.Contains(tl.Description, "not installs or people") {
			t.Errorf("counters description must say what a download is: %s", tl.Description)
		}
		if tl.Name == "reception" && !strings.Contains(tl.Description, "social listening") {
			t.Errorf("reception should list sales terms: %s", tl.Description)
		}
	}

	m := call("log_mention", map[string]any{"product": "demo", "url": "https://news.ycombinator.com/item?id=1", "source": "hn", "title": "Show HN: Pipewrench"})
	if m["created"] != true {
		t.Fatalf("log_mention: %v", m)
	}
	if again := call("log_mention", map[string]any{"product": "demo", "url": "https://news.ycombinator.com/item?id=1"}); again["created"] != false || again["id"] != m["id"] {
		t.Fatalf("duplicate: %v", again)
	}
	if r := call("set_tone", map[string]any{"id": m["id"], "tone": "positive"}); r["status"] != "ok" {
		t.Fatalf("set_tone: %v", r)
	}
	rep := call("reception", map[string]any{"product": "demo"})["reception"].([]any)[0].(map[string]any)
	if rep["total"].(float64) != 1 || rep["by_tone"].(map[string]any)["positive"].(float64) != 1 || rep["window"] == nil {
		t.Fatalf("reception: %v", rep)
	}
	if c := call("counters", map[string]any{"product": "demo"}); c["error"] != nil {
		t.Fatalf("counters: %v", c)
	}
}

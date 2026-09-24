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

func TestChatLoop(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{S: &outreach.Service{Pool: pool, River: rc}, Products: func() []*product.Product { return []*product.Product{p} }}

	st, ct := sdk.NewInMemoryTransports()
	if _, err := s.Build("test").Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) < 15 {
		t.Fatalf("tools: %v %v", err, tools)
	}
	// Tool names use plain developer terms; sales terms live in the
	// descriptions so either vocabulary finds the tool.
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
		for _, jargon := range []string{"enroll", "play", "brief", "suppress", "hot", "icp", "autonomy", "lifecycle"} {
			if strings.Contains(tl.Name, jargon) {
				t.Errorf("tool %q uses jargon %q", tl.Name, jargon)
			}
		}
	}
	for _, tl := range tools.Tools {
		if tl.Name == "start_sequence" && !strings.Contains(tl.Description, "enroll in a cadence") {
			t.Errorf("start_sequence should mention its sales terms: %s", tl.Description)
		}
	}
	for _, want := range []string{"start_sequence", "top_prospects", "do_not_contact", "automations", "research", "label_reply", "record_stage"} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
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

	ctxOut := call("product_context", map[string]any{"product": "demo"})
	if !strings.Contains(ctxOut["claims"].(string), "one thing") {
		t.Fatalf("product_context: %v", ctxOut)
	}
	call("import_people", map[string]any{"product": "demo", "csv": "email,name\nana@acme.example,Ana Ruiz\n"})
	call("start_sequence", map[string]any{"product": "demo", "sequence": "intro", "person_ids": []int{1}})
	inbox := call("inbox", map[string]any{})
	drafts := inbox["drafts"].([]any)
	if len(drafts) != 1 {
		t.Fatalf("inbox: %v", inbox)
	}
	id := drafts[0].(map[string]any)["id"]

	bad := call("write_draft", map[string]any{"action_id": id, "body": "Results guaranteed."})
	if bad["error"] != nil {
		t.Fatalf("saving is allowed; approval is what refuses: %v", bad)
	}
	res := call("approve", map[string]any{"action_ids": []any{id}})
	if !strings.Contains(res["results"].(map[string]any)["1"].(string), "never allows") {
		t.Fatalf("forbidden claim approved: %v", res)
	}
	call("write_draft", map[string]any{"action_id": id, "body": "Hi Ana, want to try it on one thing?"})
	res = call("approve", map[string]any{"action_ids": []any{id}})
	if res["results"].(map[string]any)["1"] != "ok" {
		t.Fatalf("approve: %v", res)
	}
	if e := call("record_stage", map[string]any{"product": "demo", "person": "ana@acme.example", "stage": "nope"}); e["error"] == nil {
		t.Fatal("unknown stage accepted")
	}
	if r := call("do_not_contact", map[string]any{"person": "ana@acme.example", "reason": "asked"}); r["status"] != "ok" {
		t.Fatalf("do_not_contact: %v", r)
	}
	f := call("funnel", map[string]any{"product": "demo"})
	if len(f["stages"].([]any)) != 5 {
		t.Fatalf("funnel: %v", f)
	}
}

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/render"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

func TestCampaignTools(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	product.WriteTestCampaign(t, p.Dir)
	p, err := product.Load(p.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "screenshot") {
			fmt.Fprint(w, "\x89PNG x")
		} else {
			fmt.Fprint(w, "%PDF x")
		}
	}))
	defer fake.Close()
	s := &Server{S: &outreach.Service{Pool: pool, River: rc}, Products: func() []*product.Product { return []*product.Product{p} }, Render: render.New(fake.URL)}
	st, ct := sdk.NewInMemoryTransports()
	if _, err := s.Build("test").Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := cs.ListTools(ctx, nil)
	have := map[string]string{}
	for _, tl := range tools.Tools {
		have[tl.Name] = tl.Description
	}
	for _, name := range []string{"campaigns", "campaign", "set_campaign", "campaign_history", "check_gate", "record_spend", "import_spend", "creatives",
		"render_creative", "edit_creative", "approve_creative", "export_pack", "make_campaign_tasks", "campaign_report", "campaigns_json"} {
		if have[name] == "" {
			t.Errorf("missing tool %s", name)
		}
	}
	for tool, term := range map[string]string{"campaigns": "insertion orders", "creatives": "creative review", "set_campaign": "flight"} {
		if !strings.Contains(have[tool], term) {
			t.Errorf("%s should list the sales term %q: %s", tool, term, have[tool])
		}
	}
	call := func(name string, args any) map[string]any {
		t.Helper()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := res.Content[0].(*sdk.TextContent).Text
		out := map[string]any{}
		if res.IsError {
			return map[string]any{"error": text}
		}
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("%s: %s", name, text)
		}
		return out
	}
	base := map[string]any{"product": "demo", "campaign": "launch"}
	with := func(extra map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	list := call("campaigns", map[string]any{})
	if got := list["campaigns"].([]any); len(got) != 1 || got[0].(map[string]any)["gate"] != "0 of 2 checked" {
		t.Errorf("%v", list)
	}
	// Budget change with history.
	out := call("set_campaign", with(map[string]any{"budget_cap": 300, "note": "second test"}))
	if ch := out["changed"].([]any); len(ch) != 1 || ch[0].(map[string]any)["new"] != "300.00" || ch[0].(map[string]any)["changed_by"] != "mcp" {
		t.Errorf("%v", out)
	}
	h := call("campaign_history", base)["history"].([]any)
	if len(h) != 1 {
		t.Errorf("history: %v", h)
	}
	// Spend before the gate: allowed, warned.
	sp := call("record_spend", with(map[string]any{"amount": 250.5, "platform": "linkedin", "date": "2026-12-02", "clicks": 90}))
	if sp["error"] != nil || !strings.Contains(fmt.Sprint(sp["warnings"]), "before the gate") {
		t.Errorf("%v", sp)
	}
	if g := call("check_gate", with(map[string]any{"item": "1"})); g["gate_complete"] != false {
		t.Errorf("%v", g)
	}
	imp := call("import_spend", with(map[string]any{"csv": "date,spend,clicks\n2026-12-03,5,2\n", "platform": "reddit"}))
	if imp["recorded"] != float64(1) {
		t.Errorf("%v", imp)
	}
	// Creatives: render, approve, edit withdraws.
	call("render_creative", with(map[string]any{"creative": "card"}))
	if a := call("approve_creative", with(map[string]any{"creative": "card"})); a["approved"] != true {
		t.Errorf("%v", a)
	}
	if e := call("edit_creative", with(map[string]any{"creative": "card", "copy": map[string]string{"headline": "New"}})); e["approved"] != false {
		t.Errorf("an edit must withdraw approval: %v", e)
	}
	if ex := call("export_pack", with(map[string]any{"dir": filepath.Join(t.TempDir(), "pack")})); ex["error"] == nil || !strings.Contains(fmt.Sprint(ex["error"]), "not approved") {
		t.Errorf("unapproved export: %v", ex)
	}
	if tk := call("make_campaign_tasks", base); len(tk["created"].([]any)) != 1 {
		t.Errorf("%v", tk)
	}
	rep := call("campaign_report", base)
	if txt := rep["text"].(string); !strings.Contains(txt, "success events (installed) attributed to campaign=launch: 0 of 92 platform clicks as entered") ||
		!strings.Contains(txt, "OVER THE CAP") && !strings.Contains(txt, "Near the cap") {
		t.Errorf("report:\n%s", txt)
	}
	full := call("campaign", base)
	if full["renderer"].(map[string]any)["configured"] != true {
		t.Errorf("%v", full["renderer"])
	}
	// campaigns.json is exactly [{code,label}].
	cj := call("campaigns_json", map[string]any{})
	var arr []map[string]any
	if err := json.Unmarshal([]byte(cj["file"].(string)), &arr); err != nil || len(arr) != 1 || len(arr[0]) != 2 || arr[0]["code"] != "launch" || arr[0]["label"] != "Launch week" {
		t.Errorf("%v %v", err, cj["file"])
	}
	// Unknown campaign is a clear error.
	if e := call("campaign", map[string]any{"product": "demo", "campaign": "nope"}); !strings.Contains(fmt.Sprint(e["error"]), "no campaign") {
		t.Errorf("%v", e)
	}
	// Without a renderer, render says what to start.
	s.Render = nil
	if e := call("render_creative", with(nil)); !strings.Contains(fmt.Sprint(e["error"]), "--profile creative") {
		t.Errorf("%v", e)
	}
}

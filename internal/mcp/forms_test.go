package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/forms"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

func TestFormSubmissionsAndReportedSourceOnPerson(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	p.Pipeline = &product.Pipeline{Stages: []string{"identified", "contacted"}}
	m := &product.Monitor{ID: "req", Type: "form_webhook", Signal: "form.request", Strength: 40, Every: "1h", Account: true}
	p.Monitors = []*product.Monitor{m}
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	sig := &signal.Service{Pool: pool, River: rc}
	in := &forms.Intake{Pool: pool, Signals: sig}
	if _, err := in.Submit(ctx, p, m, forms.Submission{Source: "webhook", ExternalID: "1", At: time.Now(),
		Fields: map[string]any{"email": "ana@acme.example", "name": "Ana", "how_did_you_hear_about_us": "A newsletter", "campaign": "nl-1"}}); err != nil {
		t.Fatal(err)
	}
	s := &Server{S: &outreach.Service{Pool: pool, River: rc}, Signals: sig, Products: func() []*product.Product { return []*product.Product{p} }}
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
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %v", name, err, res)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Content[0].(*sdk.TextContent).Text), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	subs := call("form_submissions", map[string]any{"product": "demo"})["submissions"].([]any)
	if len(subs) != 1 {
		t.Fatalf("submissions: %v", subs)
	}
	f := subs[0].(map[string]any)
	if f["reported_source"] != "A newsletter" || f["campaign"] != "nl-1" ||
		f["person"].(map[string]any)["email"] != "ana@acme.example" || f["account"].(map[string]any)["next_action"] != "Reply to the request" {
		t.Errorf("submission: %v", f)
	}
	person := call("person", map[string]any{"person": "ana@acme.example"})
	rs := person["reported_source"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["self_reported"] != "A newsletter" || rs[0].(map[string]any)["first_touch"] != "monitor:req" {
		t.Errorf("person reported_source: %v", person["reported_source"])
	}
}

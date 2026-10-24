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

	"github.com/AstrolabeGTM/astrolabe/internal/forms"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

func TestForgetPersonDryRunThenConfirm(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	m := &product.Monitor{ID: "req", Type: "form_webhook", Signal: "form.request", Strength: 40, Every: "1h"}
	p.Monitors = []*product.Monitor{m}
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	sig := &signal.Service{Pool: pool, River: rc}
	in := &forms.Intake{Pool: pool, Signals: sig}
	if _, err := in.Submit(ctx, p, m, forms.Submission{Source: "webhook", ExternalID: "1", At: time.Now(),
		Fields: map[string]any{"email": "ana@acme.example", "name": "Ana Lee"}}); err != nil {
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
	tools, _ := cs.ListTools(ctx, nil)
	for _, tl := range tools.Tools {
		if tl.Name == "forget_person" {
			for _, want := range []string{"GDPR erasure", "right to be forgotten", "data deletion request"} {
				if !strings.Contains(tl.Description, want) {
					t.Errorf("forget_person description lacks %q", want)
				}
			}
		}
	}
	call := func(args map[string]any) map[string]any {
		t.Helper()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "forget_person", Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("%v %v", err, res)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Content[0].(*sdk.TextContent).Text), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	dry := call(map[string]any{"person": "Ana@Acme.example"})["report"].(map[string]any)
	if dry["dry_run"] != true || dry["deleted"].(map[string]any)["people"] != float64(1) {
		t.Fatalf("dry run: %v", dry)
	}
	if _, err := people.FindByEmail(ctx, pool, "ana@acme.example"); err != nil {
		t.Fatalf("dry run deleted her: %v", err)
	}
	done := call(map[string]any{"person": "ana@acme.example", "confirm": true})["report"].(map[string]any)
	if done["dry_run"] != false || done["deleted"].(map[string]any)["form_submissions"] != float64(1) {
		t.Fatalf("confirmed: %v", done)
	}
	if _, err := people.FindByEmail(ctx, pool, "ana@acme.example"); err == nil {
		t.Fatal("still there")
	}
}

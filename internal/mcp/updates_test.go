package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
	"github.com/AstrolabeGTM/astrolabe/internal/updates"
)

type nullSender struct{}

func (nullSender) Send(context.Context, channel.Message) (channel.Result, error) {
	return channel.Result{}, nil
}

func TestUpdatesTools(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	p.Updates = &product.Updates{PostalAddress: "1 Example Street"}
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	s := &Server{S: &outreach.Service{Pool: pool, River: rc}, Products: func() []*product.Product { return []*product.Product{p} },
		Updates: &updates.Service{Pool: pool, Senders: func(channel.Message) (channel.Sender, error) { return nullSender{}, nil }}}
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
		if tl.Name == "updates_add" && !strings.Contains(tl.Description, "double opt-in") {
			t.Errorf("updates_add should list its sales terms: %s", tl.Description)
		}
		if tl.Name == "broadcast_approve" && !strings.Contains(tl.Description, "mailing list") && !strings.Contains(tl.Description, "newsletter") {
			t.Errorf("broadcast_approve should list its sales terms: %s", tl.Description)
		}
	}

	r := call("updates_add", map[string]any{"product": "demo", "email": "ana@acme.example", "name": "Ana"})
	if r["state"] != "pending" {
		t.Fatalf("add: %v", r)
	}
	l := call("updates_subscribers", map[string]any{"product": "demo"})
	if c := l["counts"].(map[string]any); c["pending"] != float64(1) || c["confirmed"] != float64(0) {
		t.Fatalf("counts: %v", l)
	}
	b := call("broadcast_create", map[string]any{"product": "demo", "subject": "Hello", "body": "Results guaranteed."})
	id := b["broadcast"].(map[string]any)["id"]
	if len(b["broadcast"].(map[string]any)["flags"].([]any)) == 0 {
		t.Fatalf("flagged copy: %v", b)
	}
	if e := call("broadcast_approve", map[string]any{"id": id}); e["error"] == nil {
		t.Fatalf("flagged copy approved: %v", e)
	}
	call("broadcast_edit", map[string]any{"id": id, "body": "Shipped."})
	if a := call("broadcast_approve", map[string]any{"id": id}); a["error"] != nil {
		t.Fatalf("approve: %v", a)
	}
	st2 := call("broadcast_status", map[string]any{"product": "demo"})
	if len(st2["broadcasts"].([]any)) != 1 {
		t.Fatalf("status: %v", st2)
	}
	ex := call("updates_export", map[string]any{"product": "demo"})
	if ex["count"] != float64(0) || !strings.HasPrefix(ex["csv"].(string), "email,name") {
		t.Fatalf("export: %v", ex)
	}
	if u := call("updates_unsubscribe", map[string]any{"product": "demo", "email": "ana@acme.example"}); u["unsubscribed"] != true {
		t.Fatalf("unsubscribe: %v", u)
	}
}

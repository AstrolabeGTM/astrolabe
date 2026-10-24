package play_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
	"github.com/AstrolabeGTM/astrolabe/internal/updates"
)

// A form signal subscribes its person: they are pending until they confirm,
// whatever their fit score, and a person known only from GitHub is refused.
func TestUpdatesSubscribeAction(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.p.Updates = &product.Updates{PostalAddress: "1 Example Street"}
	e.p.Plays = []*product.Play{{ID: "list", Trigger: product.Trigger{Signal: "form.updates"}, When: product.When{MinFit: 90},
		Action: product.Action{Updates: "subscribe"}}}
	e.sync()
	e.eng.Updates = &updates.Service{Pool: e.out.Pool, Now: func() time.Time { return e.now }}

	people.Import(ctx, e.out.Pool, "demo", []people.Row{{Email: "ana@acme.example", Name: "Ana"}, {GitHub: "octocat"}})
	_, err := e.out.Pool.Exec(ctx, `UPDATE identities SET source = 'form' WHERE value = 'ana@acme.example'`)
	e.must(err)
	for _, who := range []signal.Subject{{Email: "ana@acme.example"}, {GitHub: "octocat"}} {
		id := e.signal("f-"+who.Email+who.GitHub, "form.updates", who)
		e.must(e.eng.OnSignal(ctx, id))
		e.must(e.eng.OnSignal(ctx, id))
	}
	runs := e.runs()
	if runs["signal:1"] != "subscribed" || !strings.HasPrefix(runs["signal:2"], "skipped") {
		t.Fatalf("runs: %v", runs)
	}
	var status string
	e.must(e.out.Pool.QueryRow(ctx, `SELECT status FROM updates_subscribers WHERE email = 'ana@acme.example'`).Scan(&status))
	if status != "pending" {
		t.Fatalf("a form sign-up is pending until confirmed: %s", status)
	}
	var n int
	e.out.Pool.QueryRow(ctx, `SELECT count(*) FROM updates_messages WHERE kind = 'confirm'`).Scan(&n)
	if n != 1 {
		t.Fatalf("one confirmation: %d", n)
	}
}

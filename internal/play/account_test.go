package play_test

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/pipeline"
	"github.com/AstrolabeGTM/astrolabe/internal/play"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// account_stage and account_stuck automations run through the same play
// machinery as person triggers.
func TestAccountAutomations(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	now := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	p := product.TestProduct(t)
	p.Pipeline = &product.Pipeline{Stages: []string{"identified", "contacted", "qualified", "customer"}}
	p.Plays = []*product.Play{
		{ID: "plan", Trigger: product.Trigger{AccountStage: "contacted"}, Action: product.Action{Task: "Book a call"}},
		{ID: "slow", Trigger: product.Trigger{AccountStuck: &product.StageStuck{Stage: "contacted", For: "10d"}}, Action: product.Action{Task: "Follow up with dates"}},
		{ID: "nudge", Trigger: product.Trigger{AccountStuck: &product.StageStuck{Stage: "identified", For: "5d"}}, Action: product.Action{Sequence: "nudge"}},
	}
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	nowf := func() time.Time { return now }
	out := &outreach.Service{Pool: pool, River: rc, Now: nowf}
	eng := &play.Engine{Pool: pool, Out: out, Now: nowf}
	out.Recheck = eng.Recheck
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	add := func(domain string) int64 {
		t.Helper()
		id, err := pipeline.Add(ctx, pool, p, pipeline.AddOpts{Domain: domain, Evidence: "Asked about flaky tests",
			NextAction: "Write", Due: now}, now)
		must(err)
		return id
	}
	move := func(id int64, stage string) {
		t.Helper()
		must(pipeline.Move(ctx, pool, p, id, pipeline.MoveOpts{Stage: stage, Evidence: "They replied"}, now))
	}
	tasks := func(play string) int {
		var n int
		pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE play_id = $1 AND account_id IS NOT NULL`, play).Scan(&n)
		return n
	}

	start := now
	acme := add("acme.example")
	move(acme, "contacted")
	must(eng.Scan(ctx, p))
	must(eng.Scan(ctx, p))
	if tasks("plan") != 1 || tasks("slow") != 0 {
		t.Fatalf("stage automation: plan %d, slow %d", tasks("plan"), tasks("slow"))
	}
	var text string
	pool.QueryRow(ctx, `SELECT next_action FROM tasks WHERE play_id = 'plan'`).Scan(&text)
	if text != "Book a call: acme.example entered contacted" {
		t.Fatalf("task text: %q", text)
	}

	// A stuck rule creates its task once per stay in the stage...
	now = start.AddDate(0, 0, 11)
	for range 3 {
		must(eng.Scan(ctx, p))
	}
	if tasks("slow") != 1 {
		t.Fatalf("stuck tasks: %d", tasks("slow"))
	}
	pool.QueryRow(ctx, `SELECT next_action FROM tasks WHERE play_id = 'slow'`).Scan(&text)
	if text != "Follow up with dates: acme.example in contacted for 11 days" {
		t.Fatalf("stuck task text: %q", text)
	}
	// ...and stays quiet after the account moves on.
	must(pipeline.Close(ctx, pool, p, acme, pipeline.CloseOpts{Status: "paused", Reason: "no_reply", Revisit: now.AddDate(0, 0, 1)}, now))
	now = now.AddDate(0, 0, 20)
	must(eng.Scan(ctx, p))
	if tasks("slow") != 1 || tasks("plan") != 1 {
		t.Fatalf("fired after moving on: slow %d, plan %d", tasks("slow"), tasks("plan"))
	}
	// Coming back to the stage later is a new stay, and a new trigger.
	must(pipeline.Move(ctx, pool, p, acme, pipeline.MoveOpts{Stage: "contacted", Evidence: "Replied after all",
		NextAction: "Book a call", Due: now}, now))
	must(eng.Scan(ctx, p))
	now = now.AddDate(0, 0, 11)
	must(eng.Scan(ctx, p))
	if tasks("slow") != 2 || tasks("plan") != 2 {
		t.Fatalf("second stay: slow %d, plan %d", tasks("slow"), tasks("plan"))
	}

	// A sequence starts for the account's champion, and stops once the
	// account moves on. An account with nobody on it is skipped.
	people.Import(ctx, pool, "demo", []people.Row{{Email: "bo@umbrella.example", Name: "Bo"}, {Email: "jo@umbrella.example", Name: "Jo"}})
	bo, _ := people.FindByEmail(ctx, pool, "bo@umbrella.example")
	jo, _ := people.FindByEmail(ctx, pool, "jo@umbrella.example")
	umbrella := add("umbrella.example")
	nobody := add("hooli.example")
	must(pipeline.SetPerson(ctx, pool, p, umbrella, bo.ID, "user"))
	must(pipeline.SetPerson(ctx, pool, p, umbrella, jo.ID, "champion"))
	now = now.AddDate(0, 0, 6)
	must(eng.Scan(ctx, p))
	var outcome, detail string
	pool.QueryRow(ctx, `SELECT outcome, detail FROM play_runs WHERE play_id = 'nudge' AND account_id = $1`, nobody).Scan(&outcome, &detail)
	if outcome != "skipped" || detail != "no identifiable person" {
		t.Fatalf("account without people: %s %s", outcome, detail)
	}
	var person int64
	pool.QueryRow(ctx, `SELECT person_id FROM enrollments WHERE play_id = 'nudge'`).Scan(&person)
	if person != jo.ID {
		t.Fatalf("enrolled %d, want the champion %d", person, jo.ID)
	}
	move(umbrella, "contacted")
	if n, _ := out.Tick(ctx); n != 0 {
		t.Fatal("drafted a nudge for an account that moved on")
	}
	var stop string
	pool.QueryRow(ctx, `SELECT stop_reason FROM enrollments WHERE play_id = 'nudge'`).Scan(&stop)
	if stop != "account moved past identified" {
		t.Fatalf("stop reason: %q", stop)
	}
}

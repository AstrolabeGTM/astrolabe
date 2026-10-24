package pipeline_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/pipeline"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

type env struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	p    *product.Product
	now  time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), pool: store.TestDB(t), p: product.TestProduct(t),
		now: time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)} // a Wednesday
	e.p.Pipeline = &product.Pipeline{
		Stages:  []string{"identified", "contacted", "qualified", "trial", "customer"},
		Qualify: &product.Qualify{Gate: "qualified", Checks: []string{"pain", "champion", "timing"}, Record: []string{"budget"}},
	}
	e.sync()
	return e
}

func (e *env) sync() {
	e.t.Helper()
	e.p.Hash += "x"
	if err := product.Sync(e.ctx, e.pool, []*product.Product{e.p}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// refused checks that err is a rule refusal mentioning every want.
func (e *env) refused(err error, want ...string) {
	e.t.Helper()
	if !errors.Is(err, pipeline.ErrInvalid) {
		e.t.Fatalf("want a refusal mentioning %q, got %v", want, err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			e.t.Fatalf("refusal %q should mention %q", err, w)
		}
	}
}

func (e *env) add(domain string) int64 {
	e.t.Helper()
	id, err := pipeline.Add(e.ctx, e.pool, e.p, pipeline.AddOpts{Domain: domain, Evidence: "Opened an issue about flaky checkout tests",
		NextAction: "Write the first message", Due: e.now.AddDate(0, 0, 2)}, e.now)
	e.must(err)
	return id
}

func (e *env) get(id int64) *pipeline.Detail {
	e.t.Helper()
	d, err := pipeline.Get(e.ctx, e.pool, e.p, id, e.now)
	e.must(err)
	return d
}

func (e *env) move(id int64, stage string) error {
	return pipeline.Move(e.ctx, e.pool, e.p, id, pipeline.MoveOpts{Stage: stage, Evidence: "Call held; they want to try it"}, e.now)
}

func TestQualifyGateAndEvidence(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx

	// A new account needs evidence and one next action with a due date.
	_, err := pipeline.Add(ctx, e.pool, e.p, pipeline.AddOpts{Domain: "acme.example", NextAction: "Write", Due: e.now}, e.now)
	e.refused(err, "evidence is required")
	_, err = pipeline.Add(ctx, e.pool, e.p, pipeline.AddOpts{Domain: "acme.example", Evidence: "Issue #812"}, e.now)
	e.refused(err, "next action")
	_, err = pipeline.Add(ctx, e.pool, e.p, pipeline.AddOpts{Domain: "acme.example", Evidence: "Issue #812", NextAction: "Write"}, e.now)
	e.refused(err, "due date")
	// Nor can it start at the gate: it has no answers yet.
	_, err = pipeline.Add(ctx, e.pool, e.p, pipeline.AddOpts{Domain: "acme.example", Stage: "trial", Evidence: "Issue #812",
		NextAction: "Write", Due: e.now}, e.now)
	e.refused(err, "cannot enter trial", "pain is unknown", "champion is unknown", "timing is unknown")

	id := e.add("https://www.Acme.example/about")
	if d := e.get(id); d.Domain != "acme.example" || d.Stage != "identified" || d.Status != "open" || len(d.History) != 1 {
		t.Fatalf("added: %+v", d)
	}
	if _, err := pipeline.Add(ctx, e.pool, e.p, pipeline.AddOpts{Domain: "acme.example", Evidence: "again", NextAction: "x", Due: e.now}, e.now); err == nil {
		t.Fatal("a company is in a product's pipeline once")
	}

	// A move needs evidence; moving before the gate needs no checks.
	e.refused(pipeline.Move(ctx, e.pool, e.p, id, pipeline.MoveOpts{Stage: "contacted", Evidence: "  "}, e.now), "evidence is required")
	e.must(e.move(id, "contacted"))
	e.refused(e.move(id, "contacted"), "already in contacted")
	e.refused(e.move(id, "won"), "not a pipeline stage")

	// Moving to Qualified is refused unless every gating check is yes.
	e.refused(e.move(id, "qualified"), "cannot enter qualified", "pain is unknown", "champion is unknown", "timing is unknown")
	e.refused(pipeline.Qualify(ctx, e.pool, e.p, id, []pipeline.Answer{{Check: "pain", Answer: "yes"}}, e.now), "pain evidence is required")
	e.refused(pipeline.Qualify(ctx, e.pool, e.p, id, []pipeline.Answer{{Check: "authority", Answer: "yes", Evidence: "x"}}, e.now), "unknown check")
	e.refused(pipeline.Qualify(ctx, e.pool, e.p, id, []pipeline.Answer{{Check: "pain", Answer: "maybe", Evidence: "x"}}, e.now), "yes, no or unknown")
	e.must(pipeline.Qualify(ctx, e.pool, e.p, id, []pipeline.Answer{
		{Check: "pain", Answer: "yes", Evidence: "Flaky tests block\n three merges a day"},
		{Check: "champion", Answer: "no", Evidence: "Nobody has time this quarter"},
	}, e.now))
	err = e.move(id, "qualified")
	e.refused(err, "champion is no", "timing is unknown")
	if strings.Contains(err.Error(), "pain is") {
		t.Fatalf("pain is yes and should not be listed: %v", err)
	}
	// Record-only checks never block, whatever their answer.
	e.must(pipeline.Qualify(ctx, e.pool, e.p, id, []pipeline.Answer{
		{Check: "champion", Answer: "yes", Evidence: "Sam will run the trial"},
		{Check: "timing", Answer: "yes", Evidence: "Release freeze in November"},
		{Check: "budget", Answer: "no", Evidence: "No budget until next year"},
	}, e.now))
	e.must(e.move(id, "qualified"))
	d := e.get(id)
	if !d.Qualified || d.Checks[0].Evidence != "Flaky tests block three merges a day" || d.Checks[3].Gating || d.Checks[3].Answer != "no" {
		t.Fatalf("checks: %+v", d.Checks)
	}

	// Later stages are gated too: a gating check that turns to no blocks
	// them, while moving back before the gate stays possible.
	e.must(pipeline.Qualify(ctx, e.pool, e.p, id, []pipeline.Answer{{Check: "champion", Answer: "no", Evidence: "Sam left the company"}}, e.now))
	e.refused(e.move(id, "trial"), "cannot enter trial", "champion is no")
	e.must(e.move(id, "contacted"))
	if d := e.get(id); d.Qualified || len(d.Failing) != 1 || len(d.History) != 4 {
		t.Fatalf("after moving back: %+v", d)
	}
}

func TestOneNextActionPauseAndReopen(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx
	id := e.add("globex.example")

	// Setting the next action replaces it: there is only ever one.
	e.must(pipeline.SetNextAction(ctx, e.pool, e.p, id, "Send the trial plan", e.now.AddDate(0, 0, 1)))
	if d := e.get(id); d.NextAction != "Send the trial plan" || d.NextDue != "2026-10-08" {
		t.Fatalf("next action: %+v", d.Account)
	}
	e.refused(pipeline.SetNextAction(ctx, e.pool, e.p, id, "", e.now), "next action")
	// The database holds the rule too.
	if _, err := e.pool.Exec(ctx, `UPDATE accounts SET next_action = '' WHERE id = $1`, id); err == nil {
		t.Fatal("an open account without a next action was stored")
	}

	// Pausing needs a reason from the list and a revisit date.
	pause := pipeline.CloseOpts{Status: "paused", Reason: "no_timing", Revisit: e.now.AddDate(0, 0, 30), Note: "Back after the release freeze"}
	bad := pause
	bad.Reason = "busy"
	e.refused(pipeline.Close(ctx, e.pool, e.p, id, bad, e.now), "reason must be one of")
	bad = pause
	bad.Revisit = time.Time{}
	e.refused(pipeline.Close(ctx, e.pool, e.p, id, bad, e.now), "revisit date")
	bad.Revisit = e.now.AddDate(0, 0, -1)
	e.refused(pipeline.Close(ctx, e.pool, e.p, id, bad, e.now), "in the past")
	bad = pause
	bad.Status = "won"
	e.refused(pipeline.Close(ctx, e.pool, e.p, id, bad, e.now), "status must be one of")
	e.must(pipeline.Close(ctx, e.pool, e.p, id, pause, e.now))
	d := e.get(id)
	if d.Status != "paused" || d.Stage != "identified" || d.NextAction != "" || d.Reason != "no_timing" || d.RevisitOn != "2026-11-06" {
		t.Fatalf("paused: %+v", d.Account)
	}
	e.refused(pipeline.SetNextAction(ctx, e.pool, e.p, id, "Call", e.now), "is paused")

	// Reopening needs a new next action, and the gate still applies.
	e.refused(e.move(id, "contacted"), "needs one next action")
	e.refused(pipeline.Move(ctx, e.pool, e.p, id, pipeline.MoveOpts{Stage: "qualified", Evidence: "They came back",
		NextAction: "Book a call", Due: e.now}, e.now), "cannot enter qualified")
	e.must(pipeline.Move(ctx, e.pool, e.p, id, pipeline.MoveOpts{Stage: "contacted", Evidence: "They wrote back after the freeze",
		NextAction: "Book a call", Due: e.now}, e.now))
	if d := e.get(id); d.Status != "open" || d.Reason != "" || d.RevisitOn != "" || d.NextAction != "Book a call" {
		t.Fatalf("reopened: %+v", d.Account)
	}

	// Lost uses the configured reasons when there are some.
	e.p.Pipeline.Reasons = []string{"no_fit", "went_elsewhere"}
	e.sync()
	e.refused(pipeline.Close(ctx, e.pool, e.p, id, pipeline.CloseOpts{Status: "lost", Reason: "no_timing", Revisit: e.now}, e.now), "went_elsewhere")
	e.must(pipeline.Close(ctx, e.pool, e.p, id, pipeline.CloseOpts{Status: "lost", Reason: "went_elsewhere", Revisit: e.now.AddDate(0, 3, 0)}, e.now))
}

func TestRevisitTaskAppearsOnTheDate(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx
	id := e.add("initech.example")
	e.must(pipeline.Close(ctx, e.pool, e.p, id, pipeline.CloseOpts{Status: "paused", Reason: "no_reply", Revisit: e.now.AddDate(0, 0, 3)}, e.now))
	tasks := func() []string {
		rows, _ := e.pool.Query(ctx, `SELECT next_action FROM tasks WHERE account_id = $1 AND status = 'open'`, id)
		var out []string
		for rows.Next() {
			var s string
			rows.Scan(&s)
			out = append(out, s)
		}
		return out
	}
	n, err := pipeline.Revisit(ctx, e.pool, e.now.AddDate(0, 0, 2))
	e.must(err)
	if n != 0 || len(tasks()) != 0 {
		t.Fatalf("task before the revisit date: %v", tasks())
	}
	for range 2 {
		_, err := pipeline.Revisit(ctx, e.pool, e.now.AddDate(0, 0, 3).Add(time.Hour))
		e.must(err)
	}
	if got := tasks(); len(got) != 1 || !strings.Contains(got[0], "Revisit initech.example (paused at identified, reason no_reply)") {
		t.Fatalf("revisit tasks: %v", got)
	}
	// Board lists it; reopening then pausing again gives a new revisit later.
	b, err := pipeline.BuildBoard(ctx, e.pool, e.p, e.now.AddDate(0, 0, 3))
	e.must(err)
	if b.Paused != 1 || len(b.RevisitDue) != 1 {
		t.Fatalf("board: %+v", b)
	}
	if d := e.get(id); len(d.Tasks) != 1 {
		t.Fatalf("account tasks: %+v", d.Tasks)
	}
}

func TestBoardStuckOverdueAndSnapshot(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx
	start := e.now
	acme := e.add("acme.example")         // identified, due in 2 days
	globex := e.add("globex.example")     // will be overdue
	umbrella := e.add("umbrella.example") // will be a customer: never stuck
	e.must(pipeline.SetNextAction(ctx, e.pool, e.p, globex, "Send the trial plan", start.AddDate(0, 0, 5)))
	for _, id := range []int64{umbrella} {
		e.must(pipeline.Qualify(ctx, e.pool, e.p, id, []pipeline.Answer{{Check: "pain", Answer: "yes", Evidence: "a"},
			{Check: "champion", Answer: "yes", Evidence: "b"}, {Check: "timing", Answer: "yes", Evidence: "c"}}, e.now))
		e.must(e.move(id, "customer"))
		e.must(pipeline.SetNextAction(ctx, e.pool, e.p, id, "Ask for a case study", start.AddDate(0, 0, 30)))
	}
	e.now = start.AddDate(0, 0, 3)
	e.must(e.move(globex, "contacted"))

	e.now = start.AddDate(0, 0, 16) // acme and umbrella 16 days in, globex 13
	b, err := pipeline.BuildBoard(ctx, e.pool, e.p, e.now)
	e.must(err)
	counts := map[string]int{}
	for _, s := range b.Stages {
		counts[s.Stage] = s.Accounts
	}
	if counts["identified"] != 1 || counts["contacted"] != 1 || counts["customer"] != 1 || len(b.Stages) != 5 {
		t.Fatalf("counts: %+v", b.Stages)
	}
	if len(b.Stuck) != 1 || b.Stuck[0].ID != acme || b.Stuck[0].DaysInStage != 16 {
		t.Fatalf("stuck (the last stage never is): %+v", b.Stuck)
	}
	if len(b.Overdue) != 2 {
		t.Fatalf("overdue: %+v", b.Overdue)
	}

	// Snapshot: once per week, then a CSV with a column per check.
	saved, err := pipeline.Snapshot(ctx, e.pool, e.p, e.now)
	e.must(err)
	again, err := pipeline.Snapshot(ctx, e.pool, e.p, e.now.Add(time.Hour))
	e.must(err)
	if !saved || again {
		t.Fatalf("snapshot saved %v, again %v", saved, again)
	}
	saved, err = pipeline.Snapshot(ctx, e.pool, e.p, e.now.AddDate(0, 0, 7))
	e.must(err)
	if !saved {
		t.Fatal("next week's snapshot")
	}
	var buf bytes.Buffer
	e.must(pipeline.WriteCSV(ctx, e.pool, e.p, &buf))
	recs, err := csv.NewReader(&buf).ReadAll()
	e.must(err)
	if len(recs) != 7 || strings.Join(recs[0], ",") != "week,account_id,company,domain,status,stage,days_in_stage,stuck,next_action,next_due,overdue,reason,revisit_on,check_pain,check_champion,check_timing,check_budget" {
		t.Fatalf("csv:\n%s", buf.String())
	}
	if r := recs[1]; r[0] != "2026-10-19" || r[2] != "acme.example" || r[5] != "identified" || r[7] != "true" || r[13] != "unknown" {
		t.Fatalf("first row: %v", r)
	}
	if r := recs[3]; r[2] != "umbrella.example" || r[5] != "customer" || r[7] != "false" || r[13] != "yes" {
		t.Fatalf("customer row: %v", r)
	}
}

func TestFindAndPeople(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx
	people.Import(ctx, e.pool, "demo", []people.Row{{Email: "sam@acme.example", Name: "Sam Lee"}, {Email: "bo@acme.example", Name: "Bo"}})
	id := e.add("acme.example")
	e.pool.Exec(ctx, `UPDATE companies SET name = 'Acme' WHERE domain = 'acme.example'`)
	for _, ref := range []string{"acme.example", "Acme", "acme", "#1", "1"} {
		got, err := pipeline.Find(ctx, e.pool, e.p, ref)
		if err != nil || got != id {
			t.Errorf("find %q: %d %v", ref, got, err)
		}
	}
	if _, err := pipeline.Find(ctx, e.pool, e.p, "nope"); !errors.Is(err, pipeline.ErrInvalid) {
		t.Fatal("unknown account found")
	}
	// An exact name wins over a partial one; two partial matches are ambiguous.
	labs, err := pipeline.Add(ctx, e.pool, e.p, pipeline.AddOpts{Name: "Acme Labs", Evidence: "Asked on HN", NextAction: "Reply", Due: e.now}, e.now)
	e.must(err)
	for ref, want := range map[string]int64{"acme": id, "Acme": id, "Acme Labs": labs, "labs": labs} {
		if got, err := pipeline.Find(ctx, e.pool, e.p, ref); err != nil || got != want {
			t.Errorf("find %q: %d %v, want %d", ref, got, err, want)
		}
	}
	e.refused(func() error { _, err := pipeline.Find(ctx, e.pool, e.p, "acm"); return err }(), "matches 2 accounts")
	sam, _ := people.FindByEmail(ctx, e.pool, "sam@acme.example")
	bo, _ := people.FindByEmail(ctx, e.pool, "bo@acme.example")
	e.must(pipeline.SetPerson(ctx, e.pool, e.p, id, bo.ID, "user"))
	e.must(pipeline.SetPerson(ctx, e.pool, e.p, id, sam.ID, "Champion"))
	e.refused(pipeline.SetPerson(ctx, e.pool, e.p, id, sam.ID, "the boss!"), "role")
	e.must(pipeline.AddLink(ctx, e.pool, e.p, id, "https://github.com/acme/api/issues/812", "the issue"))
	e.refused(pipeline.AddLink(ctx, e.pool, e.p, id, "javascript:alert(1)", ""), "http(s) URL")
	goal := "Merge without re-running CI"
	e.must(pipeline.Update(ctx, e.pool, e.p, id, pipeline.UpdateOpts{Goal: &goal}))
	d := e.get(id)
	if len(d.People) != 2 || d.People[0].Name != "Sam Lee" || d.People[0].Role != "champion" || len(d.Links) != 1 || d.Goal != goal {
		t.Fatalf("detail: %+v", d)
	}
}

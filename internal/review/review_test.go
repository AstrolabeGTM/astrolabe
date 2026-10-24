package review_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/counters"
	"github.com/AstrolabeGTM/astrolabe/internal/digest"
	"github.com/AstrolabeGTM/astrolabe/internal/funnel"
	"github.com/AstrolabeGTM/astrolabe/internal/notes"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/pipeline"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/reception"
	"github.com/AstrolabeGTM/astrolabe/internal/review"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

type env struct {
	t                           *testing.T
	pool                        *pgxpool.Pool
	p                           *product.Product
	ctx                         context.Context
	sam, ria, lee, old, ancient int64
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%v\n%s", err, sql)
	}
}

func (e *env) person(email, name string) int64 {
	e.t.Helper()
	if _, err := people.Import(e.ctx, e.pool, e.p.ID, []people.Row{{Email: email, Name: name}}); err != nil {
		e.t.Fatal(err)
	}
	pe, err := people.FindByEmail(e.ctx, e.pool, email)
	if err != nil {
		e.t.Fatal(err)
	}
	return pe.ID
}

func (e *env) stage(person int64, stage, when string) {
	e.t.Helper()
	if _, err := funnel.Record(e.ctx, e.pool, e.p.ID, person, stage, at(when), "test", stage+"/"+when+"/"+string(rune('a'+person%26))); err != nil {
		e.t.Fatal(err)
	}
}

// seed builds the week 2026-W49 (Mon 30 Nov to Sun 6 Dec 2026); "now" is Friday 4 Dec 17:00.
func seed(t *testing.T) *env {
	ctx := context.Background()
	p := product.TestProduct(t)
	product.WriteTestCampaign(t, p.Dir)
	os.WriteFile(filepath.Join(p.Dir, "sequences", "exp.yaml"), []byte(`steps:
  - channel: email
    after: 0d
    goal: test
    variants:
      - {id: a, subject: "Hi", body: "Hello"}
      - {id: b, subject: "Hey", body: "Hello there"}
stop_on: [replied]
`), 0o644)
	var err error
	if p, err = product.Load(p.Dir); err != nil {
		t.Fatal(err)
	}
	p.Pipeline = &product.Pipeline{Stages: []string{"identified", "contacted", "qualified", "trial", "customer"}, StuckAfter: "10d"}
	pool := store.TestDB(t)
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, pool: pool, p: p, ctx: ctx}
	e.sam, e.ria, e.lee = e.person("sam@acme.example", "Sam"), e.person("ria@globex.example", "Ria"), e.person("lee@initech.example", "Lee")
	e.old, e.ancient = e.person("old@x.example", "Old"), e.person("anc@x.example", "Ancient")

	// Funnel.
	for _, s := range [][]string{{"reached", "2026-11-30 09:00"}, {"replied", "2026-12-01 09:00"}, {"installed", "2026-12-01 12:00"}, {"activated", "2026-12-02 12:00"}} {
		e.stage(e.sam, s[0], s[1])
	}
	e.stage(e.ria, "reached", "2026-12-01 09:00")
	e.stage(e.ria, "replied", "2026-12-02 09:00")
	e.stage(e.ria, "installed", "2026-12-03 09:00")
	e.stage(e.lee, "reached", "2026-11-20 09:00")
	e.stage(e.lee, "activated", "2026-12-03 09:00")
	e.stage(e.old, "reached", "2026-11-24 09:00")
	e.stage(e.ancient, "reached", "2026-10-01 09:00")
	e.stage(e.ancient, "installed", "2026-12-02 09:00")

	// Conversations: Sam's activation was coached, Lee's was not; Sam's PMF answer.
	if _, err := notes.Log(ctx, pool, p, notes.LogOpts{PersonID: e.sam, Channel: "call", Summary: "Setup call", Assisted: true,
		PMF: &notes.PMF{Answer: "very_disappointed", UsedTwoWeeks: true, Alternative: "my script"}}, at("2026-12-02 15:00")); err != nil {
		t.Fatal(err)
	}
	if _, err := notes.Log(ctx, pool, p, notes.LogOpts{PersonID: e.lee, Channel: "chat", Summary: "Pricing chat",
		Pricing: []notes.Pricing{{Offer: "Team $29", Replaces: "CI minutes", BuyingProcess: "card", Reaction: "fine"}}}, at("2026-12-03 15:00")); err != nil {
		t.Fatal(err)
	}
	if _, err := notes.SaveThemes(ctx, pool, p.ID, "2026-W49", "Flaky tests hurt", []notes.ThemeItem{{Theme: "Flaky tests", Count: 2}}, at("2026-12-04 10:00")); err != nil {
		t.Fatal(err)
	}

	// Form requests: replied in 1.5h (within), in 3h (over), never (over).
	for i, f := range []struct {
		when, src string
		who       int64
	}{{"2026-12-01 09:00", "HN", e.sam}, {"2026-12-02 09:00", " hn ", e.ria}, {"2026-12-03 09:00", "", e.old}} {
		e.exec(`INSERT INTO form_submissions (product_id, monitor_id, source, external_id, submitted_at, fields, reported_source, person_id)
			VALUES ($1, 'req', 'webhook', $2, $3, '{}', $4, $5)`, p.ID, string(rune('a'+i)), at(f.when), f.src, f.who)
	}
	// Two experiment sends, which are also the replies to Sam and Ria.
	for i, x := range []struct {
		who     int64
		variant string
		sent    string
	}{{e.sam, "a", "2026-12-01 10:30"}, {e.ria, "b", "2026-12-02 12:00"}} {
		var enr int64
		if err := pool.QueryRow(ctx, `INSERT INTO enrollments (product_id, person_id, sequence_id) VALUES ($1, $2, 'exp') RETURNING id`, p.ID, x.who).Scan(&enr); err != nil {
			t.Fatal(err)
		}
		e.exec(`INSERT INTO actions (enrollment_id, step, product_id, person_id, channel, status, variant, sent_at) VALUES ($1, 0, $2, $3, 'email', 'sent', $4, $5)`,
			enr, p.ID, x.who, x.variant, at(x.sent))
		_ = i
	}

	// Replies: interested (task closed in 30 min), question (unanswered), no (ignored).
	var r1 int64
	for i, r := range []struct{ class, when string }{{"interested", "2026-12-03 14:00"}, {"question", "2026-12-03 16:00"}, {"no", "2026-12-03 17:00"}} {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO replies (provider_message_id, mailbox, product_id, person_id, from_addr, received_at, classification)
			VALUES ($1, 'm', $2, $3, 'lee@initech.example', $4, $5) RETURNING id`, "m"+string(rune('a'+i)), p.ID, e.lee, at(r.when), r.class).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			r1 = id
		}
	}
	e.exec(`INSERT INTO tasks (product_id, person_id, reply_id, next_action, due_at, status, created_at, closed_at) VALUES ($1, $2, $3, 'Answer Lee', $4, 'done', $5, $6)`,
		p.ID, e.lee, r1, at("2026-12-04 00:00"), at("2026-12-03 14:05"), at("2026-12-03 14:30"))

	// Signals: two issues (answered in 4h by Ria's message; unanswered), a discussion (7h old), a star.
	sig := func(key, typ, when string, person *int64, data string) {
		e.exec(`INSERT INTO signals (product_id, type, occurred_at, person_id, strength, title, dedupe_key, data) VALUES ($1, $2, $3, $4, 10, $5, $6, $7)`,
			p.ID, typ, at(when), person, key, key, data)
	}
	sig("issue-1", "github.repo.issue", "2026-12-02 08:00", &e.ria, `{}`)
	sig("issue-2", "github.issue_pain", "2026-12-02 07:00", nil, `{"kind":"issue"}`)
	sig("disc-1", "github.repo.discussion", "2026-12-04 10:00", nil, `{}`)
	sig("star-1", "github.repo.star", "2026-12-03 10:00", nil, `{}`)
	sig("pr-1", "github.issue_pain", "2026-12-03 10:00", nil, `{"kind":"pr"}`)

	// Mentions and counters.
	for i, m := range []struct {
		tone string
		q    bool
		when string
	}{{"positive", false, "2026-12-01 10:00"}, {"negative", true, "2026-12-02 10:00"}, {"unknown", false, "2026-12-03 10:00"}} {
		if _, _, err := reception.Add(ctx, pool, reception.Mention{Product: p.ID, URL: "https://news.example/" + string(rune('a'+i)), MentionedAt: at(m.when),
			Source: "hn", Title: "t", Tone: m.tone, AsksQuestion: m.q, FoundBy: "manual"}); err != nil {
			t.Fatal(err)
		}
	}
	for day, stars := range map[string]int64{"2026-11-27 00:00": 10, "2026-12-04 00:00": 15} {
		if err := counters.Store(ctx, pool, p.ID, "rel", "me/tool", at(day), &counters.Snapshot{Points: []counters.Point{{Metric: counters.Stars, Value: stars}, {Metric: counters.Downloads, Tag: "v1", Asset: "a.tgz", Value: stars * 10}}}); err != nil {
			t.Fatal(err)
		}
	}

	// Pipeline.
	add := func(domain string, when string, due string) int64 {
		id, err := pipeline.Add(ctx, pool, p, pipeline.AddOpts{Domain: domain, Evidence: "Asked about flaky tests", NextAction: "Write", Due: at(due)}, at(when))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	add("acme.example", "2026-11-18 10:00", "2026-12-20 00:00")
	globex := add("globex.example", "2026-12-01 10:00", "2026-12-02 00:00")
	initech := add("initech.example", "2026-11-25 10:00", "2026-12-20 00:00")
	hooli := add("hooli.example", "2026-12-02 10:00", "2026-12-20 00:00")
	mv := func(id int64, when string) {
		if err := pipeline.Move(ctx, pool, p, id, pipeline.MoveOpts{Stage: "contacted", Evidence: "They replied"}, at(when)); err != nil {
			t.Fatal(err)
		}
	}
	mv(globex, "2026-12-03 10:00")
	mv(initech, "2026-12-01 10:00")
	if err := pipeline.Close(ctx, pool, p, initech, pipeline.CloseOpts{Status: "lost", Reason: "no_budget", Revisit: at("2027-03-01 00:00")}, at("2026-12-02 10:00")); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Close(ctx, pool, p, hooli, pipeline.CloseOpts{Status: "paused", Reason: "no_timing", Revisit: at("2027-03-01 00:00")}, at("2026-12-03 11:00")); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.SetPerson(ctx, pool, p, globex, e.sam, "advocate"); err != nil {
		t.Fatal(err)
	}
	return e
}

var now = at("2026-12-04 17:00")

func TestFridayReviewAnswersTheQuestions(t *testing.T) {
	e := seed(t)
	r, err := review.BuildAt(e.ctx, e.pool, e.p, "2026-W49", now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Window.From != "2026-11-30" || r.Window.To != "2026-12-04" || !r.Window.Partial || r.Weekday != "friday" {
		t.Errorf("window: %+v %s", r.Window, r.Weekday)
	}
	eq := func(name string, got, want any) {
		t.Helper()
		if g, w := toJSON(got), toJSON(want); g != w {
			t.Errorf("%s: got %s want %s", name, g, w)
		}
	}

	// Spread.
	sp := r.Spread
	eq("requests total/unanswered", []int{sp.Requests.Total, sp.Requests.Unanswered}, []int{3, 1})
	if len(sp.Requests.Sources) != 1 || sp.Requests.Sources[0].Source != "hn" || sp.Requests.Sources[0].Requests != 2 {
		t.Errorf("reported sources: %+v", sp.Requests)
	}
	eq("mentions", []int{sp.Mentions.Total, sp.Mentions.ByTone["positive"], sp.Mentions.ByTone["negative"], sp.Mentions.ByTone["unknown"], sp.Mentions.Questions}, []int{3, 1, 1, 1, 1})
	eq("signals total", sp.Signals.Total, 5)
	if sp.Counters == nil || len(sp.Counters.Repos) != 1 || sp.Counters.Repos[0].Stars == nil || *sp.Counters.Repos[0].Stars.Week != 5 || sp.Counters.Repos[0].Downloads.Total != 150 {
		t.Errorf("counters: %s", toJSON(sp.Counters))
	}

	// Users.
	stages := map[string][2]int{}
	for _, s := range r.Users.Stages {
		stages[s.Stage] = [2]int{s.Entered, s.PreviousWeek}
	}
	eq("stage entries", stages, map[string][2]int{"reached": {2, 1}, "replied": {2, 0}, "installed": {3, 0}, "activated": {2, 0}, "paid": {0, 0}})
	c := r.Users.Cohort
	if c.People != 4 || c.From != "2026-11-09" || c.Complete {
		t.Errorf("cohort: %+v", c)
	}
	var got [][3]int
	for _, s := range c.Stages {
		got = append(got, [3]int{s.Reached, s.Previous, s.Percent})
	}
	eq("cohort stages", got, [][3]int{{4, 4, 100}, {3, 4, 75}, {3, 3, 100}, {2, 3, 66}, {0, 2, 0}})
	if len(r.Users.WeeklyCohorts) == 0 {
		t.Error("no weekly cohorts")
	}
	eq("pmf", []int{r.Users.PMF.Total, r.Users.PMF.People}, []int{1, 1})
	eq("activation", []int{r.Users.Activation.Activated, r.Users.Activation.Assisted, r.Users.Activation.Unassisted, r.Users.Activation.Sessions.Total, r.Users.Activation.Sessions.Assisted}, []int{2, 1, 1, 2, 1})

	// Accounts.
	a := r.Accounts
	if a == nil {
		t.Fatal("no accounts section")
	}
	entries := map[string]int{}
	for _, x := range a.Entries {
		entries[x.Stage] = x.Accounts
	}
	eq("account entries", entries, map[string]int{"identified": 2, "contacted": 2, "qualified": 0, "trial": 0, "customer": 0, "paused": 1, "lost": 1})
	eq("open accounts", a.OpenAccounts, 2)
	if len(a.Board.Stuck) != 1 || a.Board.Stuck[0].Company != "acme.example" || len(a.Board.Overdue) != 1 || a.Board.Overdue[0].Company != "globex.example" {
		t.Errorf("stuck/overdue: %s", toJSON(a.Board))
	}
	med := map[string][2]float64{}
	for _, m := range a.MedianDays {
		med[m.Stage] = [2]float64{m.MedianDays, float64(m.Visits)}
	}
	eq("median days", med["identified"], [2]float64{2, 3})
	eq("median days contacted", med["contacted"], [2]float64{1, 1})
	eq("closed this week", a.ClosedThisWeek, []review.Reason{{Status: "lost", Reason: "no_budget", Accounts: 1}, {Status: "paused", Reason: "no_timing", Accounts: 1}})
	if len(a.Advocates) != 1 || a.Advocates[0].Company != "globex.example" || a.Advocates[0].Person != "Sam" {
		t.Errorf("advocates: %+v", a.Advocates)
	}

	// Response times.
	kinds := map[string][5]int{}
	for _, k := range r.Response.Kinds {
		kinds[k.Kind] = [5]int{k.Total, k.Answered, k.Within, k.Over, k.Waiting}
	}
	eq("response buckets", kinds, map[string][5]int{
		"request": {3, 2, 1, 2, 0}, "reply": {2, 1, 1, 1, 0}, "issue": {2, 1, 1, 1, 0}, "discussion": {1, 0, 0, 0, 1}})
	if len(r.Response.Slowest) != 5 || r.Response.Slowest[0].Ref != "signal:2" && !strings.HasPrefix(r.Response.Slowest[0].Ref, "signal:") || r.Response.Slowest[0].Hours != 58 || r.Response.Slowest[0].Status != "over" {
		t.Errorf("slowest: %s", toJSON(r.Response.Slowest))
	}
	for _, k := range r.Response.Kinds {
		if k.Within+k.Over+k.Waiting != k.Total {
			t.Errorf("%s buckets do not add up: %+v", k.Kind, k)
		}
	}

	// Themes, campaigns, experiments, questions.
	if r.Themes.Saved == nil || r.Themes.Saved.Items[0].Theme != "Flaky tests" || r.Themes.Pricing == nil {
		t.Errorf("themes: %s", toJSON(r.Themes))
	}
	if len(r.Campaigns) != 1 || r.Campaigns[0].Code != "launch" || !r.Campaigns[0].Active {
		t.Errorf("campaigns: %+v", r.Campaigns)
	}
	if len(r.Experiments) != 1 || r.Experiments[0].Key != "demo/exp/0" || len(r.Experiments[0].Variants) != 2 || r.Experiments[0].Verdict == "" {
		t.Errorf("experiments: %s", toJSON(r.Experiments))
	}
	if len(r.Questions) != 5 || r.Questions[0].Text != product.DefaultReviewQuestions[0] || r.Questions[0].Answered {
		t.Errorf("questions: %+v", r.Questions)
	}
}

func TestSavedReviewFreezesAndReloadsUnchanged(t *testing.T) {
	e := seed(t)
	saved, err := review.Save(e.ctx, e.pool, e.p, "2026-W49", review.SaveInput{
		Answers:   []review.Answer{{Number: 1, Answer: "Two: Sam (coached) and Lee. Sam said it saved him a day."}, {Question: e.p.ReviewSettings().Questions[4], Answer: "Setup docs"}},
		Decisions: []review.Decision{{Kind: "campaign", ID: "launch", Decision: "keep", Note: "on budget"}, {Kind: "experiment", ID: "demo/exp/0", Decision: "stop"}},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Unanswered != 3 || len(saved.Decisions) != 2 || saved.Review.Spread.Signals.Total != 5 {
		t.Fatalf("saved: %+v", saved)
	}
	before := toJSON(saved.Review.Spread) + toJSON(saved.Review.Response) + toJSON(saved.Review.Users) + toJSON(saved.Review.Accounts)

	// New data arrives: another request, a stage move, an account.
	e.exec(`INSERT INTO form_submissions (product_id, monitor_id, source, external_id, submitted_at, fields, reported_source) VALUES ($1, 'req', 'webhook', 'late', $2, '{}', 'x')`, e.p.ID, at("2026-12-04 12:00"))
	e.stage(e.ria, "activated", "2026-12-04 13:00")
	later := now.AddDate(0, 0, 3)

	again, err := review.Get(e.ctx, e.pool, e.p, "2026-W49")
	if err != nil {
		t.Fatal(err)
	}
	if after := toJSON(again.Review.Spread) + toJSON(again.Review.Response) + toJSON(again.Review.Users) + toJSON(again.Review.Accounts); after != before {
		t.Errorf("the saved week drifted:\nbefore %s\nafter  %s", before, after)
	}
	fresh, err := review.BuildAt(e.ctx, e.pool, e.p, "2026-W49", later)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Spread.Requests.Total != 4 || fresh.Users.Activation.Activated != 3 {
		t.Errorf("a fresh build should see the new data: %d requests, %d activated", fresh.Spread.Requests.Total, fresh.Users.Activation.Activated)
	}
	if fresh.Questions[0].Answer == "" || !again.Review.Questions[0].Answered || again.Review.Questions[1].Answered {
		t.Errorf("answers: fresh %+v saved %+v", fresh.Questions[0], again.Review.Questions)
	}

	// Saving again merges and keeps the frozen numbers; refresh rebuilds them.
	s2, err := review.Save(e.ctx, e.pool, e.p, "2026-W49", review.SaveInput{Answers: []review.Answer{{Number: 2, Answer: "HN drove it"}, {Number: 1, Answer: ""}},
		Decisions: []review.Decision{{Kind: "campaign", ID: "launch", Decision: "change", Note: "raise the cap"}}}, later)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Unanswered != 3 || len(s2.Decisions) != 2 || s2.Decisions[0].Decision != "change" || s2.Review.Spread.Requests.Total != 3 {
		t.Errorf("merged save: unanswered %d decisions %+v requests %d", s2.Unanswered, s2.Decisions, s2.Review.Spread.Requests.Total)
	}
	s3, err := review.Save(e.ctx, e.pool, e.p, "2026-W49", review.SaveInput{Refresh: true}, later)
	if err != nil || s3.Review.Spread.Requests.Total != 4 {
		t.Errorf("refresh: %v %+v", err, s3)
	}

	// Refusals.
	for name, in := range map[string]review.SaveInput{
		"question 9":     {Answers: []review.Answer{{Number: 9, Answer: "x"}}},
		"unknown text":   {Answers: []review.Answer{{Question: "Why?", Answer: "x"}}},
		"bad decision":   {Decisions: []review.Decision{{Kind: "campaign", ID: "launch", Decision: "maybe"}}},
		"not running":    {Decisions: []review.Decision{{Kind: "campaign", ID: "nope", Decision: "keep"}}},
		"neither number": {Answers: []review.Answer{{Answer: "x"}}},
	} {
		if _, err := review.Save(e.ctx, e.pool, e.p, "2026-W49", in, later); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := review.BuildAt(e.ctx, e.pool, e.p, "2027-W10", now); err == nil {
		t.Error("a future week was built")
	}

	list, err := review.List(e.ctx, e.pool, e.p, 10)
	if err != nil || len(list) != 1 || list[0].Week != "2026-W49" || list[0].Answered != 2 || list[0].Questions != 5 || list[0].Decisions != 2 {
		t.Errorf("list: %v %+v", err, list)
	}
}

func TestDigestLinksTheReview(t *testing.T) {
	e := seed(t)
	text := func() string {
		d, err := digest.Build(e.ctx, e.pool, []*product.Product{e.p}, now)
		if err != nil {
			t.Fatal(err)
		}
		return d.Text()
	}
	if got := text(); !strings.Contains(got, "Weekly review 2026-W49: not saved yet (5 questions unanswered; review day is Friday)") {
		t.Errorf("digest:\n%s", got)
	}
	if _, err := review.Save(e.ctx, e.pool, e.p, "2026-W49", review.SaveInput{Answers: []review.Answer{{Number: 1, Answer: "two"}}}, now); err != nil {
		t.Fatal(err)
	}
	if got := text(); !strings.Contains(got, "Weekly review 2026-W49: saved, 4 of 5 questions unanswered.") {
		t.Errorf("digest:\n%s", got)
	}
}

func TestReviewWithoutPipelineOrCustomTargets(t *testing.T) {
	e := seed(t)
	e.p.Pipeline = nil
	e.p.Review = &product.Review{Questions: []string{"What now?"}, Targets: &product.ReviewTargets{Requests: "1d"}}
	r, err := review.BuildAt(e.ctx, e.pool, e.p, "2026-W49", now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Accounts != nil || len(r.Questions) != 1 || r.Questions[0].Text != "What now?" {
		t.Errorf("accounts %v questions %+v", r.Accounts, r.Questions)
	}
	// A 1d target: the 3h answer is now within it; the unanswered one is 32h old.
	for _, k := range r.Response.Kinds {
		if k.Kind == "request" && (k.Within != 2 || k.Over != 1 || k.Target != "1d") {
			t.Errorf("request buckets: %+v", k)
		}
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

package notes_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/notes"
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
		now: time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)} // a Thursday, ISO week 43
	e.p.Pipeline = &product.Pipeline{
		Stages:  []string{"identified", "contacted", "qualified", "trial", "customer"},
		Qualify: &product.Qualify{Gate: "qualified", Checks: []string{"pain", "champion"}, Record: []string{"budget"}},
	}
	if err := product.Sync(e.ctx, e.pool, []*product.Product{e.p}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) person(email string) int64 {
	e.t.Helper()
	if _, err := people.Import(e.ctx, e.pool, e.p.ID, []people.Row{{Email: email, Name: email}}); err != nil {
		e.t.Fatal(err)
	}
	p, err := people.FindByEmail(e.ctx, e.pool, email)
	if err != nil {
		e.t.Fatal(err)
	}
	return p.ID
}

func (e *env) account(domain string) int64 {
	e.t.Helper()
	id, err := pipeline.Add(e.ctx, e.pool, e.p, pipeline.AddOpts{Domain: domain, Evidence: "Asked about flaky tests",
		NextAction: "Write the first message", Due: e.now.AddDate(0, 0, 5)}, e.now)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) log(o notes.LogOpts) int64 {
	e.t.Helper()
	id, err := notes.Log(e.ctx, e.pool, e.p, o, e.now)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// A note updates qualification and the next action; themes are saved to the
// week's review.
func TestNoteUpdatesAccountAndThemesAreSaved(t *testing.T) {
	e := newEnv(t)
	sam, acme := e.person("sam@acme.example"), e.account("acme.example")

	id := e.log(notes.LogOpts{PersonID: sam, AccountID: acme, Channel: "call", Duration: 30,
		Summary: "Walked through the report", TheirWords: "I stopped re-running CI by hand",
		Problem: "Flaky checkout tests", CurrentApproach: "Re-run jobs by hand", NextStep: "Send trial plan",
		Answers: []pipeline.Answer{{Check: "pain", Answer: "yes", Evidence: "Three blocked merges a day"},
			{Check: "champion", Answer: "yes", Evidence: "Sam runs the trial"}},
		NextAction: "Send the trial plan", NextDue: e.now.AddDate(0, 0, 2)})

	d, err := pipeline.Get(e.ctx, e.pool, e.p, acme, e.now)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Qualified || d.NextAction != "Send the trial plan" || d.NextDue != "2026-10-24" {
		t.Fatalf("account not updated: qualified=%v next=%q due=%q", d.Qualified, d.NextAction, d.NextDue)
	}
	for _, c := range d.Checks {
		if c.Name == "pain" && !strings.Contains(c.Evidence, "Three blocked merges a day") || c.Name == "pain" && !strings.Contains(c.Evidence, "note #") {
			t.Errorf("evidence lacks the note: %q", c.Evidence)
		}
	}
	list, err := notes.List(e.ctx, e.pool, notes.ListOpts{AccountID: acme}, e.now)
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].TheirWords != "I stopped re-running CI by hand" || list[0].Company == "" {
		t.Fatalf("list: %v %+v", err, list)
	}

	week := notes.ISOWeek(e.now)
	if week != "2026-W43" {
		t.Fatal(week)
	}
	m, err := notes.Gather(e.ctx, e.pool, e.p, week, e.now)
	if err != nil || len(m.Conversation) != 1 || m.Notes.Total != 1 || !strings.Contains(m.Instructions, "save_themes") {
		t.Fatalf("gather: %v %+v", err, m)
	}
	if _, err := notes.SaveThemes(e.ctx, e.pool, e.p.ID, week, "Flaky tests cost time", []notes.ThemeItem{{Theme: "Manual re-runs", Count: 1, Quotes: []string{"I stopped re-running CI by hand"}}}, e.now); err != nil {
		t.Fatal(err)
	}
	// Saving again replaces.
	if _, err := notes.SaveThemes(e.ctx, e.pool, e.p.ID, week, "Flaky tests cost time and merges", nil, e.now); err != nil {
		t.Fatal(err)
	}
	th, err := notes.GetThemes(e.ctx, e.pool, e.p.ID, week)
	if err != nil || th == nil || th.Text != "Flaky tests cost time and merges" {
		t.Fatalf("themes: %v %+v", err, th)
	}
	sec, err := notes.DigestSection(e.ctx, e.pool, e.p.ID, e.now)
	if err != nil || sec == nil || sec.Themes == nil || sec.Week.Total != 1 {
		t.Fatalf("digest section: %v %+v", err, sec)
	}
	if got, _ := notes.GetThemes(e.ctx, e.pool, e.p.ID, "2026-W42"); got != nil {
		t.Fatal("themes leaked into another week")
	}
}

func TestRefusalSavesNothing(t *testing.T) {
	e := newEnv(t)
	sam, acme := e.person("sam@acme.example"), e.account("acme.example")
	_, err := notes.Log(e.ctx, e.pool, e.p, notes.LogOpts{PersonID: sam, AccountID: acme, Channel: "call", Summary: "x",
		Answers: []pipeline.Answer{{Check: "pain", Answer: "yes"}}}, e.now) // yes needs evidence
	if !errors.Is(err, pipeline.ErrInvalid) {
		t.Fatalf("want a refusal, got %v", err)
	}
	if list, _ := notes.List(e.ctx, e.pool, notes.ListOpts{}, e.now); len(list) != 0 {
		t.Fatalf("a refused note was saved: %+v", list)
	}
	for name, o := range map[string]notes.LogOpts{
		"empty":      {PersonID: sam, Channel: "call"},
		"channel":    {PersonID: sam, Channel: "smoke", Summary: "x"},
		"no account": {PersonID: sam, Channel: "call", Summary: "x", Answers: []pipeline.Answer{{Check: "pain", Answer: "unknown"}}},
		"pmf":        {PersonID: sam, Channel: "call", Summary: "x", PMF: &notes.PMF{Answer: "meh", UsedTwoWeeks: true}},
		"buying":     {PersonID: sam, Channel: "call", Summary: "x", Pricing: []notes.Pricing{{Offer: "Pro", BuyingProcess: "barter"}}},
		"future":     {PersonID: sam, Channel: "call", Summary: "x", Date: e.now.AddDate(0, 0, 9)},
	} {
		if _, err := notes.Log(e.ctx, e.pool, e.p, o, e.now); !errors.Is(err, notes.ErrInvalid) {
			t.Errorf("%s: want a refusal, got %v", name, err)
		}
	}
}

func TestNextActionWithoutAccountIsATask(t *testing.T) {
	e := newEnv(t)
	tom := e.person("tom@initech.example")
	e.log(notes.LogOpts{PersonID: tom, Channel: "email", Summary: "Not this quarter", NextAction: "Check back", NextDue: e.now.AddDate(0, 0, 60)})
	var n int
	e.pool.QueryRow(e.ctx, `SELECT count(*) FROM tasks WHERE person_id = $1 AND next_action = 'Check back' AND status = 'open'`, tom).Scan(&n)
	if n != 1 {
		t.Fatalf("tasks: %d", n)
	}
}

func TestPMFCountsShowDenominatorsAndAssistedIsApart(t *testing.T) {
	e := newEnv(t)
	answer := func(email, ans string, used, assisted bool, daysAgo int) {
		e.log(notes.LogOpts{PersonID: e.person(email), Channel: "call", Summary: "survey", Assisted: assisted, Date: e.now.AddDate(0, 0, -daysAgo),
			PMF: &notes.PMF{Answer: ans, UsedTwoWeeks: used, Alternative: "my script " + email, WhoElse: "the team"}})
	}
	answer("a@x.example", "very_disappointed", true, false, 1)
	answer("b@x.example", "very_disappointed", true, true, 2)
	answer("c@x.example", "somewhat_disappointed", true, false, 3)
	answer("d@x.example", "not_disappointed", false, false, 4) // used under two weeks: not counted
	answer("e@x.example", "not_disappointed", true, false, 40) // outside a 30 day window

	all, err := notes.PMFSurvey(e.ctx, e.pool, e.p.ID, 0, e.now)
	if err != nil || all.Total != 4 || all.People != 4 || all.ExcludedUnder2Weeks != 1 {
		t.Fatalf("all: %v %+v", err, all)
	}
	want := map[string]int{"very_disappointed": 2, "somewhat_disappointed": 1, "not_disappointed": 1}
	for _, a := range all.Answers {
		if a.Count != want[a.Answer] {
			t.Errorf("%s = %d", a.Answer, a.Count)
		}
	}
	month, _ := notes.PMFSurvey(e.ctx, e.pool, e.p.ID, 30, e.now)
	if month.Total != 3 || month.Window.Days != 30 || month.Window.From != "2026-09-23" || month.Window.To != "2026-10-22" {
		t.Fatalf("window: %+v", month)
	}
	if txt := month.Text(); !strings.Contains(txt, "of 3 answers") || !strings.Contains(txt, "very disappointed 2") || strings.Contains(txt, "%") {
		t.Fatalf("text: %s", txt)
	}
	if month.Responses[0].Alternative == "" {
		t.Fatal("alternative not recorded word for word")
	}

	counts, _ := notes.Count(e.ctx, e.pool, notes.ListOpts{Product: e.p.ID, Days: 30}, e.now)
	if counts.Total != 4 || counts.Assisted != 1 || counts.Unassisted != 3 {
		t.Fatalf("counts: %+v", counts)
	}
	yes := true
	only, _ := notes.List(e.ctx, e.pool, notes.ListOpts{Assisted: &yes}, e.now)
	if len(only) != 1 || !only[0].Assisted {
		t.Fatalf("assisted filter: %+v", only)
	}
	sec, _ := notes.DigestSection(e.ctx, e.pool, e.p.ID, e.now)
	if !strings.Contains(sec.Text(), "4 logged, 1 assisted, 3 unassisted") || !strings.Contains(sec.Text(), "of 4 answers") {
		t.Fatalf("digest text:\n%s", sec.Text())
	}
}

func TestPricingSummary(t *testing.T) {
	e := newEnv(t)
	price := func(email, offer, replaces, buying, reaction string) {
		e.log(notes.LogOpts{PersonID: e.person(email), Channel: "call", Summary: "pricing",
			Pricing: []notes.Pricing{{Offer: offer, Replaces: replaces, BuyingProcess: buying, Reaction: reaction}}})
	}
	price("a@x.example", "Team plan $29/month", "CI minutes top-up", "card", "cheaper than re-runs")
	price("b@x.example", "team plan $29/month", "ci minutes top-up", "invoice", "needs sign-off")
	price("c@x.example", "Pro $99/month", "", "", "too much")
	s, err := notes.PricingFeedback(e.ctx, e.pool, e.p.ID, 30, e.now)
	if err != nil || s.Statements != 3 || s.People != 3 {
		t.Fatalf("%v %+v", err, s)
	}
	if s.ByOffer[0].Count != 2 || s.ByComparison[0].Count != 2 || len(s.ByBuying) != 3 || len(s.Quotes) != 3 {
		t.Fatalf("groups: %+v", s)
	}
}

func TestWeekBounds(t *testing.T) {
	from, to, err := notes.WeekBounds("2026-W43")
	if err != nil || from.Format("2006-01-02") != "2026-10-19" || to.Format("2006-01-02") != "2026-10-26" {
		t.Fatalf("%v %v %v", from, to, err)
	}
	for _, bad := range []string{"2026-43", "2026-W54", "2026-W00"} {
		if _, _, err := notes.WeekBounds(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestDemoConversationsLoad(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	dir := "../../examples/products/pipewrench-demo"
	p, err := product.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dir + "/demo-people.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := people.ReadCSV(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := people.Import(ctx, pool, p.ID, rows); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := pipeline.SeedFile(ctx, pool, p, dir+"/demo-accounts.yaml", now); err != nil {
		t.Fatal(err)
	}
	n, err := notes.SeedFile(ctx, pool, p, dir+"/demo-conversations.yaml", now)
	if err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
	if again, _ := notes.SeedFile(ctx, pool, p, dir+"/demo-conversations.yaml", now); again != 0 {
		t.Fatal("seeded twice")
	}
	c, _ := notes.Count(ctx, pool, notes.ListOpts{Product: p.ID}, now)
	pmf, _ := notes.PMFSurvey(ctx, pool, p.ID, 0, now)
	if c.Assisted != 1 || c.Unassisted != 2 || pmf.Total != 2 {
		t.Fatalf("%+v %+v", c, pmf)
	}
}

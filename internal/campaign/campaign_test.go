package campaign

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

var day = func(s string) time.Time { t, _ := time.ParseInLocation(dateLayout, s, time.UTC); return t }

// fixture: the demo product with the launch campaign (cap 200.50 USD, two
// gate items, stop rule at 50% with zero events), synced to a test database.
func fixture(t *testing.T) (*pgxpool.Pool, *product.Product, *product.Campaign) {
	t.Helper()
	p := product.TestProduct(t)
	product.WriteTestCampaign(t, p.Dir)
	p, err := product.Load(p.Dir)
	if err != nil {
		t.Fatal(err)
	}
	pool := store.TestDB(t)
	if err := product.Sync(context.Background(), pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	return pool, p, p.Campaign("launch")
}

func TestBudgetChangesKeepHistory(t *testing.T) {
	ctx := context.Background()
	pool, p, c := fixture(t)
	s, _ := Effective(ctx, pool, p, c)
	if s.Cap != 20050 || s.Currency != "USD" || s.Start != "2026-12-01" || s.StopWhen == nil || s.StopWhen.SpendPct != 50 {
		t.Fatalf("YAML defaults: %+v", s)
	}
	cap300, end := product.Money(30000), "2026-12-15"
	h, err := Set(ctx, pool, p, c, "mcp", Change{BudgetCap: &cap300, End: &end, Note: "second test"})
	if err != nil || len(h) != 2 {
		t.Fatalf("%v %+v", err, h)
	}
	cap100 := product.Money(10000)
	if _, err := Set(ctx, pool, p, c, "web", Change{BudgetCap: &cap100}); err != nil {
		t.Fatal(err)
	}
	// Setting what it already is records nothing.
	if h, _ := Set(ctx, pool, p, c, "web", Change{BudgetCap: &cap100}); len(h) != 0 {
		t.Errorf("no-op change recorded: %+v", h)
	}
	hist, _ := History(ctx, pool, p, c)
	if len(hist) != 3 {
		t.Fatalf("history: %+v", hist)
	}
	if hist[0].Field != "budget_cap" || hist[0].Old != "200.50" || hist[0].New != "300.00" || hist[0].ChangedBy != "mcp" || hist[0].Note != "second test" || hist[0].At.IsZero() {
		t.Errorf("first entry: %+v", hist[0])
	}
	if hist[2].Old != "300.00" || hist[2].New != "100.00" || hist[2].ChangedBy != "web" {
		t.Errorf("last entry: %+v", hist[2])
	}
	s, _ = Effective(ctx, pool, p, c)
	if s.Cap != 10000 || s.End != "2026-12-15" {
		t.Errorf("effective: %+v", s)
	}
	// Stop rule: change the checkable part, then remove it.
	pct := 80
	Set(ctx, pool, p, c, "mcp", Change{StopSpendPct: &pct})
	s, _ = Effective(ctx, pool, p, c)
	if s.StopWhen.SpendPct != 80 || s.StopWhen.MaxEvents != 0 || !strings.Contains(s.StopText, "half the budget") {
		t.Errorf("stop rule: %+v", s)
	}
	Set(ctx, pool, p, c, "mcp", Change{ClearStopRule: true})
	if s, _ = Effective(ctx, pool, p, c); s.StopWhen != nil {
		t.Error("stop rule should be cleared")
	}
	empty := ""
	Set(ctx, pool, p, c, "mcp", Change{End: &empty})
	if s, _ = Effective(ctx, pool, p, c); s.End != "" {
		t.Errorf("end should be cleared: %+v", s)
	}
	// Bad changes are refused.
	bad := "2026-11-01"
	if _, err := Set(ctx, pool, p, c, "mcp", Change{End: &bad}); err == nil {
		t.Error("end before start must be refused")
	}
	neg := product.Money(-1)
	if _, err := Set(ctx, pool, p, c, "mcp", Change{BudgetCap: &neg}); err == nil {
		t.Error("negative cap must be refused")
	}
	if _, err := Set(ctx, pool, p, c, "", Change{BudgetCap: &cap100}); err == nil {
		t.Error("a change needs a name attached")
	}
}

func TestSpendBeforeGateIsFlaggedNotBlocked(t *testing.T) {
	ctx := context.Background()
	pool, p, c := fixture(t)
	now := day("2026-12-03")
	r, err := RecordSpend(ctx, pool, p, c, "mcp", SpendInput{Date: "2026-12-02", Amount: 5000, Platform: "LinkedIn", Clicks: ptr(40)}, now)
	if err != nil {
		t.Fatalf("spend before the gate must be allowed: %v", err)
	}
	if !r.Recorded[0].BeforeGate || r.Recorded[0].Platform != "linkedin" {
		t.Errorf("entry: %+v", r.Recorded[0])
	}
	joined := strings.Join(r.Warnings, "\n")
	if !strings.Contains(joined, "before the gate was fully checked") || !strings.Contains(joined, "gate is not fully checked") {
		t.Errorf("warnings: %v", r.Warnings)
	}
	// Checking the gate afterwards does not erase the fact.
	CheckGate(ctx, pool, p, c, "1", true, "me", "")
	CheckGate(ctx, pool, p, c, "Claims checked", true, "me", "against claims.md")
	r, _ = RecordSpend(ctx, pool, p, c, "mcp", SpendInput{Date: "2026-12-03", Amount: 1000, Platform: "linkedin"}, now)
	if r.Recorded[0].BeforeGate {
		t.Error("spend after the gate is not flagged")
	}
	list, _ := SpendList(ctx, pool, p, c, 10)
	flagged := 0
	for _, s := range list {
		if s.BeforeGate {
			flagged++
		}
	}
	if len(list) != 2 || flagged != 1 {
		t.Errorf("the early entry stays flagged: %+v", list)
	}
	hist, _ := History(ctx, pool, p, c)
	if len(hist) != 2 || hist[0].Field != "gate:Landing page is live" || hist[0].New != "checked" {
		t.Errorf("gate checks are history too: %+v", hist)
	}
	// Unchecking works and is recorded.
	CheckGate(ctx, pool, p, c, "1", false, "me", "")
	if _, ok, _ := Gate(ctx, pool, p, c); ok {
		t.Error("gate should be open again")
	}
	if _, err := CheckGate(ctx, pool, p, c, "nope", true, "me", ""); err == nil {
		t.Error("unknown gate item")
	}
}

func TestCapWarnings(t *testing.T) {
	ctx := context.Background()
	pool, p, c := fixture(t) // cap 200.50
	now := day("2026-12-03")
	r, _ := RecordSpend(ctx, pool, p, c, "me", SpendInput{Date: "2026-12-02", Amount: 15000, Platform: "x"}, now)
	if strings.Contains(strings.Join(r.Warnings, "|"), "Near the cap") {
		t.Errorf("75%% is not near the cap: %v", r.Warnings)
	}
	r, _ = RecordSpend(ctx, pool, p, c, "me", SpendInput{Date: "2026-12-02", Amount: 2000, Platform: "x"}, now) // 170 of 200.50 = 84%
	if !strings.Contains(strings.Join(r.Warnings, "|"), "Near the cap: 170.00 of 200.50 USD spent (84%)") {
		t.Errorf("want near-cap warning: %v", r.Warnings)
	}
	r, _ = RecordSpend(ctx, pool, p, c, "me", SpendInput{Date: "2026-12-02", Amount: 5000, Platform: "x"}, now)
	if !strings.Contains(strings.Join(r.Warnings, "|"), "OVER THE CAP") {
		t.Errorf("want over-cap warning: %v", r.Warnings)
	}
	// Spending is never refused or stopped, and the cap never moves by itself.
	if s, _ := Effective(ctx, pool, p, c); s.Cap != 20050 {
		t.Errorf("cap moved: %v", s.Cap)
	}
	// Dated outside the window.
	r, _ = RecordSpend(ctx, pool, p, c, "me", SpendInput{Date: "2026-11-20", Amount: 100, Platform: "x"}, now)
	if !strings.Contains(strings.Join(r.Warnings, "|"), "outside the campaign dates") {
		t.Errorf("want window warning: %v", r.Warnings)
	}
	if _, err := RecordSpend(ctx, pool, p, c, "me", SpendInput{Date: "12/02/2026", Amount: 1, Platform: "x"}, now); err == nil {
		t.Error("bad date")
	}
	if _, err := RecordSpend(ctx, pool, p, c, "me", SpendInput{Date: "2026-12-02", Amount: 1}, now); err == nil {
		t.Error("platform is required")
	}
	// A currency change is refused once money is recorded.
	eur := "EUR"
	if _, err := Set(ctx, pool, p, c, "me", Change{Currency: &eur}); err == nil {
		t.Error("currency change with spend recorded")
	}
}

func TestImportSpendCSV(t *testing.T) {
	ctx := context.Background()
	pool, p, c := fixture(t)
	now := day("2026-12-05")
	csvText := "\ufeffDay,Amount spent,Clicks,Impressions,Campaign name\n" +
		"2026-12-02,$12.50,30,1000,a\n" +
		"2026-12-02,$2.50,5,200,b\n" + // same day: added together
		"Dec 3 2026,9,1,1,c\n" + // bad date format
		"2026-12-03,\"1,000.25\",10,500,a\n" +
		"2026-12-04,abc,1,1,a\n"
	res, err := ImportSpend(ctx, pool, p, c, "cli", "Reddit", strings.NewReader(csvText), now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 5 || res.Recorded != 2 || len(res.Skipped) != 2 || res.Total != "1015.25" {
		t.Fatalf("%+v", res)
	}
	st, _ := Check(ctx, pool, p, c, now)
	if st.Spend.Total != 101525 || st.Spend.Clicks != 45 || st.Spend.ClickEntries != 2 {
		t.Errorf("%+v", st.Spend)
	}
	// Importing the same file again replaces, never doubles.
	if _, err := ImportSpend(ctx, pool, p, c, "cli", "reddit", strings.NewReader(csvText), now); err != nil {
		t.Fatal(err)
	}
	if st, _ = Check(ctx, pool, p, c, now); st.Spend.Total != 101525 || st.Spend.Entries != 2 {
		t.Errorf("re-import doubled: %+v", st.Spend)
	}
	if _, err := ImportSpend(ctx, pool, p, c, "cli", "", strings.NewReader("date,spend\n2026-12-02,1\n"), now); err == nil {
		t.Error("platform needed when the CSV has no platform column")
	}
	if _, err := ImportSpend(ctx, pool, p, c, "cli", "x", strings.NewReader("when,money\n1,2\n"), now); err == nil {
		t.Error("missing columns")
	}
}

func TestReportShowsZeroHonestly(t *testing.T) {
	ctx := context.Background()
	pool, p, c := fixture(t)
	now := day("2026-12-05")
	RecordSpend(ctx, pool, p, c, "me", SpendInput{Date: "2026-12-02", Amount: 10050, Platform: "linkedin", Clicks: ptr(120)}, now)
	r, err := BuildReport(ctx, pool, p, c, now)
	if err != nil {
		t.Fatal(err)
	}
	text := r.Text()
	for _, want := range []string{
		"Spend: 100.50 of 200.50 USD cap (50%)",
		"success events (installed) attributed to campaign=launch: 0 of 120 platform clicks as entered",
		"platform clicks as entered: 120",
		"Cost per success event: not computed (0 success events",
		"STOP RULE MET",
		"Not measured:",
		"Downloads per campaign",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
	if r.Events.Success != 0 || r.CostPerEvent != "" || strings.Contains(text, "%/") {
		t.Errorf("zero must stay zero: %+v", r.Events)
	}
}

func TestAttribution(t *testing.T) {
	ctx := context.Background()
	pool, p, c := fixture(t)
	now := day("2026-12-05")
	svc := &signal.Service{Pool: pool}
	ing := func(typ, email, camp, at, key string) {
		t.Helper()
		data := map[string]any{}
		if camp != "" {
			data["campaign"] = camp
		}
		if _, err := svc.Ingest(ctx, &signal.Signal{Product: p.ID, Type: typ, OccurredAt: day(at), Strength: 10,
			Subject: signal.Subject{Email: email}, Data: data, DedupeKey: key}); err != nil {
			t.Fatal(err)
		}
	}
	ing("product.installed", "a@x.example", "launch", "2026-12-02", "1") // counts
	ing("product.installed", "a@x.example", "launch", "2026-12-03", "2") // same person: once
	ing("product.installed", "b@x.example", "other", "2026-12-02", "3")  // other campaign
	ing("product.installed", "c@x.example", "", "2026-12-02", "4")       // no code
	ing("product.installed", "d@x.example", "launch", "2026-11-20", "5") // before the window
	ing("product.signup", "e@x.example", "launch", "2026-12-02", "6")    // tagged, not the success event
	var pid int64
	pool.QueryRow(ctx, `SELECT id FROM people WHERE id IN (SELECT person_id FROM signals WHERE dedupe_key = '3')`).Scan(&pid)
	// A funnel event from a form-intake style source.
	var f int64
	if err := pool.QueryRow(ctx, `INSERT INTO people (name) VALUES ('F') RETURNING id`).Scan(&f); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO funnel_events (product_id, person_id, stage, occurred_at, source, dedupe_key) VALUES ($1, $2, 'installed', $3, 'campaign:launch', 'x1')`, p.ID, f, day("2026-12-04")); err != nil {
		t.Fatal(err)
	}
	st, err := Check(ctx, pool, p, c, now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Events.Success != 2 || st.Events.BySignal != 1 || st.Events.ByFunnel != 1 || st.Events.TaggedAll != 3 {
		t.Errorf("%+v", st.Events)
	}
	// A cost per event appears only with events.
	RecordSpend(ctx, pool, p, c, "me", SpendInput{Date: "2026-12-02", Amount: 5000, Platform: "x"}, now)
	r, _ := BuildReport(ctx, pool, p, c, now)
	if !strings.Contains(r.CostPerEvent, "25.00 USD per success event (50.00 USD over 2 events)") {
		t.Errorf("cost: %q", r.CostPerEvent)
	}
}

func TestVariantsListsOnlyCodeAndLabel(t *testing.T) {
	ctx := context.Background()
	pool, p, c := fixture(t)
	v, err := Variants(ctx, pool, []*product.Product{p}, day("2026-12-03"))
	if err != nil || len(v) != 1 || v[0].Code != "launch" || v[0].Label != "Launch week" {
		t.Fatalf("%v %+v", err, v)
	}
	// Ended campaigns drop out, using the runtime end date.
	end := "2026-12-02"
	Set(ctx, pool, p, c, "me", Change{End: &end})
	if v, _ = Variants(ctx, pool, []*product.Product{p}, day("2026-12-03")); len(v) != 0 {
		t.Errorf("ended campaign still listed: %+v", v)
	}
}

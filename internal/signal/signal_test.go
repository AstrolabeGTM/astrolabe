package signal_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/astrolabe-gtm/astrolabe/internal/github"
	"github.com/astrolabe-gtm/astrolabe/internal/people"
	"github.com/astrolabe-gtm/astrolabe/internal/product"
	"github.com/astrolabe-gtm/astrolabe/internal/score"
	"github.com/astrolabe-gtm/astrolabe/internal/signal"
	"github.com/astrolabe-gtm/astrolabe/internal/store"
)

func setup(t *testing.T) (*signal.Service, *product.Product) {
	t.Helper()
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	p.Fit = []product.FitRule{
		{Any: []string{"dep:bullmq", "dep:bee-queue"}, Weight: 40, Label: "job queue"},
		{Any: []string{"lang:typescript"}, Weight: 30},
		{Any: []string{"dep:react-native"}, Exclude: true, Label: "mobile app"},
	}
	p.Monitors = []*product.Monitor{
		{ID: "pain", Type: "github_search", Query: `"processed twice"`, Signal: "github.issue_pain", Strength: 25, Every: "1h"},
		{ID: "stars", Type: "github_repo", Repo: "me/tool", Events: []string{"star"}, Signal: "github.repo", Strength: 10, Every: "1h"},
		{ID: "talk", Type: "community", Keywords: []string{"idempotency"}, Sources: []string{"hn", "stackoverflow", "rss"}, Signal: "community.thread", Strength: 5, Every: "1h"},
	}
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc := &signal.Service{Pool: pool, River: rc}
	svc.Hooks = []signal.Hook{svc.ScoreAndEnrich}
	return svc, p
}

func TestIdentityMatchingIsDeterministic(t *testing.T) {
	svc, _ := setup(t)
	ctx := context.Background()
	now := time.Now()
	people.Import(ctx, svc.Pool, "demo", []people.Row{{Email: "ana@acmepay.example", Name: "Ana"}, {GitHub: "bo-dev", Name: "Bo"}})

	sig := func(key string, sub signal.Subject) signal.Result {
		t.Helper()
		r, err := svc.Ingest(ctx, &signal.Signal{Product: "demo", Type: "manual.note", OccurredAt: now, Strength: 10, DedupeKey: key, Subject: sub})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// Email + new GitHub login: attaches to Ana and links the login.
	r := sig("a", signal.Subject{Email: "ANA@acmepay.example", GitHub: "ana-gh"})
	ana, _ := people.FindByEmail(ctx, svc.Pool, "ana@acmepay.example")
	if r.PersonID == nil || *r.PersonID != ana.ID || ana.GitHub != "ana-gh" {
		t.Fatalf("expected Ana, got %+v / %+v", r, ana)
	}
	// Same key again: stored once.
	if r := sig("a", signal.Subject{Email: "ana@acmepay.example"}); !r.Duplicate {
		t.Fatal("duplicate signal stored twice")
	}
	// Ana's email with Bo's login: ambiguous, never guessed.
	if r := sig("b", signal.Subject{Email: "ana@acmepay.example", GitHub: "bo-dev"}); !r.Ambiguous || r.PersonID != nil {
		t.Fatalf("expected ambiguous, got %+v", r)
	}
	var reviews int
	svc.Pool.QueryRow(ctx, `SELECT count(*) FROM identity_reviews WHERE status = 'open'`).Scan(&reviews)
	if reviews != 1 {
		t.Fatalf("reviews: %d", reviews)
	}
	// Unknown HN user becomes a new person; an RSS item has no person.
	if r := sig("c", signal.Subject{HN: "pg"}); r.PersonID == nil {
		t.Fatal("hn user should become a person")
	}
	if r := sig("d", signal.Subject{}); r.PersonID != nil || r.Duplicate {
		t.Fatalf("anonymous signal: %+v", r)
	}
	// Product user ids are scoped per product.
	sig("e", signal.Subject{User: "42", Email: "u42@x.example"})
	var v string
	svc.Pool.QueryRow(ctx, `SELECT value FROM identities WHERE kind = 'user'`).Scan(&v)
	if v != "demo:42" {
		t.Fatalf("user identity %q", v)
	}
}

func fakeAPIs(t *testing.T, now time.Time) *httptest.Server {
	t.Helper()
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	mux := http.NewServeMux()
	j := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("q"), "created:>=") {
			t.Errorf("search not date-bounded: %s", r.URL.Query().Get("q"))
		}
		j(w, map[string]any{"items": []map[string]any{
			{"number": 812, "title": "Webhook processed twice", "html_url": "https://github.com/acmepay/api/issues/812",
				"repository_url": "https://api.github.com/repos/acmepay/api", "created_at": now.Add(-3 * 24 * time.Hour), "user": map[string]any{"login": "ana-dev", "type": "User"}},
			{"number": 9, "title": "bot noise", "repository_url": "https://api.github.com/repos/x/y", "created_at": now, "user": map[string]any{"login": "dependabot[bot]", "type": "Bot"}},
		}})
	})
	mux.HandleFunc("/repos/me/tool", func(w http.ResponseWriter, r *http.Request) { j(w, map[string]any{"stargazers_count": 2}) })
	mux.HandleFunc("/repos/me/tool/stargazers", func(w http.ResponseWriter, r *http.Request) {
		j(w, []map[string]any{
			{"starred_at": now.Add(-30 * 24 * time.Hour), "user": map[string]any{"login": "old-fan"}},
			{"starred_at": now.Add(-time.Hour), "user": map[string]any{"login": "ana-dev"}},
		})
	})
	mux.HandleFunc("/users/ana-dev", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]any{"login": "ana-dev", "name": "Ana Ruiz", "company": "@AcmePay", "blog": "https://acmepay.example", "email": "ana@acmepay.example"})
	})
	mux.HandleFunc("/repos/acmepay/api/languages", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]int{"TypeScript": 9000, "Shell": 100})
	})
	mux.HandleFunc("/repos/acmepay/api/contents/package.json", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]string{"encoding": "base64", "content": b64(`{"dependencies":{"bullmq":"^5","pg":"^8"}}`)})
	})
	mux.HandleFunc("/repos/acmepay/api/contents/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/hn/search_by_date", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]any{"hits": []map[string]any{{"objectID": "123", "author": "pg", "title": "Idempotency keys done right", "created_at_i": now.Add(-time.Hour).Unix()}}})
	})
	mux.HandleFunc("/so/search/advanced", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]any{"items": []map[string]any{{"question_id": 77, "title": "Idempotency &amp; retries", "link": "https://stackoverflow.com/q/77",
			"creation_date": now.Add(-time.Hour).Unix(), "owner": map[string]any{"user_id": 555, "display_name": "Sam"}}}})
	})
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<rss><channel>
<item><title>On idempotency in queues</title><link>https://blog.example/a</link><guid>a</guid><pubDate>%s</pubDate></item>
<item><title>Unrelated</title><link>https://blog.example/b</link><guid>b</guid><pubDate>%s</pubDate></item>
</channel></rss>`, now.Add(-time.Hour).Format(time.RFC1123Z), now.Format(time.RFC1123Z))
	})
	return httptest.NewServer(mux)
}

func TestMonitorsEnrichmentAndScoring(t *testing.T) {
	svc, p := setup(t)
	ctx := context.Background()
	now := time.Now()
	srv := fakeAPIs(t, now)
	defer srv.Close()
	gh := &github.Client{Base: srv.URL}
	src := &signal.Sources{GitHub: gh, HNBase: srv.URL + "/hn", StackBase: srv.URL + "/so"}
	p.Monitors[2].Feeds = []string{srv.URL + "/feed.xml"}
	p.Hash = "changed"
	product.Sync(ctx, svc.Pool, []*product.Product{p})

	for _, m := range []string{"pain", "stars", "talk"} {
		if _, err := svc.RunMonitor(ctx, src, "demo", m, now); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
	var n int
	svc.Pool.QueryRow(ctx, `SELECT count(*) FROM signals`).Scan(&n)
	// 1 issue (bot skipped) + 1 recent star (old one skipped) + hn + so + 1 matching rss item.
	if n != 5 {
		rows, _ := svc.Pool.Query(ctx, `SELECT dedupe_key FROM signals`)
		keys, _ := pgxCollect(rows)
		t.Fatalf("signals: %d %v", n, keys)
	}
	// Re-running finds nothing new.
	if got, _ := svc.RunMonitor(ctx, src, "demo", "pain", now); got != 0 {
		t.Fatalf("rerun produced %d new signals", got)
	}

	// The issue author and the stargazer are the same GitHub user: one person.
	var personID int64
	svc.Pool.QueryRow(ctx, `SELECT person_id FROM identities WHERE kind = 'github' AND value = 'ana-dev'`).Scan(&personID)
	if err := svc.Enrich(ctx, gh, personID, "acmepay/api"); err != nil {
		t.Fatal(err)
	}
	facts, _ := people.Facts(ctx, svc.Pool, personID)
	if facts["dep:bullmq"] == "" || facts["lang:typescript"] == "" || facts["lang:shell"] != "" {
		t.Fatalf("facts: %v", facts)
	}
	ana, _ := people.Get(ctx, svc.Pool, personID)
	if ana.Name != "Ana Ruiz" || ana.Email != "ana@acmepay.example" {
		t.Fatalf("profile not applied: %+v", ana)
	}

	sc, err := score.Recompute(ctx, svc.Pool, "demo", personID, now)
	if err != nil {
		t.Fatal(err)
	}
	// fit 40+30; intent 25·0.5^(3/10) + 10·0.5^(1h) ≈ 20.3+10 = 30; why-now 0.
	wantIntent := int(math.Round(25*math.Pow(0.5, 3.0/10) + 10*math.Pow(0.5, (1.0/24)/10)))
	if sc.Fit != 70 || sc.Intent != wantIntent || sc.WhyNow != 0 || !sc.Eligible {
		t.Fatalf("score: %+v (want intent %d)", sc, wantIntent)
	}
	if want := int(math.Round(0.4*70 + 0.4*float64(wantIntent))); sc.Priority != want || sc.Tier != score.Tier(want) {
		t.Fatalf("priority %d tier %s, want %d", sc.Priority, sc.Tier, want)
	}
	if !strings.Contains(sc.Summary(), "job queue +40") || !strings.Contains(sc.Summary(), "github.issue_pain 3d ago") {
		t.Fatalf("breakdown: %s", sc.Summary())
	}

	// An exclude rule and suppression both make the person ineligible.
	people.SetFacts(ctx, svc.Pool, personID, []string{"dep:react-native"}, "manual")
	if sc, _ := score.Recompute(ctx, svc.Pool, "demo", personID, now); sc.Eligible || sc.Breakdown.Excluded != "mobile app" {
		t.Fatalf("exclude: %+v", sc)
	}
}

func pgxCollect(rows interface {
	Next() bool
	Scan(...any) error
	Close()
}) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out, nil
}

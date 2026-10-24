package signal_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/counters"
	"github.com/AstrolabeGTM/astrolabe/internal/digest"
	"github.com/AstrolabeGTM/astrolabe/internal/github"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/reception"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
)

// fakeRelease serves a repo with one release asset whose count the test sets.
func fakeRelease(t *testing.T, downloads *atomic.Int64, stars *atomic.Int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	j := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("/repos/me/tool", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]any{"stargazers_count": stars.Load(), "forks_count": 3, "subscribers_count": 5, "open_issues_count": 7})
	})
	mux.HandleFunc("/repos/me/tool/releases", func(w http.ResponseWriter, r *http.Request) {
		j(w, []map[string]any{
			{"tag_name": "v1.0.0", "assets": []map[string]any{
				{"name": "tool-linux.tgz", "download_count": downloads.Load()},
				{"name": "tool-mac.tgz", "download_count": 10},
			}},
			{"tag_name": "v0.9.0-draft", "draft": true, "assets": []map[string]any{{"name": "x", "download_count": 999}}},
		})
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("graphql without token")
		}
		j(w, map[string]any{"data": map[string]any{"repository": map[string]any{"discussions": map[string]any{"totalCount": 12}}}})
	})
	return httptest.NewServer(mux)
}

func addMonitors(t *testing.T, svc *signal.Service, p *product.Product, ms ...*product.Monitor) {
	t.Helper()
	p.Monitors = ms
	p.Hash = "counters-" + ms[0].ID
	if err := product.Sync(context.Background(), svc.Pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
}

func TestCounterSnapshotsAreIdempotentAndWeeklyChangesShown(t *testing.T) {
	svc, p := setup(t)
	ctx := context.Background()
	var downloads, stars atomic.Int64
	srv := fakeRelease(t, &downloads, &stars)
	defer srv.Close()
	src := &signal.Sources{GitHub: &github.Client{Base: srv.URL, Token: "tok"}}
	addMonitors(t, svc, p, &product.Monitor{ID: "rel", Type: "github_release", Repo: "me/tool", Every: "24h"})

	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	run := func(at time.Time, dl, st int64) {
		t.Helper()
		downloads.Store(dl)
		stars.Store(st)
		if n, err := svc.RunMonitor(ctx, src, "demo", "rel", at); err != nil || n != 0 {
			t.Fatalf("run: %d %v", n, err)
		}
	}
	run(now.AddDate(0, 0, -14), 100, 20)
	run(now.AddDate(0, 0, -7), 150, 25)
	run(now, 190, 31)
	// Rerunning the same day replaces that day's value; nothing is added twice.
	var rows int
	svc.Pool.QueryRow(ctx, `SELECT count(*) FROM counter_snapshots`).Scan(&rows)
	run(now, 195, 31)
	run(now.Add(time.Hour), 195, 31)
	var rows2 int
	svc.Pool.QueryRow(ctx, `SELECT count(*) FROM counter_snapshots`).Scan(&rows2)
	if rows != rows2 {
		t.Fatalf("rerun added rows: %d -> %d", rows, rows2)
	}
	var v int64
	svc.Pool.QueryRow(ctx, `SELECT value FROM counter_snapshots WHERE day = '2026-10-24' AND asset = 'tool-linux.tgz'`).Scan(&v)
	if v != 195 {
		t.Fatalf("day's value %d, want 195", v)
	}

	rep, err := counters.Weekly(ctx, svc.Pool, "demo", now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Current.From != "2026-10-17" || rep.Current.To != "2026-10-24" || rep.Prev.From != "2026-10-10" {
		t.Fatalf("windows: %+v %+v", rep.Current, rep.Prev)
	}
	r := rep.Repos[0]
	// linux asset: 195-150 = 45 this week, 150-100 = 50 before; mac asset is flat.
	if r.Downloads.Total != 205 || *r.Downloads.Week != 45 || *r.Downloads.PrevWeek != 50 {
		t.Fatalf("downloads: %+v", r.Downloads)
	}
	if len(r.Assets) != 2 || r.Assets[0].Asset != "tool-linux.tgz" || *r.Assets[0].Week != 45 || *r.Assets[1].Week != 0 {
		t.Fatalf("assets: %+v", r.Assets)
	}
	if r.Stars.Total != 31 || *r.Stars.Week != 6 || *r.Stars.PrevWeek != 5 || r.Discussions == nil || r.Discussions.Total != 12 {
		t.Fatalf("repo numbers: stars %+v discussions %+v", r.Stars, r.Discussions)
	}
	if r.Forks.Total != 3 || r.Watchers.Total != 5 || r.OpenIssues.Total != 7 {
		t.Fatalf("forks/watchers/issues: %+v %+v %+v", r.Forks, r.Watchers, r.OpenIssues)
	}

	// A counter monitor makes no signals and no people.
	var sigs, ppl int
	svc.Pool.QueryRow(ctx, `SELECT count(*) FROM signals`).Scan(&sigs)
	svc.Pool.QueryRow(ctx, `SELECT count(*) FROM people`).Scan(&ppl)
	if sigs != 0 || ppl != 0 {
		t.Fatalf("counter monitor made %d signals, %d people", sigs, ppl)
	}

	// The weekly digest shows the changes and says what a download is.
	d, err := digest.Build(ctx, svc.Pool, []*product.Product{p}, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	text := d.Text()
	for _, want := range []string{"Public counters, last 7 days 2026-10-17 to 2026-10-24", "release downloads 205 (7d +45, prior 7d +50)", "stars 31 (7d +6", "not installs or people"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest lacks %q:\n%s", want, text)
		}
	}
}

func TestCounterWithoutTokenSkipsDiscussionsAndSaysSo(t *testing.T) {
	svc, p := setup(t)
	ctx := context.Background()
	var downloads, stars atomic.Int64
	srv := fakeRelease(t, &downloads, &stars)
	defer srv.Close()
	src := &signal.Sources{GitHub: &github.Client{Base: srv.URL}}
	addMonitors(t, svc, p, &product.Monitor{ID: "rel", Type: "github_release", Repo: "me/tool", Every: "24h"})
	now := time.Now()
	if _, err := svc.RunMonitor(ctx, src, "demo", "rel", now); err != nil {
		t.Fatal(err)
	}
	var msg string
	var okAt *time.Time
	svc.Pool.QueryRow(ctx, `SELECT last_error, last_ok_at FROM monitor_runs WHERE monitor_id = 'rel'`).Scan(&msg, &okAt)
	if !strings.Contains(msg, "discussions not recorded") || okAt == nil {
		t.Fatalf("run note %q ok=%v", msg, okAt)
	}
	rep, _ := counters.Weekly(ctx, svc.Pool, "demo", now)
	if rep.Repos[0].Discussions != nil || rep.Repos[0].Stars == nil || !strings.Contains(strings.Join(rep.Notes, "\n"), "discussions are not recorded") {
		t.Fatalf("report: %+v %v", rep.Repos[0], rep.Notes)
	}
}

func TestReceptionMonitorsFileMentionsOnce(t *testing.T) {
	svc, p := setup(t)
	ctx := context.Background()
	now := time.Now()
	srv := fakeAPIs(t, now)
	defer srv.Close()
	src := &signal.Sources{GitHub: &github.Client{Base: srv.URL}, HNBase: srv.URL + "/hn", StackBase: srv.URL + "/so"}
	addMonitors(t, svc, p, &product.Monitor{ID: "talk", Type: "community", Keywords: []string{"idempotency"}, Sources: []string{"hn", "stackoverflow", "rss"},
		Feeds: []string{srv.URL + "/feed.xml"}, Signal: "community.thread", Strength: 5, Every: "1h", Reception: true})
	for i := 0; i < 2; i++ {
		if _, err := svc.RunMonitor(ctx, src, "demo", "talk", now); err != nil {
			t.Fatal(err)
		}
	}
	// Normal signal behaviour is unchanged.
	var sigs int
	svc.Pool.QueryRow(ctx, `SELECT count(*) FROM signals`).Scan(&sigs)
	if sigs != 3 {
		t.Fatalf("signals: %d", sigs)
	}
	rep, err := reception.Week(ctx, svc.Pool, "demo", now.Add(time.Minute), 50)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 3 || rep.ByTone["unknown"] != 3 {
		t.Fatalf("mentions after two runs: %+v", rep)
	}
	// The same page logged by hand is stored once, and keeps the first record.
	id, created, err := reception.Add(ctx, svc.Pool, reception.Mention{Product: "demo", URL: "https://stackoverflow.com/q/77/", Tone: "positive"})
	if err != nil || created {
		t.Fatalf("duplicate: id %d created %v err %v", id, created, err)
	}
	if err := reception.SetTone(ctx, svc.Pool, id, "mixed"); err != nil {
		t.Fatal(err)
	}
	// A new one by hand, dated in the window, positive and asking a question.
	if _, created, err := reception.Add(ctx, svc.Pool, reception.Mention{Product: "demo", URL: "https://blog.example/review", Source: "manual",
		Title: "Does it retry?", Tone: "positive", AsksQuestion: true, MentionedAt: now.AddDate(0, 0, -2)}); err != nil || !created {
		t.Fatalf("manual add: %v %v", created, err)
	}
	// Older than the window: stored, not counted this week.
	reception.Add(ctx, svc.Pool, reception.Mention{Product: "demo", URL: "https://blog.example/old", MentionedAt: now.AddDate(0, 0, -10)})
	rep, _ = reception.Week(ctx, svc.Pool, "demo", now.Add(time.Minute), 50)
	if rep.Total != 4 || rep.ByTone["mixed"] != 1 || rep.ByTone["positive"] != 1 || rep.ByTone["unknown"] != 2 || rep.Questions != 1 {
		t.Fatalf("week: %+v", rep)
	}
	if err := reception.SetTone(ctx, svc.Pool, id, "angry"); err == nil {
		t.Fatal("bad tone accepted")
	}
	d, err := digest.Build(ctx, svc.Pool, []*product.Product{p}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if text := d.Text(); !strings.Contains(text, "Reception, last 7 days") || !strings.Contains(text, "4 mentions — 1 positive, 1 mixed, 0 negative, 2 not yet rated; 1 ask a question") {
		t.Fatalf("digest:\n%s", text)
	}
}

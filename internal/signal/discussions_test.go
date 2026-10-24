package signal_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/github"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
)

func fakeDiscussions(t *testing.T, now time.Time) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/me/tool", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"stargazers_count": 1})
	})
	mux.HandleFunc("/repos/me/tool/stargazers", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{"starred_at": now.Add(-time.Hour), "user": map[string]any{"login": "star-fan"}}})
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("graphql without token")
		}
		d := func(n int, title, login, typ string, age time.Duration) map[string]any {
			var author any
			if login != "" {
				author = map[string]any{"login": login, "__typename": typ}
			}
			return map[string]any{"number": n, "title": title, "url": "https://github.com/me/tool/discussions/" + string(rune('0'+n)),
				"createdAt": now.Add(-age), "author": author, "category": map[string]any{"name": "Q&A"}}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"discussions": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false},
			"nodes": []any{
				d(5, "How do I retry a webhook?", "ana-dev", "User", time.Hour),
				d(4, "bot post", "helper", "Bot", 2*time.Hour),
				d(3, "from the maintainer", "me", "User", 3*time.Hour),
				d(2, "deleted account", "", "", 4*time.Hour),
				d(1, "too old", "old-timer", "User", 30*24*time.Hour),
			}}}}})
	})
	return httptest.NewServer(mux)
}

func TestDiscussionSignals(t *testing.T) {
	svc, p := setup(t)
	ctx := context.Background()
	now := time.Now()
	srv := fakeDiscussions(t, now)
	defer srv.Close()
	addMonitors(t, svc, p, &product.Monitor{ID: "repo", Type: "github_repo", Repo: "me/tool",
		Events: []string{"discussion", "star"}, Signal: "github.repo", Strength: 10, Every: "1h"})

	// Without a token the discussion event reports a clear error; stars still run.
	src := &signal.Sources{GitHub: &github.Client{Base: srv.URL}}
	n, err := svc.RunMonitor(ctx, src, "demo", "repo", now)
	if err != nil || n != 1 {
		t.Fatalf("tokenless run: %d %v", n, err)
	}
	var lastErr string
	svc.Pool.QueryRow(ctx, `SELECT last_error FROM monitor_runs WHERE monitor_id = 'repo'`).Scan(&lastErr)
	if !strings.Contains(lastErr, "discussion") || !strings.Contains(lastErr, "ASTROLABE_GITHUB_TOKEN") {
		t.Fatalf("error should name the event and the token: %q", lastErr)
	}

	// With a token: one signal for the human, non-owner author created since the last run.
	src = &signal.Sources{GitHub: &github.Client{Base: srv.URL, Token: "tok"}}
	if n, err = svc.RunMonitor(ctx, src, "demo", "repo", now); err != nil || n != 1 {
		t.Fatalf("run: %d %v", n, err)
	}
	var typ, key, url string
	var person int64
	svc.Pool.QueryRow(ctx, `SELECT type, dedupe_key, evidence_url FROM signals WHERE type = 'github.repo.discussion'`).Scan(&typ, &key, &url)
	if key != "gh-discussion:me/tool#5" || !strings.Contains(url, "/discussions/5") {
		t.Fatalf("signal: %s %s %s", typ, key, url)
	}
	svc.Pool.QueryRow(ctx, `SELECT person_id FROM identities WHERE kind = 'github' AND value = 'ana-dev'`).Scan(&person)
	if person == 0 {
		t.Fatal("the author should be a person identified by GitHub login")
	}
	var total int
	svc.Pool.QueryRow(ctx, `SELECT count(*) FROM signals WHERE type = 'github.repo.discussion'`).Scan(&total)
	if total != 1 {
		t.Fatalf("discussion signals: %d", total)
	}
	// Re-running adds nothing.
	if n, _ = svc.RunMonitor(ctx, src, "demo", "repo", now); n != 0 {
		t.Fatalf("rerun produced %d", n)
	}
}

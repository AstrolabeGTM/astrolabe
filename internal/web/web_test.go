package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/content"
	"github.com/AstrolabeGTM/astrolabe/internal/gmail"
	"github.com/AstrolabeGTM/astrolabe/internal/llm"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// TestEveryPageRenders logs in and loads each page with some data, so a
// template error fails the tests instead of the server.
func TestEveryPageRenders(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	product.Sync(ctx, pool, []*product.Product{p})
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	o := &outreach.Service{Pool: pool, River: rc}
	sig := &signal.Service{Pool: pool, River: rc}
	people.Import(ctx, pool, "demo", []people.Row{{Email: "ana@acme.example", Name: "Ana"}})
	sig.Ingest(ctx, &signal.Signal{Product: "demo", Type: "manual.note", OccurredAt: time.Now(), Strength: 20, DedupeKey: "x",
		Subject: signal.Subject{Email: "ana@acme.example"}, Title: "said hi"})
	o.Enroll(ctx, "demo", "intro", []int64{1})
	o.Tick(ctx)

	srv := &Server{S: o, Signals: sig, Accounts: &gmail.Accounts{Dir: t.TempDir()}, Password: "test-password-1",
		Products: func() []*product.Product { return []*product.Product{p} }, Reload: func(context.Context) error { return nil },
		Studio: &content.Studio{Pool: pool, LLM: &llm.Client{}, Out: o}, Mode: "sandbox"}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	jar := &cookieJar{}
	client := &http.Client{Jar: jar}
	if _, err := client.PostForm(ts.URL+"/login", url.Values{"password": {"test-password-1"}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/people?product=demo", "/people/1", "/automations", "/signals", "/funnel?product=demo", "/settings", "/content", "/digest", "/digest?preview", "/outbox"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		buf := make([]byte, 1<<16)
		for {
			n, err := resp.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(b.String(), "</html>") {
			t.Errorf("%s: %d, complete=%v", path, resp.StatusCode, strings.Contains(b.String(), "</html>"))
		}
	}
	// Webhook-free pages require login.
	resp, _ := http.Get(ts.URL + "/people")
	if resp.Request.URL.Path != "/login" {
		t.Errorf("unauthenticated request reached %s", resp.Request.URL.Path)
	}
}

type cookieJar struct{ cookies []*http.Cookie }

func (j *cookieJar) SetCookies(_ *url.URL, cs []*http.Cookie) { j.cookies = append(j.cookies, cs...) }
func (j *cookieJar) Cookies(*url.URL) []*http.Cookie          { return j.cookies }

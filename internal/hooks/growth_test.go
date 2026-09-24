package hooks_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/astrolabe-gtm/astrolabe/internal/content"
	"github.com/astrolabe-gtm/astrolabe/internal/digest"
	"github.com/astrolabe-gtm/astrolabe/internal/experiment"
	"github.com/astrolabe-gtm/astrolabe/internal/hooks"
	"github.com/astrolabe-gtm/astrolabe/internal/llm"
	"github.com/astrolabe-gtm/astrolabe/internal/outreach"
	"github.com/astrolabe-gtm/astrolabe/internal/people"
	"github.com/astrolabe-gtm/astrolabe/internal/product"
	"github.com/astrolabe-gtm/astrolabe/internal/signal"
	"github.com/astrolabe-gtm/astrolabe/internal/store"
)

func TestLinksVariantsAndAttribution(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	os.WriteFile(filepath.Join(p.Dir, "product.yaml"), []byte(strings.Replace(string(mustRead(t, filepath.Join(p.Dir, "product.yaml"))),
		"funnel: [reached, replied, installed, activated, paid]", "funnel: [reached, replied, site_visit, installed, activated, paid]", 1)), 0o644)
	os.WriteFile(filepath.Join(p.Dir, "sequences", "intro.yaml"), []byte(`steps:
  - channel: email
    after: 0d
    goal: intro
    subject: "Hi {{.FirstName}}"
    variants:
      - {id: short, body: "Short. {{.Link}}"}
      - {id: story, body: "A story. {{.Link}}"}
stop_on: [replied]
`), 0o644)
	p, err := product.Load(p.Dir)
	if err != nil {
		t.Fatal(err)
	}
	product.Sync(ctx, pool, []*product.Product{p})
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	out := &outreach.Service{Pool: pool, River: rc, PublicURL: "https://go.example"}
	sig := &signal.Service{Pool: pool, River: rc}

	// Two coworkers (b2b: company-level assignment) and one at another company.
	people.Import(ctx, pool, "demo", []people.Row{{Email: "a@acme.example"}, {Email: "b@acme.example"}, {Email: "c@other.example"}})
	out.Enroll(ctx, "demo", "intro", []int64{1, 2, 3})
	out.Tick(ctx)
	drafts, _ := out.Actions(ctx, "demo", "draft")
	variant := map[string]string{}
	var linkURL string
	for _, d := range drafts {
		variant[d.Recipient] = d.Variant
		if d.LinkCode == nil || !strings.Contains(d.Body, "https://go.example/l/"+*d.LinkCode) {
			t.Fatalf("draft without tracked link: %+v", d)
		}
		if d.Recipient == "a@acme.example" {
			linkURL = "/l/" + *d.LinkCode
		}
	}
	if variant["a@acme.example"] == "" || variant["a@acme.example"] != variant["b@acme.example"] {
		t.Fatalf("coworkers must share a variant: %v", variant)
	}

	// Clicking the link redirects with UTM + ref and records a signal and site_visit.
	rd := &hooks.Redirector{Signals: sig, Out: out}
	mux := http.NewServeMux()
	mux.Handle("GET /l/{code}", rd)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", srv.URL+linkURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	loc := resp.Header.Get("Location")
	if resp.StatusCode != 302 || !strings.HasPrefix(loc, "https://demo.example/start?") || !strings.Contains(loc, "utm_source=astrolabe") || !strings.Contains(loc, "ref=") {
		t.Fatalf("redirect: %d %s", resp.StatusCode, loc)
	}
	code := strings.TrimPrefix(linkURL, "/l/")
	if err := rd.Record(ctx, code, "demo", "sequence:intro", ptr(int64(1)), "", "Mozilla", time.Now()); err != nil {
		t.Fatal(err)
	}
	var visits int
	pool.QueryRow(ctx, `SELECT count(*) FROM funnel_events WHERE stage = 'site_visit' AND person_id = 1`).Scan(&visits)
	if visits != 1 {
		t.Fatal("click did not record site_visit")
	}

	// A new signup arriving with ref=<code> is credited to the link.
	h := &hooks.Handler{Signals: sig, Out: out, Secret: func(string, string) string { return "s" }}
	if err := hEvent(h, hooks.Event{ID: "s1", Type: "installed", UserID: "new1", Email: "new@else.example", Data: map[string]any{"ref": code}}); err != nil {
		t.Fatal(err)
	}
	var src string
	pool.QueryRow(ctx, `SELECT pp.source FROM product_people pp JOIN identities i ON i.person_id = pp.person_id WHERE i.value = 'new@else.example'`).Scan(&src)
	if src != "link:sequence:intro" {
		t.Fatalf("signup source: %q", src)
	}
}

func TestExperimentVerdicts(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	product.Sync(ctx, pool, []*product.Product{p})
	// 40 sends per variant: a converts 20, b converts 4 (synthetic people).
	for i := range 80 {
		var pid int64
		pool.QueryRow(ctx, `INSERT INTO people (name) VALUES ('p') RETURNING id`).Scan(&pid)
		var eid int64
		pool.QueryRow(ctx, `INSERT INTO enrollments (product_id, person_id, sequence_id, status) VALUES ('demo', $1, 'intro', 'completed') RETURNING id`, pid).Scan(&eid)
		v := "a"
		if i >= 40 {
			v = "b"
		}
		pool.Exec(ctx, `INSERT INTO actions (enrollment_id, step, product_id, person_id, channel, status, variant, sent_at) VALUES ($1, 0, 'demo', $2, 'email', 'sent', $3, now() - interval '1 day')`, eid, pid, v)
		if (v == "a" && i < 20) || (v == "b" && i < 44) {
			if _, err := pool.Exec(ctx, `INSERT INTO funnel_events (product_id, person_id, stage, occurred_at, source, dedupe_key) VALUES ('demo', $1, 'activated', now(), 't', $2)`,
				pid, fmt.Sprint("k", pid)); err != nil {
				t.Fatal(err)
			}
		}
	}
	res, err := experiment.Report(ctx, pool, p)
	if err != nil || len(res) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if !strings.HasPrefix(res[0].Verdict, "a wins") {
		t.Fatalf("verdict: %s", res[0].Verdict)
	}
	a := res[0].Variants[0]
	if math.Abs(a.Rate-0.5) > 1e-9 || a.Low < 0.34 || a.Low > 0.36 || a.High < 0.64 || a.High > 0.66 {
		t.Fatalf("wilson interval for 20/40: %+v", a) // ≈ 35.2%–64.8%
	}
}

func TestContentAndDigest(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	product.Sync(ctx, pool, []*product.Product{p})
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	out := &outreach.Service{Pool: pool, River: rc}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"content": []map[string]string{{"type": "text",
			"text": `{"items":[{"platform":"x","text":"v0.4 ships replays {{LINK}}"},{"platform":"hn","text":"Show HN: demo\n\nDetails {{LINK}}"}]}`}},
			"usage": map[string]int{"input_tokens": 1, "output_tokens": 1}, "stop_reason": "end_turn"})
	}))
	defer fake.Close()
	st := &content.Studio{Pool: pool, Out: out, PublicURL: "https://go.example", LLM: &llm.Client{APIKey: "k", Base: fake.URL, Pool: pool}}
	ids, err := st.Repurpose(ctx, "demo", "v0.4", "notes", []string{"x", "hn"}, true)
	if err != nil || len(ids) != 2 {
		t.Fatalf("%v %v", err, ids)
	}
	items, _ := st.List(ctx, "demo", 10)
	for _, it := range items {
		if !strings.Contains(it.Body, "https://go.example/l/") || it.TaskID == nil {
			t.Fatalf("content item: %+v", it)
		}
	}
	if err := st.Mark(ctx, ids[0], "posted", "https://x.com/me/1"); err != nil {
		t.Fatal(err)
	}
	tasks, _ := out.Tasks(ctx, true, 10)
	if len(tasks) != 1 {
		t.Fatalf("posting should close its task: %d open", len(tasks))
	}

	// Digest: an A-tier person nobody contacted becomes action #1.
	people.Import(ctx, pool, "demo", []people.Row{{Email: "hot@acme.example", Name: "Hot Lead"}})
	pool.Exec(ctx, `INSERT INTO scores (product_id, person_id, fit, intent, why_now, priority, tier, eligible, breakdown, computed_at)
		VALUES ('demo', 1, 90, 90, 50, 82, 'A', true, '{}', now())`)
	d, err := digest.Build(ctx, pool, []*product.Product{p}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Products[0].Actions) == 0 || !strings.Contains(d.Products[0].Actions[0], "Hot Lead") {
		t.Fatalf("digest actions: %v\n%s", d.Products[0].Actions, d.Text())
	}
	if isNew, _ := digest.Save(ctx, pool, d, ""); !isNew {
		t.Fatal("first save")
	}
	if isNew, _ := digest.Save(ctx, pool, d, ""); isNew {
		t.Fatal("digest saved twice for one week")
	}
}

func ptr[T any](v T) *T { return &v }

package forms_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/forms"
	"github.com/AstrolabeGTM/astrolabe/internal/funnel"
	"github.com/AstrolabeGTM/astrolabe/internal/hooks"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/play"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

type env struct {
	t     *testing.T
	ctx   context.Context
	svc   *signal.Service
	eng   *play.Engine
	p     *product.Product
	now   time.Time
	calls *[]string
}

// netlify is a fake Netlify API with one form, "request-demo", holding
// two verified submissions, one spam, and one older than any first run.
func netlify(t *testing.T, now time.Time, calls *[]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls = append(*calls, r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("Authorization") != "Bearer nf-token" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/sites/pipewrench.example/forms":
			json.NewEncoder(w).Encode([]map[string]string{{"id": "f0", "name": "contact"}, {"id": "f1", "name": "request-demo"}})
		case "/forms/f1/submissions":
			if r.URL.Query().Get("state") == "spam" {
				t.Error("asked Netlify for spam")
			}
			at := func(h int) string { return now.Add(-time.Duration(h) * time.Hour).Format(time.RFC3339) }
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": "s3", "created_at": at(1), "data": map[string]any{"email": "carol@acme.example", "name": "Carol Ng", "company": "Acme",
					"How did you hear about us?": "x", "how_did_you_hear_about_us": "A friend", "message": "Also us"}},
				{"id": "s2", "created_at": at(2), "data": map[string]any{"email": "bo@gmail.com", "name": "Bo Park", "company": "Bo Labs",
					"how_did_you_hear_about_us": "Hacker News", "campaign": "hn-launch", "ip": "1.2.3.4"}},
				{"id": "s1", "created_at": at(3), "data": map[string]any{"email": "Ana@Acme.example", "name": "Ana Lee", "company": "Acme",
					"role": "Staff engineer", "how_did_you_hear_about_us": "hacker news", "message": "Our CI flakes\nevery day"}},
				{"id": "spam1", "state": "spam", "created_at": at(2), "data": map[string]any{"email": "buy@pills.example"}},
				{"id": "old", "created_at": now.AddDate(0, 0, -30).Format(time.RFC3339), "data": map[string]any{"email": "old@old.example"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	p.Pipeline = &product.Pipeline{Stages: []string{"identified", "contacted", "qualified"}}
	p.Monitors = []*product.Monitor{
		{ID: "request-demo", Type: "netlify_forms", Site: "pipewrench.example", Form: "request-demo", Signal: "form.request",
			Strength: 40, WhyNow: true, Every: "1h", Account: true},
		{ID: "updates", Type: "form_webhook", Signal: "form.updates", Strength: 5, Every: "1h"},
	}
	p.Plays = []*product.Play{{ID: "form-request-reply", Trigger: product.Trigger{Signal: "form.request"},
		Action: product.Action{Task: "Reply to the request"}}}
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	now := time.Now().UTC().Truncate(time.Second)
	e := &env{t: t, ctx: ctx, p: p, now: now, calls: new([]string)}
	nf := netlify(t, now, e.calls)
	e.svc = &signal.Service{Pool: pool, River: rc}
	e.svc.Forms = &forms.Intake{Pool: pool, Signals: e.svc, Netlify: &forms.Netlify{Token: "nf-token", Base: nf.URL}, Now: func() time.Time { return now }}
	out := &outreach.Service{Pool: pool, River: rc, Now: func() time.Time { return now }}
	e.eng = &play.Engine{Pool: pool, Out: out, Now: func() time.Time { return now }}
	return e
}

func (e *env) count(sql string, args ...any) (n int) {
	e.t.Helper()
	if err := e.svc.Pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) poll() int {
	e.t.Helper()
	n, err := e.svc.RunMonitor(e.ctx, &signal.Sources{}, "demo", "request-demo", e.now)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestNetlifySubmissionsBecomePersonAccountSignalAndTaskOnce(t *testing.T) {
	e := newEnv(t)
	if n := e.poll(); n != 3 {
		t.Fatalf("new submissions: %d, want 3 (spam and old skipped)", n)
	}
	// Running again, and replaying the automation, changes nothing.
	if n := e.poll(); n != 0 {
		t.Fatalf("second poll made %d", n)
	}
	rows, _ := e.svc.Pool.Query(e.ctx, `SELECT id FROM signals WHERE type = 'form.request' ORDER BY id`)
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for i := 0; i < 2; i++ {
		for _, id := range ids {
			if err := e.eng.OnSignal(e.ctx, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	for what, want := range map[string]int{
		"form_submissions": 3, "signals": 3, "people": 3, "tasks": 3,
		"accounts": 2, // Acme (Ana, then Carol) and Bo Labs
	} {
		if got := e.count(`SELECT count(*) FROM ` + what); got != want {
			t.Errorf("%s: %d, want %d", what, got, want)
		}
	}
	if got := e.count(`SELECT count(*) FROM tasks WHERE next_action LIKE 'Reply to the request%'`); got != 3 {
		t.Errorf("reply tasks: %d", got)
	}

	// Ana's address is one she gave us, and her request is stored as written.
	var src string
	var personID int64
	if err := e.svc.Pool.QueryRow(e.ctx, `SELECT source, person_id FROM identities WHERE kind = 'email' AND value = 'ana@acme.example'`).Scan(&src, &personID); err != nil || src != "form" {
		t.Fatalf("email identity source %q, %v", src, err)
	}
	var reported, first, title string
	e.svc.Pool.QueryRow(e.ctx, `SELECT reported_source, source FROM product_people WHERE person_id = $1`, personID).Scan(&reported, &first)
	e.svc.Pool.QueryRow(e.ctx, `SELECT title FROM people WHERE id = $1`, personID).Scan(&title)
	if reported != "hacker news" || first != "monitor:request-demo" || title != "Staff engineer" {
		t.Errorf("reported %q first touch %q title %q", reported, first, title)
	}

	// The account is in the first stage with the reply due in a day; Carol
	// joined it without moving it.
	var stage, next string
	var due time.Time
	var people int
	err := e.svc.Pool.QueryRow(e.ctx, `
		SELECT a.stage, a.next_action, a.next_due, (SELECT count(*) FROM account_people WHERE account_id = a.id)
		FROM accounts a JOIN companies c ON c.id = a.company_id WHERE c.domain = 'acme.example'`).Scan(&stage, &next, &due, &people)
	if err != nil || stage != "identified" || next != "Reply to the request" || people != 2 ||
		due.Format("2006-01-02") != e.now.Add(24*time.Hour).Format("2006-01-02") {
		t.Fatalf("acme account: %s %q %v %d %v", stage, next, due, people, err)
	}

	// Bo used a free-mail address but named a company, so an account by name.
	if e.count(`SELECT count(*) FROM accounts a JOIN companies c ON c.id = a.company_id WHERE c.name = 'Bo Labs' AND c.domain IS NULL`) != 1 {
		t.Error("no account for Bo Labs")
	}
	if e.count(`SELECT count(*) FROM companies WHERE domain = 'gmail.com'`) != 0 {
		t.Error("free-mail domain became a company")
	}

	// The signal carries every field, the campaign, and the reported source; no IP.
	var data map[string]any
	e.svc.Pool.QueryRow(e.ctx, `SELECT data FROM signals s JOIN identities i ON i.person_id = s.person_id WHERE i.value = 'bo@gmail.com'`).Scan(&data)
	if data["campaign"] != "hn-launch" || data["reported_source"] != "Hacker News" || data["company"] != "Bo Labs" || data["ip"] != nil {
		t.Errorf("signal data %v", data)
	}

	// Requests by what people said, with denominators.
	r, err := funnel.BuildReport(e.ctx, e.svc.Pool, e.p, e.now.AddDate(0, 0, -7), e.now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Requests.Total != 3 || len(r.Requests.Sources) != 2 || r.Requests.Sources[0].Source != "hacker news" ||
		r.Requests.Sources[0].Requests != 2 || r.Requests.Sources[0].Percent != 66 {
		t.Errorf("requests by source: %+v", r.Requests)
	}
}

func TestNetlifyErrorsAreReported(t *testing.T) {
	e := newEnv(t)
	e.svc.Forms.(*forms.Intake).Netlify.Token = ""
	if _, err := e.svc.RunMonitor(e.ctx, &signal.Sources{}, "demo", "request-demo", e.now); err == nil {
		t.Fatal("ran without a token")
	}
	e.svc.Forms.(*forms.Intake).Netlify.Token = "nf-token"
	e.p.Monitors[0].Form = "nope"
	e.p.Hash += "x"
	product.Sync(e.ctx, e.svc.Pool, []*product.Product{e.p})
	if _, err := e.svc.RunMonitor(e.ctx, &signal.Sources{}, "demo", "request-demo", e.now); err == nil {
		t.Fatal("unknown form accepted")
	}
	if _, err := e.svc.RunMonitor(e.ctx, &signal.Sources{}, "demo", "updates", e.now); err == nil {
		t.Fatal("a webhook monitor was polled")
	}
}

func TestWebhookFormIsSignedAndRetriesAreHarmless(t *testing.T) {
	e := newEnv(t)
	pool := e.svc.Pool
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	h := &hooks.Handler{Signals: e.svc, Out: &outreach.Service{Pool: pool, River: rc}, Forms: e.svc.Forms.(*forms.Intake),
		Secret: func(kind, product string) string {
			if kind == "forms" {
				return "forms-secret"
			}
			return ""
		}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/forms/{product}", h.FormPost)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	post := func(path, secret, body string) int {
		req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewBufferString(body))
		req.Header.Set("Astrolabe-Signature", hooks.Sign([]byte(body), secret, time.Now()))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	body := `{"email":"dee@tiny.example","name":"Dee","utm_campaign":"newsletter","how_did_you_hear_about_us":"A podcast"}`
	if code := post("/hooks/forms/demo?form=updates", "wrong", body); code != 401 {
		t.Fatalf("bad signature: %d", code)
	}
	if e.count(`SELECT count(*) FROM form_submissions`) != 0 || e.count(`SELECT count(*) FROM people`) != 0 {
		t.Fatal("a refused post left data behind")
	}
	// Two form_webhook monitors would need ?form=; here only "updates" is one.
	for i := 0; i < 2; i++ {
		if code := post("/hooks/forms/demo", "forms-secret", body); code != 204 {
			t.Fatalf("post: %d", code)
		}
	}
	if code := post("/hooks/forms/demo", "forms-secret", `[1]`); code != 400 {
		t.Fatalf("non-object body: %d", code)
	}
	if code := post("/hooks/forms/demo?form=zzz", "forms-secret", body); code != 404 {
		t.Fatalf("unknown form: %d", code)
	}
	if e.count(`SELECT count(*) FROM form_submissions`) != 1 || e.count(`SELECT count(*) FROM signals WHERE type = 'form.updates'`) != 1 {
		t.Fatal("a retried post was taken twice")
	}
	var src, campaign string
	e.svc.Pool.QueryRow(e.ctx, `SELECT i.source, s.data->>'campaign' FROM signals s JOIN identities i ON i.person_id = s.person_id AND i.kind = 'email' WHERE s.type = 'form.updates'`).Scan(&src, &campaign)
	if src != "form" || campaign != "newsletter" {
		t.Errorf("identity %q campaign %q", src, campaign)
	}
	// No pipeline account: this monitor does not ask for one.
	if e.count(`SELECT count(*) FROM accounts`) != 0 {
		t.Error("account opened")
	}
}

func TestFieldMappingAndDefaults(t *testing.T) {
	m := forms.Map(map[string]any{"Work Email": "a@b.example", "first_name": "Ana", "last_name": "Lee",
		"How did you hear about us?": "HN", "Heard": "x", "tags": []any{"a", "b"}}, map[string]string{"source": "Heard"})
	if m.Email != "a@b.example" || m.Name != "Ana Lee" || m.Source != "x" {
		t.Errorf("%+v", m)
	}
	m = forms.Map(map[string]any{"How did you hear about us?": "HN"}, nil)
	if m.Source != "HN" {
		t.Errorf("default source name: %+v", m)
	}
}

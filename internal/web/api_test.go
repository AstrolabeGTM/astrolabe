package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/content"
	"github.com/AstrolabeGTM/astrolabe/internal/gmail"
	"github.com/AstrolabeGTM/astrolabe/internal/llm"
	"github.com/AstrolabeGTM/astrolabe/internal/mcp"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

const testPassword = "test-password-1"

// apiServer starts the web server on a fresh database with the test product.
func apiServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	o := &outreach.Service{Pool: pool, River: rc}
	srv := &Server{S: o, Signals: &signal.Service{Pool: pool, River: rc}, Accounts: &gmail.Accounts{Dir: t.TempDir()}, Password: testPassword,
		Products: func() []*product.Product { return []*product.Product{p} }, Reload: func(context.Context) error { return nil },
		Studio: &content.Studio{Pool: pool, LLM: &llm.Client{}, Out: o}, Mode: "sandbox"}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

// signIn returns a client holding a session cookie (no redirects followed).
func signIn(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	jar := &cookieJar{}
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.PostForm(ts.URL+"/login", url.Values{"password": {testPassword}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(jar.cookies) == 0 {
		t.Fatal("no session cookie after signing in")
	}
	return c
}

func do(t *testing.T, c *http.Client, method, u string, body string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func TestAPIAuth(t *testing.T) {
	ts, srv := apiServer(t)

	// No session: 401 with a JSON error, never a redirect.
	code, body := do(t, http.DefaultClient, "GET", ts.URL+"/api/bootstrap", "", nil)
	if code != http.StatusUnauthorized || body["error"] == nil {
		t.Fatalf("anonymous bootstrap: %d %v", code, body)
	}
	code, _ = do(t, http.DefaultClient, "POST", ts.URL+"/api/pause", `{"scope":"global"}`, map[string]string{"X-CSRF-Token": "x"})
	if code != http.StatusUnauthorized {
		t.Fatalf("anonymous write: %d", code)
	}

	c := signIn(t, ts)
	code, boot := do(t, c, "GET", ts.URL+"/api/bootstrap", "", nil)
	token, _ := boot["csrf_token"].(string)
	if code != http.StatusOK || token == "" || boot["mode"] != "sandbox" {
		t.Fatalf("bootstrap: %d %v", code, boot)
	}

	write := `{"scope":"global"}`
	same := map[string]string{"X-CSRF-Token": token, "Origin": ts.URL, "Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"}
	for name, h := range map[string]map[string]string{
		"no token":       {"Origin": ts.URL},
		"wrong token":    {"X-CSRF-Token": token + "0", "Origin": ts.URL},
		"foreign origin": {"X-CSRF-Token": token, "Origin": "https://evil.example"},
		"cross-site":     {"X-CSRF-Token": token, "Sec-Fetch-Site": "cross-site"},
		"same-site":      {"X-CSRF-Token": token, "Sec-Fetch-Site": "same-site"},
	} {
		if code, body := do(t, c, "POST", ts.URL+"/api/pause", write, h); code != http.StatusForbidden || body["error"] == nil {
			t.Errorf("%s: %d %v", name, code, body)
		}
	}
	code, body = do(t, c, "POST", ts.URL+"/api/pause", write, same)
	if code != http.StatusOK {
		t.Fatalf("valid write: %d %v", code, body)
	}
	if _, boot = do(t, c, "GET", ts.URL+"/api/bootstrap", "", nil); len(boot["pauses"].([]any)) != 1 {
		t.Fatalf("the pause did not happen: %v", boot)
	}
	// Another session's token does not work.
	c2 := &http.Client{Jar: &cookieJar{cookies: []*http.Cookie{{Name: cookieName, Value: srv.sign(time.Now().Add(time.Hour).Unix())}}}}
	if code, _ := do(t, c2, "POST", ts.URL+"/api/resume", `{"scope":"global"}`, same); code != http.StatusForbidden {
		t.Fatalf("token from another session accepted: %d", code)
	}
	if code, _ := do(t, c, "GET", ts.URL+"/api/nope", "", nil); code != http.StatusNotFound {
		t.Fatalf("unknown endpoint: %d", code)
	}
}

func TestAppNeedsLogin(t *testing.T) {
	ts, _ := apiServer(t)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(ts.URL + "/app/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != "/login?next=%2Fapp%2F" {
		t.Fatalf("anonymous /app/: %d %q", resp.StatusCode, loc)
	}
	// The mark is public: the sign-in page shows it.
	if resp, _ := noFollow.Get(ts.URL + "/app/mark.svg"); resp.StatusCode != http.StatusOK {
		t.Fatalf("mark: %d", resp.StatusCode)
	}

	// Signing in returns to where you were going, but only on this site.
	for next, want := range map[string]string{"/app/#people": "/app/#people", "//evil.example/": "/", "https://evil.example": "/", "/\\evil": "/"} {
		jar := &cookieJar{}
		c := &http.Client{Jar: jar, CheckRedirect: noFollow.CheckRedirect}
		resp, err := c.PostForm(ts.URL+"/login", url.Values{"password": {testPassword}, "next": {next}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); loc != want {
			t.Errorf("next=%q redirected to %q, want %q", next, loc, want)
		}
	}

	c := signIn(t, ts)
	for _, path := range []string{"/app/", "/app/app.css", "/app/js/main.js", "/app/js/app.js", "/app/js/lib.js"} {
		resp, err := c.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || len(b) == 0 {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%s: the app's CSP allows inline code: %s", path, csp)
		}
	}
	if resp, _ := c.Get(ts.URL + "/app/js/"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("directory listing: %d", resp.StatusCode)
	}
}

// TestEveryAPIRouteIsAnMCPTool keeps the browser app and MCP equal: each
// API route names the MCP tool that does the same, and that tool exists.
func TestEveryAPIRouteIsAnMCPTool(t *testing.T) {
	ctx := context.Background()
	m := &mcp.Server{Mode: "sandbox", Products: func() []*product.Product { return nil }}
	st, ct := sdk.NewInMemoryTransports()
	if _, err := m.Build("test").Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, tl := range tools.Tools {
		have[tl.Name] = true
	}
	seen := map[string]bool{}
	for _, rt := range (&Server{}).apiRoutes() {
		if !strings.HasPrefix(rt.Pattern, "GET /api/") && !strings.HasPrefix(rt.Pattern, "POST /api/") {
			t.Errorf("%s: API routes are GET or POST under /api/", rt.Pattern)
		}
		if seen[rt.Pattern] {
			t.Errorf("%s is listed twice", rt.Pattern)
		}
		seen[rt.Pattern] = true
		if rt.Tool == "" {
			t.Errorf("%s has no MCP tool", rt.Pattern)
		} else if !have[rt.Tool] {
			t.Errorf("%s maps to MCP tool %q, which is not registered", rt.Pattern, rt.Tool)
		}
	}
}

// session is a signed-in client with its CSRF token.
type session struct {
	t     *testing.T
	c     *http.Client
	base  string
	token string
}

func newSession(t *testing.T, ts *httptest.Server) *session {
	c := signIn(t, ts)
	_, boot := do(t, c, "GET", ts.URL+"/api/bootstrap", "", nil)
	return &session{t, c, ts.URL, boot["csrf_token"].(string)}
}

func (s *session) get(path string) map[string]any {
	s.t.Helper()
	code, body := do(s.t, s.c, "GET", s.base+path, "", nil)
	if code != http.StatusOK {
		s.t.Fatalf("GET %s: %d %v", path, code, body)
	}
	return body
}

func (s *session) post(path string, v any) (int, map[string]any) {
	s.t.Helper()
	b, _ := json.Marshal(v)
	return do(s.t, s.c, "POST", s.base+path, string(b), map[string]string{"X-CSRF-Token": s.token, "Origin": s.base, "Content-Type": "application/json"})
}

func (s *session) mustPost(path string, v any) map[string]any {
	s.t.Helper()
	code, body := s.post(path, v)
	if code != http.StatusOK {
		s.t.Fatalf("POST %s: %d %v", path, code, body)
	}
	return body
}

func count(today map[string]any, kind string) float64 {
	return today["counts"].(map[string]any)[kind].(map[string]any)["total"].(float64)
}

func TestTodayAPI(t *testing.T) {
	ts, srv := apiServer(t)
	ctx := context.Background()
	if _, err := people.Import(ctx, srv.S.Pool, "demo", []people.Row{{Email: "ana@acme.example", Name: "Ana"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.S.Enroll(ctx, "demo", "intro", []int64{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.S.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	s := newSession(t, ts)
	today := s.get("/api/today")
	if count(today, "approval") != 1 {
		t.Fatalf("today: %v", today)
	}
	item := today["items"].([]any)[0].(map[string]any)
	a := item["action"].(map[string]any)
	id := fmt.Sprint(a["id"])
	if item["kind"] != "approval" || a["content_hash"] == "" || a["sender"] == nil || item["target_seconds"].(float64) != 86400 {
		t.Fatalf("approval item: %v", item)
	}

	// A draft that changed since it was shown is not approved.
	if code, body := s.post("/api/actions/"+id+"/approve", map[string]string{"content_hash": "stale"}); code != http.StatusConflict {
		t.Fatalf("stale approve: %d %v", code, body)
	}
	if code, body := s.post("/api/actions/"+id+"/edit", map[string]any{"body": "Results guaranteed.", "approve": true}); code != http.StatusBadRequest ||
		!strings.Contains(body["error"].(string), "never allows") {
		t.Fatalf("a never-claim was approved: %d %v", code, body)
	}
	s.mustPost("/api/actions/"+id+"/edit", map[string]any{"body": "Hi Ana, want to try it on one repo?", "approve": true})
	today = s.get("/api/today")
	if count(today, "approval") != 0 || count(today, "queued") != 1 {
		t.Fatalf("after approving: %v", today["counts"])
	}

	// A reply from someone we never wrote to can still be labelled.
	if _, err := srv.S.Pool.Exec(ctx, `INSERT INTO replies (provider_message_id, mailbox, from_addr, subject, body, received_at)
		VALUES ('m1', 'demo@x.example', 'bo@else.example', 'Hi', 'Hello?', now() - interval '3 hours')`); err != nil {
		t.Fatal(err)
	}
	today = s.get("/api/today")
	first := today["items"].([]any)[0].(map[string]any)
	if first["kind"] != "reply" || first["state"] != "past" {
		t.Fatalf("a reply 3 hours old should be first and past its 2-hour target: %v", first)
	}
	if code, _ := s.post("/api/replies/1/label", map[string]string{"class": "maybe"}); code != http.StatusBadRequest {
		t.Fatalf("unknown label accepted: %d", code)
	}
	s.mustPost("/api/replies/1/label", map[string]string{"class": "question"})

	task := s.mustPost("/api/tasks", map[string]any{"product": "demo", "next_action": "Send Ana the guide", "person_id": 1})
	if count(s.get("/api/today"), "task") != 1 {
		t.Fatal("the task is not due today")
	}
	if code, _ := s.post(fmt.Sprintf("/api/tasks/%v/close", task["task_id"]), map[string]any{"outcome": ""}); code != http.StatusBadRequest {
		t.Fatalf("closing without an outcome: %d", code)
	}
	s.mustPost(fmt.Sprintf("/api/tasks/%v/close", task["task_id"]), map[string]any{"outcome": "later", "minutes": 5})
	today = s.get("/api/today")
	if count(today, "task") != 0 || count(today, "reply") != 0 {
		t.Fatalf("after closing: %v", today["counts"])
	}
	if code, _ := s.post("/api/actions/999/skip", nil); code != http.StatusBadRequest {
		t.Fatalf("skipping a missing action: %d", code)
	}
}

func TestPeopleAPI(t *testing.T) {
	ts, srv := apiServer(t)
	ctx := context.Background()
	s := newSession(t, ts)
	imp := s.mustPost("/api/people/import", map[string]string{"product": "demo", "csv": "email,name,facts\nana@acme.example,Ana,team:5-50\nnot-an-email,Bo,\n"})
	if imp["created"].(float64) != 1 || len(imp["skipped"].([]any)) != 1 {
		t.Fatalf("import: %v", imp)
	}
	if code, _ := s.post("/api/people/import", map[string]string{"product": "nope", "csv": "email\na@b.example\n"}); code != http.StatusBadRequest {
		t.Fatalf("import into an unknown product: %d", code)
	}
	// Someone known only from GitHub: listed as not emailable, refused with the reason.
	var gh int64
	if err := srv.S.Pool.QueryRow(ctx, `INSERT INTO people (name, first_name) VALUES ('octo', 'octo') RETURNING id`).Scan(&gh); err != nil {
		t.Fatal(err)
	}
	if err := people.LinkIdentity(ctx, srv.S.Pool, "github", "octo", gh, people.SourceGitHub, false); err != nil {
		t.Fatal(err)
	}
	srv.S.Pool.Exec(ctx, `INSERT INTO product_people (product_id, person_id) VALUES ('demo', $1)`, gh)

	list := s.get("/api/people?product=demo")
	if list["total"].(float64) != 2 {
		t.Fatalf("people: %v", list)
	}
	refused := map[string]string{}
	for _, p := range list["people"].([]any) {
		m := p.(map[string]any)
		r, _ := m["email_refused"].(string)
		refused[m["name"].(string)] = r
	}
	if refused["Ana"] != "" || !strings.Contains(refused["octo"], "Acceptable Use Policies") {
		t.Fatalf("email guard in the list: %v", refused)
	}
	res := s.mustPost("/api/sequences/start", map[string]any{"product": "demo", "sequence": "intro", "person_ids": []int64{1, gh}})
	if res["started"].(float64) != 1 || len(res["skipped"].([]any)) != 1 || !strings.Contains(res["skipped"].([]any)[0].(string), "Acceptable Use Policies") {
		t.Fatalf("start: %v", res)
	}

	p := s.get(fmt.Sprintf("/api/people/%d", gh))
	pol := p["email_policy"].([]any)[0].(map[string]any)
	if pol["may_email"] != false || !strings.Contains(pol["reason"].(string), "GitHub") {
		t.Fatalf("person email policy: %v", pol)
	}
	if ids := p["identities"].([]any); len(ids) != 1 || ids[0].(map[string]any)["source"] != "github" {
		t.Fatalf("identities: %v", ids)
	}
	if code, _ := s.post(fmt.Sprintf("/api/people/%d/facts", gh), map[string]string{"facts": "nocolon"}); code != http.StatusBadRequest {
		t.Fatalf("a fact without a colon: %d", code)
	}
	facts := s.mustPost(fmt.Sprintf("/api/people/%d/facts", gh), map[string]string{"facts": "dep:bullmq, team:5-50"})
	if len(facts["facts"].(map[string]any)) != 2 {
		t.Fatalf("facts: %v", facts)
	}
	s.mustPost("/api/people/1/do-not-contact", map[string]any{"reason": "asked"})
	if p := s.get("/api/people/1"); p["do_not_contact"] == nil || len(p["messages"].([]any)) != 1 {
		t.Fatalf("after do-not-contact: %v", p)
	}
	s.mustPost("/api/people/1/do-not-contact", map[string]any{"allow": true})
	if p := s.get("/api/people/1"); p["do_not_contact"] != nil {
		t.Fatal("still on the do-not-contact list")
	}
	if code, _ := s.post("/api/people/1/research", map[string]string{"product": "demo"}); code != http.StatusBadRequest {
		t.Fatalf("research without an API key: %d", code)
	}
	if code, _ := do(t, s.c, "GET", ts.URL+"/api/people/999", "", nil); code != http.StatusBadRequest {
		t.Fatalf("missing person: %d", code)
	}
	comp := s.mustPost("/api/companies/import", map[string]string{"product": "demo", "csv": "domain,company,facts\nacme.example,Acme,team:5-50\n"})
	if c := comp["result"].(map[string]any)["companies"].([]any)[0].(map[string]any); c["fit"] == nil || c["domain"] != "acme.example" {
		t.Fatalf("company import: %v", comp)
	}
}

func TestSignalsAPI(t *testing.T) {
	ts, _ := apiServer(t)
	s := newSession(t, ts)
	if code, body := s.post("/api/signals", map[string]any{"product": "demo", "who": "", "type": "referral", "strength": 30}); code != http.StatusBadRequest {
		t.Fatalf("a signal about nobody: %d %v", code, body)
	}
	if code, _ := s.post("/api/signals", map[string]any{"product": "demo", "who": "ana@acme.example", "type": "referral", "strength": 300}); code != http.StatusBadRequest {
		t.Fatalf("strength over 100: %d", code)
	}
	res := s.mustPost("/api/signals", map[string]any{"product": "demo", "who": "Ana@Acme.example", "type": "referral", "strength": 30, "note": "Bo introduced us"})
	if res["person_id"] == nil {
		t.Fatalf("add signal: %v", res)
	}
	feed := s.get("/api/signals?days=7")
	if feed["total"].(float64) != 1 {
		t.Fatalf("feed: %v", feed)
	}
	sig := feed["signals"].([]any)[0].(map[string]any)
	if sig["type"] != "manual.referral" || sig["person_id"] == nil || sig["tier"] == "" {
		t.Fatalf("the signal should be about a scored person: %v", sig)
	}
	if types := feed["types"].([]any); len(types) != 1 {
		t.Fatalf("types: %v", types)
	}
	if f := s.get("/api/signals?type=github.star"); f["total"].(float64) != 0 {
		t.Fatalf("type filter: %v", f)
	}
	if r := s.get("/api/reviews"); r["total"].(float64) != 0 {
		t.Fatalf("reviews: %v", r)
	}
	s.get("/api/monitors")
	if code, _ := s.post("/api/monitors/run", map[string]string{"product": "demo", "monitor": "nope"}); code != http.StatusBadRequest {
		t.Fatalf("running a missing monitor: %d", code)
	}
}

func TestReceptionAPI(t *testing.T) {
	ts, _ := apiServer(t)
	s := newSession(t, ts)
	if code, _ := s.post("/api/mentions", map[string]any{"product": "demo", "url": "javascript:alert(1)"}); code != http.StatusBadRequest {
		t.Fatalf("a non-http mention: %d", code)
	}
	m := s.mustPost("/api/mentions", map[string]any{"product": "demo", "url": "https://news.example/item/1#top", "title": "Does it work with Jest?", "source": "hn", "asks_question": true})
	if m["created"] != true {
		t.Fatalf("log mention: %v", m)
	}
	if again := s.mustPost("/api/mentions", map[string]any{"product": "demo", "url": "https://news.example/item/1/"}); again["created"] != false {
		t.Fatalf("the same link was stored twice: %v", again)
	}
	if code, _ := s.post(fmt.Sprintf("/api/mentions/%v/tone", m["id"]), map[string]string{"tone": "great"}); code != http.StatusBadRequest {
		t.Fatalf("an unknown tone: %d", code)
	}
	s.mustPost(fmt.Sprintf("/api/mentions/%v/tone", m["id"]), map[string]string{"tone": "positive"})
	rep := s.get("/api/reception?product=demo&days=30")["reception"].([]any)[0].(map[string]any)
	if rep["total"].(float64) != 1 || rep["days"].(float64) != 30 || rep["by_tone"].(map[string]any)["positive"].(float64) != 1 || rep["asking_a_question"].(float64) != 1 {
		t.Fatalf("reception: %v", rep)
	}
	c := s.get("/api/counters?product=demo")["counters"].([]any)
	if len(c) != 1 || !strings.Contains(fmt.Sprint(c[0].(map[string]any)["notes"]), "not installs or people") {
		t.Fatalf("counters: %v", c)
	}
	if all := s.get("/api/counters")["counters"].([]any); len(all) != 0 {
		t.Fatalf("products without a counter monitor are left out of all: %v", all)
	}
}

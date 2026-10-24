package web

import (
	"context"
	"encoding/json"
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

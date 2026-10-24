package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

const testToken = "test-token-0123456789abcdef"

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestHTTPTransport(t *testing.T) {
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
	s := &Server{S: &outreach.Service{Pool: pool, River: rc}, Products: func() []*product.Product { return []*product.Product{p} }}
	ts := httptest.NewServer(s.HTTPHandler("test", func() string { return testToken }))
	defer ts.Close()

	connect := func(token string) (*sdk.ClientSession, error) {
		tr := &sdk.StreamableClientTransport{Endpoint: ts.URL, HTTPClient: &http.Client{Transport: bearerTransport{token}}}
		return sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, tr, nil)
	}

	cs, err := connect(testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) < 15 {
		t.Fatalf("tools: %v %v", err, tools)
	}
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "products"})
	if err != nil || res.IsError {
		t.Fatalf("products: %v %+v", err, res)
	}
	if b, _ := res.StructuredContent.(map[string]any); b == nil && len(res.Content) == 0 {
		t.Fatalf("empty products result: %+v", res)
	}

	// Same tool set as stdio.
	st, ct := sdk.NewInMemoryTransports()
	if _, err := s.Build("test").Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	ms, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	mt, err := ms.ListTools(ctx, nil)
	if err != nil || len(mt.Tools) != len(tools.Tools) {
		t.Fatalf("stdio has %d tools, http %d (%v)", len(mt.Tools), len(tools.Tools), err)
	}

	for name, token := range map[string]string{"missing": "", "wrong": "nope-nope-nope-nope-nope"} {
		if _, err := connect(token); err == nil {
			t.Errorf("%s token: connected", name)
		}
	}
}

func TestBearerRejects(t *testing.T) {
	h := RequireBearer(func() string { return testToken }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("in"))
	}))
	for header, want := range map[string]int{
		"":                        401,
		"Bearer wrong":            401,
		"Basic " + testToken:      401,
		testToken:                 401,
		"Bearer " + testToken:     200,
		"bearer " + testToken:     200,
		"Bearer " + testToken[1:]: 401,
	} {
		r := httptest.NewRequest("POST", "/mcp", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Authorization %q: %d, want %d", header, w.Code, want)
		}
		if w.Code == 401 && (strings.Contains(w.Body.String(), testToken) || w.Header().Get("WWW-Authenticate") == "") {
			t.Errorf("401 body/header wrong: %q", w.Body.String())
		}
	}
	// An unset token rejects everything, even an empty bearer.
	h = RequireBearer(func() string { return "" }, http.NotFoundHandler())
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Errorf("empty token accepted: %d", w.Code)
	}
}

func TestBrowserProtections(t *testing.T) {
	s := &Server{}
	ts := httptest.NewServer(s.HTTPHandler("test", func() string { return testToken }))
	defer ts.Close()
	do := func(mod func(*http.Request)) int {
		r, _ := http.NewRequest("POST", ts.URL, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		mod(r)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// ts listens on 127.0.0.1, so a foreign Host header is DNS rebinding.
	if c := do(func(r *http.Request) { r.Host = "evil.example" }); c != 403 {
		t.Errorf("foreign Host on localhost: %d, want 403", c)
	}
	// A browser page on another origin is refused even with the token.
	if c := do(func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); c != 403 {
		t.Errorf("cross-origin request: %d, want 403", c)
	}
}

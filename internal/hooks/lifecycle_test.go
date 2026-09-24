package hooks_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/hooks"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

type recorder struct{ sent []channel.Message }

func (r *recorder) Send(_ context.Context, m channel.Message) (channel.Result, error) {
	r.sent = append(r.sent, m)
	return channel.Result{ProviderID: "wamid." + m.IdempotencyKey}, nil
}

func TestLifecycleWhatsAppWithConsentRepliesAndStop(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	os.WriteFile(filepath.Join(p.Dir, "product.yaml"), append(mustRead(t, filepath.Join(p.Dir, "product.yaml")),
		[]byte("whatsapp: {phone_number_id: \"555\"}\nnotify: {url: \"https://app.example/notify\"}\n")...), 0o644)
	os.WriteFile(filepath.Join(p.Dir, "sequences", "lesson.yaml"), []byte(`kind: users
steps:
  - {channel: whatsapp, after: 0d, goal: first lesson, template: first_lesson, params: ["{{.FirstName}}", "iron condor"]}
  - {channel: push, after: 1d, goal: nudge, subject: "Try it", body: "Your first strategy is one tap away"}
stop_on: [replied, activated]
`), 0o644)
	os.WriteFile(filepath.Join(p.Dir, "sequences", "cold-wa.yaml"), []byte("steps:\n  - {channel: whatsapp, after: 0d, goal: x, template: t}\nstop_on: [replied]\n"), 0o644)
	if _, err := product.Load(p.Dir); err == nil || !strings.Contains(err.Error(), "only reaches existing users") {
		t.Fatalf("cold WhatsApp must be refused: %v", err)
	}
	os.Remove(filepath.Join(p.Dir, "sequences", "cold-wa.yaml"))
	p, err := product.Load(p.Dir)
	if err != nil {
		t.Fatal(err)
	}
	product.Sync(ctx, pool, []*product.Product{p})

	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	rec := &recorder{}
	out := &outreach.Service{Pool: pool, River: rc, Senders: func(channel.Message) (channel.Sender, error) { return rec, nil }}
	sig := &signal.Service{Pool: pool, River: rc}
	h := &hooks.Handler{Signals: sig, Out: out, Secret: func(string, string) string { return "s" }}

	// Two users arrive through product events; only Asha allowed WhatsApp.
	for _, ev := range []hooks.Event{
		{ID: "1", Type: "installed", UserID: "u1", Phone: "+91 98123 45678", Name: "Asha K", Consent: map[string]bool{"whatsapp": true}},
		{ID: "2", Type: "installed", UserID: "u2", Phone: "+91 90000 00000", Name: "Ravi"},
	} {
		if err := hEvent(h, ev); err != nil {
			t.Fatal(err)
		}
	}
	out.Enroll(ctx, "demo", "lesson", []int64{1, 2})
	out.Tick(ctx)
	drafts, _ := out.Actions(ctx, "demo", "draft")
	if len(drafts) != 2 {
		t.Fatalf("drafts: %d", len(drafts))
	}
	for _, d := range drafts {
		if d.Subject != "first_lesson" || !strings.HasSuffix(d.Body, "iron condor") || d.Sender != "whatsapp:555" {
			t.Fatalf("whatsapp draft: %+v", d)
		}
		if err := out.Approve(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		out.SendAction(ctx, d.ID)
	}
	if len(rec.sent) != 1 || rec.sent[0].To != "+919812345678" || rec.sent[0].Language != "en" || rec.sent[0].Kind != "users" {
		t.Fatalf("sent: %+v", rec.sent)
	}
	var blocked string
	pool.QueryRow(ctx, `SELECT error FROM actions WHERE status = 'blocked'`).Scan(&blocked)
	if !strings.Contains(blocked, "no permission") {
		t.Fatalf("Ravi without consent: %q", blocked)
	}

	// Asha replies on WhatsApp: the sequence stops (no push tomorrow).
	srv := httptest.NewServer(http.HandlerFunc(h.WhatsApp))
	defer srv.Close()
	t.Setenv("ASTROLABE_WHATSAPP_APP_SECRET", "app")
	post := func(body string) int {
		req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(body))
		mac := hmac.New(sha256.New, []byte("app"))
		mac.Write([]byte(body))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	msg := func(id, text string) string {
		return `{"entry":[{"changes":[{"field":"messages","value":{"metadata":{"phone_number_id":"555"},
			"messages":[{"from":"919812345678","id":"` + id + `","timestamp":"` + itoa(time.Now().Unix()) + `","type":"text","text":{"body":"` + text + `"}}]}}]}]}`
	}
	if code := post(msg("w1", "Which strategy should I start with?")); code != 200 {
		t.Fatalf("whatsapp webhook: %d", code)
	}
	var status string
	pool.QueryRow(ctx, `SELECT status FROM enrollments WHERE person_id = 1`).Scan(&status)
	if status != "stopped" {
		t.Fatalf("reply did not stop the sequence: %s", status)
	}
	if code := post(msg("w2", "STOP")); code != 200 {
		t.Fatal(code)
	}
	asha, _ := people.Get(ctx, pool, 1)
	if asha.Suppressed != "unsubscribed" {
		t.Fatalf("STOP must unsubscribe: %+v", asha)
	}
	// Forged payloads are refused.
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(msg("w3", "hi")))
	req.Header.Set("X-Hub-Signature-256", "sha256=00")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("forged: %d", resp.StatusCode)
	}
}

func hEvent(h *hooks.Handler, ev hooks.Event) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/events/{product}", h.ProductEvents)
	s2 := httptest.NewServer(mux)
	defer s2.Close()
	b := mustJSON(ev)
	req, _ := http.NewRequest("POST", s2.URL+"/hooks/events/demo", strings.NewReader(b))
	req.Header.Set("Astrolabe-Signature", hooks.Sign([]byte(b), "s", time.Now()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		return &httpErr{resp.StatusCode}
	}
	return nil
}

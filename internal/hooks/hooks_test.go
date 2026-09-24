package hooks_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/astrolabe-gtm/astrolabe/internal/funnel"
	"github.com/astrolabe-gtm/astrolabe/internal/hooks"
	"github.com/astrolabe-gtm/astrolabe/internal/outreach"
	"github.com/astrolabe-gtm/astrolabe/internal/people"
	"github.com/astrolabe-gtm/astrolabe/internal/product"
	"github.com/astrolabe-gtm/astrolabe/internal/signal"
	"github.com/astrolabe-gtm/astrolabe/internal/store"
)

func TestVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"id":"evt_1"}`)
	good := hooks.Sign(body, "whsec_abc", now)
	if err := hooks.Verify(good, body, "whsec_abc", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A rolled secret: two v1 signatures, one matching; v0 is ignored.
	other := hooks.Sign(body, "whsec_old", now)
	multi := other + ",v1=" + strings.Split(good, "v1=")[1] + ",v0=deadbeef"
	if err := hooks.Verify(multi, body, "whsec_abc", now); err != nil {
		t.Fatalf("multi: %v", err)
	}
	for name, c := range map[string]struct {
		header, secret string
		body           []byte
		at             time.Time
	}{
		"wrong secret": {good, "whsec_x", body, now},
		"changed body": {good, "whsec_abc", []byte(`{"id":"evt_2"}`), now},
		"too old":      {good, "whsec_abc", body, now.Add(6 * time.Minute)},
		"only v0":      {"t=1800000000,v0=" + strings.Split(good, "v1=")[1], "whsec_abc", body, now},
		"no secret":    {good, "", body, now},
		"garbage":      {"nonsense", "whsec_abc", body, now},
	} {
		if err := hooks.Verify(c.header, c.body, c.secret, c.at); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type env struct {
	t   *testing.T
	srv *httptest.Server
	out *outreach.Service
	p   *product.Product
	now time.Time
}

func newEnv(t *testing.T) *env {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	product.Sync(ctx, pool, []*product.Product{p})
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	e := &env{t: t, p: p, now: time.Now()}
	e.out = &outreach.Service{Pool: pool, River: rc}
	h := &hooks.Handler{Signals: &signal.Service{Pool: pool, River: rc}, Out: e.out,
		Secret: func(kind, product string) string { return kind + "-secret" }}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/stripe/{product}", h.Stripe)
	mux.HandleFunc("POST /hooks/events/{product}", h.ProductEvents)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) post(path, header, secret, body string) int {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path, bytes.NewBufferString(body))
	req.Header.Set(header, hooks.Sign([]byte(body), secret, time.Now()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (e *env) event(body string) int {
	return e.post("/hooks/events/demo", "Astrolabe-Signature", "events-secret", body)
}

func (e *env) stripe(id, typ string, created time.Time, object string) int {
	return e.post("/hooks/stripe/demo", "Stripe-Signature", "stripe-secret",
		fmt.Sprintf(`{"id":%q,"type":%q,"created":%d,"data":{"object":%s}}`, id, typ, created.Unix(), object))
}

func TestProductEventsJoinOutreachAndStopSequences(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	people.Import(ctx, e.out.Pool, "demo", []people.Row{{Email: "ana@acme.example", Name: "Ana"}})
	e.out.Enroll(ctx, "demo", "intro", []int64{1})
	e.out.Tick(ctx)

	if code := e.post("/hooks/events/demo", "Astrolabe-Signature", "wrong", `{"id":"1","type":"installed","email":"x@y"}`); code != 401 {
		t.Fatalf("bad signature accepted: %d", code)
	}
	body := `{"events":[{"id":"e1","type":"installed","user_id":"u42","email":"ANA@acme.example","consent":{"whatsapp":true}},
		{"id":"e2","type":"opened_dashboard","user_id":"u42"}]}`
	if code := e.event(body); code != 204 {
		t.Fatalf("events: %d", code)
	}
	if code := e.event(body); code != 204 {
		t.Fatalf("duplicate delivery: %d", code)
	}
	var stages, signals, perms int
	e.out.Pool.QueryRow(ctx, `SELECT count(*) FROM funnel_events WHERE stage = 'installed' AND person_id = 1`).Scan(&stages)
	e.out.Pool.QueryRow(ctx, `SELECT count(*) FROM signals WHERE person_id = 1 AND type LIKE 'product.%'`).Scan(&signals)
	e.out.Pool.QueryRow(ctx, `SELECT count(*) FROM channel_permissions WHERE person_id = 1 AND channel = 'whatsapp' AND allowed`).Scan(&perms)
	if stages != 1 || signals != 2 || perms != 1 {
		t.Fatalf("stages %d signals %d perms %d", stages, signals, perms)
	}
	// The signup matched the person I emailed, and "installed" is in intro's stop_on.
	var status, reason string
	e.out.Pool.QueryRow(ctx, `SELECT status, stop_reason FROM enrollments`).Scan(&status, &reason)
	if status != "stopped" || reason != "installed" {
		t.Fatalf("enrollment: %s %s", status, reason)
	}
	if code := e.event(`{"id":"e3","type":"installed"}`); code != 400 {
		t.Fatalf("event without a user: %d", code)
	}
}

func TestStripePaymentsRefundsAndReport(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()
	people.Import(ctx, e.out.Pool, "demo", []people.Row{{Email: "ana@acme.example"}})
	e.out.MarkStage(ctx, "demo", 1, "reached", now.Add(-10*24*time.Hour))
	e.out.MarkStage(ctx, "demo", 1, "activated", now.Add(-5*24*time.Hour))

	inv := `{"id":"in_1","customer":"cus_1","customer_email":"ana@acme.example","amount_paid":4900,"currency":"usd"}`
	for range 2 {
		if code := e.stripe("evt_1", "invoice.paid", now, inv); code != 200 {
			t.Fatalf("invoice.paid: %d", code)
		}
	}
	// Two partial refunds on one charge, reported cumulatively.
	e.stripe("evt_2", "charge.refunded", now, `{"id":"ch_1","customer":"cus_1","amount_refunded":1000,"currency":"usd"}`)
	e.stripe("evt_3", "charge.refunded", now, `{"id":"ch_1","customer":"cus_1","amount_refunded":1500,"currency":"usd"}`)
	// Subscription events carry only the customer id.
	e.stripe("evt_4", "invoice.payment_failed", now, `{"id":"in_2","customer":"cus_1","currency":"usd"}`)
	e.stripe("evt_5", "some.unhandled", now, `{"id":"x"}`)

	var n int
	var net int64
	e.out.Pool.QueryRow(ctx, `SELECT count(*), sum(amount_cents) FROM payments`).Scan(&n, &net)
	if n != 3 || net != 4900-1500 {
		t.Fatalf("payments %d net %d", n, net)
	}
	var failedFor *int64
	e.out.Pool.QueryRow(ctx, `SELECT person_id FROM signals WHERE type = 'stripe.payment_failed'`).Scan(&failedFor)
	if failedFor == nil || *failedFor != 1 {
		t.Fatal("customer-id-only event not matched to the person")
	}

	rep, err := funnel.BuildReport(ctx, e.out.Pool, e.p, now.AddDate(0, 0, -90), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Revenue) != 1 || rep.Revenue[0].Net != 3400 || rep.Revenue[0].Refunds != 1500 {
		t.Fatalf("revenue: %+v", rep.Revenue)
	}
	if rep.Stages[len(rep.Stages)-1].People != 1 || rep.Spend.Paying != 1 {
		t.Fatalf("paid stage: %+v", rep.Stages)
	}
	if len(rep.Sources) != 1 || rep.Sources[0].Key != "import" || rep.Sources[0].Paid != 1 || rep.Sources[0].Revenue["usd"] != 3400 {
		t.Fatalf("sources: %+v", rep.Sources)
	}
	if rep.Times[len(rep.Times)-1].People != 1 { // activated → paid
		t.Fatalf("times: %+v", rep.Times)
	}
	if len(rep.Cohorts) != 1 || rep.Cohorts[0].Complete {
		t.Fatalf("a 10-day-old cohort with a 14d window must be incomplete: %+v", rep.Cohorts)
	}
	if rep.Spend.MinPerActive != nil {
		t.Fatal("no minutes recorded, yet a per-activation ratio was shown")
	}
}

func TestStuckList(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()
	people.Import(ctx, e.out.Pool, "demo", []people.Row{{Email: "a@x.example", Name: "A"}, {Email: "b@x.example", Name: "B"}})
	e.out.MarkStage(ctx, "demo", 1, "installed", now.Add(-10*24*time.Hour))
	e.out.MarkStage(ctx, "demo", 2, "installed", now.Add(-2*24*time.Hour))
	rep, err := funnel.BuildReport(ctx, e.out.Pool, e.p, now.AddDate(0, 0, -90), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range rep.Stuck {
		if s.Stage == "installed" && (s.Count != 1 || s.People[0].Name != "A") {
			t.Fatalf("stuck: %+v", s)
		}
	}
}

type httpErr struct{ code int }

func (e *httpErr) Error() string { return fmt.Sprintf("status %d", e.code) }

func mustRead(t *testing.T, path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

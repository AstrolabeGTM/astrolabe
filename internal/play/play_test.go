package play_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/astrolabe-gtm/astrolabe/internal/draft"
	"github.com/astrolabe-gtm/astrolabe/internal/llm"
	"github.com/astrolabe-gtm/astrolabe/internal/outreach"
	"github.com/astrolabe-gtm/astrolabe/internal/people"
	"github.com/astrolabe-gtm/astrolabe/internal/play"
	"github.com/astrolabe-gtm/astrolabe/internal/product"
	"github.com/astrolabe-gtm/astrolabe/internal/signal"
	"github.com/astrolabe-gtm/astrolabe/internal/store"
)

type env struct {
	t    *testing.T
	p    *product.Product
	out  *outreach.Service
	eng  *play.Engine
	sig  *signal.Service
	now  time.Time
	llm  *atomic.Value // reply text the fake Claude returns
	w    *draft.Writer
	sync func()
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	pool := store.TestDB(t)
	e := &env{t: t, p: product.TestProduct(t), now: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC), llm: &atomic.Value{}}
	e.p.Fit = []product.FitRule{{Any: []string{"dep:bullmq"}, Weight: 60, Label: "queue"}}
	e.p.Plays = []*product.Play{
		{ID: "pain", Trigger: product.Trigger{Signal: "github.issue_pain"}, When: product.When{MinFit: 50, NotContactedWithin: "30d"},
			Delay: "2d", Action: product.Action{Sequence: "intro"}, LimitPerDay: 2},
		{ID: "stalled", Trigger: product.Trigger{StageStuck: &product.StageStuck{Stage: "installed", For: "3d"}}, Action: product.Action{Sequence: "nudge"}},
		{ID: "thread", Trigger: product.Trigger{Signal: "community.thread"}, Action: product.Action{Task: "Reply personally", Draft: true}},
	}
	e.sync = func() {
		e.p.Hash += "x"
		if err := product.Sync(ctx, pool, []*product.Product{e.p}); err != nil {
			t.Fatal(err)
		}
	}
	e.sync()
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	nowf := func() time.Time { return e.now }
	e.out = &outreach.Service{Pool: pool, River: rc, Now: nowf}
	e.eng = &play.Engine{Pool: pool, Out: e.out, Now: nowf}
	e.out.Recheck = e.eng.Recheck
	e.sig = &signal.Service{Pool: pool, River: rc}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			http.Error(w, "bad auth", 401)
			return
		}
		text, _ := e.llm.Load().(string)
		json.NewEncoder(w).Encode(map[string]any{"content": []map[string]string{{"type": "text", "text": text}},
			"usage": map[string]int{"input_tokens": 100, "output_tokens": 50}, "stop_reason": "end_turn"})
	}))
	t.Cleanup(srv.Close)
	e.w = &draft.Writer{Pool: pool, Out: e.out, LLM: &llm.Client{APIKey: "k", Base: srv.URL, Pool: pool, Writer: "w", Fast: "f"}}
	e.out.OnDraftCreated = func(ctx context.Context, tx pgx.Tx, id int64) (bool, error) { return true, nil }
	return e
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) signal(key, typ string, sub signal.Subject) int64 {
	e.t.Helper()
	r, err := e.sig.Ingest(context.Background(), &signal.Signal{Product: "demo", Type: typ, OccurredAt: e.now, Strength: 25,
		Subject: sub, DedupeKey: key, Title: "Webhook processed twice", EvidenceURL: "https://github.com/acme/api/issues/812",
		Data: map[string]any{"repo": "acme/api"}})
	e.must(err)
	return r.SignalID
}

func (e *env) runs() map[string]string {
	rows, _ := e.out.Pool.Query(context.Background(), `SELECT trigger_key, outcome || ' ' || detail FROM play_runs`)
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		rows.Scan(&k, &v)
		out[k] = strings.TrimSpace(v)
	}
	return out
}

func TestSignalPlayFiltersDelaysAndRechecks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	people.Import(ctx, e.out.Pool, "demo", []people.Row{
		{Email: "fit@acme.example", GitHub: "fit", Facts: "dep:bullmq"},
		{Email: "unfit@acme.example", GitHub: "unfit"},
		{Email: "fit2@acme.example", GitHub: "fit2", Facts: "dep:bullmq"},
		{Email: "fit3@acme.example", GitHub: "fit3", Facts: "dep:bullmq"},
	})
	for _, who := range []string{"fit", "unfit", "fit2", "fit3"} {
		id := e.signal("s-"+who, "github.issue_pain", signal.Subject{GitHub: who})
		e.must(e.eng.OnSignal(ctx, id))
		e.must(e.eng.OnSignal(ctx, id)) // at most once per trigger
	}
	runs := e.runs()
	if !strings.HasPrefix(runs["signal:1"], "enrolled") || !strings.Contains(runs["signal:2"], "fit 0 < 50") ||
		!strings.HasPrefix(runs["signal:3"], "enrolled") || !strings.Contains(runs["signal:4"], "daily limit") {
		t.Fatalf("runs: %v", runs)
	}

	// Delay: nothing is drafted for 2 days.
	if n, _ := e.out.Tick(ctx); n != 0 {
		t.Fatal("drafted before the play's delay")
	}
	// Person 1 installs meanwhile (goal of intro is its stop_on "installed");
	// person 3 reaches the goal stage (offer success event "activated").
	fit, _ := people.FindByEmail(ctx, e.out.Pool, "fit@acme.example")
	fit2, _ := people.FindByEmail(ctx, e.out.Pool, "fit2@acme.example")
	e.must(e.out.MarkStage(ctx, "demo", fit.ID, "installed", e.now))
	e.must(e.out.MarkStage(ctx, "demo", fit2.ID, "activated", e.now))
	e.now = e.now.Add(49 * time.Hour)
	if n, _ := e.out.Tick(ctx); n != 0 {
		t.Fatalf("drafted %d steps for people who already converted", n)
	}
	var reasons []string
	rows, _ := e.out.Pool.Query(ctx, `SELECT stop_reason FROM enrollments ORDER BY id`)
	for rows.Next() {
		var r string
		rows.Scan(&r)
		reasons = append(reasons, r)
	}
	if strings.Join(reasons, ",") != "installed,activated" {
		t.Fatalf("stop reasons: %v", reasons)
	}
}

func TestStuckPlayFiresOncePerStallAndStopsOnProgress(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	people.Import(ctx, e.out.Pool, "demo", []people.Row{{Email: "u@acme.example"}})
	u, _ := people.FindByEmail(ctx, e.out.Pool, "u@acme.example")
	e.must(e.out.MarkStage(ctx, "demo", u.ID, "installed", e.now))
	e.must(e.eng.Scan(ctx, e.p))
	if len(e.runs()) != 0 {
		t.Fatal("fired before the stall period")
	}
	e.now = e.now.Add(4 * 24 * time.Hour)
	e.must(e.eng.Scan(ctx, e.p))
	e.must(e.eng.Scan(ctx, e.p))
	if len(e.runs()) != 1 {
		t.Fatalf("runs: %v", e.runs())
	}
	// Drafted now; then the user activates before approval: the next step is cancelled.
	if n, _ := e.out.Tick(ctx); n != 1 {
		t.Fatal("stalled play did not draft")
	}
	e.must(e.out.MarkStage(ctx, "demo", u.ID, "activated", e.now))
	var status string
	e.out.Pool.QueryRow(ctx, `SELECT status FROM actions`).Scan(&status)
	if status != "skipped" {
		t.Fatalf("pending nudge after activation: %s", status)
	}
}

func TestTaskPlayAndAIDrafts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// A community thread with an unknown author still becomes a task with a draft.
	id := e.signal("hn-1", "community.thread", signal.Subject{})
	e.must(e.eng.OnSignal(ctx, id))
	tasks, _ := e.out.Tasks(ctx, true, 10)
	if len(tasks) != 1 || !strings.Contains(tasks[0].NextAction, "Reply personally: Webhook processed twice") {
		t.Fatalf("tasks: %+v", tasks)
	}
	e.llm.Store("Happy to help: use an idempotency key per event.")
	e.must(e.w.DraftTask(ctx, tasks[0].ID))
	tasks, _ = e.out.Tasks(ctx, true, 10)
	if tasks[0].Draft == "" {
		t.Fatal("task draft missing")
	}
	e.must(e.out.CloseTask(ctx, tasks[0].ID, "other", "posted", nil))

	// A play enrollment gets an AI-written first email that cites the signal.
	e.p.Sequences["intro"].Steps[0].Body = ""
	e.sync()
	people.Import(ctx, e.out.Pool, "demo", []people.Row{{Email: "fit@acme.example", GitHub: "fit", Name: "Ana Ruiz", Facts: "dep:bullmq"}})
	sid := e.signal("s1", "github.issue_pain", signal.Subject{GitHub: "fit"})
	e.must(e.eng.OnSignal(ctx, sid))
	e.now = e.now.Add(49 * time.Hour)
	e.out.Tick(ctx)
	drafts, _ := e.out.Actions(ctx, "demo", "draft")
	if len(drafts) != 1 || drafts[0].AIState != "pending" {
		t.Fatalf("drafts: %+v", drafts)
	}
	a := drafts[0]
	// The step has a subject template, so only the body is asked for.
	e.llm.Store("Hi Ana, saw acme/api#812 about webhooks processed twice. Want me to test one workflow? Naval")
	e.must(e.w.DraftAction(ctx, a.ID))
	got, _ := e.out.Action(ctx, a.ID)
	if got.AIState != "done" || !strings.Contains(got.Body, "#812") || len(got.Flags) != 0 || got.Status != "draft" {
		t.Fatalf("AI draft: %+v", got)
	}

	// My edit is kept as a voice example.
	e.must(e.out.Edit(ctx, a.ID, got.Subject, "Ana, acme/api#812 looks like a retry race. Worth a 10-minute test?"))
	var examples int
	e.out.Pool.QueryRow(ctx, `SELECT count(*) FROM voice_examples`).Scan(&examples)
	if examples != 1 {
		t.Fatal("edit not recorded as a voice example")
	}
	var calls int
	e.out.Pool.QueryRow(ctx, `SELECT count(*) FROM llm_calls WHERE input_tokens = 100`).Scan(&calls)
	if calls != 2 {
		t.Fatalf("llm calls logged: %d", calls)
	}
}

func TestAutoApproveAndFlags(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.p.AutoApprove.Cold = "all"
	e.sync()
	people.Import(ctx, e.out.Pool, "demo", []people.Row{{Email: "a@acme.example"}, {Email: "b@acme.example"}, {Email: "c@acme.example"}})
	e.out.Enroll(ctx, "demo", "intro", []int64{1, 2, 3})
	e.out.Tick(ctx)
	drafts, _ := e.out.Actions(ctx, "demo", "draft")
	// Step templates filled these; mark them as pending AI drafts for the test.
	for _, d := range drafts {
		e.out.Pool.Exec(ctx, `UPDATE actions SET body = '', ai_state = 'pending' WHERE id = $1`, d.ID)
	}

	e.llm.Store("Short and clean. Naval")
	e.must(e.w.DraftAction(ctx, drafts[0].ID))
	e.llm.Store("Results are guaranteed. Naval")
	e.must(e.w.DraftAction(ctx, drafts[1].ID))
	// I edit the third while Claude is writing: my text wins.
	e.must(e.out.Edit(ctx, drafts[2].ID, drafts[2].Subject, "My own words"))
	e.llm.Store("Claude's words")
	e.must(e.w.DraftAction(ctx, drafts[2].ID))

	st := func(id int64) *outreach.Action { a, _ := e.out.Action(ctx, id); return a }
	if st(drafts[0].ID).Status != "approved" {
		t.Fatalf("auto_approve all did not approve a clean draft: %+v", st(drafts[0].ID))
	}
	if a := st(drafts[1].ID); a.Status != "draft" || len(a.Flags) == 0 {
		t.Fatalf("flagged draft must wait for me: %+v", a)
	}
	if a := st(drafts[2].ID); a.Body != "My own words" {
		t.Fatalf("AI overwrote my edit: %q", a.Body)
	}
}

func TestReplyClassificationAndAnswer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.out.Senders = nil
	people.Import(ctx, e.out.Pool, "demo", []people.Row{{Email: "a@acme.example"}, {Email: "b@acme.example"}})
	e.out.Enroll(ctx, "demo", "intro", []int64{1, 2})
	e.out.Tick(ctx)
	drafts, _ := e.out.Actions(ctx, "demo", "draft")
	for _, d := range drafts {
		// Pretend both went out, each in its own thread (t1 for a@, t2 for b@).
		thread := map[string]string{"a@acme.example": "t1", "b@acme.example": "t2"}[d.Recipient]
		e.out.Pool.Exec(ctx, `UPDATE actions SET status = 'sent', sent_at = $2, thread_id = $3, message_id = $4 WHERE id = $1`,
			d.ID, e.now, thread, "<"+thread+"@x>")
	}
	reply := func(key, thread, from, body string) int64 {
		ok, err := e.out.RecordReply(ctx, outreach.Inbound{Mailbox: "me@demo-mail.example", ProviderID: key, ThreadID: thread,
			From: from, Subject: "Re: Hi", Body: body, ReceivedAt: e.now, MessageID: "<" + key + "@them>"})
		e.must(err)
		if !ok {
			t.Fatal("reply not recorded")
		}
		var id int64
		e.out.Pool.QueryRow(ctx, `SELECT id FROM replies WHERE provider_message_id = $1`, key).Scan(&id)
		return id
	}
	r1 := reply("r1", "t1", "a@acme.example", "Not interested, thanks.")
	e.llm.Store(`{"class":"no","confidence":0.95,"remind_on":"","answer":""}`)
	e.must(e.w.ClassifyReply(ctx, r1))
	a, _ := people.FindByEmail(ctx, e.out.Pool, "a@acme.example")
	if a.Suppressed != "replied_no" {
		t.Fatalf("confident 'no' should suppress: %+v", a)
	}

	r2 := reply("r2", "t2", "b@acme.example", "Interesting, does it work with BullMQ?")
	e.llm.Store(`{"class":"question","confidence":0.6,"remind_on":"","answer":"Yes, it tests BullMQ workers. Want to try one?"}`)
	e.must(e.w.ClassifyReply(ctx, r2))
	var class, suggested string
	e.out.Pool.QueryRow(ctx, `SELECT classification, suggested_class FROM replies WHERE id = $1`, r2).Scan(&class, &suggested)
	if class != "" || suggested != "question" {
		t.Fatalf("unsure classification should only suggest: %q %q", class, suggested)
	}
	answers, _ := e.out.Actions(ctx, "demo", "draft")
	if len(answers) != 1 || answers[0].Kind != "reply" || answers[0].InReplyTo != "<r2@them>" || answers[0].ThreadID != "t2" {
		t.Fatalf("answer draft: %+v", answers)
	}
}

func TestLateEnrichmentReopensThresholdSkips(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	people.Import(ctx, e.out.Pool, "demo", []people.Row{{Email: "late@acme.example", GitHub: "late"}})
	id := e.signal("s-late", "github.issue_pain", signal.Subject{GitHub: "late"})
	e.must(e.eng.OnSignal(ctx, id))
	if r := e.runs()["signal:1"]; !strings.Contains(r, "fit 0 < 50") {
		t.Fatalf("first run: %q", r)
	}
	// Enrichment finds the queue dependency; the play is evaluated again.
	late, _ := people.FindByEmail(ctx, e.out.Pool, "late@acme.example")
	e.must(people.SetFacts(ctx, e.out.Pool, late.ID, []string{"dep:bullmq"}, "github"))
	e.must(e.eng.OnSignal(ctx, id))
	if r := e.runs()["signal:1"]; !strings.HasPrefix(r, "enrolled") {
		t.Fatalf("after enrichment: %q", r)
	}
	// A final outcome is never re-run.
	e.must(e.eng.OnSignal(ctx, id))
	var n int
	e.out.Pool.QueryRow(ctx, `SELECT count(*) FROM enrollments`).Scan(&n)
	if n != 1 {
		t.Fatalf("enrollments: %d", n)
	}
}

func TestStageEnteredPlay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.p.Plays = append(e.p.Plays, &product.Play{ID: "referral-ask", Trigger: product.Trigger{Stage: "activated"}, Action: product.Action{Task: "Ask for a referral"}})
	e.sync()
	people.Import(ctx, e.out.Pool, "demo", []people.Row{{Email: "happy@acme.example"}})
	e.must(e.out.MarkStage(ctx, "demo", 1, "activated", e.now))
	e.must(e.eng.Scan(ctx, e.p))
	e.must(e.eng.Scan(ctx, e.p))
	tasks, _ := e.out.Tasks(ctx, true, 10)
	if len(tasks) != 1 || !strings.HasPrefix(tasks[0].NextAction, "Ask for a referral") {
		t.Fatalf("tasks: %+v", tasks)
	}
}

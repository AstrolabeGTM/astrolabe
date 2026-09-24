package outreach_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/funnel"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

type fakeSender struct {
	mu   sync.Mutex
	sent []channel.Message
	err  error
}

func (f *fakeSender) Send(_ context.Context, m channel.Message) (channel.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return channel.Result{}, f.err
	}
	f.sent = append(f.sent, m)
	return channel.Result{ProviderID: "gm-" + m.MessageID, ThreadID: "thread-1"}, nil
}

type world struct {
	t      *testing.T
	s      *outreach.Service
	sender *fakeSender
	now    time.Time
	people []int64
}

func newWorld(t *testing.T, n int) *world {
	t.Helper()
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	var rows []people.Row
	for i := range n {
		rows = append(rows, people.Row{Email: "p" + string(rune('a'+i)) + "@acme.example", Name: "Pat " + string(rune('A'+i)), Company: "Acme"})
	}
	if _, err := people.Import(ctx, pool, "demo", rows); err != nil {
		t.Fatal(err)
	}
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, sender: &fakeSender{}, now: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)}
	w.s = &outreach.Service{Pool: pool, River: rc, Now: func() time.Time { return w.now },
		Senders: func(channel.Message) (channel.Sender, error) { return w.sender, nil }}
	list, _ := people.List(ctx, pool, "demo", "", 100)
	for _, p := range list {
		w.people = append(w.people, p.ID)
	}
	return w
}

func (w *world) must(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

// enrollAndDraft enrolls everyone and returns the first-step drafts.
func (w *world) enrollAndDraft() []*outreach.Action {
	ctx := context.Background()
	_, err := w.s.Enroll(ctx, "demo", "intro", w.people)
	w.must(err)
	_, err = w.s.Tick(ctx)
	w.must(err)
	drafts, err := w.s.Actions(ctx, "demo", "draft")
	w.must(err)
	return drafts
}

func (w *world) status(id int64) string {
	a, err := w.s.Action(context.Background(), id)
	w.must(err)
	return a.Status
}

func TestDraftApproveSendOnce(t *testing.T) {
	w := newWorld(t, 1)
	ctx := context.Background()
	d := w.enrollAndDraft()
	if len(d) != 1 || d[0].Subject != "Hi Pat" || d[0].Body != "Hello Pat at Acme" || d[0].Recipient != "pa@acme.example" {
		t.Fatalf("unexpected draft: %+v", d)
	}
	w.must(w.s.Approve(ctx, d[0].ID))

	// Two jobs for the same action (duplicate delivery, retry) send once.
	for range 2 {
		w.send(d[0].ID)
	}
	if len(w.sender.sent) != 1 || w.status(d[0].ID) != "sent" {
		t.Fatalf("sent %d, status %s", len(w.sender.sent), w.status(d[0].ID))
	}

	// The follow-up is due at start+3d and replies in the same thread.
	n, _ := w.s.Tick(ctx)
	if n != 0 {
		t.Fatal("follow-up drafted too early")
	}
	w.now = w.now.Add(72 * time.Hour)
	w.s.Tick(ctx)
	f, _ := w.s.Actions(ctx, "demo", "draft")
	if len(f) != 1 || f[0].Subject != "Re: Hi Pat" || f[0].Body != "" {
		t.Fatalf("follow-up: %+v", f)
	}
	if err := w.s.Approve(ctx, f[0].ID); !errors.Is(err, outreach.ErrInvalid) {
		t.Fatalf("empty draft must not be approvable, got %v", err)
	}
	w.must(w.s.Edit(ctx, f[0].ID, f[0].Subject, "Following up."))
	w.must(w.s.Approve(ctx, f[0].ID))
	w.send(f[0].ID)
	last := w.sender.sent[1]
	if last.ThreadID != "thread-1" || last.InReplyTo != w.sender.sent[0].MessageID {
		t.Fatalf("follow-up not threaded: %+v", last)
	}

	stages, _ := funnel.Counts(ctx, w.s.Pool, mustProduct(t, w), time.Time{})
	if stages[0].Name != "reached" || stages[0].People != 1 {
		t.Fatalf("funnel: %+v", stages)
	}
}

func TestEditAfterApprovalNeedsApprovalAgain(t *testing.T) {
	w := newWorld(t, 1)
	ctx := context.Background()
	d := w.enrollAndDraft()[0]
	w.must(w.s.Approve(ctx, d.ID))
	w.must(w.s.Edit(ctx, d.ID, d.Subject, "Something else"))
	w.send(d.ID)
	if len(w.sender.sent) != 0 || w.status(d.ID) != "draft" {
		t.Fatalf("edited message was sent or not returned to draft: %s", w.status(d.ID))
	}

	// A change made behind the approval (direct DB write) is also caught.
	w.must(w.s.Approve(ctx, d.ID))
	w.s.Pool.Exec(ctx, `UPDATE actions SET recipient = 'someone@else.example' WHERE id = $1`, d.ID)
	out := w.send(d.ID)
	if len(w.sender.sent) != 0 || out.Status != "draft" {
		t.Fatalf("changed recipient was sent: %+v", out)
	}
}

func TestSuppressionAfterApprovalBlocksSend(t *testing.T) {
	w := newWorld(t, 1)
	ctx := context.Background()
	d := w.enrollAndDraft()[0]
	w.must(w.s.Approve(ctx, d.ID))
	w.must(people.Suppress(ctx, w.s.Pool, d.PersonID, "manual", "asked by a friend"))
	w.send(d.ID)
	if len(w.sender.sent) != 0 || w.status(d.ID) != "blocked" {
		t.Fatalf("suppressed person was sent to: %s", w.status(d.ID))
	}
	if r, _ := w.s.Enroll(ctx, "demo", "intro", []int64{d.PersonID}); len(r.Enrolled) != 0 {
		t.Fatal("suppressed person was enrolled")
	}
}

func TestPauseAndLimitsDelayButDoNotDrop(t *testing.T) {
	w := newWorld(t, 5)
	ctx := context.Background()
	drafts := w.enrollAndDraft()
	w.must(w.s.Pause(ctx, "sequence:demo/intro"))
	w.must(w.s.Approve(ctx, drafts[0].ID))
	out := w.send(drafts[0].ID)
	if out.Retry == 0 || len(w.sender.sent) != 0 || w.status(drafts[0].ID) != "approved" {
		t.Fatalf("paused send: %+v", out)
	}
	w.must(w.s.Resume(ctx, "sequence:demo/intro"))

	// Daily limit is 3: the fourth waits.
	for i, d := range drafts[:4] {
		w.must(w.s.Approve(ctx, d.ID))
		out = w.send(d.ID)
		if i < 3 && out.Status != "sent" || i == 3 && (out.Retry == 0 || w.status(d.ID) != "approved") {
			t.Fatalf("send %d: %+v", i, out)
		}
	}
	w.now = w.now.Add(25 * time.Hour)
	if out = w.send(drafts[3].ID); out.Status != "sent" {
		t.Fatalf("after a day: %+v", out)
	}
}

func TestUncertainDeliveryIsHeldNotResent(t *testing.T) {
	w := newWorld(t, 2)
	ctx := context.Background()
	d := w.enrollAndDraft()

	// Provider error with unknown outcome.
	w.sender.err = errors.New("connection reset")
	w.must(w.s.Approve(ctx, d[0].ID))
	out := w.send(d[0].ID)
	w.sender.err = nil
	w.send(d[0].ID)
	if out.Status != "unknown" || w.status(d[0].ID) != "unknown" || len(w.sender.sent) != 0 {
		t.Fatalf("unknown send was retried: %+v %s", out, w.status(d[0].ID))
	}
	// I checked the mailbox; it did go out.
	w.must(w.s.Resolve(ctx, d[0].ID, true, &channel.Result{ProviderID: "gm-1", ThreadID: "thread-found"}))
	if w.status(d[0].ID) != "sent" {
		t.Fatal("resolve did not record the send")
	}
	var thread string
	w.s.Pool.QueryRow(ctx, `SELECT thread_id FROM actions WHERE id = $1`, d[0].ID).Scan(&thread)
	if thread != "thread-found" {
		t.Fatalf("resolved send must keep its thread for follow-ups, got %q", thread)
	}

	// Process died after claiming: the sweep turns it unknown.
	w.must(w.s.Approve(ctx, d[1].ID))
	w.s.Pool.Exec(ctx, `UPDATE actions SET status = 'sending', claimed_at = $2 WHERE id = $1`, d[1].ID, w.now)
	w.now = w.now.Add(11 * time.Minute)
	w.s.Tick(ctx)
	w.send(d[1].ID)
	if w.status(d[1].ID) != "unknown" || len(w.sender.sent) != 0 {
		t.Fatalf("crashed send: %s, sent %d", w.status(d[1].ID), len(w.sender.sent))
	}
	// It did not go out: back to draft, and it needs a fresh approval.
	w.must(w.s.Resolve(ctx, d[1].ID, false, nil))
	w.must(w.s.Approve(ctx, d[1].ID))
	if out := w.send(d[1].ID); out.Status != "sent" {
		t.Fatalf("re-approved send: %+v", out)
	}

	// Definite failure is recorded as failed, not unknown.
	w.sender.err = channel.NotSent("400 invalid To header")
	w.now = w.now.Add(72 * time.Hour)
	w.s.Tick(ctx)
	f, _ := w.s.Actions(ctx, "demo", "draft")
	w.s.Edit(ctx, f[0].ID, f[0].Subject, "x")
	w.must(w.s.Approve(ctx, f[0].ID))
	if out := w.send(f[0].ID); out.Status != "failed" {
		t.Fatalf("definite failure: %+v", out)
	}
}

func TestReplyStopsSequenceAndClassifies(t *testing.T) {
	w := newWorld(t, 2)
	ctx := context.Background()
	d := w.enrollAndDraft()
	for _, a := range d {
		w.must(w.s.Approve(ctx, a.ID))
		w.send(a.ID)
	}
	// Person 1 replies in the thread (the fake puts every send in thread-1,
	// so use the sender address to disambiguate via a new thread).
	ok, err := w.s.RecordReply(ctx, outreach.Inbound{Mailbox: "me@demo-mail.example", ProviderID: "r1", ThreadID: "other",
		From: "pa@acme.example", Subject: "Re: Hi Pat", Body: "Sounds good", ReceivedAt: w.now})
	w.must(err)
	again, _ := w.s.RecordReply(ctx, outreach.Inbound{Mailbox: "me@demo-mail.example", ProviderID: "r1", From: "pa@acme.example", ReceivedAt: w.now})
	if !ok || again {
		t.Fatal("reply must be recorded exactly once")
	}
	// Person 2 bounces.
	w.must2(w.s.RecordReply(ctx, outreach.Inbound{Mailbox: "me@demo-mail.example", ProviderID: "b1", ThreadID: "unrelated",
		From: "mailer-daemon@googlemail.com", Subject: "Delivery Status Notification (Failure)", Bounce: true,
		FailedRecipients: []string{"pb@acme.example"}, ReceivedAt: w.now}))

	// Neither gets a follow-up.
	w.now = w.now.Add(4 * 24 * time.Hour)
	if n, _ := w.s.Tick(ctx); n != 0 {
		t.Fatalf("drafted %d follow-ups after reply/bounce", n)
	}
	pb, _ := people.FindByEmail(ctx, w.s.Pool, "pb@acme.example")
	if pb.Suppressed != "bounced" {
		t.Fatalf("bounce did not suppress: %q", pb.Suppressed)
	}

	replies, _ := w.s.Replies(ctx, true, 10)
	if len(replies) != 1 {
		t.Fatalf("unclassified replies: %d", len(replies))
	}
	w.must(w.s.Classify(ctx, replies[0].ID, "interested", nil))
	tasks, _ := w.s.Tasks(ctx, true, 10)
	if len(tasks) != 1 || !strings.Contains(tasks[0].NextAction, "Reply") {
		t.Fatalf("interested reply should open a task: %+v", tasks)
	}
	w.must(w.s.CloseTask(ctx, tasks[0].ID, "activated", "set up together", nil))
	stages, _ := funnel.Counts(ctx, w.s.Pool, mustProduct(t, w), time.Time{})
	got := map[string]int{}
	for _, s := range stages {
		got[s.Name] = s.People
	}
	// Person 1 reached activation, so they count for every stage before it.
	if got["reached"] != 2 || got["replied"] != 1 || got["installed"] != 1 || got["activated"] != 1 || got["paid"] != 0 {
		t.Fatalf("funnel: %v", got)
	}
}

func TestStageInStopOnStopsEnrollmentAndClaimsGuard(t *testing.T) {
	w := newWorld(t, 1)
	ctx := context.Background()
	d := w.enrollAndDraft()[0]
	w.must(w.s.Edit(ctx, d.ID, d.Subject, "Results are guaranteed!"))
	if err := w.s.Approve(ctx, d.ID); !errors.Is(err, outreach.ErrInvalid) {
		t.Fatalf("forbidden claim approved: %v", err)
	}
	w.must(w.s.MarkStage(ctx, "demo", d.PersonID, "installed", w.now))
	if w.status(d.ID) != "skipped" {
		t.Fatalf("draft should be skipped once the person installed: %s", w.status(d.ID))
	}
}

func (w *world) must2(_ bool, err error) { w.t.Helper(); w.must(err) }

func mustProduct(t *testing.T, w *world) *product.Product {
	p, err := product.Get(context.Background(), w.s.Pool, "demo")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (w *world) send(id int64) outreach.SendOutcome {
	w.t.Helper()
	out, err := w.s.SendAction(context.Background(), id)
	w.must(err)
	return out
}

func TestConcurrentJobsSendOnce(t *testing.T) {
	w := newWorld(t, 1)
	ctx := context.Background()
	d := w.enrollAndDraft()[0]
	w.must(w.s.Approve(ctx, d.ID))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := w.s.SendAction(ctx, d.ID); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(w.sender.sent) != 1 {
		t.Fatalf("sent %d times", len(w.sender.sent))
	}
}

func TestBrokenEnrollmentDoesNotBlockOthers(t *testing.T) {
	w := newWorld(t, 2)
	ctx := context.Background()
	// A sequence that slipped past validation (e.g. edited in the DB).
	w.s.Pool.Exec(ctx, `INSERT INTO sequences (product_id, id, config) VALUES ('demo', 'broken',
		'{"steps":[{"channel":"email","after":"0d","goal":"x","subject":"{{.Nope}}"}],"stop_on":["replied"]}')`)
	_, err := w.s.Enroll(ctx, "demo", "broken", w.people[:1])
	w.must(err)
	_, err = w.s.Enroll(ctx, "demo", "intro", w.people[1:])
	w.must(err)
	n, err := w.s.Tick(ctx)
	if n != 1 || err == nil {
		t.Fatalf("want 1 draft and an error for the broken one, got %d, %v", n, err)
	}
}

func TestOneActiveSequencePerProductAndReEnrollment(t *testing.T) {
	w := newWorld(t, 1)
	ctx := context.Background()
	d := w.enrollAndDraft()[0]
	if r, _ := w.s.Enroll(ctx, "demo", "nudge", w.people); len(r.Enrolled) != 0 || len(r.Skipped) != 1 {
		t.Fatalf("second active sequence allowed: %+v", r)
	}
	// Finish intro (skip every step), then a new sequence is allowed.
	w.must(w.s.Skip(ctx, d.ID))
	for range 2 {
		w.now = w.now.Add(6 * 24 * time.Hour)
		w.s.Tick(ctx)
		for _, a := range must1(w.s.Actions(ctx, "demo", "draft")) {
			w.must(w.s.Skip(ctx, a.ID))
		}
	}
	if r, _ := w.s.Enroll(ctx, "demo", "nudge", w.people); len(r.Enrolled) != 1 {
		t.Fatalf("re-enrollment after completion refused: %+v", r)
	}
	if r, _ := w.s.Enroll(ctx, "demo", "intro", w.people); len(r.Enrolled) != 0 {
		t.Fatalf("second active sequence allowed after re-enroll: %+v", r)
	}
}

func must1[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

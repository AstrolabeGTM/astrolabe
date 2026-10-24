package updates_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/sandbox"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
	"github.com/AstrolabeGTM/astrolabe/internal/updates"
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
	return channel.Result{ProviderID: "p-" + m.MessageID}, nil
}

func (f *fakeSender) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeSender) to(addr, subjectPrefix string) []channel.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []channel.Message
	for _, m := range f.sent {
		if m.To == addr && strings.HasPrefix(m.Subject, subjectPrefix) {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type world struct {
	t   *testing.T
	ctx context.Context
	svc *updates.Service
	out *outreach.Service
	p   *product.Product
	snd *fakeSender
	now time.Time
}

func newWorld(t *testing.T, tweak func(*product.Updates)) *world {
	t.Helper()
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	p.Updates = &product.Updates{PostalAddress: "Pipewrench Ltd, 1 Example Street, Exampleville", PerDay: 100}
	if tweak != nil {
		tweak(p.Updates)
	}
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, ctx: ctx, p: p, snd: &fakeSender{}, now: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)}
	nowf := func() time.Time { return w.now }
	w.svc = &updates.Service{Pool: pool, Now: nowf, Senders: func(channel.Message) (channel.Sender, error) { return w.snd, nil }}
	w.out = &outreach.Service{Pool: pool, Now: nowf}
	w.out.OnInbound = w.svc.HandleInbound
	return w
}

func (w *world) must(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) process() updates.Result {
	w.t.Helper()
	r, err := w.svc.Process(w.ctx)
	w.must(err)
	return r
}

func (w *world) counts() updates.Counts {
	w.t.Helper()
	c, err := w.svc.Counts(w.ctx, "demo")
	w.must(err)
	return c
}

func (w *world) add(email string) {
	w.t.Helper()
	_, err := w.svc.AddByEmail(w.ctx, "demo", email, "Pat", "manual")
	w.must(err)
}

// subscribe asks, sends the confirmation and confirms by replying to it.
func (w *world) subscribe(email string) {
	w.t.Helper()
	w.add(email)
	w.process()
	w.reply(email, "yes")
}

// reply answers the latest list message sent to an address, as a mail client would.
func (w *world) reply(email, body string) {
	w.t.Helper()
	w.snd.mu.Lock()
	var last channel.Message
	for _, m := range w.snd.sent {
		if m.To == email {
			last = m
		}
	}
	w.snd.mu.Unlock()
	if last.MessageID == "" {
		w.t.Fatalf("nothing was sent to %s", email)
	}
	w.replyTo(last, email, body)
}

var replySeq int

func (w *world) replyTo(m channel.Message, from, body string) {
	w.t.Helper()
	replySeq++
	_, err := w.out.RecordReply(w.ctx, outreach.Inbound{Mailbox: m.From, ProviderID: fmt.Sprintf("r-%d", replySeq), From: from,
		Subject: "Re: " + m.Subject, Body: body, References: []string{m.MessageID}, MessageID: fmt.Sprintf("<r%d@x>", replySeq), ReceivedAt: w.now})
	w.must(err)
}

func (w *world) status(email string) string {
	w.t.Helper()
	var s string
	if err := w.svc.Pool.QueryRow(w.ctx, `SELECT status FROM updates_subscribers WHERE email = $1`, email).Scan(&s); err != nil {
		w.t.Fatal(err)
	}
	return s
}

func (w *world) broadcast(subject, body string) *updates.Broadcast {
	w.t.Helper()
	b, err := w.svc.CreateBroadcast(w.ctx, updates.CreateOpts{Product: "demo", Subject: subject, Body: body})
	w.must(err)
	return b
}

func (w *world) approve(b *updates.Broadcast) *updates.Broadcast {
	w.t.Helper()
	b, err := w.svc.ApproveBroadcast(w.ctx, b.ID)
	w.must(err)
	return b
}

func (w *world) resync() {
	w.t.Helper()
	w.p.Hash += "x"
	w.must(product.Sync(w.ctx, w.svc.Pool, []*product.Product{w.p}))
}

func TestSignUpIsNotAddedUntilConfirmed(t *testing.T) {
	w := newWorld(t, nil)
	res, err := w.svc.AddByEmail(w.ctx, "demo", "ana@acme.example", "Ana", "manual")
	w.must(err)
	if res.State != "pending" || res.Subscriber.Status != "pending" {
		t.Fatalf("a sign-up is pending: %+v", res)
	}
	// A broadcast approved now reaches nobody: she is not a subscriber yet.
	b := w.approve(w.broadcast("Hello", "News."))
	if b.Queued != 0 {
		t.Fatalf("a pending sign-up must not be queued: %+v", b)
	}
	w.process()
	if c := w.counts(); c.Pending != 1 || c.Confirmed != 0 {
		t.Fatalf("counts: %+v", c)
	}
	msgs := w.snd.to("ana@acme.example", "")
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0].Subject, "Confirm:") {
		t.Fatalf("only the confirmation goes out: %+v", msgs)
	}
	// Asking again while pending sends nothing more.
	w.add("ana@acme.example")
	w.process()
	if n := len(w.snd.to("ana@acme.example", "")); n != 1 {
		t.Fatalf("second request sent another message: %d", n)
	}
	// Silence is not consent: it only expires.
	w.now = w.now.Add(20 * 24 * time.Hour)
	w.process()
	if c := w.counts(); c.Confirmed != 0 || c.Expired != 1 {
		t.Fatalf("expired sign-up: %+v", c)
	}
}

func TestReplyToConfirmWorksWithoutPublicURL(t *testing.T) {
	w := newWorld(t, nil)
	w.add("ana@acme.example")
	w.process()
	m := w.snd.to("ana@acme.example", "Confirm:")[0]
	if !strings.Contains(m.Body, "reply to this email") || strings.Contains(m.Body, "http") {
		t.Fatalf("without a public URL the mail says to reply and has no link:\n%s", m.Body)
	}
	if m.Headers["Auto-Submitted"] == "" {
		t.Fatal("confirmation must be marked auto-submitted so out-of-office replies don't confirm")
	}
	lu := m.Headers["List-Unsubscribe"]
	if !strings.Contains(lu, "mailto:"+m.From) || strings.Contains(lu, "https:") || m.Headers["List-Unsubscribe-Post"] != "" {
		t.Fatalf("mailto only without a public URL: %q", lu)
	}

	// An out-of-office reply, and a reply from someone else, do not confirm.
	_, err := w.out.RecordReply(w.ctx, outreach.Inbound{Mailbox: m.From, ProviderID: "ooo", From: "ana@acme.example", Subject: "Automatic reply: Confirm", Body: "I am away",
		References: []string{m.MessageID}, ReceivedAt: w.now})
	w.must(err)
	w.replyTo(m, "someone.else@elsewhere.example", "yes")
	if w.status("ana@acme.example") != "pending" {
		t.Fatal("an auto-reply or a stranger confirmed the sign-up")
	}
	// Her reply confirms.
	w.replyTo(m, "ana@acme.example", "yes please\n\n> quoted")
	if w.status("ana@acme.example") != "confirmed" {
		t.Fatal("reply did not confirm")
	}
	w.process()
	if n := len(w.snd.to("ana@acme.example", "You're on the list")); n != 1 {
		t.Fatalf("welcome: %d", n)
	}
	var class string
	w.must(w.svc.Pool.QueryRow(w.ctx, `SELECT classification FROM replies WHERE from_addr = 'ana@acme.example' AND body LIKE 'yes please%'`).Scan(&class))
	if class != "list" {
		t.Fatalf("classification %q", class)
	}
}

func TestReplyAfterExpiryDoesNotConfirm(t *testing.T) {
	w := newWorld(t, func(u *product.Updates) { u.ConfirmWithin = "2d" })
	w.add("ana@acme.example")
	w.process()
	m := w.snd.to("ana@acme.example", "Confirm:")[0]
	w.now = w.now.Add(3 * 24 * time.Hour)
	w.replyTo(m, "ana@acme.example", "yes")
	if w.status("ana@acme.example") == "confirmed" {
		t.Fatal("confirmed after expiry")
	}
	// She can ask again, and confirm that request.
	w.add("ana@acme.example")
	w.process()
	if n := len(w.snd.to("ana@acme.example", "Confirm:")); n != 2 {
		t.Fatalf("a new request sends a new confirmation: %d", n)
	}
	w.reply("ana@acme.example", "yes")
	if w.status("ana@acme.example") != "confirmed" {
		t.Fatal("second request not confirmed")
	}
}

func TestConfirmByLinkNeedsPostAndPublicURL(t *testing.T) {
	w := newWorld(t, nil)
	w.svc.PublicURL = "https://list.example"
	mux := http.NewServeMux()
	w.svc.Routes(mux)
	w.add("ana@acme.example")
	w.process()
	m := w.snd.to("ana@acme.example", "Confirm:")[0]
	link := regexp.MustCompile(`https://list.example(/updates/confirm/\S+)`).FindStringSubmatch(m.Body)
	if link == nil || !strings.Contains(m.Body, "reply to this email") {
		t.Fatalf("both ways to confirm are offered:\n%s", m.Body)
	}
	if !strings.Contains(m.Headers["List-Unsubscribe"], "<https://list.example/updates/unsubscribe/") || m.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Fatalf("one-click headers: %v", m.Headers)
	}
	do := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	if rec := do("GET", link[1]); rec.Code != 200 || w.status("ana@acme.example") != "pending" {
		t.Fatal("a GET (a mail scanner) must not confirm")
	}
	if rec := do("POST", link[1]+"x"); rec.Code != 404 {
		t.Fatalf("a wrong token must be refused: %d", rec.Code)
	}
	// A confirm token cannot unsubscribe.
	if rec := do("POST", strings.Replace(link[1], "confirm", "unsubscribe", 1)); rec.Code != 404 {
		t.Fatalf("confirm token used to unsubscribe: %d", rec.Code)
	}
	if rec := do("POST", link[1]); rec.Code != 200 || w.status("ana@acme.example") != "confirmed" {
		t.Fatal("POST did not confirm")
	}

	// One-click unsubscribe, as a mail provider sends it.
	w.process()
	w.approve(w.broadcast("Hello", "News."))
	w.process()
	got := w.snd.to("ana@acme.example", "Hello")
	if len(got) != 1 {
		t.Fatalf("broadcast: %d", len(got))
	}
	unsub := regexp.MustCompile(`<https://list.example(/updates/unsubscribe/[^>]+)>`).FindStringSubmatch(got[0].Headers["List-Unsubscribe"])[1]
	req := httptest.NewRequest("POST", unsub, strings.NewReader(url.Values{"List-Unsubscribe": {"One-Click"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || w.status("ana@acme.example") != "unsubscribed" {
		t.Fatalf("one-click: %d %s", rec.Code, w.status("ana@acme.example"))
	}
}

func TestUnsubscribeIsHonouredBeforeTheNextSend(t *testing.T) {
	w := newWorld(t, func(u *product.Updates) { u.PerDay = 2 })
	for _, e := range []string{"a@acme.example", "b@acme.example", "c@acme.example"} {
		w.subscribe(e)
	}
	w.process() // welcomes
	before := w.snd.count()
	b := w.approve(w.broadcast("Release 1.0", "It shipped."))
	if b.Queued != 3 {
		t.Fatalf("queued %d", b.Queued)
	}
	if r := w.process(); r.Sent != 2 {
		t.Fatalf("daily cap of 2: sent %d", r.Sent)
	}
	// The one still waiting asks to leave before their turn.
	var third string
	for _, e := range []string{"a@acme.example", "b@acme.example", "c@acme.example"} {
		if len(w.snd.to(e, "Release")) == 0 {
			third = e
		}
	}
	w.reply(third, "Unsubscribe")
	if w.status(third) != "unsubscribed" {
		t.Fatal("reply did not unsubscribe")
	}
	// Leaving the list is not do-not-contact.
	var n int
	w.svc.Pool.QueryRow(w.ctx, `SELECT count(*) FROM suppressions`).Scan(&n)
	if n != 0 {
		t.Fatal("unsubscribing from the list must not suppress the person globally")
	}
	w.now = w.now.Add(25 * time.Hour)
	if r := w.process(); r.Sent != 0 {
		t.Fatalf("an unsubscribed person was sent a broadcast: %+v", r)
	}
	if got := w.snd.count() - before; got != 2 {
		t.Fatalf("broadcast copies: %d", got)
	}
	bb, _ := w.svc.Broadcast(w.ctx, b.ID)
	if bb.Sent != 2 || bb.Skipped != 1 || bb.State != "sent" {
		t.Fatalf("status: %+v", bb)
	}
}

func TestStopAllIsDoNotContact(t *testing.T) {
	w := newWorld(t, nil)
	w.subscribe("a@acme.example")
	w.reply("a@acme.example", "STOP ALL")
	var n int
	w.svc.Pool.QueryRow(w.ctx, `SELECT count(*) FROM suppressions`).Scan(&n)
	if n != 1 || w.status("a@acme.example") != "unsubscribed" {
		t.Fatalf("stop all: suppressions %d", n)
	}
	if _, err := w.svc.AddByEmail(w.ctx, "demo", "a@acme.example", "", "manual"); !errors.Is(err, updates.ErrRefused) {
		t.Fatalf("do-not-contact must refuse a sign-up: %v", err)
	}
}

func TestMailtoUnsubscribeUsesTheSubjectToken(t *testing.T) {
	w := newWorld(t, nil)
	w.subscribe("a@acme.example")
	w.process()
	w.approve(w.broadcast("Hello", "News."))
	w.process()
	m := w.snd.to("a@acme.example", "Hello")[0]
	mailto := regexp.MustCompile(`<mailto:([^?]+)\?subject=([^>]+)>`).FindStringSubmatch(m.Headers["List-Unsubscribe"])
	subject, _ := url.PathUnescape(mailto[2])
	// What a mail client sends: a fresh message, no References, empty body.
	_, err := w.out.RecordReply(w.ctx, outreach.Inbound{Mailbox: mailto[1], ProviderID: "mailto-1", From: "a@acme.example", Subject: subject, ReceivedAt: w.now})
	w.must(err)
	if w.status("a@acme.example") != "unsubscribed" {
		t.Fatalf("mailto unsubscribe with subject %q failed", subject)
	}
	// A guessed token does nothing.
	w.subscribe("b@acme.example")
	_, err = w.out.RecordReply(w.ctx, outreach.Inbound{Mailbox: mailto[1], ProviderID: "mailto-2", From: "x@acme.example", Subject: "unsubscribe 2.000000000000000000000000", ReceivedAt: w.now})
	w.must(err)
	if w.status("b@acme.example") != "confirmed" {
		t.Fatal("a forged token unsubscribed someone")
	}
}

func TestNobodyReceivesABroadcastTwice(t *testing.T) {
	w := newWorld(t, nil)
	for _, e := range []string{"a@acme.example", "b@acme.example", "c@acme.example"} {
		w.subscribe(e)
	}
	w.process()
	before := w.snd.count()
	b := w.approve(w.broadcast("Hello", "News."))
	w.approve(b) // approving again changes nothing
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); w.svc.Process(w.ctx) }()
	}
	wg.Wait()
	w.process()
	w.process()
	if got := w.snd.count() - before; got != 3 {
		t.Fatalf("each of 3 subscribers gets it once, got %d messages", got)
	}
	// A subscriber who joins later does not get it, and nobody gets it twice.
	w.subscribe("d@acme.example")
	w.process()
	if _, err := w.svc.ApproveBroadcast(w.ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	w.process()
	if got := len(w.snd.to("d@acme.example", "Hello")); got != 0 {
		t.Fatalf("a later subscriber got an old broadcast: %d", got)
	}
	for _, e := range []string{"a@acme.example", "b@acme.example", "c@acme.example"} {
		if got := len(w.snd.to(e, "Hello")); got != 1 {
			t.Fatalf("%s got %d copies", e, got)
		}
	}
}

func TestLostProviderAnswerIsUnknownAndNeverResent(t *testing.T) {
	w := newWorld(t, nil)
	w.subscribe("a@acme.example")
	w.process()
	b := w.approve(w.broadcast("Hello", "News."))
	before := w.snd.count()
	w.snd.setErr(errors.New("smtp: no answer after sending; delivery unknown"))
	w.process()
	w.snd.setErr(nil)
	w.process()
	w.now = w.now.Add(48 * time.Hour)
	w.process()
	bb, _ := w.svc.Broadcast(w.ctx, b.ID)
	if bb.Unknown != 1 || bb.Queued != 0 || w.snd.count() != before {
		t.Fatalf("an uncertain send must be marked unknown and never retried: %+v sent=%d", bb, w.snd.count()-before)
	}
	// A claim that was never finished (the process died) becomes unknown too.
	w.subscribe("b@acme.example")
	w.process()
	b2 := w.approve(w.broadcast("Second", "More."))
	_, err := w.svc.Pool.Exec(w.ctx, `UPDATE updates_messages SET status = 'sending', claimed_at = $1 WHERE broadcast_id = $2 AND status = 'queued'`, w.now.Add(-time.Hour), b2.ID)
	w.must(err)
	w.process()
	bb, _ = w.svc.Broadcast(w.ctx, b2.ID)
	if bb.Unknown != 2 && bb.Unknown != 1 || bb.Sent != 0 && bb.Queued != 0 {
		t.Fatalf("stale claim: %+v", bb)
	}
	if got := len(w.snd.to("a@acme.example", "Second")); got > 1 {
		t.Fatalf("resent: %d", got)
	}
}

func TestNotSentIsRetriedThenFails(t *testing.T) {
	w := newWorld(t, nil)
	w.subscribe("a@acme.example")
	w.process()
	b := w.approve(w.broadcast("Hello", "News."))
	w.snd.setErr(channel.NotSent("auth failed"))
	for range 3 {
		w.process()
	}
	bb, _ := w.svc.Broadcast(w.ctx, b.ID)
	if bb.Failed != 1 {
		t.Fatalf("%+v", bb)
	}
}

func TestBroadcastWithoutPostalAddressIsRefused(t *testing.T) {
	w := newWorld(t, func(u *product.Updates) { u.PostalAddress = "" })
	w.subscribe("a@acme.example")
	w.process() // the welcome
	b := w.broadcast("Hello", "News.")
	if _, err := w.svc.ApproveBroadcast(w.ctx, b.ID); err == nil || !strings.Contains(err.Error(), "postal_address") {
		t.Fatalf("approval without a postal address: %v", err)
	}
	// Approved while it had one, a product that loses its address sends nothing.
	w.p.Updates.PostalAddress = "1 Example Street"
	w.resync()
	w.approve(b)
	w.p.Updates.PostalAddress = ""
	w.resync()
	before := w.snd.count()
	w.process()
	if w.snd.count() != before {
		t.Fatal("broadcast sent without a postal address")
	}
	// A different address means the approved text is no longer what would
	// go out: it goes back to draft instead of sending.
	w.p.Updates.PostalAddress = "2 Other Street"
	w.resync()
	w.process()
	if len(w.snd.to("a@acme.example", "Hello")) != 0 {
		t.Fatal("approval covers the exact message including its footer")
	}
	if bb, _ := w.svc.Broadcast(w.ctx, b.ID); bb.Status != "draft" {
		t.Fatalf("status %s", bb.Status)
	}
}

func TestDailyCapHolds(t *testing.T) {
	w := newWorld(t, func(u *product.Updates) { u.PerDay = 3 })
	for i := range 8 {
		w.subscribe(fmt.Sprintf("s%d@acme.example", i))
	}
	w.process()
	b := w.approve(w.broadcast("Hello", "News."))
	step := func(advance time.Duration, want int) {
		t.Helper()
		w.now = w.now.Add(advance)
		if r := w.process(); r.Sent != want {
			t.Fatalf("after +%v sent %d, want %d", advance, r.Sent, want)
		}
	}
	step(0, 3)
	step(13*time.Hour, 0) // 13h later: still inside the 24 hours
	step(12*time.Hour, 3) // 25h
	step(time.Hour, 0)
	step(24*time.Hour, 2) // the rest
	if bb, _ := w.svc.Broadcast(w.ctx, b.ID); bb.Sent != 8 || bb.State != "sent" {
		t.Fatalf("%+v", bb)
	}
}

func TestEditingWithdrawsApprovalAndFlagsWait(t *testing.T) {
	w := newWorld(t, nil)
	w.subscribe("a@acme.example")
	w.process()
	b := w.approve(w.broadcast("Hello", "News."))
	if b.State != "approved" || b.Queued != 1 {
		t.Fatalf("%+v", b)
	}
	body := "News, edited."
	b, err := w.svc.EditBroadcast(w.ctx, b.ID, nil, &body)
	w.must(err)
	if b.Status != "draft" || b.Queued != 0 {
		t.Fatalf("an edit withdraws approval: %+v", b)
	}
	before := w.snd.count()
	w.process()
	if w.snd.count() != before {
		t.Fatal("withdrawn broadcast was sent")
	}
	// A forbidden claim is flagged and cannot be approved.
	bad := "Results guaranteed."
	b, err = w.svc.EditBroadcast(w.ctx, b.ID, nil, &bad)
	w.must(err)
	if len(b.Flags) == 0 {
		t.Fatal("never-claim not flagged")
	}
	if _, err := w.svc.ApproveBroadcast(w.ctx, b.ID); err == nil {
		t.Fatal("flagged copy was approved")
	}
	long := strings.Repeat("word ", 400)
	b, err = w.svc.EditBroadcast(w.ctx, b.ID, nil, &long)
	w.must(err)
	w.approve(b) // length alone is not a problem for a newsletter
	w.process()
	if _, err := w.svc.EditBroadcast(w.ctx, b.ID, nil, &body); err == nil {
		t.Fatal("a broadcast that has started sending must not be editable")
	}
}

func TestBroadcastFromContentAndMessageShape(t *testing.T) {
	w := newWorld(t, nil)
	w.subscribe("a@acme.example")
	w.process()
	_, err := w.svc.Pool.Exec(w.ctx, `
		INSERT INTO content_items (product_id, batch, platform, body, status, posted_at) VALUES
		('demo', 'v1', 'newsletter', 'Shipped the thing.', 'posted', $1),
		('demo', 'v1', 'x', 'tweet text', 'posted', $1),
		('demo', 'old', 'newsletter', 'Last year.', 'posted', $2)`, w.now.AddDate(0, 0, -3), w.now.AddDate(-1, 0, 0))
	w.must(err)
	b, err := w.svc.CreateBroadcast(w.ctx, updates.CreateOpts{Product: "demo", Period: "this-month"})
	w.must(err)
	if b.Body != "Shipped the thing." || b.Subject != "Demo updates: September 2026" || len(b.ContentIDs) != 1 {
		t.Fatalf("%+v", b)
	}
	w.approve(b)
	w.process()
	m := w.snd.to("a@acme.example", "Demo updates")[0]
	for _, want := range []string{"Shipped the thing.", `reply "unsubscribe"`, "Pipewrench Ltd, 1 Example Street"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("body missing %q:\n%s", want, m.Body)
		}
	}
	if m.Kind != "updates" || m.Via != "smtp" || m.From != "hello@demo.example" {
		t.Fatalf("%+v", m)
	}
	if _, err := w.svc.CreateBroadcast(w.ctx, updates.CreateOpts{Product: "demo", Period: "2020-01"}); err == nil {
		t.Fatal("an empty period should say there is nothing to send")
	}
}

func TestGitHubOnlyPersonIsNotMailed(t *testing.T) {
	w := newWorld(t, nil)
	_, err := people.Import(w.ctx, w.svc.Pool, "demo", []people.Row{{GitHub: "octocat", Name: "Octo"}})
	w.must(err)
	p, _ := people.List(w.ctx, w.svc.Pool, "demo", "octo", 1)
	tx, _ := w.svc.Pool.Begin(w.ctx)
	defer tx.Rollback(w.ctx)
	if _, err := w.svc.SubscribeTx(w.ctx, tx, "demo", p[0].ID, "automation:x"); !errors.Is(err, updates.ErrRefused) {
		t.Fatalf("known only from GitHub, no address: %v", err)
	}
}

func TestExportAndDigestSummary(t *testing.T) {
	w := newWorld(t, nil)
	w.subscribe("a@acme.example")
	if _, err := w.svc.AddByEmail(w.ctx, "demo", "not-an-address", "x", "manual"); err == nil {
		t.Fatal("not an address")
	}
	_, err := w.svc.AddByEmail(w.ctx, "demo", "b@acme.example", "=HYPERLINK(1)", "manual")
	w.must(err)
	csv, err := w.svc.ExportCSV(w.ctx, "demo")
	w.must(err)
	if !strings.Contains(csv, "a@acme.example") || strings.Contains(csv, "b@acme.example") {
		t.Fatalf("only confirmed subscribers are exported:\n%s", csv)
	}
	w.process()
	w.approve(w.broadcast("Hello", "News."))
	sum, err := updates.WeekSummary(w.ctx, w.svc.Pool, "demo", "Demo updates", w.now.Add(time.Hour))
	w.must(err)
	if sum.Confirmed != 1 || sum.Pending != 1 || sum.NewConfirmed != 1 || len(sum.Broadcasts) != 1 || !strings.Contains(sum.Text(), "Hello") {
		t.Fatalf("%+v\n%s", sum, sum.Text())
	}
}

// In sandbox mode the confirmation goes to the local outbox, and the
// simulated reply runs the real reply handling.
func TestSandboxConfirmationAndBroadcast(t *testing.T) {
	w := newWorld(t, nil)
	w.svc.Senders = func(channel.Message) (channel.Sender, error) { return &sandbox.Sender{Pool: w.svc.Pool}, nil }
	w.add("ana@acme.example")
	w.process()
	box, err := sandbox.Outbox(w.ctx, w.svc.Pool, 10)
	w.must(err)
	if len(box) != 1 || !strings.HasPrefix(box[0].Subject, "Confirm:") {
		t.Fatalf("outbox: %+v", box)
	}
	w.must(sandbox.SimulateReply(w.ctx, w.out, box[0].ID, "yes"))
	if w.status("ana@acme.example") != "confirmed" {
		t.Fatal("simulated reply did not confirm")
	}
	w.process()
	w.approve(w.broadcast("Hello", "News."))
	w.process()
	box, _ = sandbox.Outbox(w.ctx, w.svc.Pool, 10)
	if len(box) != 3 || box[0].Subject != "Hello" {
		t.Fatalf("outbox after a broadcast: %+v", box)
	}
}

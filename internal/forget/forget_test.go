package forget_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/forget"
	"github.com/AstrolabeGTM/astrolabe/internal/forms"
	"github.com/AstrolabeGTM/astrolabe/internal/links"
	"github.com/AstrolabeGTM/astrolabe/internal/notes"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/pipeline"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
	"github.com/AstrolabeGTM/astrolabe/internal/updates"
)

const (
	anaEmail  = "ana@pipewrench.example"
	anaAlt    = "ana.lee@gmail.example" // a second address, only seen in a payment and a reply
	anaGitHub = "analee-dev"
	anaName   = "Ana Lee"
)

// fakeNetlify serves one form with Ana's and Bo's submissions and records
// deletes. failDelete makes DELETE of those ids return 500.
type fakeNetlify struct {
	mu         sync.Mutex
	subs       []map[string]any
	deleted    []string
	failDelete map[string]bool
	srv        *httptest.Server
}

func newFakeNetlify(t *testing.T, now time.Time) *fakeNetlify {
	f := &fakeNetlify{failDelete: map[string]bool{}}
	at := func(h int) string { return now.Add(-time.Duration(h) * time.Minute).Format(time.RFC3339) }
	f.subs = []map[string]any{
		{"id": "s-ana", "created_at": at(5), "data": map[string]any{"email": strings.ToUpper(anaEmail[:1]) + anaEmail[1:], "name": anaName, "company": "Pipewrench",
			"message": "Our CI is flaky", "how_did_you_hear_about_us": "Hacker News"}},
		{"id": "s-bo", "created_at": at(6), "data": map[string]any{"email": "bo@gmail.example", "name": "Bo Park", "company": "Bo Labs"}},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer nf-token" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/sites/pipewrench.example/forms":
			json.NewEncoder(w).Encode([]map[string]string{{"id": "f1", "name": "request-demo"}})
		case r.Method == "GET" && r.URL.Path == "/forms/f1/submissions":
			json.NewEncoder(w).Encode(f.subs)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/submissions/"):
			id := strings.TrimPrefix(r.URL.Path, "/submissions/")
			if f.failDelete[id] {
				http.Error(w, "boom", 500)
				return
			}
			f.deleted = append(f.deleted, id)
			for i, sub := range f.subs {
				if sub["id"] == id {
					f.subs = append(f.subs[:i], f.subs[i+1:]...)
					break
				}
			}
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNetlify) deletes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

type sender struct{}

func (sender) Send(_ context.Context, m channel.Message) (channel.Result, error) {
	return channel.Result{ProviderID: "gm-" + m.MessageID, ThreadID: "thread-" + m.To}, nil
}

type world struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	p       *product.Product
	now     time.Time
	nf      *fakeNetlify
	sig     *signal.Service
	intake  *forms.Intake
	out     *outreach.Service
	upd     *updates.Service
	monitor *product.Monitor
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	p.Pipeline = &product.Pipeline{Stages: []string{"identified", "contacted", "qualified"}}
	p.Updates = &product.Updates{PostalAddress: "Pipewrench Ltd, 1 Example Street", PerDay: 100}
	p.Monitors = []*product.Monitor{{ID: "request-demo", Type: "netlify_forms", Site: "pipewrench.example", Form: "request-demo", Signal: "form.request",
		Strength: 40, WhyNow: true, Every: "1h", Account: true}}
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	now := time.Now().UTC().Truncate(time.Second)
	nowf := func() time.Time { return now }
	w := &world{t: t, ctx: ctx, pool: pool, p: p, now: now, nf: newFakeNetlify(t, now), monitor: p.Monitors[0]}
	w.sig = &signal.Service{Pool: pool, River: rc}
	w.intake = &forms.Intake{Pool: pool, Signals: w.sig, Netlify: &forms.Netlify{Token: "nf-token", Base: w.nf.srv.URL}, Now: nowf}
	w.sig.Forms = w.intake
	snd := func(channel.Message) (channel.Sender, error) { return sender{}, nil }
	w.out = &outreach.Service{Pool: pool, River: rc, Now: nowf, Senders: snd}
	w.upd = &updates.Service{Pool: pool, Now: nowf, Senders: snd}
	w.out.OnInbound = w.upd.HandleInbound
	return w
}

func (w *world) must(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) exec(sql string, args ...any) {
	w.t.Helper()
	_, err := w.pool.Exec(w.ctx, sql, args...)
	w.must(err)
}

func (w *world) count(sql string, args ...any) (n int) {
	w.t.Helper()
	w.must(w.pool.QueryRow(w.ctx, sql, args...).Scan(&n))
	return n
}

func (w *world) pollForms() int {
	w.t.Helper()
	n, err := w.sig.RunMonitor(w.ctx, &signal.Sources{}, "demo", "request-demo", w.now)
	w.must(err)
	return n
}

// seedEverything records Ana through every feature and Bo (who is not
// erased) through some. It returns Ana's person id.
func (w *world) seedEverything() int64 {
	t, ctx := w.t, w.ctx

	// Form intake: person, company, account, signal, form_submissions.
	if n := w.pollForms(); n != 2 {
		t.Fatalf("form intake took in %d submissions, want 2", n)
	}
	ana, err := people.FindByEmail(ctx, w.pool, anaEmail)
	w.must(err)

	// A monitor signal from GitHub for the same person, and a bystander.
	_, err = w.sig.Ingest(ctx, &signal.Signal{Product: "demo", Monitor: "gh", Type: "github.issue_pain", OccurredAt: w.now, Strength: 50,
		Subject: signal.Subject{GitHub: anaGitHub, Email: anaEmail, Name: anaName}, Title: anaName + " opened an issue", EvidenceURL: "https://github.example/issues/1",
		DedupeKey: "gh:1", Data: map[string]any{"login": anaGitHub}})
	w.must(err)
	w.must(people.SetFacts(ctx, w.pool, ana.ID, []string{"lang:go"}, "test"))

	// Sequence, draft, approval, send, and the reply.
	_, err = w.out.Enroll(ctx, "demo", "intro", []int64{ana.ID})
	w.must(err)
	_, err = w.out.Tick(ctx)
	w.must(err)
	drafts, err := w.out.Actions(ctx, "demo", "draft")
	w.must(err)
	var anaDraft *outreach.Action
	for _, d := range drafts {
		if d.PersonID == ana.ID {
			anaDraft = d
		}
	}
	if anaDraft == nil {
		t.Fatalf("no draft for Ana among %d", len(drafts))
	}
	w.must(w.out.Edit(ctx, anaDraft.ID, "Hi Ana", "Hello Ana Lee, saw your issue"))
	w.must(w.out.Approve(ctx, anaDraft.ID))
	_, err = w.out.SendAction(ctx, anaDraft.ID)
	w.must(err)
	ok, err := w.out.RecordReply(ctx, outreach.Inbound{Mailbox: "me@demo-mail.example", ProviderID: "r-ana", ThreadID: "thread-" + anaEmail,
		From: anaEmail, Subject: "Re: Hi Ana", Body: "Sounds good, call me", ReceivedAt: w.now})
	w.must(err)
	if !ok {
		t.Fatal("reply not recorded")
	}
	// A second address of hers, seen only on an unmatched reply.
	_, err = w.out.RecordReply(ctx, outreach.Inbound{Mailbox: "me@demo-mail.example", ProviderID: "r-ana2", ThreadID: "thread-" + anaEmail,
		From: anaAlt, Subject: "Re: Hi Ana", Body: "From my other address", ReceivedAt: w.now})
	w.must(err)
	var replyID int64
	w.must(w.pool.QueryRow(ctx, `SELECT id FROM replies WHERE provider_message_id = 'r-ana'`).Scan(&replyID))
	w.must(w.out.Classify(ctx, replyID, "interested", nil)) // opens a task
	w.exec(`INSERT INTO voice_examples (product_id, action_id, original, edited) VALUES ('demo', $1, 'Hello', 'Hello Ana Lee, saw your issue')`, anaDraft.ID)
	w.exec(`INSERT INTO sandbox_outbox (action_id, channel, sender, recipient, subject, body) VALUES ($1, 'email', 'me@demo-mail.example', $2, 'Hi Ana', 'Hello Ana Lee')`, anaDraft.ID, anaEmail)
	w.exec(`INSERT INTO sandbox_outbox (channel, sender, recipient, subject, body) VALUES ('email', 'hello@demo.example', $1, 'Welcome', 'Dear Ana')`, anaEmail)

	// Scores, research notes (and the LLM call behind them), unrelated LLM call naming her.
	w.exec(`INSERT INTO scores (product_id, person_id, fit, intent, why_now, priority, tier, eligible, breakdown, computed_at)
		VALUES ('demo', $1, 50, 50, 10, 60, 'B', true, '{"why":"opened an issue"}', now())`, ana.ID)
	var callID int64
	w.must(w.pool.QueryRow(ctx, `INSERT INTO llm_calls (purpose, product_id, model, prompt, output) VALUES ('brief', 'demo', 'm', 'PERSON: someone with a flaky CI', 'Who: a dev') RETURNING id`).Scan(&callID))
	w.exec(`INSERT INTO briefs (product_id, person_id, body, llm_call_id) VALUES ('demo', $1, 'research', $2)`, ana.ID, callID)
	w.exec(`INSERT INTO llm_calls (purpose, product_id, model, prompt, output) VALUES ('draft', 'demo', 'm', 'Write to '||$1||' about CI', 'Hi')`, anaEmail)
	w.exec(`INSERT INTO llm_calls (purpose, product_id, model, prompt, output) VALUES ('draft', 'demo', 'm', 'Write to a bystander', 'Hi')`)

	// Account membership, conversation notes with survey and pricing.
	var accountID int64
	w.must(w.pool.QueryRow(ctx, `SELECT id FROM accounts WHERE product_id = 'demo' AND company_id = (SELECT company_id FROM people WHERE id = $1)`, ana.ID).Scan(&accountID))
	w.must(pipeline.SetPerson(ctx, w.pool, w.p, accountID, ana.ID, "champion"))
	_, err = notes.Log(ctx, w.pool, w.p, notes.LogOpts{PersonID: ana.ID, AccountID: accountID, Channel: "call", Summary: "Call with Ana Lee about flaky CI",
		TheirWords: "we lose a day a week", Problem: "flaky CI", PMF: &notes.PMF{Answer: "very_disappointed", UsedTwoWeeks: true, Alternative: "nothing"},
		Pricing: []notes.Pricing{{Offer: "Team plan", Reaction: "fine"}}}, w.now)
	w.must(err)
	w.exec(`INSERT INTO weekly_themes (product_id, iso_week, body) VALUES ('demo', to_char(now(), 'IYYY"-W"IW'), 'Flaky CI is the theme')`)
	w.exec(`INSERT INTO digests (week, body, data) VALUES (current_date, 'This week Ana Lee asked for a demo', '{}')`)

	// Updates list: confirmation sent, and a broadcast message inside the 24h cap.
	res, err := w.upd.AddByEmail(ctx, "demo", anaEmail, anaName, "manual")
	w.must(err)
	_ = res
	_, err = w.upd.Process(ctx)
	w.must(err)
	w.exec(`INSERT INTO updates_broadcasts (product_id, subject, body, status) VALUES ('demo', 'News', 'Body', 'approved')`)
	w.exec(`INSERT INTO updates_messages (product_id, subscriber_id, kind, broadcast_id, status, claimed_at, sent_at)
		SELECT 'demo', s.id, 'broadcast', (SELECT max(id) FROM updates_broadcasts), 'sent', $2, $2 FROM updates_subscribers s WHERE s.person_id = $1`, ana.ID, w.now)

	// Funnel, payments, links and clicks, assignments, channel permissions, do-not-contact.
	w.must(w.out.RecordStage(ctx, "demo", ana.ID, "installed", w.now, "event", "installed:ana"))
	w.exec(`INSERT INTO payments (product_id, person_id, provider, provider_id, kind, amount_cents, currency, occurred_at, email)
		VALUES ('demo', $1, 'stripe', 'in_1', 'payment', 2900, 'usd', now(), $2)`, ana.ID, anaAlt)
	pid := ana.ID
	code, err := links.Create(ctx, w.pool, links.Link{Product: "demo", Destination: "https://demo.example/start", Label: "campaign:launch", PersonID: &pid})
	w.must(err)
	w.exec(`INSERT INTO clicks (code, referer, user_agent) VALUES ($1, 'https://news.example', 'Mozilla')`, code)
	w.exec(`INSERT INTO assignments (experiment, unit, variant) VALUES ('demo/intro/0', $1, 'b')`, fmt.Sprintf("person:%d", ana.ID))
	w.exec(`INSERT INTO channel_permissions (product_id, person_id, channel, allowed) VALUES ('demo', $1, 'whatsapp', true)`, ana.ID)
	w.must(people.Suppress(ctx, w.pool, ana.ID, "manual", "asked politely"))

	// An ambiguous match naming her, and a task about her.
	var sigID, boID int64
	w.must(w.pool.QueryRow(ctx, `SELECT id FROM signals WHERE dedupe_key = 'gh:1'`).Scan(&sigID))
	bo, err := people.FindByEmail(ctx, w.pool, "bo@gmail.example")
	w.must(err)
	boID = bo.ID
	w.exec(`INSERT INTO identity_reviews (signal_id, candidates) VALUES ($1, $2)`, sigID, []int64{ana.ID, boID})
	w.exec(`INSERT INTO tasks (product_id, person_id, next_action, due_at) VALUES ('demo', $1, 'Send Ana Lee the CI guide', now())`, ana.ID)
	w.exec(`INSERT INTO tasks (product_id, next_action, due_at) VALUES ('demo', 'Ask ana@pipewrench.example about pricing', now())`)
	// An automation run inside its daily window is kept, scrubbed; an old one goes.
	w.exec(`INSERT INTO play_runs (product_id, play_id, person_id, trigger_key, outcome, detail) VALUES ('demo', 'form-request-reply', $1, 'sig:1', 'task', 'Ana Lee')`, ana.ID)
	w.exec(`INSERT INTO play_runs (product_id, play_id, person_id, trigger_key, outcome, detail, created_at) VALUES ('demo', 'form-request-reply', $1, 'sig:0', 'task', 'Ana Lee', now() - interval '3 days')`, ana.ID)

	// Free text elsewhere that names her; the company stays.
	w.exec(`UPDATE companies SET notes = 'Met Ana Lee at a conference' WHERE id = (SELECT company_id FROM people WHERE id = $1)`, ana.ID)
	return ana.ID
}

// dumpHits counts, over every text, varchar, jsonb and text[] column of
// every table, the rows containing any of terms (case-insensitive). It
// returns table.column -> rows.
func dumpHits(t *testing.T, pool *pgxpool.Pool, terms ...string) map[string]int {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT c.table_name, c.column_name, c.data_type FROM information_schema.columns c
		JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name AND t.table_type = 'BASE TABLE'
		WHERE c.table_schema = 'public' AND (c.data_type IN ('text', 'character varying', 'jsonb', 'json') OR (c.data_type = 'ARRAY' AND c.udt_name IN ('_text', '_varchar')))`)
	if err != nil {
		t.Fatal(err)
	}
	type col struct{ table, name string }
	var cols []col
	for rows.Next() {
		var c col
		var dt string
		rows.Scan(&c.table, &c.name, &dt)
		cols = append(cols, c)
	}
	rows.Close()
	hits := map[string]int{}
	for _, c := range cols {
		var n int
		err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %q WHERE EXISTS (SELECT 1 FROM unnest($1::text[]) x WHERE strpos(lower(%q::text), x) > 0)`, c.table, c.name),
			lowerAll(terms)).Scan(&n)
		if err != nil {
			t.Fatalf("%s.%s: %v", c.table, c.name, err)
		}
		if n > 0 {
			hits[c.table+"."+c.name] = n
		}
	}
	return hits
}

func lowerAll(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = strings.ToLower(v)
	}
	return out
}

var anaTerms = []string{anaEmail, anaAlt, anaGitHub, anaName, "ana@"}

func TestForgetErasesEverythingAndKeepsTheRest(t *testing.T) {
	w := newWorld(t)
	ctx := w.ctx
	anaID := w.seedEverything()

	if hits := dumpHits(t, w.pool, anaTerms...); len(hits) < 15 {
		t.Fatalf("seed too thin to prove anything: only %d columns mention her: %v", len(hits), hits)
	}
	boBefore := w.count(`SELECT count(*) FROM form_submissions WHERE fields::text ILIKE '%bo@gmail.example%'`)
	if boBefore != 1 {
		t.Fatalf("bystander submission: %d", boBefore)
	}

	// Dry run first: counts, no changes, no Netlify calls.
	before := tableCounts(t, w.pool)
	dry, err := forget.Forget(ctx, w.pool, anaEmail, forget.Options{DryRun: true, Netlify: w.intake.Netlify, Now: func() time.Time { return w.now }})
	w.must(err)
	if !dry.DryRun || dry.Total() == 0 || dry.PersonID == nil || *dry.PersonID != anaID {
		t.Fatalf("dry run: %+v", dry)
	}
	if after := tableCounts(t, w.pool); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("dry run changed the database:\nbefore %v\nafter  %v", before, after)
	}
	if len(w.nf.deletes()) != 0 || len(dry.RemotePlanned) != 1 || dry.RemotePlanned[0].ID != "s-ana" {
		t.Fatalf("dry run touched Netlify or did not plan the copy: deletes=%v planned=%v", w.nf.deletes(), dry.RemotePlanned)
	}
	if forgotten, _ := people.IsForgotten(ctx, w.pool, "email", anaEmail); forgotten {
		t.Fatal("dry run left a tombstone")
	}

	rep, err := forget.Forget(ctx, w.pool, anaEmail, forget.Options{Netlify: w.intake.Netlify, Now: func() time.Time { return w.now }})
	w.must(err)
	if fmt.Sprint(rep.Deleted) != fmt.Sprint(dry.Deleted) {
		t.Fatalf("dry run and real run disagree:\ndry  %v\nreal %v", dry.Deleted, rep.Deleted)
	}
	for _, table := range []string{"people", "identities", "product_people", "signals", "scores", "briefs", "llm_calls", "enrollments", "actions", "replies",
		"sandbox_outbox", "voice_examples", "tasks", "conversations", "pmf_answers", "pricing_feedback", "account_people", "form_submissions",
		"updates_subscribers", "updates_messages", "funnel_events", "payments", "links", "clicks", "assignments", "channel_permissions", "suppressions",
		"identity_reviews", "play_runs"} {
		if rep.Deleted[table] == 0 {
			t.Errorf("nothing deleted from %s: %v", table, rep.Deleted)
		}
	}
	if rep.Scrubbed["erased_sends"] < 2 {
		t.Errorf("sends inside the cap windows were not kept as anonymous counts: %v", rep.Scrubbed)
	}
	for _, k := range []string{"play_runs", "account_stages.evidence", "companies.notes", "digests.body"} {
		if rep.Scrubbed[k] == 0 {
			t.Errorf("%s not scrubbed: %v", k, rep.Scrubbed)
		}
	}
	if rep.Tombstones < 3 || len(rep.RemoteDeleted) != 1 || len(rep.RemoteLeft) != 0 {
		t.Fatalf("tombstones/remote: %+v", rep)
	}
	if got := w.nf.deletes(); len(got) != 1 || got[0] != "s-ana" {
		t.Fatalf("Netlify deletes: %v (Bo's must stay)", got)
	}
	if len(rep.Review) == 0 {
		t.Error("weekly themes written from her conversation weeks were not flagged for review")
	}

	// Nothing personal is left anywhere in the database.
	if hits := dumpHits(t, w.pool, anaTerms...); len(hits) > 0 {
		t.Fatalf("her data is still in: %v", hits)
	}
	if w.count(`SELECT count(*) FROM people WHERE id = $1`, anaID) != 0 {
		t.Fatal("person row remains")
	}
	// The bystander and the company stay.
	if _, err := people.FindByEmail(ctx, w.pool, "bo@gmail.example"); err != nil {
		t.Fatalf("Bo is gone: %v", err)
	}
	if w.count(`SELECT count(*) FROM form_submissions WHERE fields::text ILIKE '%bo@gmail.example%'`) != 1 {
		t.Fatal("Bo's submission was deleted")
	}
	if w.count(`SELECT count(*) FROM companies WHERE domain = 'pipewrench.example'`) != 1 || w.count(`SELECT count(*) FROM accounts`) == 0 {
		t.Fatal("company or account was deleted")
	}
	if w.count(`SELECT count(*) FROM llm_calls WHERE prompt = 'Write to a bystander'`) != 1 {
		t.Fatal("an unrelated LLM call was deleted")
	}
	// The sends kept for the cap carry no person.
	var product, counter string
	w.must(w.pool.QueryRow(ctx, `SELECT product_id, counter FROM erased_sends ORDER BY id LIMIT 1`).Scan(&product, &counter))
	if product != "demo" {
		t.Fatalf("erased_sends: %s %s", product, counter)
	}

	// Her address cannot be recreated, from any door.
	assertBlocked(t, w)

	// Forgetting someone twice is not an error and finds nothing more.
	again, err := forget.Forget(ctx, w.pool, anaEmail, forget.Options{Now: func() time.Time { return w.now }})
	w.must(err)
	if again.Total() != 0 || again.PersonID != nil {
		t.Fatalf("second run: %+v", again)
	}
}

// assertBlocked checks every way back in: a monitor signal, a form, an
// import, a reply, a manual add, and the single identity path.
func assertBlocked(t *testing.T, w *world) {
	t.Helper()
	ctx := w.ctx
	mustNoPerson := func(what string) {
		t.Helper()
		if n := w.count(`SELECT count(*) FROM people p JOIN identities i ON i.person_id = p.id WHERE i.value IN ($1, $2, $3)`, anaEmail, anaAlt, anaGitHub); n != 0 {
			t.Fatalf("%s recreated her", what)
		}
		if hits := dumpHits(t, w.pool, anaTerms...); len(hits) > 0 {
			t.Fatalf("%s stored her data: %v", what, hits)
		}
	}
	// A monitor finds her GitHub login again.
	res, err := w.sig.Ingest(ctx, &signal.Signal{Product: "demo", Monitor: "gh", Type: "github.issue_pain", OccurredAt: w.now, Strength: 50,
		Subject: signal.Subject{GitHub: strings.ToUpper(anaGitHub)}, Title: "new issue", DedupeKey: "gh:2"})
	w.must(err)
	if !res.Forgotten || res.PersonID != nil {
		t.Fatalf("monitor signal: %+v", res)
	}
	mustNoPerson("a monitor")
	if n := w.count(`SELECT count(*) FROM signals WHERE dedupe_key = 'gh:2'`); n != 0 {
		t.Fatal("the dropped signal was stored")
	}
	// A CSV import.
	rows, _ := people.ReadCSV(strings.NewReader("Email,Name\n" + strings.ToUpper(anaEmail) + ",Someone\nnew@x.example,New\n"))
	imp, err := people.Import(ctx, w.pool, "demo", rows)
	w.must(err)
	if imp.Created != 1 || len(imp.Skipped) != 1 {
		t.Fatalf("import: %+v", imp)
	}
	mustNoPerson("an import")
	// A reply from the same address.
	ok, err := w.out.RecordReply(ctx, outreach.Inbound{Mailbox: "me@demo-mail.example", ProviderID: "r-again", From: anaEmail, Subject: "hello?", Body: "still there", ReceivedAt: w.now})
	w.must(err)
	if ok || w.count(`SELECT count(*) FROM replies WHERE provider_message_id = 'r-again'`) != 0 {
		t.Fatal("a reply from a forgotten address was stored")
	}
	// A bounce naming her.
	ok, err = w.out.RecordReply(ctx, outreach.Inbound{Mailbox: "me@demo-mail.example", ProviderID: "b-again", From: "mailer-daemon@x.example", Bounce: true,
		FailedRecipients: []string{anaEmail}, ReceivedAt: w.now})
	w.must(err)
	if ok {
		t.Fatal("a bounce for a forgotten address was stored")
	}
	// The operator adding her to the list by hand is refused.
	if _, err := w.upd.AddByEmail(ctx, "demo", anaEmail, anaName, "manual"); err == nil {
		t.Fatal("AddByEmail accepted a forgotten address")
	}
	// The one identity path.
	bo, _ := people.FindByEmail(ctx, w.pool, "bo@gmail.example")
	if err := people.LinkIdentity(ctx, w.pool, "email", anaAlt, bo.ID, people.SourceManual, true); !errors.Is(err, people.ErrForgotten) {
		t.Fatalf("LinkIdentity: %v", err)
	}
	// She submits the form herself again: dropped, and one task says so.
	w.nf.mu.Lock()
	w.nf.subs = append(w.nf.subs, map[string]any{"id": "s-ana-2", "created_at": w.now.Add(-time.Minute).Format(time.RFC3339),
		"data": map[string]any{"email": anaEmail, "name": anaName, "message": "Please do get in touch"}})
	w.nf.mu.Unlock()
	for range 2 { // a second poll overlapping the first makes no second task
		w.must(func() error {
			_, err := w.sig.RunMonitor(ctx, &signal.Sources{}, "demo", "request-demo", w.now.Add(-time.Hour))
			return err
		}())
	}
	mustNoPerson("a form")
	var task string
	if n := w.count(`SELECT count(*) FROM tasks WHERE next_action LIKE 'A forgotten person wrote again%'`); n != 1 {
		t.Fatalf("operator tasks about the return: %d", n)
	}
	w.must(w.pool.QueryRow(ctx, `SELECT next_action FROM tasks WHERE next_action LIKE 'A forgotten person wrote again%'`).Scan(&task))
	if !strings.Contains(task, "request-demo") || !strings.Contains(task, "s-ana-2") || !strings.Contains(task, "decide whether to accept") {
		t.Fatalf("task: %q", task)
	}
	if w.count(`SELECT count(*) FROM form_submissions WHERE external_id = 's-ana-2'`) != 0 {
		t.Fatal("the new submission was stored")
	}
}

// tableCounts is the row count of every table, for comparing before and after.
func tableCounts(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE' AND table_name NOT LIKE 'river%' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	out := map[string]int{}
	for _, n := range names {
		var c int
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %q`, n)).Scan(&c); err != nil {
			t.Fatal(err)
		}
		out[n] = c
	}
	return out
}

func TestNetlifyFailureStillErasesLocallyAndListsRemoteCopies(t *testing.T) {
	w := newWorld(t)
	ctx := w.ctx
	w.pollForms()
	w.nf.failDelete["s-ana"] = true

	rep, err := forget.Forget(ctx, w.pool, anaEmail, forget.Options{Netlify: w.intake.Netlify, Now: func() time.Time { return w.now }})
	w.must(err)
	if len(rep.RemoteLeft) != 1 || rep.RemoteLeft[0].ID != "s-ana" || rep.RemoteLeft[0].Form != "request-demo" || !strings.Contains(rep.RemoteLeft[0].Error, "500") {
		t.Fatalf("remote copies left: %+v", rep.RemoteLeft)
	}
	if len(rep.RemoteDeleted) != 0 {
		t.Fatalf("deleted: %+v", rep.RemoteDeleted)
	}
	if hits := dumpHits(t, w.pool, anaTerms...); len(hits) > 0 {
		t.Fatalf("local data remains after a Netlify failure: %v", hits)
	}
	if rep.Deleted["form_submissions"] != 1 || rep.Deleted["people"] != 1 {
		t.Fatalf("local erase: %v", rep.Deleted)
	}
}

func TestNoNetlifyTokenLeavesRemoteCopiesListed(t *testing.T) {
	w := newWorld(t)
	w.pollForms()
	rep, err := forget.Forget(w.ctx, w.pool, anaEmail, forget.Options{Netlify: &forms.Netlify{}, Now: func() time.Time { return w.now }})
	w.must(err)
	if len(rep.RemoteLeft) != 1 || !strings.Contains(rep.RemoteLeft[0].Error, "ASTROLABE_NETLIFY_TOKEN") {
		t.Fatalf("remote copies left: %+v", rep.RemoteLeft)
	}
	if len(w.nf.deletes()) != 0 {
		t.Fatal("deleted without a token")
	}
	if w.count(`SELECT count(*) FROM people p JOIN identities i ON i.person_id = p.id WHERE i.value = $1`, anaEmail) != 0 {
		t.Fatal("local erase did not happen")
	}
}

func TestForgetByIDAndUnknownInput(t *testing.T) {
	w := newWorld(t)
	w.pollForms()
	ana, _ := people.FindByEmail(w.ctx, w.pool, anaEmail)
	if _, err := forget.Forget(w.ctx, w.pool, "999999", forget.Options{}); err == nil {
		t.Fatal("unknown id accepted")
	}
	if _, err := forget.Forget(w.ctx, w.pool, "not an address", forget.Options{}); err == nil {
		t.Fatal("garbage accepted")
	}
	rep, err := forget.Forget(w.ctx, w.pool, fmt.Sprint(ana.ID), forget.Options{Netlify: w.intake.Netlify})
	w.must(err)
	if rep.Deleted["people"] != 1 {
		t.Fatalf("%+v", rep)
	}
	if gone, _ := people.IsForgotten(w.ctx, w.pool, "email", anaEmail); !gone {
		t.Fatal("no tombstone for the id route")
	}
}

// An address with no person row (data left by an earlier partial run or
// kept in a copied row) is still swept and tombstoned.
func TestForgetAddressWithoutPersonSweepsLeftovers(t *testing.T) {
	w := newWorld(t)
	w.exec(`INSERT INTO replies (provider_message_id, mailbox, from_addr, body, received_at) VALUES ('orphan', 'm', $1, 'hi', now())`, anaEmail)
	rep, err := forget.Forget(w.ctx, w.pool, anaEmail, forget.Options{Now: func() time.Time { return w.now }})
	w.must(err)
	if rep.PersonID != nil || rep.Deleted["replies"] != 1 || rep.Tombstones != 1 {
		t.Fatalf("%+v", rep)
	}
}

// Sends to an erased person must keep counting toward the daily cap.
func TestErasedSendsStillCountTowardTheDailyCap(t *testing.T) {
	w := newWorld(t) // cold_per_day is 3
	ctx := w.ctx
	var rows []people.Row
	for _, e := range []string{"a1", "a2", "a3", "a4"} {
		rows = append(rows, people.Row{Email: e + "@acme.example", Name: "Pat " + e})
	}
	_, err := people.Import(ctx, w.pool, "demo", rows)
	w.must(err)
	var ids []int64
	for _, e := range []string{"a1", "a2", "a3", "a4"} {
		p, _ := people.FindByEmail(ctx, w.pool, e+"@acme.example")
		ids = append(ids, p.ID)
	}
	_, err = w.out.Enroll(ctx, "demo", "intro", ids)
	w.must(err)
	_, err = w.out.Tick(ctx)
	w.must(err)
	drafts, _ := w.out.Actions(ctx, "demo", "draft")
	for _, d := range drafts[:3] {
		w.must(w.out.Approve(ctx, d.ID))
		out, err := w.out.SendAction(ctx, d.ID)
		w.must(err)
		if out.Retry > 0 {
			t.Fatalf("held early: %+v", out)
		}
	}
	// Erase one of the three; the cap of 3 is still used up.
	var who string
	w.must(w.pool.QueryRow(ctx, `SELECT recipient FROM actions WHERE id = $1`, drafts[0].ID).Scan(&who))
	if _, err := forget.Forget(ctx, w.pool, who, forget.Options{Now: func() time.Time { return w.now }}); err != nil {
		t.Fatal(err)
	}
	w.must(w.out.Approve(ctx, drafts[3].ID))
	out, err := w.out.SendAction(ctx, drafts[3].ID)
	w.must(err)
	if out.Retry == 0 {
		t.Fatalf("erasing a send freed room under the daily cap: %+v", out)
	}
}

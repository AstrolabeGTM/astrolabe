package outreach_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

// githubOnly adds a person known only from GitHub: a login and a profile
// email found by enrichment.
func (w *world) githubOnly(login string) int64 {
	w.t.Helper()
	ctx := context.Background()
	var id int64
	w.must(w.s.Pool.QueryRow(ctx, `INSERT INTO people (name, first_name) VALUES ($1, $1) RETURNING id`, login).Scan(&id))
	w.must(people.LinkIdentity(ctx, w.s.Pool, "github", login, id, people.SourceGitHub, false))
	w.must(people.LinkIdentity(ctx, w.s.Pool, "email", login+"@users.example", id, people.SourceGitHub, false))
	return id
}

func (w *world) tick() {
	w.t.Helper()
	_, err := w.s.Tick(context.Background())
	w.must(err)
}

func (w *world) exec(sql string, args ...any) {
	w.t.Helper()
	_, err := w.s.Pool.Exec(context.Background(), sql, args...)
	w.must(err)
}

func (w *world) enrollGitHub(id int64, seq string) outreach.EnrollResult {
	w.t.Helper()
	r, err := w.s.Enroll(context.Background(), "demo", seq, []int64{id})
	w.must(err)
	return r
}

func TestColdDraftKeepsPlainOfferLinkWhenNotPersonal(t *testing.T) {
	off := false
	w := newWorldWith(t, 1, func(p *product.Product) {
		p.Links.Personal = &off
		p.Sequences["intro"].Steps[0].Body = "Try it: {{.Link}}"
	})
	w.s.PublicURL = "https://go.example"
	d := w.enrollAndDraft()
	if len(d) != 1 || !strings.Contains(d[0].Body, "https://demo.example/start") || strings.Contains(d[0].Body, "go.example") || d[0].LinkCode != nil {
		t.Fatalf("draft must keep the plain offer link: %+v", d)
	}
	var n int
	w.must(w.s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM links`).Scan(&n))
	if n != 0 {
		t.Fatalf("%d tracked links created", n)
	}
}

func TestPersonalLinkIsDefault(t *testing.T) {
	w := newWorldWith(t, 1, func(p *product.Product) { p.Sequences["intro"].Steps[0].Body = "Try it: {{.Link}}" })
	w.s.PublicURL = "https://go.example"
	d := w.enrollAndDraft()
	if len(d) != 1 || !strings.Contains(d[0].Body, "https://go.example/") {
		t.Fatalf("default keeps tracked links: %+v", d)
	}
}

func TestGitHubOnlyPersonIsRefusedAnyEmailSequence(t *testing.T) {
	w := newWorld(t, 0)
	ctx := context.Background()
	id := w.githubOnly("octo")
	for _, seq := range []string{"intro", "nudge"} {
		r := w.enrollGitHub(id, seq)
		if len(r.Enrolled) != 0 || len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0], "GitHub") || !strings.Contains(r.Skipped[0], "Acceptable Use") {
			t.Fatalf("%s: want refusal naming the policy, got %+v", seq, r)
		}
	}
	ok, reason, err := outreach.MayEmail(ctx, w.s.Pool, "demo", id)
	w.must(err)
	if ok || reason == "" {
		t.Fatalf("MayEmail = %v %q", ok, reason)
	}
}

func TestGitHubOnlyPersonMayHaveManualTaskSequence(t *testing.T) {
	w := newWorldWith(t, 0, func(p *product.Product) {
		p.Sequences["thread"] = &product.Sequence{ID: "thread", Kind: "cold",
			Steps: []product.Step{{Channel: "linkedin_task", After: "0d", Goal: "help in the thread"}}, StopOn: []string{"replied"}}
	})
	id := w.githubOnly("octo")
	if r := w.enrollGitHub(id, "thread"); len(r.Enrolled) != 1 {
		t.Fatalf("a task-only sequence needs no email: %+v", r)
	}
}

func TestAddressTheyGaveThemselvesCanBeEmailed(t *testing.T) {
	for _, src := range []string{people.SourceForm, people.SourceEvent, people.SourceImport, people.SourceManual, people.SourceStripe} {
		t.Run(src, func(t *testing.T) {
			w := newWorld(t, 0)
			ctx := context.Background()
			id := w.githubOnly("octo")
			if r := w.enrollGitHub(id, "intro"); len(r.Enrolled) != 0 {
				t.Fatalf("enrolled before they gave an address: %+v", r)
			}
			w.must(people.LinkIdentity(ctx, w.s.Pool, "email", "octo@own.example", id, src, true))
			r := w.enrollGitHub(id, "intro")
			if len(r.Enrolled) != 1 {
				t.Fatalf("want enrolled, got %+v", r)
			}
			w.tick()
			d, _ := w.s.Actions(ctx, "demo", "draft")
			if len(d) != 1 || d[0].Recipient != "octo@own.example" {
				t.Fatalf("must write to the address they gave, not the GitHub one: %+v", d)
			}
		})
	}
}

func TestGivingTheGitHubAddressUpgradesIt(t *testing.T) {
	w := newWorld(t, 0)
	ctx := context.Background()
	id := w.githubOnly("octo")
	// Same address, now supplied by a form submission.
	w.must(people.LinkIdentity(ctx, w.s.Pool, "email", "octo@users.example", id, people.SourceForm, true))
	if ok, _, _ := outreach.MayEmail(ctx, w.s.Pool, "demo", id); !ok {
		t.Fatal("an address they submitted themselves must count")
	}
	// A tracked-link click re-presents the address but is not consent.
	id2 := w.githubOnly("other")
	w.must(people.LinkIdentity(ctx, w.s.Pool, "email", "other@users.example", id2, people.SourceEvent, false))
	if ok, _, _ := outreach.MayEmail(ctx, w.s.Pool, "demo", id2); ok {
		t.Fatal("non-upgrading link must not change the source")
	}
}

func TestReplyMakesTheAddressTheirs(t *testing.T) {
	w := newWorld(t, 1)
	ctx := context.Background()
	// pa@acme.example was imported; a GitHub-only person who was emailed
	// before the guard replies from their GitHub profile address.
	id := w.githubOnly("octo")
	enroll := func() int64 {
		var eid int64
		w.must(w.s.Pool.QueryRow(ctx, `
			INSERT INTO enrollments (product_id, person_id, sequence_id, next_due_at, started_at, next_step)
			VALUES ('demo', $1, 'intro', now() + interval '1 day', now(), 1) RETURNING id`, id).Scan(&eid))
		return eid
	}
	eid := enroll()
	var aid int64
	w.must(w.s.Pool.QueryRow(ctx, `
		INSERT INTO actions (enrollment_id, step, product_id, person_id, channel, sender, recipient, subject, body, goal, status, thread_id, message_id, sent_at)
		VALUES ($1, 0, 'demo', $2, 'email', 'me@demo-mail.example', 'octo@users.example', 'Hi', 'Hello', 'intro', 'sent', 't9', '<m9@demo-mail.example>', now()) RETURNING id`,
		eid, id).Scan(&aid))
	_, err := w.s.RecordReply(ctx, outreach.Inbound{Mailbox: "me@demo-mail.example", ProviderID: "rr", ThreadID: "t9",
		From: "octo@users.example", Subject: "Re: Hi", Body: "Sure, tell me more", ReceivedAt: w.now})
	w.must(err)
	if ok, reason, err := outreach.MayEmail(ctx, w.s.Pool, "demo", id); err != nil || !ok {
		t.Fatalf("a person who wrote to us can be answered: %v %q %v", ok, reason, err)
	}
}

func TestSendRecheckHoldsGitHubOnlyMessage(t *testing.T) {
	w := newWorld(t, 1)
	ctx := context.Background()
	d := w.enrollAndDraft()[0]
	w.must(w.s.Approve(ctx, d.ID))
	// The person's only non-GitHub address goes away (or the draft predates
	// the guard): they are GitHub-only at send time.
	w.exec(`UPDATE identities SET source = 'github' WHERE person_id = $1`, d.PersonID)
	out := w.send(d.ID)
	if out.Status != "blocked" || !strings.Contains(out.Reason, "GitHub") || len(w.sender.sent) != 0 {
		t.Fatalf("must hold with a reason, got %+v sent=%d", out, len(w.sender.sent))
	}
	a, _ := w.s.Action(ctx, d.ID)
	if a.Status != "blocked" || !strings.Contains(a.Error, "Acceptable Use") {
		t.Fatalf("reason must be visible on the action: %+v", a)
	}
}

func TestEmailFromGitHubOptIn(t *testing.T) {
	w := newWorldWith(t, 0, func(p *product.Product) { p.Outreach.EmailFromGitHub = true })
	id := w.githubOnly("octo")
	if r := w.enrollGitHub(id, "intro"); len(r.Enrolled) != 1 {
		t.Fatalf("opted-in product may email GitHub-found addresses: %+v", r)
	}
}

func TestEnrolledBeforeGuardIsBlockedAtDraft(t *testing.T) {
	w := newWorldWith(t, 0, func(p *product.Product) { p.Outreach.EmailFromGitHub = true })
	ctx := context.Background()
	id := w.githubOnly("octo")
	w.enrollGitHub(id, "intro")
	// The operator turns the opt-in off again.
	w.exec(`UPDATE products SET config = jsonb_set(config, '{outreach,email_from_github}', 'false') WHERE id = 'demo'`)
	w.now = w.now.Add(time.Hour)
	w.tick()
	b, err := w.s.Actions(ctx, "demo", "blocked")
	w.must(err)
	if len(b) != 1 || !strings.Contains(b[0].Error, "Acceptable Use") {
		t.Fatalf("want one blocked draft with the policy reason: %+v", b)
	}
}

func TestAnswerToGitHubOnlyReplyIsRefused(t *testing.T) {
	w := newWorld(t, 1)
	ctx := context.Background()
	d := w.enrollAndDraft()[0]
	w.must(w.s.Approve(ctx, d.ID))
	w.send(d.ID)
	w.must2(w.s.RecordReply(ctx, outreach.Inbound{Mailbox: "me@demo-mail.example", ProviderID: "r1", ThreadID: "thread-1",
		From: "pa@acme.example", Subject: "Re: Hi Pat", Body: "ok", ReceivedAt: w.now}))
	r, _ := w.s.Replies(ctx, false, 5)
	w.exec(`UPDATE identities SET source = 'github' WHERE person_id = $1`, d.PersonID)
	if _, err := w.s.AnswerReply(ctx, r[0].ID, "Thanks"); err == nil || !strings.Contains(err.Error(), "GitHub") {
		t.Fatalf("answer must be refused with the reason, got %v", err)
	}
}

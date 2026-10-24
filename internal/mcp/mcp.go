// Package mcp exposes Astrolabe to Claude as MCP tools over stdio. The tools
// call the same outreach service as the web app, so every rule (approval of
// exact text, suppression, limits, pauses) applies to chat too.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AstrolabeGTM/astrolabe/internal/content"
	"github.com/AstrolabeGTM/astrolabe/internal/doctor"
	"github.com/AstrolabeGTM/astrolabe/internal/draft"
	"github.com/AstrolabeGTM/astrolabe/internal/forms"
	"github.com/AstrolabeGTM/astrolabe/internal/funnel"
	"github.com/AstrolabeGTM/astrolabe/internal/gmail"
	"github.com/AstrolabeGTM/astrolabe/internal/mail"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/render"
	"github.com/AstrolabeGTM/astrolabe/internal/sentmail"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
	"github.com/AstrolabeGTM/astrolabe/internal/updates"
	"github.com/AstrolabeGTM/astrolabe/internal/views"
)

type Server struct {
	S           *outreach.Service
	Signals     *signal.Service
	Sources     *signal.Sources // nil disables run_monitor
	Writer      *draft.Writer   // nil or disabled without an API key
	Studio      *content.Studio
	Render      *render.Client              // optional Chromium sidecar for creatives; nil or unconfigured is fine
	Updates     *updates.Service            // nil disables the updates list tools
	Netlify     *forms.Netlify              // deletes Netlify copies in forget_person; nil lists them as left
	Reload      func(context.Context) error // re-read product folders
	Mode        string                      // sandbox or live
	ProductsDir string
	Products    func() []*product.Product
	// LoadIssues lists product folders that failed to load, and why.
	LoadIssues func() []views.LoadIssue
	// Mail and Accounts are the sending mailboxes (check_sent, setup).
	Mail     *mail.Store
	Accounts *gmail.Accounts
	// Doctor runs the setup checks (astrolabe doctor); connect also tests
	// logins, keys and DNS.
	Doctor func(ctx context.Context, connect bool) []doctor.Check
}

// Run serves MCP over stdin/stdout until the client disconnects.
func (s *Server) Run(ctx context.Context, version string) error {
	return s.Build(version).Run(ctx, &sdk.StdioTransport{})
}

func (s *Server) Build(version string) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "astrolabe", Version: version}, &sdk.ServerOptions{
		Instructions: `Astrolabe is go-to-market for developers: it finds people who need your product, drafts outreach you approve, and measures what converts. Each product is a folder of YAML + Markdown (products/<id>/).

The operator may use developer or sales words; both mean the same things here. Answer in whichever vocabulary they use.
Sales term -> Astrolabe term: lead/contact/prospect -> person; ICP/persona -> target and fit rules; lead scoring/MQL -> score/priority; intent data/buying signal -> signal; cadence/drip/email campaign/nurture -> sequence; enroll -> start_sequence; play/playbook/workflow -> automation; suppress/unsubscribe/DNC -> do_not_contact; lifecycle stage -> funnel stage; ad campaign/flight/insertion order/launch plan -> campaign (a file in campaigns/ with a budget cap, dates, stop rule and gate checklist); creative review/ad creative -> creative (copy plus an HTML template, rendered at each size, approved by hash); outbound -> kind cold; lifecycle/retention messaging -> kind users; brief/account research -> research; disposition -> label_reply or close_task outcome.

Words, in developer terms:
- signal: an event about a person (a GitHub issue they opened, a star, an HN post, a product event). Stored once per dedupe key.
- score: priority 0-100 = 0.4 fit + 0.4 intent + 0.2 why_now; tier A (>=70, contact now) to D. fit = how well they match product.yaml "fit" rules; intent = recent signals, decaying over 10 days; why_now = time-sensitive signals in the last 14 days.
- sequence: a list of timed message steps (sequences/*.yaml); kind cold (people who don't know you) or users (existing users).
- automation: trigger -> when -> action rule in automations.yaml (like a CI workflow): e.g. on a github.issue_pain signal, if fit >= 50, start a sequence.
- stage: a step in the product's funnel (product.yaml "funnel"); activation is the first-success stage.
- do not contact: a person nobody will message again, across all products.

Typical session: inbox -> label replies -> handle due tasks -> write or review drafts (read product_context first) -> show the operator the exact text -> approve only what they approve.
To find people: top_prospects, then person. To see what's working: funnel, experiments, digest.
Drafts may already be written by the Claude API (ai_state "done"; "flags" lists problems): show flags.
If inbox reports mode "sandbox", nothing reaches real people: say "sent to the sandbox outbox", never "sent", and offer simulate_reply to try the rest of the flow.
Rules: use only claims from product_context.claims; never use never_claim phrases; approve sends exactly the saved text, so never approve without the operator's say-so.` + pipelineInstructions + notesInstructions + reviewInstructions + updatesInstructions + forgetInstructions,
	})

	// Products and setup
	register(srv, "product_setup", "Create products/<id>/ from a short interview: name, one-liner, who it's for, funnel stages, activation stage, one offer (promise, call to action, URL), sender mailboxes, never-claims, and (if the operator agreed them) claims and voice text. Ask for each before calling. Without claims/voice the files are STUB and the product won't load until filled in. Never overwrites.", s.productSetup)
	register(srv, "products", "List loaded products: offer, funnel stages, sequences and their steps.", s.products)
	register(srv, "setup", "Configuration as the operator checks it: loaded products with their folders, product folders that failed to load with each problem and its fix, sending mailboxes and whether each is connected, every pause switch (all sending, per product, per sequence) and webhook endpoints with whether their secret is set.", s.setup)
	register(srv, "doctor", "Run the setup checks (astrolabe doctor): database, mode, password, products, mailboxes, channel secrets, public URL, webhooks and tokens, each ok, a warning or a problem, with the fix. connect=true also tests mailbox logins, the GitHub token, the Claude key and SPF/DMARC, without sending anything.", s.doctor)
	register(srv, "reload_products", "Re-read the product folders (after editing product.yaml, sequences, automations or monitors) and report any that failed to load.", s.reloadProducts)
	register(srv, "status", "The instance at a glance: mode (sandbox or live), loaded products with their sequences, how many product folders failed to load, active pauses, and whether AI drafting is on.", s.status)
	register(srv, "product_context", "Everything needed to write for a product: one-liner, who it's for (target), offer, allowed claims (claims.md), never-claims and voice (voice.md). Call before drafting.", s.productContext)

	// People and signals
	register(srv, "top_prospects", "Highest-priority people for a product who aren't excluded, with the score breakdown (fit/intent/why_now) and whether they were already contacted. min_tier defaults to B.", s.hot)
	register(srv, "people", "List or search a product's people by name, email or company domain, best priority first: score (fit, intent, why now), sequences, and the outreach guard's reason if the product may not email them. total counts all matches; limit defaults to 100.", s.people)
	register(srv, "person", "Everything about one person (id or email): identities and where each came from, how they said they heard of you (reported_source, apart from first touch), conversation notes, facts, scores with breakdown, research notes, sequences, messages, replies, signals, open tasks, do-not-contact, and for each product whether the outreach guard lets it email them (and why not).", s.person)
	register(srv, "import_people", "Import people from CSV text with a header row. Columns: email, github, name, first_name, title, company, company_domain, notes, facts (';'-separated, e.g. dep:bullmq). Matches existing people by email or GitHub.", s.importPeople)
	register(srv, "import_companies", "Import companies (no people needed) from CSV text with a header row: domain, company (or name), facts (';'-separated lowercase kind:value, e.g. fact:sells_to_enterprise;dep:bullmq), notes. Other columns are ignored. Matches existing companies by domain, so re-importing is safe. Returns each company's fit against the product's fit rules.", s.importCompanies)
	register(srv, "set_company_facts", "Add kind:value facts to a company (id or domain) by hand; they count toward the company's fit and toward everyone who works there. Rescores.", s.setCompanyFacts)
	register(srv, "signals", "Recent signals (GitHub issues, stars, HN/Stack Overflow threads, product events, manual notes) and who they're about. Filter by product, type, days.", s.signals)
	register(srv, "add_signal", "Record an event about a person yourself (e.g. type=referral, strength 30). Finds or creates the person by email or GitHub login and rescores them.", s.addSignal)
	register(srv, "set_facts", "Add kind:value facts to a person (dep:bullmq, lang:go, team:5-50) that fit rules match on; rescores.", s.setFacts)
	register(srv, "ambiguous_matches", "Signals whose email/GitHub point at two different people. Nothing is guessed; pick one with resolve_match.", s.reviews)
	register(srv, "resolve_match", "Attach an ambiguous signal to one candidate person, or dismiss it (omit person_id).", s.resolveReview)
	register(srv, "monitors", "Status of each monitor in monitors.yaml: last run, new signals, errors.", s.monitors)
	register(srv, "run_monitor", "Run one monitor now and store what it finds. For a netlify_forms monitor this pulls new verified submissions from Netlify and takes each in (person, account, signal).", s.runMonitor)
	register(srv, "form_submissions", "Recent form submissions (netlify_forms and form_webhook monitors) with the person, company and pipeline account each created, the self-reported source (how they said they heard of you) and the campaign code.", s.formSubmissions)
	register(srv, "do_not_contact", "Put a person (id or email) on the do-not-contact list: no product will message them again. allow=true removes them.", s.doNotContact)

	// Sequences and messages
	register(srv, "start_sequence", "Start a sequence for people (by id). One active sequence per person per product; the first draft appears immediately. A person known only from GitHub is refused for any sequence with an email step (GitHub's Acceptable Use Policies); the reason is in `skipped`.", s.enroll)
	register(srv, "inbox", "Everything waiting on the operator: sends needing a decision, replies to label, due tasks, drafts, and queued sends.", s.inbox)
	register(srv, "today", "What is waiting on the operator, most urgent first: replies to label, drafts to approve (exact sender, recipient, text and flags), held, blocked, failed or unknown sends with their reason, and tasks due today. Each item has its age and its response target (replies 2 hours, drafts and held sends 1 day, tasks their due time) and whether it is past or near it; counts are complete even when the list is capped. Same as the browser app's Today screen.", s.today)
	register(srv, "check_sent", "For a send whose delivery is unknown: look for it in the sending mailbox's Sent folder by Message-ID. If found it is recorded as sent; otherwise nothing changes and the result says what to check by hand.", s.checkSent)
	register(srv, "write_draft", "Set a draft's text (email subject/body, or the note for a manual LinkedIn/X task). Any earlier approval is withdrawn.", s.writeDraft)
	register(srv, "ai_draft", "Have the Claude API write a draft now from claims, voice, research and the triggering signal. Returns it with flags; never sends.", s.aiDraft)
	register(srv, "approve", "Approve drafts exactly as saved. Emails queue for sending (still checked against do-not-contact, limits and pauses); manual tasks are marked done. Pass content_hashes (from inbox/today, one per id) to refuse a draft that changed after the operator saw it.", s.approve)
	register(srv, "skip", "Skip drafts; the sequence moves on to its next step.", s.skip)
	register(srv, "resolve_send", "For a send whose delivery is unknown or failed: sent=true records it as sent; sent=false puts it back in drafts for a fresh approval. Nothing is ever re-sent automatically.", s.resolve)
	register(srv, "label_reply", "Label a reply: interested, question, not_now, no, out_of_office, unsubscribe. no/unsubscribe add the person to do-not-contact; interested/question create a task; not_now creates a reminder (remind_on, default 30 days).", s.classify)
	register(srv, "answer_reply", "Save an answer to a reply as an email draft in the same thread; approve it to send.", s.answerReply)
	register(srv, "research", "Research notes on a person for a product via the Claude API: who they are, why now, fit, an angle, risks, with source links.", s.brief)
	register(srv, "add_task", "Add a to-do with one next action and a due date (YYYY-MM-DD), optionally about a person.", s.task)
	register(srv, "close_task", "Close a to-do with an outcome: activated, paid, wrong_fit, unclear_value, setup_blocked, price, later, other. activated/paid also record the funnel stage.", s.closeTask)
	register(srv, "record_stage", "Record that a person reached a funnel stage (e.g. installed). Sequences that stop on that stage stop.", s.markStage)
	register(srv, "pause", "Pause sending for global, product:<id> or sequence:<product>/<id>. Paused sends wait; nothing is dropped.", s.pause)
	register(srv, "resume", "Resume a paused scope.", s.resume)

	// Automations and results
	register(srv, "automations", "Automations per product (automations.yaml): trigger, when, action, and how many times each started a sequence, created a task or skipped.", s.plays)
	register(srv, "automation_runs", "Recent automation runs with outcome and the reason for skips.", s.playRuns)
	register(srv, "funnel", "Funnel report for the last N days: people per stage, median days between stages, who is stuck, weekly cohorts, what brought people in (first touch and automation), form requests by the source people reported, revenue, spend, task outcomes.", s.funnel)
	register(srv, "experiments", "A/B results per sequence step: sends, replies, reached goal with 95% intervals, and a verdict (says when there is too little data).", s.experiments)
	register(srv, "digest", "This week's summary per product: what moved, automations, experiments, stuck people, uncontacted A-tier people, biggest drop, and up to three concrete actions.", s.digestNow)
	register(srv, "counters", "Public counters from github_release monitors: release download counts per asset and in total, plus stars, forks, watchers, open issues and discussions, with the change over the last 7 days against the 7 before (both windows stated). A download count counts downloads of a release file, not installs or people. These numbers are never tied to a person and make no signals. Omit product for all.", s.counters)
	register(srv, "reception", "Public mentions of a product (HN, Stack Overflow, RSS, GitHub, or logged by hand) over the last days days (default 7) with counts by tone (positive, mixed, negative, unknown), how many ask a question, and the window's dates. Monitors with reception: true fill it. Omit product for all.", s.reception)
	register(srv, "log_mention", "Log a public mention of a product by hand: URL, optional title/excerpt, source (hn, stackoverflow, rss, github, manual, ...), date (YYYY-MM-DD, default today), tone and whether it asks a question. A URL already logged is kept once.", s.logMention)
	register(srv, "set_tone", "Set the tone of a logged mention by id: positive, mixed, negative or unknown.", s.setTone)
	register(srv, "content", "Turn release notes or a post into drafts for x, linkedin, hn, reddit, newsletter, producthunt, directories, each with a tracked link. launch=true also adds a posting task per platform.", s.content)
	register(srv, "content_list", "Recent content drafts and posts with click counts.", s.contentList)
	if s.Mode == "sandbox" {
		register(srv, "outbox", "Sandbox only: messages Astrolabe 'sent' (nothing left this machine).", s.outbox)
		register(srv, "simulate_reply", "Sandbox only: play the recipient and reply to an outbox message; runs the real reply handling (sequence stops, reply appears to label).", s.simulateReply)
		register(srv, "simulate_bounce", "Sandbox only: make an outbox email bounce; the person goes on do-not-contact.", s.simulateBounce)
	}
	register(srv, "content_mark", "Mark a content item posted (with its URL) or skipped.", s.contentMark)
	s.registerPipeline(srv) // account pipeline: pipeline.go
	s.registerNotes(srv)    // conversation notes: notes.go
	s.registerCampaigns(srv)
	s.registerUpdates(srv) // updates list: updates.go
	s.registerReview(srv)  // weekly review: review.go
	s.registerForget(srv)  // erase on request: forget.go
	return srv
}

// register adds a tool whose handler returns any JSON-able value.
// salesTerms lists what sales and marketing people call each tool, so a
// request in either vocabulary finds the right one. Names stay in plain
// developer terms; these go in the description.
var salesTerms = map[string]string{
	"product_setup":     "onboarding a product, defining ICP and positioning",
	"product_context":   "ICP, positioning, messaging, value props",
	"top_prospects":     "lead scoring, hot leads, MQLs, target accounts, ABM list",
	"people":            "leads, contacts, prospects, accounts",
	"person":            "lead/contact record, account research",
	"import_people":     "upload a lead list, import contacts",
	"import_companies":  "upload a target account list, ABM list, import accounts",
	"set_company_facts": "account firmographics, technographics, account research",
	"signals":           "intent data, buying signals, triggers",
	"add_signal":        "log an activity, referral, intent signal",
	"set_facts":         "firmographics, technographics, enrichment",
	"ambiguous_matches": "duplicate or unmatched leads, identity resolution",
	"resolve_match":     "merge or dedupe a lead",
	"monitors":          "intent/social listening sources",
	"run_monitor":       "refresh listening, pull new intent",
	"do_not_contact":    "suppress, unsubscribe, opt out, DNC list, blacklist",
	"start_sequence":    "enroll in a cadence/sequence/drip, start an email campaign or nurture",
	"inbox":             "tasks queue, approvals, rep inbox",
	"write_draft":       "edit email copy",
	"ai_draft":          "AI copywriting, personalize an email",
	"approve":           "send, launch the step",
	"skip":              "skip a cadence step",
	"label_reply":       "reply triage, disposition, sentiment",
	"answer_reply":      "respond to a lead",
	"research":          "account/lead research, brief, call prep",
	"add_task":          "follow-up task, reminder, next step",
	"close_task":        "log the outcome, disposition, closed won/lost",
	"record_stage":      "move a lead to a lifecycle/pipeline stage, conversion",
	"pause":             "pause an email campaign or cadence",
	"automations":       "plays, playbooks, workflows, triggers",
	"automation_runs":   "play/workflow history",
	"funnel":            "pipeline, conversion rates, attribution, revenue, CAC",
	"experiments":       "A/B tests, message testing",
	"digest":            "weekly report, pipeline review",
	"counters":          "download and star metrics, repo traction, adoption numbers",
	"reception":         "brand mentions, social listening, press and buzz, sentiment",
	"log_mention":       "log a brand mention, coverage or press hit",
	"set_tone":          "tag sentiment of a mention",
	"content":           "social posts, launch posts, content marketing",
}

func register[In any](srv *sdk.Server, name, desc string, f func(context.Context, In) (any, error)) {
	if aka := salesTerms[name]; aka != "" {
		desc += " (Sales terms: " + aka + ".)"
	}
	sdk.AddTool(srv, &sdk.Tool{Name: name, Description: desc},
		func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
			out, err := f(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return nil, out, nil
		})
}

type noArgs struct{}

type productArg struct {
	Product string `json:"product" jsonschema:"product id"`
}

type peopleArg struct {
	Product string `json:"product" jsonschema:"product id"`
	Search  string `json:"search,omitempty" jsonschema:"optional text to match"`
	Limit   int    `json:"limit,omitempty"`
}

type personArg struct {
	Person string `json:"person" jsonschema:"person id or email"`
}

type importArg struct {
	Product string `json:"product"`
	CSV     string `json:"csv" jsonschema:"CSV text including the header row"`
}

type enrollArg struct {
	Product   string  `json:"product"`
	Sequence  string  `json:"sequence"`
	PersonIDs []int64 `json:"person_ids"`
}

type inboxArg struct {
	Product string `json:"product,omitempty" jsonschema:"limit drafts to one product"`
}

type draftArg struct {
	ActionID int64  `json:"action_id"`
	Subject  string `json:"subject,omitempty" jsonschema:"email subject; leave empty to keep the current one"`
	Body     string `json:"body"`
}

type idsArg struct {
	ActionIDs []int64 `json:"action_ids"`
}

type approveArg struct {
	ActionIDs []int64 `json:"action_ids"`
	// ContentHashes, if given, holds one content_hash per action id: the
	// version the operator saw.
	ContentHashes []string `json:"content_hashes,omitempty" jsonschema:"optional: the content_hash of each draft as the operator saw it, in the order of action_ids; a draft that changed since is refused"`
}

type resolveArg struct {
	ActionID int64 `json:"action_id"`
	Sent     bool  `json:"sent"`
}

type classifyArg struct {
	ReplyID  int64  `json:"reply_id"`
	Class    string `json:"class"`
	RemindOn string `json:"remind_on,omitempty" jsonschema:"YYYY-MM-DD, for not_now"`
}

type taskArg struct {
	Product    string `json:"product"`
	PersonID   int64  `json:"person_id,omitempty"`
	NextAction string `json:"next_action"`
	Due        string `json:"due,omitempty" jsonschema:"YYYY-MM-DD; default today"`
}

type closeTaskArg struct {
	TaskID  int64  `json:"task_id"`
	Outcome string `json:"outcome"`
	Note    string `json:"note,omitempty"`
	Minutes *int   `json:"minutes,omitempty" jsonschema:"time I spent"`
}

type stageArg struct {
	Product string `json:"product"`
	Person  string `json:"person" jsonschema:"person id or email"`
	Stage   string `json:"stage"`
}

type funnelArg struct {
	Product string `json:"product"`
	Days    int    `json:"days,omitempty" jsonschema:"default 90"`
}

type scopeArg struct {
	Scope string `json:"scope"`
}

func (s *Server) find(id string) (*product.Product, error) {
	for _, p := range s.Products() {
		if p.ID == id {
			return p, nil
		}
	}
	var ids []string
	for _, p := range s.Products() {
		ids = append(ids, p.ID)
	}
	return nil, fmt.Errorf("no valid product %q (have %v)", id, ids)
}

func (s *Server) products(ctx context.Context, _ noArgs) (any, error) {
	type seq struct {
		ID    string         `json:"id"`
		Steps []product.Step `json:"steps"`
	}
	type out struct {
		ID        string        `json:"id"`
		Name      string        `json:"name"`
		OneLiner  string        `json:"one_liner"`
		Offer     product.Offer `json:"offer"`
		Funnel    []string      `json:"funnel"`
		Sequences []seq         `json:"sequences"`
	}
	var list []out
	for _, p := range s.Products() {
		o := out{ID: p.ID, Name: p.Name, OneLiner: p.OneLiner, Offer: p.Offer, Funnel: p.Funnel}
		for id, sq := range p.Sequences {
			o.Sequences = append(o.Sequences, seq{id, sq.Steps})
		}
		list = append(list, o)
	}
	return map[string]any{"products": list}, nil
}

func (s *Server) productContext(ctx context.Context, a productArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	read := func(f string) string {
		b, err := os.ReadFile(filepath.Join(p.Dir, f))
		if err != nil {
			return ""
		}
		return string(b)
	}
	return map[string]any{
		"name": p.Name, "url": p.URL, "one_liner": p.OneLiner, "audience": p.Audience, "target": p.Target,
		"offer": p.Offer, "claims": read(p.Claims.Allowed), "never_claim": p.Claims.Never, "voice": read(p.Voice),
		"objections": read("objections.md"),
	}, nil
}

func (s *Server) people(ctx context.Context, a peopleArg) (any, error) {
	if _, err := s.find(a.Product); err != nil {
		return nil, err
	}
	return views.GetPeople(ctx, s.S.Pool, a.Product, a.Search, a.Limit)
}

func (s *Server) resolvePerson(ctx context.Context, who string) (*people.Person, error) {
	if id, err := strconv.ParseInt(strings.TrimSpace(who), 10, 64); err == nil {
		return people.Get(ctx, s.S.Pool, id)
	}
	p, err := people.FindByEmail(ctx, s.S.Pool, who)
	if err != nil {
		return nil, fmt.Errorf("no person with id or email %q", who)
	}
	return p, nil
}

func (s *Server) person(ctx context.Context, a personArg) (any, error) {
	p, err := s.resolvePerson(ctx, a.Person)
	if err != nil {
		return nil, err
	}
	return views.GetPerson(ctx, s.S, s.Products(), p.ID, time.Now())
}

func (s *Server) importPeople(ctx context.Context, a importArg) (any, error) {
	if _, err := s.find(a.Product); err != nil {
		return nil, err
	}
	return views.ImportPeople(ctx, s.S.Pool, a.Product, a.CSV)
}

func (s *Server) enroll(ctx context.Context, a enrollArg) (any, error) {
	res, err := views.StartSequence(ctx, s.S, a.Product, a.Sequence, a.PersonIDs)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Server) inbox(ctx context.Context, a inboxArg) (any, error) {
	replies, err := s.S.Replies(ctx, true, 50)
	if err != nil {
		return nil, err
	}
	tasks, err := s.S.Tasks(ctx, true, 100)
	if err != nil {
		return nil, err
	}
	attention, err := s.S.Actions(ctx, a.Product, "unknown", "failed", "blocked")
	if err != nil {
		return nil, err
	}
	drafts, err := s.S.Actions(ctx, a.Product, "draft")
	if err != nil {
		return nil, err
	}
	queued, err := s.S.Actions(ctx, a.Product, "approved", "sending")
	if err != nil {
		return nil, err
	}
	pauses, err := s.S.Pauses(ctx)
	if err != nil {
		return nil, err
	}
	mode := s.Mode
	if mode == "" {
		mode = "live"
	}
	return map[string]any{"mode": mode, "replies_to_sort": replies, "tasks": tasks, "needs_decision": attention,
		"drafts": drafts, "queued": queued, "paused": pauses}, nil
}

func (s *Server) today(ctx context.Context, a inboxArg) (any, error) {
	t, err := views.GetToday(ctx, s.S, a.Product, time.Now())
	if err != nil {
		return nil, err
	}
	mode := s.Mode
	if mode == "" {
		mode = "live"
	}
	return map[string]any{"mode": mode, "today": t}, nil
}

func (s *Server) checkSent(ctx context.Context, a actionArg) (any, error) {
	return sentmail.Check(ctx, s.S, s.Mail, s.Accounts, a.ActionID)
}

func (s *Server) writeDraft(ctx context.Context, a draftArg) (any, error) {
	cur, err := s.S.Action(ctx, a.ActionID)
	if err != nil {
		return nil, err
	}
	subject := a.Subject
	if subject == "" {
		subject = cur.Subject
	}
	if err := s.S.Edit(ctx, a.ActionID, subject, strings.TrimSpace(a.Body)); err != nil {
		return nil, err
	}
	return s.S.Action(ctx, a.ActionID)
}

func (s *Server) approve(ctx context.Context, a approveArg) (any, error) {
	if len(a.ContentHashes) > 0 && len(a.ContentHashes) != len(a.ActionIDs) {
		return nil, errors.New("content_hashes must have one entry per action id")
	}
	seen := map[int64]string{}
	for i, h := range a.ContentHashes {
		seen[a.ActionIDs[i]] = h
	}
	res, err := each(a.ActionIDs, func(id int64) error { return s.S.ApproveSeen(ctx, id, seen[id]) })
	if m, ok := res.(map[string]any); ok && s.Mode == "sandbox" {
		m["note"] = "Sandbox mode: approved messages go to the local outbox, not to real people."
	}
	return res, err
}

func (s *Server) skip(ctx context.Context, a idsArg) (any, error) {
	return each(a.ActionIDs, func(id int64) error { return s.S.Skip(ctx, id) })
}

// each applies f to every id and reports per-id results; one failure does
// not stop the rest.
func each(ids []int64, f func(int64) error) (any, error) {
	if len(ids) == 0 {
		return nil, errors.New("action_ids is empty")
	}
	res := map[string]string{}
	for _, id := range ids {
		if err := f(id); err != nil {
			res[strconv.FormatInt(id, 10)] = "error: " + err.Error()
		} else {
			res[strconv.FormatInt(id, 10)] = "ok"
		}
	}
	return map[string]any{"results": res}, nil
}

func (s *Server) resolve(ctx context.Context, a resolveArg) (any, error) {
	return ok(s.S.Resolve(ctx, a.ActionID, a.Sent, nil))
}

func (s *Server) classify(ctx context.Context, a classifyArg) (any, error) {
	var remind *time.Time
	if a.RemindOn != "" {
		t, err := time.ParseInLocation("2006-01-02", a.RemindOn, time.Local)
		if err != nil {
			return nil, fmt.Errorf("remind_on: %w", err)
		}
		remind = &t
	}
	return ok(s.S.Classify(ctx, a.ReplyID, a.Class, remind))
}

func (s *Server) task(ctx context.Context, a taskArg) (any, error) {
	due := time.Now()
	if a.Due != "" {
		t, err := time.ParseInLocation("2006-01-02", a.Due, time.Local)
		if err != nil {
			return nil, fmt.Errorf("due: %w", err)
		}
		due = t.Add(9 * time.Hour)
	}
	var person *int64
	if a.PersonID != 0 {
		person = &a.PersonID
	}
	id, err := s.S.CreateTask(ctx, a.Product, person, a.NextAction, due)
	return map[string]any{"task_id": id}, err
}

func (s *Server) closeTask(ctx context.Context, a closeTaskArg) (any, error) {
	return ok(s.S.CloseTask(ctx, a.TaskID, a.Outcome, a.Note, a.Minutes))
}

func (s *Server) markStage(ctx context.Context, a stageArg) (any, error) {
	p, err := s.resolvePerson(ctx, a.Person)
	if err != nil {
		return nil, err
	}
	return ok(s.S.MarkStage(ctx, a.Product, p.ID, a.Stage, time.Now()))
}

func (s *Server) funnel(ctx context.Context, a funnelArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	if a.Days <= 0 {
		a.Days = 90
	}
	return funnel.BuildReport(ctx, s.S.Pool, p, time.Now().AddDate(0, 0, -a.Days), time.Now())
}

func (s *Server) pause(ctx context.Context, a scopeArg) (any, error) {
	return ok(s.S.Pause(ctx, a.Scope))
}

func (s *Server) resume(ctx context.Context, a scopeArg) (any, error) {
	return ok(s.S.Resume(ctx, a.Scope))
}

func ok(err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return map[string]string{"status": "ok"}, nil
}

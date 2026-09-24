// Package draft uses Claude to research people (briefs), write sequence
// steps, classify replies and draft answers. It never sends: its output
// lands as drafts that go through the same approval as my own text, and
// auto_approve settings decide whether an unflagged draft is approved for me.
package draft

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/astrolabe-gtm/astrolabe/internal/links"
	"github.com/astrolabe-gtm/astrolabe/internal/llm"
	"github.com/astrolabe-gtm/astrolabe/internal/outreach"
	"github.com/astrolabe-gtm/astrolabe/internal/people"
	"github.com/astrolabe-gtm/astrolabe/internal/product"
	"github.com/astrolabe-gtm/astrolabe/internal/score"
)

type Writer struct {
	Pool *pgxpool.Pool
	LLM  *llm.Client
	Out  *outreach.Service
}

const briefMaxAge = 14 * 24 * time.Hour

// ---- context gathering ----

type personContext struct {
	Person  *people.Person
	Facts   map[string]string
	Score   score.Score
	Signals []sig
}

type sig struct {
	ID         int64
	Type       string
	Title      string
	URL        string
	OccurredAt time.Time
	Data       map[string]any
}

func (w *Writer) gather(ctx context.Context, productID string, personID int64) (*personContext, error) {
	pc := &personContext{}
	var err error
	if pc.Person, err = people.Get(ctx, w.Pool, personID); err != nil {
		return nil, err
	}
	if pc.Facts, err = people.Facts(ctx, w.Pool, personID); err != nil {
		return nil, err
	}
	pc.Score, _, _ = score.Get(ctx, w.Pool, productID, personID)
	rows, err := w.Pool.Query(ctx, `
		SELECT id, type, title, evidence_url, occurred_at, data FROM signals
		WHERE product_id = $1 AND (person_id = $2 OR company_id = (SELECT company_id FROM people WHERE id = $2))
		ORDER BY occurred_at DESC LIMIT 10`, productID, personID)
	if err != nil {
		return nil, err
	}
	pc.Signals, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (sig, error) {
		var s sig
		return s, r.Scan(&s.ID, &s.Type, &s.Title, &s.URL, &s.OccurredAt, &s.Data)
	})
	return pc, err
}

func (pc *personContext) describe() string {
	var b strings.Builder
	p := pc.Person
	fmt.Fprintf(&b, "Name: %s\nTitle: %s\nCompany: %s (%s)\n", orUnknown(p.Name), orUnknown(p.Title), orUnknown(p.Company), orUnknown(p.Domain))
	if p.GitHub != "" {
		fmt.Fprintf(&b, "GitHub: https://github.com/%s\n", p.GitHub)
	}
	if p.Notes != "" {
		fmt.Fprintf(&b, "My notes: %s\n", p.Notes)
	}
	if len(pc.Facts) > 0 {
		keys := make([]string, 0, len(pc.Facts))
		for k := range pc.Facts {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if len(keys) > 40 {
			keys = keys[:40]
		}
		fmt.Fprintf(&b, "Facts: %s\n", strings.Join(keys, ", "))
	}
	if pc.Score.Tier != "" {
		fmt.Fprintf(&b, "Score: %s\n", pc.Score.Summary())
	}
	b.WriteString("Signals (newest first):\n")
	for _, s := range pc.Signals {
		ex, _ := s.Data["excerpt"].(string)
		fmt.Fprintf(&b, "- [%s] %s — %s (%s)", s.OccurredAt.Format("2006-01-02"), s.Type, s.Title, s.URL)
		if ex != "" {
			fmt.Fprintf(&b, "\n  excerpt: %s", ex)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func productBlock(p *product.Product) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PRODUCT: %s — %s (%s)\n", p.Name, p.OneLiner, p.URL)
	fmt.Fprintf(&b, "IDEAL CUSTOMER: %s\nNOT FOR: %s\n", strings.Join(p.Target.Include, "; "), strings.Join(p.Target.Exclude, "; "))
	fmt.Fprintf(&b, "OFFER: %s. Next step we ask for: %s (%s)\n", p.Offer.Promise, p.Offer.CTA, p.Offer.Destination)
	fmt.Fprintf(&b, "\nCLAIMS (the only product claims you may make):\n%s\n", p.ClaimsText)
	fmt.Fprintf(&b, "NEVER SAY: %s\n", strings.Join(p.Claims.Never, "; "))
	fmt.Fprintf(&b, "\nVOICE:\n%s\n", p.VoiceText)
	if strings.TrimSpace(p.ObjectionsText) != "" {
		fmt.Fprintf(&b, "\nOBJECTIONS AND HONEST ANSWERS:\n%s\n", p.ObjectionsText)
	}
	return b.String()
}

func (w *Writer) voiceExamples(ctx context.Context, productID string) string {
	rows, err := w.Pool.Query(ctx, `SELECT original, edited FROM voice_examples WHERE product_id = $1 ORDER BY id DESC LIMIT 4`, productID)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for rows.Next() {
		var o, e string
		if rows.Scan(&o, &e) == nil {
			fmt.Fprintf(&b, "\n--- A draft I rewrote ---\nDRAFT:\n%s\nMY VERSION:\n%s\n", o, e)
		}
	}
	rows.Close()
	if b.Len() == 0 {
		return ""
	}
	return "\nHOW I EDIT DRAFTS (match my version's style):" + b.String()
}

// ---- briefs ----

// Brief returns a research brief less than two weeks old, writing one if needed.
func (w *Writer) Brief(ctx context.Context, productID string, personID int64, force bool) (string, error) {
	var body string
	var at time.Time
	err := w.Pool.QueryRow(ctx, `SELECT body, created_at FROM briefs WHERE product_id = $1 AND person_id = $2 ORDER BY id DESC LIMIT 1`,
		productID, personID).Scan(&body, &at)
	if err == nil && !force && time.Since(at) < briefMaxAge {
		return body, nil
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	p, err := product.Get(ctx, w.Pool, productID)
	if err != nil {
		return "", err
	}
	pc, err := w.gather(ctx, productID, personID)
	if err != nil {
		return "", err
	}
	res, err := w.LLM.Complete(ctx, llm.Request{Purpose: "brief", Product: productID, MaxTokens: 900,
		System: "You research prospects for a founder doing their own sales. Be factual and brief. Use only the information given; say \"unknown\" instead of guessing. Cite sources as URLs from the input.",
		Prompt: productBlock(p) + "\nPERSON:\n" + pc.describe() + `
Write a research brief in plain text with these headings:
Who: role and company in one line.
Why now: the specific signal(s) that make this timely, with URLs.
Fit: how the ideal-customer description applies or doesn't, from the facts.
Angle: the one specific observation a message should open with.
Risks: reasons not to contact or things to avoid.
Keep it under 180 words.`})
	if err != nil {
		return "", err
	}
	_, err = w.Pool.Exec(ctx, `INSERT INTO briefs (product_id, person_id, body, llm_call_id) VALUES ($1, $2, $3, $4)`,
		productID, personID, strings.TrimSpace(res.Text), res.CallID)
	return strings.TrimSpace(res.Text), err
}

// ---- sequence steps ----

var subjectLine = regexp.MustCompile(`(?i)^\s*subject:\s*(.+)$`)

// DraftAction writes the text of a pending draft and, if auto_approve allows
// and nothing was flagged, approves it.
func (w *Writer) DraftAction(ctx context.Context, actionID int64) error {
	a, err := w.Out.Action(ctx, actionID)
	if err != nil {
		return err
	}
	if a.Status != "draft" || a.AIState != "pending" {
		return nil
	}
	p, err := product.Get(ctx, w.Pool, a.ProductID)
	if err != nil {
		return err
	}
	seq, err := product.GetSequence(ctx, w.Pool, a.ProductID, a.SequenceID)
	if err != nil {
		return err
	}
	var st product.Step
	if a.Step >= 0 && a.Step < len(seq.Steps) {
		st = seq.Steps[a.Step]
	}
	pc, err := w.gather(ctx, a.ProductID, a.PersonID)
	if err != nil {
		return err
	}
	brief := ""
	if st.Brief {
		if brief, err = w.Brief(ctx, a.ProductID, a.PersonID, false); err != nil {
			return err
		}
	}
	var trigger *sig
	var signalID *int64
	w.Pool.QueryRow(ctx, `SELECT signal_id FROM enrollments WHERE id = $1`, a.EnrollmentID).Scan(&signalID)
	for i := range pc.Signals {
		if signalID != nil && pc.Signals[i].ID == *signalID {
			trigger = &pc.Signals[i]
		}
	}
	history := w.thread(ctx, a.EnrollmentID)

	var task strings.Builder
	fmt.Fprintf(&task, "Write step %d of the %q sequence. Channel: %s. Goal of this step: %s.\n", a.Step+1, a.SequenceID, a.Channel, a.Goal)
	if trigger != nil {
		fmt.Fprintf(&task, "It was triggered by this signal; refer to it specifically: %s — %s (%s)\n", trigger.Type, trigger.Title, trigger.URL)
	}
	if history != "" {
		fmt.Fprintf(&task, "\nEARLIER MESSAGES IN THIS SEQUENCE (they have not replied):\n%s\n", history)
	}
	if brief != "" {
		fmt.Fprintf(&task, "\nRESEARCH BRIEF:\n%s\n", brief)
	}
	needSubject := a.Channel == "email" && a.ThreadID == "" && a.Subject == ""
	switch {
	case a.Channel == "push" || a.Channel == "in_app" || a.Channel == "webhook":
		task.WriteString("\nThis is an in-app/push notification body for an existing user. Under 120 characters, specific, no urgency about money. Reply with the text only.")
	case product.ManualChannel(a.Channel):
		task.WriteString("\nThis is a note I will send by hand (LinkedIn/X). Under 300 characters. Reply with the note text only.")
	case needSubject:
		task.WriteString("\nPlain-text email under 120 words, no links except the offer destination if needed. Reply exactly as:\nSubject: <subject, under 8 words, lowercase is fine>\n\n<body, signed with just my first name>")
	default:
		task.WriteString("\nPlain-text email under 90 words that continues the thread. Reply with the body only, signed with just my first name.")
	}

	res, err := w.LLM.Complete(ctx, llm.Request{Purpose: "draft", Product: a.ProductID, MaxTokens: 700,
		System: "You write short, specific, honest outreach for a technical founder. One observation, one clear ask. No flattery, no hype, no placeholders like [Name]. Only use the product claims given. If you lack information, write less rather than inventing.",
		Prompt: productBlock(p) + w.voiceExamples(ctx, a.ProductID) + "\nPERSON:\n" + pc.describe() + "\nTASK:\n" + task.String()})
	if err != nil {
		return err
	}
	subject, body := "", strings.TrimSpace(res.Text)
	if needSubject {
		first, rest, _ := strings.Cut(body, "\n")
		if m := subjectLine.FindStringSubmatch(first); m != nil {
			subject, body = strings.TrimSpace(m[1]), strings.TrimSpace(rest)
		}
	}
	// Point the offer link at this message's tracked link.
	if a.LinkCode != nil && w.Out.PublicURL != "" && p.Offer.Destination != "" {
		body = strings.ReplaceAll(body, p.Offer.Destination, links.URL(w.Out.PublicURL, *a.LinkCode))
	}
	flags := Check(p, subject+"\n"+body, trigger)
	if needSubject && subject == "" {
		flags = append(flags, "no subject line")
	}
	filled, err := w.Out.FillDraft(ctx, actionID, subject, body, flags)
	if err != nil || !filled {
		return err
	}
	_, err = w.Out.ApproveIfAllowed(ctx, actionID)
	return err
}

func (w *Writer) thread(ctx context.Context, enrollmentID int64) string {
	rows, err := w.Pool.Query(ctx, `SELECT channel, subject, body FROM actions WHERE enrollment_id = $1 AND status = 'sent' AND kind = 'step' ORDER BY step`, enrollmentID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var ch, subj, body string
		if rows.Scan(&ch, &subj, &body) == nil {
			fmt.Fprintf(&b, "[%s] %s\n%s\n---\n", ch, subj, body)
		}
	}
	return b.String()
}

var placeholder = regexp.MustCompile(`\[[A-Z][A-Za-z ]+\]|\{\{|<[a-z_ ]+>|\bTODO\b`)

// Check lists problems in a draft: forbidden claims, leftover placeholders,
// no reference to the triggering signal, excessive length.
func Check(p *product.Product, text string, trigger *sig) []string {
	flags := []string{}
	lower := strings.ToLower(text)
	for _, never := range p.Claims.Never {
		if strings.Contains(lower, strings.ToLower(never)) {
			flags = append(flags, "uses a never-claim: "+never)
		}
	}
	if placeholder.MatchString(text) {
		flags = append(flags, "has a placeholder")
	}
	if trigger != nil && !mentions(lower, trigger) {
		flags = append(flags, "doesn't reference the signal it was triggered by")
	}
	if n := len(strings.Fields(text)); n > 200 {
		flags = append(flags, fmt.Sprintf("long (%d words)", n))
	}
	return flags
}

var numberRef = regexp.MustCompile(`#\d+`)

func mentions(lower string, s *sig) bool {
	if repo, _ := s.Data["repo"].(string); repo != "" {
		_, name, _ := strings.Cut(strings.ToLower(repo), "/")
		if strings.Contains(lower, strings.ToLower(repo)) || (len(name) > 3 && strings.Contains(lower, name)) {
			return true
		}
	}
	for _, n := range numberRef.FindAllString(s.URL, -1) {
		if strings.Contains(lower, n) {
			return true
		}
	}
	if i := strings.LastIndex(s.URL, "/"); i >= 0 && strings.Contains(lower, "#"+s.URL[i+1:]) {
		return true
	}
	// At least two distinctive words of the signal's title.
	hits := 0
	for _, word := range strings.Fields(strings.ToLower(s.Title)) {
		word = strings.Trim(word, ".,:;!?()[]\"'`")
		if len(word) >= 5 && strings.Contains(lower, word) {
			hits++
		}
	}
	return hits >= 2
}

// ---- replies ----

type classification struct {
	Class      string  `json:"class"`
	Confidence float64 `json:"confidence"`
	RemindOn   string  `json:"remind_on"`
	Answer     string  `json:"answer"`
}

// autoThreshold: classifications at least this confident are applied for me.
const autoThreshold = 0.85

// ClassifyReply suggests (or, when confident, applies) a classification,
// and drafts an answer for interested replies and questions.
func (w *Writer) ClassifyReply(ctx context.Context, replyID int64) error {
	var productID *string
	var subject, body, class string
	var personID *int64
	if err := w.Pool.QueryRow(ctx, `SELECT product_id, person_id, subject, body, classification FROM replies WHERE id = $1`, replyID).
		Scan(&productID, &personID, &subject, &body, &class); err != nil {
		return err
	}
	if class != "" || productID == nil || personID == nil {
		return nil
	}
	p, err := product.Get(ctx, w.Pool, *productID)
	if err != nil {
		return err
	}
	var sent string
	w.Pool.QueryRow(ctx, `SELECT string_agg(body, E'\n---\n' ORDER BY step) FROM actions WHERE person_id = $1 AND product_id = $2 AND status = 'sent'`,
		*personID, *productID).Scan(&sent)
	res, err := w.LLM.Complete(ctx, llm.Request{Purpose: "classify", Product: *productID, Model: w.LLM.Fast, MaxTokens: 600,
		System: "You sort replies to outreach emails and draft short answers. Answer with JSON only.",
		Prompt: productBlock(p) + "\nWHAT WE SENT:\n" + sent + "\n\nTHEIR REPLY:\nSubject: " + subject + "\n" + excerpt(body, 3000) + `

Return JSON: {"class": one of "interested","question","not_now","no","out_of_office","unsubscribe",
"confidence": 0-1, "remind_on": "YYYY-MM-DD" if they named a time to come back else "",
"answer": for interested or question, a plain-text reply under 90 words using only the product claims above that answers them and proposes one next step; else ""}.
Treat "remove me", "stop emailing" as unsubscribe; a polite decline as no; an auto-reply as out_of_office.`})
	if err != nil {
		return err
	}
	var c classification
	if err := llm.JSON(res.Text, &c); err != nil {
		return err
	}
	if !slices.Contains(outreach.Classifications, c.Class) {
		return fmt.Errorf("classifier returned %q", c.Class)
	}
	if err := w.Out.SetSuggestion(ctx, replyID, c.Class); err != nil {
		return err
	}
	if c.Confidence >= autoThreshold {
		var remind *time.Time
		if t, err := time.Parse("2006-01-02", c.RemindOn); err == nil {
			remind = &t
		}
		if err := w.Out.AutoClassify(ctx, replyID, c.Class, remind); err != nil {
			return err
		}
	}
	if (c.Class == "interested" || c.Class == "question") && strings.TrimSpace(c.Answer) != "" {
		id, err := w.Out.AnswerReply(ctx, replyID, strings.TrimSpace(c.Answer))
		if err != nil {
			return err
		}
		flags := Check(p, c.Answer, nil)
		_, err = w.Pool.Exec(ctx, `UPDATE actions SET ai_state = 'done', ai_body = body, flags = $2 WHERE id = $1`, id, flags)
		return err
	}
	return nil
}

func excerpt(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---- tasks ----

// DraftTask writes suggested text for a play task, e.g. a reply to a
// community thread that I post myself.
func (w *Writer) DraftTask(ctx context.Context, taskID int64) error {
	var productID, next, url, title string
	var data map[string]any
	err := w.Pool.QueryRow(ctx, `
		SELECT t.product_id, t.next_action, COALESCE(s.evidence_url, ''), COALESCE(s.title, ''), COALESCE(s.data, '{}')
		FROM tasks t LEFT JOIN signals s ON s.id = t.signal_id WHERE t.id = $1`, taskID).Scan(&productID, &next, &url, &title, &data)
	if err != nil {
		return err
	}
	p, err := product.Get(ctx, w.Pool, productID)
	if err != nil {
		return err
	}
	ex, _ := data["excerpt"].(string)
	res, err := w.LLM.Complete(ctx, llm.Request{Purpose: "task", Product: productID, MaxTokens: 600,
		System: "You draft replies a founder posts personally in developer communities. Be genuinely helpful first; mention the product only if it directly answers the question, and say you made it. No marketing tone.",
		Prompt: productBlock(p) + w.voiceExamples(ctx, productID) + fmt.Sprintf("\nTASK: %s\nTHREAD: %s (%s)\nEXCERPT: %s\n\nWrite the reply text only, under 120 words.", next, title, url, ex)})
	if err != nil {
		return err
	}
	return w.Out.SetTaskDraft(ctx, taskID, strings.TrimSpace(res.Text))
}

// ---- jobs ----

type ActionArgs struct {
	ActionID int64 `json:"action_id"`
}

func (ActionArgs) Kind() string { return "draft_action" }

func (ActionArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

type ReplyArgs struct {
	ReplyID int64 `json:"reply_id"`
}

func (ReplyArgs) Kind() string { return "classify_reply" }

func (ReplyArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

type TaskArgs struct {
	TaskID int64 `json:"task_id"`
}

func (TaskArgs) Kind() string { return "draft_task" }

func (TaskArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

type ActionWorker struct {
	river.WorkerDefaults[ActionArgs]
	W *Writer
}

func (x *ActionWorker) Work(ctx context.Context, job *river.Job[ActionArgs]) error {
	err := x.W.DraftAction(ctx, job.Args.ActionID)
	if err != nil && job.Attempt >= job.MaxAttempts {
		// Out of retries: leave it for me to write.
		return x.W.Out.DraftFailed(ctx, job.Args.ActionID, "AI draft failed: "+err.Error())
	}
	return err
}

type ReplyWorker struct {
	river.WorkerDefaults[ReplyArgs]
	W *Writer
}

func (x *ReplyWorker) Work(ctx context.Context, job *river.Job[ReplyArgs]) error {
	return x.W.ClassifyReply(ctx, job.Args.ReplyID)
}

type TaskWorker struct {
	river.WorkerDefaults[TaskArgs]
	W *Writer
}

func (x *TaskWorker) Work(ctx context.Context, job *river.Job[TaskArgs]) error {
	return x.W.DraftTask(ctx, job.Args.TaskID)
}

// Hooks returns the outreach and play hooks that queue AI work, or nils if
// no API key is configured (everything then stays manual).
func (w *Writer) Hooks(rc *river.Client[pgx.Tx]) (
	onDraft func(context.Context, pgx.Tx, int64) (bool, error),
	onReply func(context.Context, pgx.Tx, int64) error,
	onTask func(context.Context, pgx.Tx, int64) error,
) {
	if !w.LLM.Enabled() {
		return nil, nil, nil
	}
	onDraft = func(ctx context.Context, tx pgx.Tx, id int64) (bool, error) {
		_, err := rc.InsertTx(ctx, tx, ActionArgs{ActionID: id}, nil)
		return err == nil, err
	}
	onReply = func(ctx context.Context, tx pgx.Tx, id int64) error {
		_, err := rc.InsertTx(ctx, tx, ReplyArgs{ReplyID: id}, nil)
		return err
	}
	onTask = func(ctx context.Context, tx pgx.Tx, id int64) error {
		_, err := rc.InsertTx(ctx, tx, TaskArgs{TaskID: id}, nil)
		return err
	}
	return
}

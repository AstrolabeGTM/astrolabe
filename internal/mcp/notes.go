package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AstrolabeGTM/astrolabe/internal/notes"
	"github.com/AstrolabeGTM/astrolabe/internal/pipeline"
)

const notesInstructions = `

Conversation notes: after a call, email thread or chat, log_conversation records the five text fields (summary, their_words as exact quotes, problem, current_approach, next_step), whether we coached them (assisted: never count it as unassisted first use), and optionally qualification answers, a new next action, the survey answer "How would you feel if you could no longer use <product>?" (only for people who used it two weeks or more) and pricing statements. Quote people word for word. pmf_survey gives counts with the denominator; never turn them into percentages without the counts. For the weekly review use the weekly_themes prompt (or week_notes), then save_themes.`

var notesTerms = map[string]string{
	"log_conversation": "call notes, meeting notes, discovery call log, customer interview, activity log, voice of customer",
	"conversations":    "call notes, customer interviews, meeting history, activity timeline",
	"pmf_survey":       "Sean Ellis test, product-market fit survey, 40% rule counts",
	"pricing_feedback": "willingness to pay, budget, pricing research, price sensitivity",
	"week_notes":       "weekly call review, voice of customer, win/loss themes",
	"save_themes":      "weekly insights, voice-of-customer themes",
	"themes":           "weekly insights, voice-of-customer themes",
}

func init() { maps.Copy(salesTerms, notesTerms) }

func (s *Server) registerNotes(srv *sdk.Server) {
	register(srv, "log_conversation", "Log a call, email thread or chat with a person (and optionally their account): date, channel (call, email, chat, in_person, other), duration, assisted (we coached them; counted apart, never as unassisted first use) and five text fields: summary, their_words (exact quotes, they become marketing copy), problem, current_approach (what they do today) and next_step. Optional in the same call: qualification answers (applied to the account like qualify, with this note as evidence), a new next action with a due date (replaces the account's, or becomes a task for the person when there is no account), the survey answer (very_disappointed, somewhat_disappointed or not_disappointed, plus what they would use instead and who else would benefit, word for word) and pricing statements (offer, what it replaces, who approves, card/invoice/procurement, reaction). All or nothing: a refusal saves nothing.", s.logConversation)
	register(srv, "conversations", "Conversation notes, newest first, with counts of assisted and unassisted ones. Filter by product, person, account, the last N days and assisted (true or false). Each note carries its survey answer and pricing statements.", s.conversations)
	register(srv, "pmf_survey", "The product-market-fit survey as counts: people per answer (very, somewhat, not disappointed), the total answers and people behind them, the window, and every answer with what they would use instead and who else would benefit. Only people who used the product two weeks or more count; shorter-use answers are shown as excluded. No percentages: read the counts against the total.", s.pmfSurvey)
	register(srv, "pricing_feedback", "What people said about price in a window: counts by offer, by what it would replace (comparison) and by buying process (card, invoice, procurement), with every statement and quote. These are statements, not commitments.", s.pricingFeedback)
	register(srv, "week_notes", "Everything from one ISO week's conversation notes gathered for the weekly review (their words, problems, current approaches, survey answers, pricing reactions, assisted counts) plus the instructions for grouping them into themes. The weekly_themes prompt returns the same; call save_themes with the result.", s.weekNotes)
	register(srv, "save_themes", "Save the week's themes for a product (week like 2026-W43, default this week): text, plus optional items with theme, count, exact quotes and a note. Saving again for the same week replaces it. The weekly review and the digest show them.", s.saveThemes)
	register(srv, "themes", "The saved themes for a product and ISO week (default this week), or an empty answer if none were saved.", s.themes)

	srv.AddPrompt(&sdk.Prompt{
		Name:        "weekly_themes",
		Description: "Gather a week's conversation notes (their words, problems, objections, survey answers) and group them into themes with quotes and counts, then save them with save_themes.",
		Arguments: []*sdk.PromptArgument{
			{Name: "product", Description: "product id", Required: true},
			{Name: "week", Description: "ISO week like 2026-W43; default this week"},
		},
	}, s.weeklyThemesPrompt)
}

type logConversationArg struct {
	Product  string `json:"product"`
	Person   string `json:"person" jsonschema:"person id or email"`
	Account  string `json:"account,omitempty" jsonschema:"account id, company domain or name, if the conversation is about an account"`
	Date     string `json:"date,omitempty" jsonschema:"YYYY-MM-DD; default today"`
	Channel  string `json:"channel" jsonschema:"call, email, chat, in_person or other"`
	Duration int    `json:"duration_minutes,omitempty"`
	Assisted bool   `json:"assisted,omitempty" jsonschema:"true if we coached them through using the product"`

	Summary         string `json:"summary,omitempty"`
	TheirWords      string `json:"their_words,omitempty" jsonschema:"exact quotes, word for word"`
	Problem         string `json:"problem,omitempty"`
	CurrentApproach string `json:"current_approach,omitempty" jsonschema:"what they do today (the baseline)"`
	NextStep        string `json:"next_step,omitempty"`

	Qualification []pipeline.Answer `json:"qualification,omitempty" jsonschema:"check answers for the account, as in qualify"`
	NextAction    string            `json:"next_action,omitempty" jsonschema:"new next action for the account, or a task for the person without an account"`
	Due           string            `json:"due,omitempty" jsonschema:"YYYY-MM-DD for next_action; default today"`
	PMFSurvey     *notes.PMF        `json:"pmf_survey,omitempty"`
	Pricing       []notes.Pricing   `json:"pricing_feedback,omitempty"`
}

func (s *Server) logConversation(ctx context.Context, a logConversationArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	who, err := s.resolvePerson(ctx, a.Person)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	o := notes.LogOpts{PersonID: who.ID, Channel: a.Channel, Duration: a.Duration, Assisted: a.Assisted,
		Summary: a.Summary, TheirWords: a.TheirWords, Problem: a.Problem, CurrentApproach: a.CurrentApproach, NextStep: a.NextStep,
		Answers: a.Qualification, NextAction: a.NextAction, PMF: a.PMFSurvey, Pricing: a.Pricing}
	if o.Date, err = parseDay("date", a.Date, now); err != nil {
		return nil, err
	}
	if a.NextAction != "" {
		if o.NextDue, err = parseDay("due", a.Due, now); err != nil {
			return nil, err
		}
	}
	var p2 = p
	if a.Account != "" {
		pp, id, err := s.findAccount(ctx, accountArg{a.Product, a.Account})
		if err != nil {
			return nil, err
		}
		p2, o.AccountID = pp, id
	}
	id, err := notes.Log(ctx, s.S.Pool, p2, o, now)
	if err != nil {
		return nil, err
	}
	list, err := notes.List(ctx, s.S.Pool, notes.ListOpts{Product: p.ID, PersonID: who.ID, Limit: 50}, now)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"note_id": id}
	for _, n := range list {
		if n.ID == id {
			out["note"] = n
		}
	}
	if o.AccountID != 0 {
		if out["account"], err = s.accountDetail(ctx, p2, o.AccountID); err != nil {
			return nil, err
		}
		s.scanSoon(ctx)
	}
	return out, nil
}

type conversationsArg struct {
	Product  string `json:"product"`
	Person   string `json:"person,omitempty" jsonschema:"person id or email"`
	Account  string `json:"account,omitempty" jsonschema:"account id, company domain or name"`
	Days     int    `json:"days,omitempty" jsonschema:"only notes from the last N days; default all"`
	Assisted *bool  `json:"assisted,omitempty" jsonschema:"true: only assisted sessions; false: only unassisted"`
	Limit    int    `json:"limit,omitempty" jsonschema:"default 50"`
}

func (s *Server) conversations(ctx context.Context, a conversationsArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	o := notes.ListOpts{Product: p.ID, Days: a.Days, Assisted: a.Assisted, Limit: a.Limit}
	if o.Limit <= 0 {
		o.Limit = 50
	}
	if a.Person != "" {
		who, err := s.resolvePerson(ctx, a.Person)
		if err != nil {
			return nil, err
		}
		o.PersonID = who.ID
	}
	if a.Account != "" {
		_, id, err := s.findAccount(ctx, accountArg{a.Product, a.Account})
		if err != nil {
			return nil, err
		}
		o.AccountID = id
	}
	list, err := notes.List(ctx, s.S.Pool, o, now)
	if err != nil {
		return nil, err
	}
	counts, err := notes.Count(ctx, s.S.Pool, o, now)
	if err != nil {
		return nil, err
	}
	days := "all time"
	if a.Days > 0 {
		days = fmt.Sprintf("last %d days", a.Days)
	}
	return map[string]any{"window": days, "counts_all_matching": counts, "shown": len(list), "conversations": list}, nil
}

type windowArg struct {
	Product string `json:"product"`
	Days    int    `json:"days,omitempty" jsonschema:"window in days ending today; default all time"`
}

func (s *Server) pmfSurvey(ctx context.Context, a windowArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	return notes.PMFSurvey(ctx, s.S.Pool, p.ID, a.Days, time.Now())
}

func (s *Server) pricingFeedback(ctx context.Context, a windowArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	return notes.PricingFeedback(ctx, s.S.Pool, p.ID, a.Days, time.Now())
}

type weekArg struct {
	Product string `json:"product"`
	Week    string `json:"week,omitempty" jsonschema:"ISO week like 2026-W43; default this week"`
}

func (s *Server) weekNotes(ctx context.Context, a weekArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	return notes.Gather(ctx, s.S.Pool, p, a.Week, time.Now())
}

type saveThemesArg struct {
	Product string            `json:"product"`
	Week    string            `json:"week,omitempty" jsonschema:"ISO week like 2026-W43; default this week"`
	Text    string            `json:"text" jsonschema:"the themes as readable text"`
	Items   []notes.ThemeItem `json:"items,omitempty"`
}

func (s *Server) saveThemes(ctx context.Context, a saveThemesArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	return notes.SaveThemes(ctx, s.S.Pool, p.ID, a.Week, a.Text, a.Items, time.Now())
}

func (s *Server) themes(ctx context.Context, a weekArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	if a.Week == "" {
		a.Week = notes.ISOWeek(time.Now())
	}
	t, err := notes.GetThemes(ctx, s.S.Pool, p.ID, a.Week)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return map[string]any{"product": p.ID, "week": a.Week, "themes": nil}, nil
	}
	return t, nil
}

func (s *Server) weeklyThemesPrompt(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
	args := req.Params.Arguments
	p, err := s.find(args["product"])
	if err != nil {
		return nil, err
	}
	m, err := notes.Gather(ctx, s.S.Pool, p, args["week"], time.Now())
	if err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf("Weekly themes for %s, %s (%s to %s).\n\n%s\n\nThe week's material (JSON):\n%s", p.Name, m.Week, m.From, m.To, notes.Instructions, body)
	return &sdk.GetPromptResult{
		Description: "Conversation notes of " + m.Week + " for " + p.ID,
		Messages:    []*sdk.PromptMessage{{Role: "user", Content: &sdk.TextContent{Text: text}}},
	}, nil
}

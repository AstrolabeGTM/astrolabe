package notes

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// window is the date range a summary covers: Days back from now, or all
// time when Days is zero. To is exclusive.
type Window struct {
	Days int    `json:"days,omitempty"` // 0: all time
	From string `json:"from,omitempty"` // first day counted
	To   string `json:"to"`             // last day counted
}

func window(days int, now time.Time) (Window, time.Time) {
	w := Window{Days: days, To: day(now)}
	if days <= 0 {
		return w, time.Time{}
	}
	from := now.AddDate(0, 0, -days+1)
	w.From = day(from)
	return w, from
}

// OptionCount is one survey answer and how many people gave it.
type OptionCount struct {
	Answer string `json:"answer"`
	Count  int    `json:"count"`
}

// PMFResponse is one survey answer, word for word.
type PMFResponse struct {
	Date        string `json:"date"`
	Person      string `json:"person"`
	NoteID      int64  `json:"note_id"`
	Answer      string `json:"answer"`
	Alternative string `json:"alternative,omitempty"`
	WhoElse     string `json:"who_else,omitempty"`
}

// PMFCounts is the survey result: a count per answer and the full
// denominator. There are deliberately no percentages; read the counts.
type PMFCounts struct {
	Product string `json:"product"`
	Window  Window `json:"window"`
	// Total: answers from people who used the product two weeks or more.
	// Answers adds up to Total.
	Total   int           `json:"total_answers"`
	Answers []OptionCount `json:"answers"`
	// People: distinct people behind Total.
	People int `json:"people"`
	// ExcludedUnder2Weeks: answers recorded from people who had not used the
	// product two weeks; kept, but not counted in Total.
	ExcludedUnder2Weeks int           `json:"excluded_used_under_two_weeks"`
	Responses           []PMFResponse `json:"responses"`
}

// PMFSurvey counts survey answers for a product over the last days days (0: all
// time).
func PMFSurvey(ctx context.Context, q store.Q, productID string, days int, now time.Time) (*PMFCounts, error) {
	w, from := window(days, now)
	r := &PMFCounts{Product: productID, Window: w, Answers: []OptionCount{}, Responses: []PMFResponse{}}
	for _, a := range PMFAnswers {
		r.Answers = append(r.Answers, OptionCount{Answer: a})
	}
	args := []any{productID, day(now)}
	cond := "pa.product_id = $1 AND pa.answered_on <= $2::date"
	if !from.IsZero() {
		args = append(args, day(from))
		cond += " AND pa.answered_on >= $3::date"
	}
	rows, err := q.Query(ctx, `SELECT pa.conversation_id, pa.person_id, pe.name, to_char(pa.answered_on, 'YYYY-MM-DD'), pa.answer, pa.used_two_weeks, pa.alternative, pa.who_else
		FROM pmf_answers pa JOIN people pe ON pe.id = pa.person_id WHERE `+cond+` ORDER BY pa.answered_on DESC, pa.conversation_id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	people := map[int64]bool{}
	for rows.Next() {
		var x PMFResponse
		var pid int64
		var used bool
		if err := rows.Scan(&x.NoteID, &pid, &x.Person, &x.Date, &x.Answer, &used, &x.Alternative, &x.WhoElse); err != nil {
			return nil, err
		}
		if !used {
			r.ExcludedUnder2Weeks++
			continue
		}
		r.Total++
		people[pid] = true
		for i := range r.Answers {
			if r.Answers[i].Answer == x.Answer {
				r.Answers[i].Count++
			}
		}
		r.Responses = append(r.Responses, x)
	}
	r.People = len(people)
	return r, rows.Err()
}

// Text is one line for the digest: counts of every answer, with the total.
func (r *PMFCounts) Text() string {
	var parts []string
	for _, a := range r.Answers {
		parts = append(parts, fmt.Sprintf("%s %d", strings.ReplaceAll(a.Answer, "_", " "), a.Count))
	}
	s := fmt.Sprintf("%s (of %d answers from %d people who used it two weeks or more", strings.Join(parts, ", "), r.Total, r.People)
	if r.ExcludedUnder2Weeks > 0 {
		s += fmt.Sprintf("; %d more not counted: used it under two weeks", r.ExcludedUnder2Weeks)
	}
	return s + ")"
}

// Tally is one group in a pricing summary.
type Tally struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Statement is one pricing statement with its quote.
type Statement struct {
	Date          string `json:"date"`
	Person        string `json:"person"`
	NoteID        int64  `json:"note_id"`
	Offer         string `json:"offer"`
	Replaces      string `json:"replaces,omitempty"`
	Approver      string `json:"approver,omitempty"`
	BuyingProcess string `json:"buying_process,omitempty"`
	Reaction      string `json:"reaction,omitempty"`
}

// PricingSummary groups what people said about price. These are statements,
// not commitments.
type PricingSummary struct {
	Product      string      `json:"product"`
	Window       Window      `json:"window"`
	Statements   int         `json:"statements"`
	People       int         `json:"people"`
	ByOffer      []Tally     `json:"by_offer"`
	ByComparison []Tally     `json:"by_comparison"`
	ByBuying     []Tally     `json:"by_buying_process"`
	Quotes       []Statement `json:"statements_in_full"`
	Note         string      `json:"note"`
}

func tally(m map[string]*Tally) []Tally {
	out := []Tally{}
	for _, c := range m {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func bump(m map[string]*Tally, name string) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		name = "(not said)"
	}
	k := strings.ToLower(name)
	if m[k] == nil {
		m[k] = &Tally{Name: name}
	}
	m[k].Count++
}

// PricingFeedback summarises pricing feedback over the last days days (0: all time).
func PricingFeedback(ctx context.Context, q store.Q, productID string, days int, now time.Time) (*PricingSummary, error) {
	w, from := window(days, now)
	r := &PricingSummary{Product: productID, Window: w, Quotes: []Statement{},
		Note: "Statements people made, not commitments to buy."}
	args := []any{productID, day(now)}
	cond := "pf.product_id = $1 AND pf.stated_on <= $2::date"
	if !from.IsZero() {
		args = append(args, day(from))
		cond += " AND pf.stated_on >= $3::date"
	}
	rows, err := q.Query(ctx, `SELECT pf.conversation_id, pf.person_id, pe.name, to_char(pf.stated_on, 'YYYY-MM-DD'), pf.offer, pf.replaces, pf.approver, pf.buying_process, pf.reaction
		FROM pricing_feedback pf JOIN people pe ON pe.id = pf.person_id WHERE `+cond+` ORDER BY pf.stated_on DESC, pf.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	offer, cmp, buy := map[string]*Tally{}, map[string]*Tally{}, map[string]*Tally{}
	people := map[int64]bool{}
	for rows.Next() {
		var s Statement
		var pid int64
		if err := rows.Scan(&s.NoteID, &pid, &s.Person, &s.Date, &s.Offer, &s.Replaces, &s.Approver, &s.BuyingProcess, &s.Reaction); err != nil {
			return nil, err
		}
		r.Statements++
		people[pid] = true
		bump(offer, s.Offer)
		bump(cmp, s.Replaces)
		bump(buy, s.BuyingProcess)
		r.Quotes = append(r.Quotes, s)
	}
	r.People = len(people)
	r.ByOffer, r.ByComparison, r.ByBuying = tally(offer), tally(cmp), tally(buy)
	return r, rows.Err()
}

// ---- weekly themes ----

var isoWeekRE = regexp.MustCompile(`^(\d{4})-W(\d{2})$`)

// ISOWeek names the ISO week of t, like 2026-W43.
func ISOWeek(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// WeekBounds returns Monday of the ISO week and the Monday after it.
func WeekBounds(week string) (from, to time.Time, err error) {
	m := isoWeekRE.FindStringSubmatch(week)
	if m == nil {
		return from, to, invalid("week must look like 2026-W43")
	}
	var y, w int
	fmt.Sscanf(m[1]+" "+m[2], "%d %d", &y, &w)
	// January 4th is always in week 1.
	jan4 := time.Date(y, 1, 4, 0, 0, 0, 0, time.UTC)
	mon := jan4.AddDate(0, 0, -((int(jan4.Weekday()) + 6) % 7))
	from = mon.AddDate(0, 0, 7*(w-1))
	if ISOWeek(from) != week {
		return from, to, invalid("%s is not an ISO week", week)
	}
	return from, from.AddDate(0, 0, 7), nil
}

// ThemeItem is one theme with its quotes and count.
type ThemeItem struct {
	Theme  string   `json:"theme"`
	Count  int      `json:"count,omitempty" jsonschema:"how many notes or people mention it"`
	Quotes []string `json:"quotes,omitempty" jsonschema:"their exact words"`
	Note   string   `json:"note,omitempty"`
}

// Themes are a week's themes for a product.
type Themes struct {
	Product string      `json:"product"`
	Week    string      `json:"week"`
	Text    string      `json:"text"`
	Items   []ThemeItem `json:"items"`
	SavedAt time.Time   `json:"saved_at"`
}

// SaveThemes stores the week's themes (text with optional structured
// items). Saving again for the same product and week replaces them.
func SaveThemes(ctx context.Context, q store.Q, productID, week, text string, items []ThemeItem, now time.Time) (*Themes, error) {
	if week == "" {
		week = ISOWeek(now)
	}
	if _, _, err := WeekBounds(week); err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if text == "" && len(items) == 0 {
		return nil, invalid("give the themes as text or items")
	}
	if len(text) > 4*maxText {
		return nil, invalid("themes text is too long")
	}
	if items == nil {
		items = []ThemeItem{}
	}
	for i, it := range items {
		if strings.TrimSpace(it.Theme) == "" {
			return nil, invalid("item %d has no theme", i+1)
		}
	}
	raw, _ := json.Marshal(items)
	t := &Themes{Product: productID, Week: week, Text: text, Items: items, SavedAt: now}
	_, err := q.Exec(ctx, `INSERT INTO weekly_themes (product_id, iso_week, body, items, saved_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (product_id, iso_week) DO UPDATE SET body = $3, items = $4, saved_at = $5`, productID, week, text, raw, now)
	return t, err
}

// GetThemes returns the saved themes for a product and ISO week, or nil.
func GetThemes(ctx context.Context, q store.Q, productID, week string) (*Themes, error) {
	t := &Themes{Product: productID, Week: week}
	var raw []byte
	err := q.QueryRow(ctx, `SELECT body, items, saved_at FROM weekly_themes WHERE product_id = $1 AND iso_week = $2`, productID, week).Scan(&t.Text, &raw, &t.SavedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	t.Items = []ThemeItem{}
	return t, json.Unmarshal(raw, &t.Items)
}

// ---- gathering the week for the model ----

// Instructions tell the model what to do with the gathered material.
const Instructions = `Group this week's conversation notes into themes.
- A theme is something at least one person said in their own words; name it in plain terms a developer would use.
- For each theme give a count (how many notes or people mention it), one to three exact quotes copied from their_words (never paraphrase a quote), and one line on what it means for the product or the message.
- Cover: the problems people have, what they do today (current_approach), objections and pricing reactions, and the survey answers. Keep very, somewhat and not disappointed apart and quote what people said they would use instead.
- Do not turn counts into percentages and do not invent notes: if a theme has one note, say one.
- Keep assisted sessions apart: say when a theme comes only from sessions where we coached them.
- Then call save_themes with the product, the week, the themes as text and, if you can, as structured items (theme, count, quotes, note).`

// Material is the week's notes gathered for the model.
type Material struct {
	Product      string          `json:"product"`
	Week         string          `json:"week"`
	From         string          `json:"from"`
	To           string          `json:"to"`
	Notes        Counts          `json:"notes_logged"`
	Conversation []*Note         `json:"conversations"`
	PMF          *PMFCounts      `json:"pmf_survey_this_week"`
	PMFSoFar     *PMFCounts      `json:"pmf_survey_so_far"`
	Pricing      *PricingSummary `json:"pricing_feedback_this_week"`
	Themes       *Themes         `json:"themes_already_saved,omitempty"`
	Instructions string          `json:"instructions"`
}

// Gather collects the notes, survey answers and pricing statements of an ISO
// week (default: the one containing now).
func Gather(ctx context.Context, q store.Q, p *product.Product, week string, now time.Time) (*Material, error) {
	if week == "" {
		week = ISOWeek(now)
	}
	from, to, err := WeekBounds(week)
	if err != nil {
		return nil, err
	}
	m := &Material{Product: p.ID, Week: week, From: day(from), To: day(to.AddDate(0, 0, -1)), Instructions: Instructions}
	o := ListOpts{Product: p.ID, From: from, To: to}
	if m.Conversation, err = List(ctx, q, o, now); err != nil {
		return nil, err
	}
	if m.Notes, err = Count(ctx, q, o, now); err != nil {
		return nil, err
	}
	// The survey and pricing summaries cover the same days as the notes.
	end := to.AddDate(0, 0, -1)
	days := int(end.Sub(from).Hours()/24) + 1
	if m.PMF, err = PMFSurvey(ctx, q, p.ID, days, end); err != nil {
		return nil, err
	}
	if m.PMFSoFar, err = PMFSurvey(ctx, q, p.ID, 0, end); err != nil {
		return nil, err
	}
	if m.Pricing, err = PricingFeedback(ctx, q, p.ID, days, end); err != nil {
		return nil, err
	}
	m.Themes, err = GetThemes(ctx, q, p.ID, week)
	return m, err
}

// ---- digest ----

// Section is the digest's part about conversations.
type Section struct {
	// This week: the last 7 days.
	Week   Counts     `json:"notes_this_week"`
	PMF    *PMFCounts `json:"pmf_survey_so_far,omitempty"`
	Themes *Themes    `json:"themes,omitempty"`
}

// DigestSection summarises conversations for the weekly digest; nil when
// there is nothing to show. Themes are those of the current ISO week, or
// of the one before when the current one has none yet.
func DigestSection(ctx context.Context, q store.Q, productID string, now time.Time) (*Section, error) {
	s := &Section{}
	var err error
	if s.Week, err = Count(ctx, q, ListOpts{Product: productID, Days: 7}, now); err != nil {
		return nil, err
	}
	if s.PMF, err = PMFSurvey(ctx, q, productID, 0, now); err != nil {
		return nil, err
	}
	if s.PMF.Total == 0 && s.PMF.ExcludedUnder2Weeks == 0 {
		s.PMF = nil
	}
	for _, w := range []string{ISOWeek(now), ISOWeek(now.AddDate(0, 0, -7))} {
		if s.Themes, err = GetThemes(ctx, q, productID, w); err != nil || s.Themes != nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if s.Week.Total == 0 && s.PMF == nil && s.Themes == nil {
		return nil, nil
	}
	return s, nil
}

// Text renders the section for the digest.
func (s *Section) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Conversations (7d): %d logged, %d assisted, %d unassisted\n", s.Week.Total, s.Week.Assisted, s.Week.Unassisted)
	if s.PMF != nil {
		fmt.Fprintf(&b, "Would you miss it? So far: %s\n", s.PMF.Text())
	}
	if t := s.Themes; t != nil {
		fmt.Fprintf(&b, "Themes (%s):", t.Week)
		if len(t.Items) > 0 {
			for _, it := range t.Items {
				if it.Count > 0 {
					fmt.Fprintf(&b, " %s (%d);", it.Theme, it.Count)
				} else {
					fmt.Fprintf(&b, " %s;", it.Theme)
				}
			}
			b.WriteString("\n")
		} else {
			b.WriteString("\n" + t.Text + "\n")
		}
	}
	return b.String()
}

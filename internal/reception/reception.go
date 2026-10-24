// Package reception keeps the reception log: public mentions of a product
// (a Hacker News thread, a Stack Overflow question, a blog post, a GitHub
// issue) with a date, a source, a tone and whether it asks a question. It is
// about what is said in public, not about people; mentions create no
// signals and no people.
package reception

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

var Tones = []string{"positive", "mixed", "negative", "unknown"}

type Mention struct {
	ID           int64     `json:"id,omitempty"`
	Product      string    `json:"product"`
	URL          string    `json:"url"`
	MentionedAt  time.Time `json:"date"`
	Source       string    `json:"source"`
	Title        string    `json:"title"`
	Tone         string    `json:"tone"`
	AsksQuestion bool      `json:"asks_question"`
	FoundBy      string    `json:"found_by"` // monitor:<id> or manual
}

// NormalizeURL trims space, drops the fragment and a trailing slash, so the
// same page is stored once.
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("mention needs a full http(s) URL, got %q", raw)
	}
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	s := u.String()
	if u.RawQuery == "" {
		s = strings.TrimRight(s, "/")
	}
	return s, nil
}

func validTone(t string) bool {
	for _, x := range Tones {
		if x == t {
			return true
		}
	}
	return false
}

// Add stores a mention. A URL already logged for the product is left as it
// is (including any tone set by hand) and created is false.
func Add(ctx context.Context, q store.Q, m Mention) (id int64, created bool, err error) {
	if m.Product == "" {
		return 0, false, errors.New("mention needs a product")
	}
	if m.URL, err = NormalizeURL(m.URL); err != nil {
		return 0, false, err
	}
	if m.Tone == "" {
		m.Tone = "unknown"
	}
	if !validTone(m.Tone) {
		return 0, false, fmt.Errorf("tone must be one of %v", Tones)
	}
	if m.Source == "" {
		m.Source = "manual"
	}
	if m.FoundBy == "" {
		m.FoundBy = "manual"
	}
	if m.MentionedAt.IsZero() {
		m.MentionedAt = time.Now()
	}
	m.Title = excerpt(m.Title, 300)
	err = q.QueryRow(ctx, `
		INSERT INTO reception_mentions (product_id, url, mentioned_at, source, title, tone, asks_question, found_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (product_id, url) DO NOTHING RETURNING id`,
		m.Product, m.URL, m.MentionedAt, m.Source, m.Title, m.Tone, m.AsksQuestion, m.FoundBy).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = q.QueryRow(ctx, `SELECT id FROM reception_mentions WHERE product_id = $1 AND url = $2`, m.Product, m.URL).Scan(&id)
		return id, false, err
	}
	return id, err == nil, err
}

// SetTone sets the tone of a mention by id.
func SetTone(ctx context.Context, q store.Q, id int64, tone string) error {
	if !validTone(tone) {
		return fmt.Errorf("tone must be one of %v", Tones)
	}
	tag, err := q.Exec(ctx, `UPDATE reception_mentions SET tone = $2 WHERE id = $1`, id, tone)
	if err == nil && tag.RowsAffected() == 0 {
		err = fmt.Errorf("no mention %d", id)
	}
	return err
}

// LooksLikeQuestion is a plain guess from the text: a question mark.
func LooksLikeQuestion(texts ...string) bool {
	for _, t := range texts {
		if strings.Contains(t, "?") {
			return true
		}
	}
	return false
}

type Window struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Report struct {
	Product   string         `json:"product"`
	Days      int            `json:"days"`
	Window    Window         `json:"window"`
	Total     int            `json:"total"`
	ByTone    map[string]int `json:"by_tone"`
	Questions int            `json:"asking_a_question"`
	Mentions  []Mention      `json:"mentions"`
}

// Week reports the mentions dated in the 7 days up to now, newest first,
// with counts by tone. limit caps the listed mentions (counts cover all).
func Week(ctx context.Context, q store.Q, productID string, now time.Time, limit int) (*Report, error) {
	return Days(ctx, q, productID, 7, now, limit)
}

// Days is Week for the last n days.
func Days(ctx context.Context, q store.Q, productID string, days int, now time.Time, limit int) (*Report, error) {
	from := now.AddDate(0, 0, -days)
	r := &Report{Product: productID, Days: days, ByTone: map[string]int{}, Mentions: []Mention{},
		Window: Window{from.UTC().Format("2006-01-02 15:04 UTC"), now.UTC().Format("2006-01-02 15:04 UTC")}}
	for _, t := range Tones {
		r.ByTone[t] = 0
	}
	rows, err := q.Query(ctx, `
		SELECT id, product_id, url, mentioned_at, source, title, tone, asks_question, found_by
		FROM reception_mentions WHERE product_id = $1 AND mentioned_at > $2 AND mentioned_at <= $3
		ORDER BY mentioned_at DESC, id DESC`, productID, from, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m Mention
		if err := rows.Scan(&m.ID, &m.Product, &m.URL, &m.MentionedAt, &m.Source, &m.Title, &m.Tone, &m.AsksQuestion, &m.FoundBy); err != nil {
			return nil, err
		}
		r.Total++
		r.ByTone[m.Tone]++
		if m.AsksQuestion {
			r.Questions++
		}
		if limit <= 0 || len(r.Mentions) < limit {
			r.Mentions = append(r.Mentions, m)
		}
	}
	return r, rows.Err()
}

// Text renders a short summary for the weekly digest.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Reception, last %d days (%s to %s): %d mentions", r.Days, r.Window.From, r.Window.To, r.Total)
	if r.Total > 0 {
		fmt.Fprintf(&b, " — %d positive, %d mixed, %d negative, %d not yet rated; %d ask a question",
			r.ByTone["positive"], r.ByTone["mixed"], r.ByTone["negative"], r.ByTone["unknown"], r.Questions)
	}
	b.WriteString("\n")
	for i, m := range r.Mentions {
		if i == 5 {
			break
		}
		fmt.Fprintf(&b, "  [%s] %s: %s %s\n", m.Tone, m.Source, m.Title, m.URL)
	}
	return b.String()
}

func excerpt(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

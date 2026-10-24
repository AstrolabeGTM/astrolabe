package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/score"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// Item is a stored signal as the operator sees it: what happened, where,
// and who it is about with their current score.
type Item struct {
	ID         int64          `json:"id"`
	Product    string         `json:"product"`
	Type       string         `json:"type"`
	OccurredAt time.Time      `json:"occurred_at"`
	Title      string         `json:"title"`
	URL        string         `json:"url"`
	Strength   int            `json:"strength"`
	WhyNow     bool           `json:"why_now,omitempty"`
	PersonID   *int64         `json:"person_id,omitempty"`
	Person     string         `json:"person,omitempty"`
	Company    string         `json:"company,omitempty"`
	Tier       string         `json:"tier,omitempty"`
	Priority   *int           `json:"priority,omitempty"`
	Why        string         `json:"why,omitempty"` // the person's score breakdown
	Data       map[string]any `json:"data,omitempty"`
}

// Filter narrows the feed. Empty fields match everything.
type Filter struct {
	Product string
	Type    string
	// MinTier keeps signals about people at this tier or better (A best).
	MinTier  string
	PersonID int64
	Days     int // default 7
	Limit    int // default 50
}

// Feed is the signals in a window, newest first. Total counts every match
// in the window, also those beyond Limit.
type Feed struct {
	Signals []Item    `json:"signals"`
	Total   int       `json:"total"`
	Days    int       `json:"days"`
	Since   time.Time `json:"since"`
}

// Recent lists signals from the last f.Days days. The MCP tool signals, the
// browser app's Signals screen and the person page all use it.
func Recent(ctx context.Context, q store.Q, f Filter, now time.Time) (*Feed, error) {
	if f.Days <= 0 {
		f.Days = 7
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	f.MinTier = strings.ToUpper(f.MinTier)
	since := now.AddDate(0, 0, -f.Days)
	feed := &Feed{Signals: []Item{}, Days: f.Days, Since: since}
	const where = `
		WHERE s.occurred_at > $1 AND ($2 = '' OR s.product_id = $2) AND ($3 = '' OR s.type = $3)
		  AND ($4 = '' OR sc.tier <= $4) AND ($5 = 0 OR s.person_id = $5)`
	if err := q.QueryRow(ctx, `SELECT count(*) FROM signals s
		LEFT JOIN scores sc ON sc.product_id = s.product_id AND sc.person_id = s.person_id`+where,
		since, f.Product, f.Type, f.MinTier, f.PersonID).Scan(&feed.Total); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		SELECT s.id, s.product_id, s.type, s.occurred_at, s.title, s.evidence_url, s.strength, s.why_now, s.person_id,
			COALESCE(NULLIF(p.name, ''), (SELECT value FROM identities WHERE person_id = p.id ORDER BY kind = 'email' DESC, kind = 'github' DESC, value LIMIT 1), ''),
			COALESCE(NULLIF(c.name, ''), c.domain, c.github_org, ''),
			COALESCE(sc.tier, ''), sc.priority, COALESCE(sc.fit, 0), COALESCE(sc.intent, 0), COALESCE(sc.why_now, 0), sc.breakdown, s.data
		FROM signals s
		LEFT JOIN people p ON p.id = s.person_id
		LEFT JOIN companies c ON c.id = s.company_id
		LEFT JOIN scores sc ON sc.product_id = s.product_id AND sc.person_id = s.person_id`+where+`
		ORDER BY s.occurred_at DESC, s.id DESC LIMIT $6`, since, f.Product, f.Type, f.MinTier, f.PersonID, f.Limit)
	if err != nil {
		return nil, err
	}
	feed.Signals, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Item, error) {
		var it Item
		var sc score.Score
		var bd []byte
		err := row.Scan(&it.ID, &it.Product, &it.Type, &it.OccurredAt, &it.Title, &it.URL, &it.Strength, &it.WhyNow, &it.PersonID,
			&it.Person, &it.Company, &it.Tier, &it.Priority, &sc.Fit, &sc.Intent, &sc.WhyNow, &bd, &it.Data)
		if err == nil && it.Priority != nil {
			sc.Tier, sc.Priority = it.Tier, *it.Priority
			json.Unmarshal(bd, &sc.Breakdown)
			it.Why = sc.Summary()
		}
		return it, err
	})
	if feed.Signals == nil {
		feed.Signals = []Item{}
	}
	return feed, err
}

// Types lists the signal types seen so far, for filters.
func Types(ctx context.Context, q store.Q) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT type FROM signals ORDER BY type`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if out == nil {
		out = []string{}
	}
	return out, err
}

// Candidate is a person an ambiguous signal might be about.
type Candidate struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

// Review is a signal whose identities point at more than one person. Nothing
// is guessed: the operator picks one (Resolve) or dismisses it.
type Review struct {
	ID         int64          `json:"review_id"`
	SignalID   int64          `json:"signal_id"`
	Product    string         `json:"product"`
	Type       string         `json:"type"`
	Title      string         `json:"title"`
	URL        string         `json:"url"`
	OccurredAt time.Time      `json:"occurred_at"`
	Subject    map[string]any `json:"subject"`
	IDs        []int64        `json:"candidate_person_ids"`
	Candidates []Candidate    `json:"candidates"`
}

// OpenReviews lists unresolved ambiguous matches, oldest first, and how many
// there are in all.
func OpenReviews(ctx context.Context, q store.Q, limit int) ([]Review, int, error) {
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM identity_reviews WHERE status = 'open'`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := q.Query(ctx, `
		SELECT r.id, r.signal_id, s.product_id, s.type, s.title, s.evidence_url, s.occurred_at, s.subject, r.candidates
		FROM identity_reviews r JOIN signals s ON s.id = r.signal_id
		WHERE r.status = 'open' ORDER BY r.id LIMIT $1`, limit)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Review, error) {
		var r Review
		return r, row.Scan(&r.ID, &r.SignalID, &r.Product, &r.Type, &r.Title, &r.URL, &r.OccurredAt, &r.Subject, &r.IDs)
	})
	if err != nil {
		return nil, 0, err
	}
	for i := range list {
		list[i].Candidates = []Candidate{}
		for _, id := range list[i].IDs {
			p, err := people.Get(ctx, q, id)
			if err != nil {
				continue
			}
			label := strings.Join(strings.Fields(fmt.Sprintf("%s %s %s", p.Name, p.Email, githubLabel(p.GitHub))), " ")
			if label == "" {
				label = fmt.Sprintf("person %d", id)
			}
			list[i].Candidates = append(list[i].Candidates, Candidate{id, label})
		}
	}
	if list == nil {
		list = []Review{}
	}
	return list, total, nil
}

func githubLabel(login string) string {
	if login == "" {
		return ""
	}
	return "github.com/" + login
}

// MonitorStatus is how a monitor in monitors.yaml last ran.
type MonitorStatus struct {
	Product   string     `json:"product"`
	Monitor   string     `json:"monitor"`
	Type      string     `json:"type"`
	Every     string     `json:"every"`
	LastRun   *time.Time `json:"last_run,omitempty"`
	LastOK    *time.Time `json:"last_ok,omitempty"`
	LastCount int        `json:"last_new_signals"`
	Error     string     `json:"error,omitempty"`
	// Counters is true for github_release monitors: they record public
	// counts, never signals or people.
	Counters  bool `json:"counters,omitempty"`
	Reception bool `json:"reception,omitempty"`
	// NextRun is when the scheduler will run it next (at most every 5 minutes
	// it checks what is due).
	NextRun time.Time `json:"next_run"`
}

// Monitors reports every monitor of the given products.
func Monitors(ctx context.Context, q store.Q, products []*product.Product, now time.Time) ([]MonitorStatus, error) {
	out := []MonitorStatus{}
	for _, p := range products {
		for _, m := range p.Monitors {
			st := MonitorStatus{Product: p.ID, Monitor: m.ID, Type: m.Type, Every: m.Every, Counters: product.CounterMonitor(m.Type), Reception: m.Reception}
			err := q.QueryRow(ctx, `SELECT last_run_at, last_ok_at, last_count, last_error FROM monitor_runs WHERE product_id = $1 AND monitor_id = $2`,
				p.ID, m.ID).Scan(&st.LastRun, &st.LastOK, &st.LastCount, &st.Error)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			st.NextRun = now
			if st.LastRun != nil && st.LastRun.Add(m.Interval()).After(now) {
				st.NextRun = st.LastRun.Add(m.Interval())
			}
			out = append(out, st)
		}
	}
	return out, nil
}

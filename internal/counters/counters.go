// Package counters records daily snapshots of public numbers for a repo
// (release asset download counts, stars, forks, watchers, open issues,
// discussions) and reports the weekly change. These are totals about a
// project, never about people: a download count counts downloads, not
// installs or users, and nothing here creates a signal or a person.
package counters

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/github"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

const (
	Downloads   = "downloads"
	Stars       = "stars"
	Forks       = "forks"
	Watchers    = "watchers"
	OpenIssues  = "open_issues"
	Discussions = "discussions"
)

// Point is one value in a series.
type Point struct {
	Metric, Tag, Asset string
	Value              int64
}

// Snapshot is everything read from GitHub in one run.
type Snapshot struct {
	Points []Point
	// Notes explain anything skipped, e.g. discussions without a token.
	Notes []string
}

// Collect reads the repo's current numbers. Discussions are skipped, with a
// note, when there is no token or the lookup fails.
func Collect(ctx context.Context, gh *github.Client, repo string) (*Snapshot, error) {
	r, err := gh.Repo(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("repo %s: %w", repo, err)
	}
	rels, err := gh.Releases(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("releases of %s: %w", repo, err)
	}
	s := &Snapshot{Points: []Point{
		{Metric: Stars, Value: int64(r.StargazersCount)},
		{Metric: Forks, Value: int64(r.ForksCount)},
		{Metric: Watchers, Value: int64(r.SubscribersCount)},
		{Metric: OpenIssues, Value: int64(r.OpenIssuesCount)},
	}}
	for _, rel := range rels {
		for _, a := range rel.Assets {
			s.Points = append(s.Points, Point{Metric: Downloads, Tag: rel.TagName, Asset: a.Name, Value: a.DownloadCount})
		}
	}
	if n, err := gh.DiscussionCount(ctx, repo); err != nil {
		s.Notes = append(s.Notes, "discussions not recorded: "+err.Error()+" (set ASTROLABE_GITHUB_TOKEN to count them)")
	} else {
		s.Points = append(s.Points, Point{Metric: Discussions, Value: n})
	}
	return s, nil
}

// Day is the calendar day (UTC) a snapshot belongs to.
func Day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Store saves a snapshot for a day. Storing the same day again replaces that
// day's values, so reruns never double-count.
func Store(ctx context.Context, q store.Q, productID, monitorID, repo string, day time.Time, s *Snapshot) error {
	for _, p := range s.Points {
		if _, err := q.Exec(ctx, `
			INSERT INTO counter_snapshots (product_id, monitor_id, repo, day, metric, tag, asset, value)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (product_id, monitor_id, day, metric, tag, asset)
			DO UPDATE SET value = $8, repo = $3, taken_at = now()`,
			productID, monitorID, repo, Day(day), p.Metric, p.Tag, p.Asset, p.Value); err != nil {
			return err
		}
	}
	return nil
}

// Window is a span of days, inclusive of both ends' snapshots: the change
// over a window is the value on To minus the value on From.
type Window struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Change is a current total with its change over the last 7 days and the 7
// days before. A nil change means there was no earlier snapshot to compare to.
type Change struct {
	Total     int64  `json:"total"`
	Week      *int64 `json:"last_7_days,omitempty"`
	PrevWeek  *int64 `json:"previous_7_days,omitempty"`
	CountedAt string `json:"counted_since,omitempty"` // set when history is shorter than 7 days
}

type AssetChange struct {
	Tag   string `json:"release"`
	Asset string `json:"asset"`
	Change
}

// Repo is the report for one counter monitor.
type Repo struct {
	Monitor     string        `json:"monitor"`
	Repo        string        `json:"repo"`
	LatestDay   string        `json:"latest_snapshot"`
	Downloads   Change        `json:"downloads_total"`
	Assets      []AssetChange `json:"downloads_by_asset,omitempty"`
	Stars       *Change       `json:"stars,omitempty"`
	Forks       *Change       `json:"forks,omitempty"`
	Watchers    *Change       `json:"watchers,omitempty"`
	OpenIssues  *Change       `json:"open_issues,omitempty"`
	Discussions *Change       `json:"discussions,omitempty"`
}

type Report struct {
	Product string   `json:"product"`
	Current Window   `json:"last_7_days"`
	Prev    Window   `json:"previous_7_days"`
	Repos   []Repo   `json:"repos"`
	Notes   []string `json:"notes"`
}

type seriesKey struct{ monitor, metric, tag, asset string }

type series struct {
	days []time.Time // ascending
	vals []int64
}

// at returns the latest value on or before day.
func (s *series) at(day time.Time) (int64, bool) {
	v, ok := int64(0), false
	for i, d := range s.days {
		if d.After(day) {
			break
		}
		v, ok = s.vals[i], true
	}
	return v, ok
}

// delta is the change from one day to another. With no snapshot on or before
// from: a series that appeared after its monitor already had history is new,
// so it started at 0; otherwise the earliest snapshot is the baseline.
func (s *series) delta(from, to time.Time, monitorFirst time.Time) (d int64, ok bool, since time.Time) {
	end, ok := s.at(to)
	if !ok {
		return 0, false, time.Time{}
	}
	if base, has := s.at(from); has {
		return end - base, true, time.Time{}
	}
	if monitorFirst.Before(s.days[0]) {
		return end, true, time.Time{} // new series: it started at 0
	}
	if s.days[0].Equal(to) || s.days[0].After(to) {
		return 0, false, time.Time{} // a single snapshot: no change to show yet
	}
	return end - s.vals[0], true, s.days[0]
}

// Weekly builds the report for a product as of now: the change over the last
// 7 days (the snapshot 7 days ago to the latest) against the 7 days before.
func Weekly(ctx context.Context, q store.Q, productID string, now time.Time) (*Report, error) {
	to := Day(now)
	mid, from := to.AddDate(0, 0, -7), to.AddDate(0, 0, -14)
	rep := &Report{Product: productID,
		Current: Window{mid.Format("2006-01-02"), to.Format("2006-01-02")},
		Prev:    Window{from.Format("2006-01-02"), mid.Format("2006-01-02")}}
	rows, err := q.Query(ctx, `
		SELECT monitor_id, repo, day, metric, tag, asset, value FROM counter_snapshots
		WHERE product_id = $1 AND day <= $2 ORDER BY monitor_id, day`, productID, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := map[seriesKey]*series{}
	repos := map[string]string{}
	first := map[string]time.Time{}
	latest := map[string]time.Time{}
	for rows.Next() {
		var k seriesKey
		var repo string
		var day time.Time
		var v int64
		if err := rows.Scan(&k.monitor, &repo, &day, &k.metric, &k.tag, &k.asset, &v); err != nil {
			return nil, err
		}
		day = Day(day)
		repos[k.monitor] = repo
		if f, ok := first[k.monitor]; !ok || day.Before(f) {
			first[k.monitor] = day
		}
		if day.After(latest[k.monitor]) {
			latest[k.monitor] = day
		}
		s := all[k]
		if s == nil {
			s = &series{}
			all[k] = s
		}
		s.days, s.vals = append(s.days, day), append(s.vals, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var monitors []string
	for m := range repos {
		monitors = append(monitors, m)
	}
	sort.Strings(monitors)
	for _, mon := range monitors {
		r := Repo{Monitor: mon, Repo: repos[mon], LatestDay: latest[mon].Format("2006-01-02")}
		change := func(s *series) Change {
			total, _ := s.at(to)
			c := Change{Total: total}
			if d, ok, since := s.delta(mid, to, first[mon]); ok {
				c.Week = &d
				if !since.IsZero() {
					c.CountedAt = since.Format("2006-01-02")
				}
			}
			if d, ok, since := s.delta(from, mid, first[mon]); ok && since.IsZero() {
				c.PrevWeek = &d
			}
			return c
		}
		var dl Change
		var sumWeek, sumPrev, sumTotal int64
		var haveWeek, havePrev bool
		var since string
		for k, s := range all {
			if k.monitor != mon || k.metric != Downloads {
				continue
			}
			c := change(s)
			r.Assets = append(r.Assets, AssetChange{Tag: k.tag, Asset: k.asset, Change: c})
			sumTotal += c.Total
			if c.Week != nil {
				sumWeek, haveWeek = sumWeek+*c.Week, true
			}
			if c.PrevWeek != nil {
				sumPrev, havePrev = sumPrev+*c.PrevWeek, true
			}
			if c.CountedAt != "" && (since == "" || c.CountedAt < since) {
				since = c.CountedAt
			}
		}
		dl.Total, dl.CountedAt = sumTotal, since
		if haveWeek {
			dl.Week = &sumWeek
		}
		if havePrev {
			dl.PrevWeek = &sumPrev
		}
		r.Downloads = dl
		sort.Slice(r.Assets, func(i, j int) bool {
			a, b := r.Assets[i], r.Assets[j]
			if a.Tag != b.Tag {
				return a.Tag > b.Tag
			}
			return a.Asset < b.Asset
		})
		for metric, dst := range map[string]**Change{Stars: &r.Stars, Forks: &r.Forks, Watchers: &r.Watchers, OpenIssues: &r.OpenIssues, Discussions: &r.Discussions} {
			if s := all[seriesKey{mon, metric, "", ""}]; s != nil {
				c := change(s)
				*dst = &c
			}
		}
		if r.Discussions == nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("%s: discussions are not recorded (they need ASTROLABE_GITHUB_TOKEN); the other numbers are unaffected.", mon))
		}
		rep.Repos = append(rep.Repos, r)
	}
	rep.Notes = append(rep.Notes,
		"A download count counts downloads of a release file, not installs or people: one person can download many times, and CI and mirrors count too.",
		"Open issues includes open pull requests (GitHub's own count).")
	if len(rep.Repos) == 0 {
		rep.Notes = append([]string{"No counter snapshots yet: add a github_release monitor to monitors.yaml and run it."}, rep.Notes...)
	}
	return rep, nil
}

func signed(n *int64) string {
	if n == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+d", *n)
}

func line(name string, c Change) string {
	s := fmt.Sprintf("%s %d (7d %s, prior 7d %s)", name, c.Total, signed(c.Week), signed(c.PrevWeek))
	if c.CountedAt != "" {
		s += " [history starts " + c.CountedAt + "]"
	}
	return s
}

// Text renders the report for the weekly digest and chat.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Public counters, last 7 days %s to %s (prior 7 days %s to %s):\n", r.Current.From, r.Current.To, r.Prev.From, r.Prev.To)
	for _, x := range r.Repos {
		fmt.Fprintf(&b, "  %s: %s", x.Repo, line("release downloads", x.Downloads))
		for _, c := range []struct {
			n string
			c *Change
		}{{"stars", x.Stars}, {"forks", x.Forks}, {"watchers", x.Watchers}, {"open issues", x.OpenIssues}, {"discussions", x.Discussions}} {
			if c.c != nil {
				b.WriteString("; " + line(c.n, *c.c))
			}
		}
		b.WriteString("\n")
		for _, a := range x.Assets {
			fmt.Fprintf(&b, "    %s\n", line(a.Tag+" "+a.Asset, a.Change))
		}
	}
	b.WriteString("  Downloads count downloads, not installs or people.\n")
	return b.String()
}

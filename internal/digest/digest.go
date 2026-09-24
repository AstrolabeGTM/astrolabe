// Package digest writes the weekly "what's working and what to do" note per
// product and overall, sends it to me, and keeps it for chat and the web.
// The actions are rule-based from the numbers, each tied to people or a
// specific stage, so they are checkable rather than generated prose.
package digest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/experiment"
	"github.com/AstrolabeGTM/astrolabe/internal/funnel"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/score"
)

type ProductDigest struct {
	ID          string              `json:"product"`
	Name        string              `json:"name"`
	Moved       []Moved             `json:"moved"`
	Plays       []funnel.Attrib     `json:"plays"`
	Experiments []experiment.Result `json:"experiments"`
	Stuck       []funnel.StuckStage `json:"stuck"`
	Uncontacted []score.Ranked      `json:"a_tier_not_contacted"`
	Drop        string              `json:"biggest_drop,omitempty"`
	Revenue     []funnel.Money      `json:"revenue_this_week"`
	Actions     []string            `json:"actions"`
}

// Moved: people entering a stage this week vs the week before.
type Moved struct {
	Stage    string `json:"stage"`
	ThisWeek int    `json:"this_week"`
	LastWeek int    `json:"last_week"`
}

type Digest struct {
	Week     time.Time       `json:"week"`
	Products []ProductDigest `json:"products"`
}

// WeekStart is Monday 00:00 of t's week in t's location.
func WeekStart(t time.Time) time.Time {
	d := (int(t.Weekday()) + 6) % 7
	y, m, day := t.AddDate(0, 0, -d).Date()
	return time.Date(y, m, day, 0, 0, 0, 0, t.Location())
}

// Build summarises the last 7 days before now for every product.
func Build(ctx context.Context, pool *pgxpool.Pool, products []*product.Product, now time.Time) (*Digest, error) {
	d := &Digest{Week: WeekStart(now)}
	weekAgo, twoWeeks := now.AddDate(0, 0, -7), now.AddDate(0, 0, -14)
	for _, p := range products {
		pd := ProductDigest{ID: p.ID, Name: p.Name}
		for _, st := range p.Funnel {
			var m Moved
			m.Stage = st
			if err := pool.QueryRow(ctx, `
				WITH first AS (SELECT person_id, min(occurred_at) t FROM funnel_events WHERE product_id = $1 AND stage = $2 GROUP BY 1)
				SELECT count(*) FILTER (WHERE t >= $3), count(*) FILTER (WHERE t >= $4 AND t < $3) FROM first`,
				p.ID, st, weekAgo, twoWeeks).Scan(&m.ThisWeek, &m.LastWeek); err != nil {
				return nil, err
			}
			pd.Moved = append(pd.Moved, m)
		}
		rep, err := funnel.BuildReport(ctx, pool, p, now.AddDate(0, 0, -30), now)
		if err != nil {
			return nil, err
		}
		pd.Plays, pd.Stuck = rep.Plays, nil
		for _, s := range rep.Stuck {
			if s.Count > 0 {
				pd.Stuck = append(pd.Stuck, s)
			}
		}
		weekRep, err := funnel.BuildReport(ctx, pool, p, weekAgo, now)
		if err != nil {
			return nil, err
		}
		pd.Revenue = weekRep.Revenue
		if pd.Experiments, err = experiment.Report(ctx, pool, p); err != nil {
			return nil, err
		}
		ranked, err := score.List(ctx, pool, p.ID, "A", true, 50)
		if err != nil {
			return nil, err
		}
		for _, r := range ranked {
			if !r.Contacted && r.Enrolled == "" && len(pd.Uncontacted) < 5 {
				pd.Uncontacted = append(pd.Uncontacted, r)
			}
		}
		// Biggest observed drop over 30 days, among stages with enough people.
		worst := 101.0
		for i := 1; i < len(rep.Stages); i++ {
			prev, cur := rep.Stages[i-1], rep.Stages[i]
			if prev.People >= 5 && cur.FromPrevious < worst {
				worst = cur.FromPrevious
				pd.Drop = fmt.Sprintf("%s → %s: %.0f%% (%d of %d)", prev.Name, cur.Name, cur.FromPrevious, cur.People, prev.People)
			}
		}
		pd.Actions = actions(p, &pd)
		d.Products = append(d.Products, pd)
	}
	return d, nil
}

func actions(p *product.Product, pd *ProductDigest) []string {
	var out []string
	if len(pd.Uncontacted) > 0 {
		var names []string
		for _, r := range pd.Uncontacted {
			names = append(names, fmt.Sprintf("%s (#%d)", orID(r.Name, r.PersonID), r.PersonID))
		}
		out = append(out, fmt.Sprintf("Contact %d A-tier people not yet reached: %s.", len(pd.Uncontacted), strings.Join(names, ", ")))
	}
	for _, s := range pd.Stuck {
		if len(out) == 3 {
			break
		}
		if s.Stage == p.Funnel[len(p.Funnel)-1] {
			continue
		}
		names := []string{}
		for i, x := range s.People {
			if i == 3 {
				break
			}
			names = append(names, orID(x.Name, x.ID))
		}
		out = append(out, fmt.Sprintf("Help the %d people stuck at %s for over %dd (e.g. %s), or add a stage_stuck automation.", s.Count, s.Stage, s.Days, strings.Join(names, ", ")))
	}
	for _, e := range pd.Experiments {
		if len(out) < 3 && strings.Contains(e.Verdict, " wins:") {
			out = append(out, fmt.Sprintf("Experiment %s: %s — make it the default.", e.Key, e.Verdict))
		}
	}
	if len(out) < 3 && pd.Drop != "" {
		out = append(out, "Biggest drop is "+pd.Drop+"; fix the offer or onboarding step there before finding more people.")
	}
	return out
}

func orID(name string, id int64) string {
	if name != "" {
		return name
	}
	return fmt.Sprintf("#%d", id)
}

// Text renders the digest as plain text for email and chat.
func (d *Digest) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Astrolabe weekly — week of %s\n", d.Week.Format("2 Jan 2006"))
	for _, p := range d.Products {
		fmt.Fprintf(&b, "\n== %s ==\n", p.Name)
		b.WriteString("Moved this week (last week):")
		for _, m := range p.Moved {
			fmt.Fprintf(&b, " %s %d (%d);", m.Stage, m.ThisWeek, m.LastWeek)
		}
		b.WriteString("\n")
		for _, m := range p.Revenue {
			fmt.Fprintf(&b, "Revenue this week: %.2f %s net (%d payments)\n", float64(m.Net)/100, strings.ToUpper(m.Currency), m.Count)
		}
		for _, pl := range p.Plays {
			fmt.Fprintf(&b, "Automation %s (30d): %d enrolled, %d replied, %d activated, %d paid\n", pl.Key, pl.People, pl.Replied, pl.Activated, pl.Paid)
		}
		for _, e := range p.Experiments {
			fmt.Fprintf(&b, "Experiment %s: %s\n", e.Key, e.Verdict)
		}
		if p.Drop != "" {
			fmt.Fprintf(&b, "Biggest drop (30d): %s\n", p.Drop)
		}
		if len(p.Actions) > 0 {
			b.WriteString("Do this week:\n")
			for i, a := range p.Actions {
				fmt.Fprintf(&b, "  %d. %s\n", i+1, a)
			}
		} else {
			b.WriteString("Nothing urgent from the numbers.\n")
		}
	}
	return b.String()
}

// Save stores this week's digest (once per week) and returns whether it was new.
func Save(ctx context.Context, pool *pgxpool.Pool, d *Digest, sentTo string) (bool, error) {
	data, _ := json.Marshal(d)
	tag, err := pool.Exec(ctx, `INSERT INTO digests (week, body, data, sent_to) VALUES ($1, $2, $3, $4) ON CONFLICT (week) DO NOTHING`,
		d.Week, d.Text(), data, sentTo)
	return tag.RowsAffected() == 1, err
}

// Latest returns the most recent stored digest text.
func Latest(ctx context.Context, pool *pgxpool.Pool) (string, time.Time, error) {
	var body string
	var at time.Time
	err := pool.QueryRow(ctx, `SELECT body, created_at FROM digests ORDER BY week DESC LIMIT 1`).Scan(&body, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, nil
	}
	return body, at, err
}

// ---- weekly job ----

type Args struct{}

func (Args) Kind() string { return "digest" }

type Worker struct {
	river.WorkerDefaults[Args]
	Pool     *pgxpool.Pool
	Products func() []*product.Product
	Senders  channel.Senders
}

// Work runs hourly; on Monday from 08:00 local it builds, sends and stores
// the week's digest once.
func (w *Worker) Work(ctx context.Context, _ *river.Job[Args]) error {
	now := time.Now()
	if now.Weekday() != time.Monday || now.Hour() < 8 {
		return nil
	}
	var exists bool
	if err := w.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM digests WHERE week = $1)`, WeekStart(now)).Scan(&exists); err != nil || exists {
		return err
	}
	d, err := Build(ctx, w.Pool, w.Products(), now)
	if err != nil {
		return err
	}
	to, from := os.Getenv("ASTROLABE_DIGEST_TO"), os.Getenv("ASTROLABE_DIGEST_FROM")
	sentTo := ""
	if to != "" && from != "" {
		msg := channel.Message{Channel: "email", Kind: "users", From: from, To: to,
			Subject: "Astrolabe weekly: " + d.Week.Format("2 Jan"), Body: d.Text(),
			MessageID:      fmt.Sprintf("<digest.%s@%s>", d.Week.Format("20060102"), from[strings.LastIndex(from, "@")+1:]),
			IdempotencyKey: "astrolabe-digest-" + d.Week.Format("2006-01-02")}
		sender, err := w.Senders(msg)
		if err == nil {
			_, err = sender.Send(ctx, msg)
		}
		if err != nil {
			slog.Error("digest email", "err", err) // still stored for chat and web
		} else {
			sentTo = to
		}
	}
	_, err = Save(ctx, w.Pool, d, sentTo)
	return err
}

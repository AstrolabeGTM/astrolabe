// Package score ranks people per product with readable rules:
// fit (ICP facts), intent (decaying signal strength) and why-now (recent
// time-bound triggers). Scores rank prospects; they never authorise contact.
package score

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

const (
	halfLifeDays = 10.0
	whyNowWindow = 14 * 24 * time.Hour
	// Coworkers' signals count at half strength toward a person's intent.
	companyWeight = 0.5
)

type Breakdown struct {
	Fit      []string `json:"fit"`
	Intent   []string `json:"intent"`
	WhyNow   []string `json:"why_now"`
	Excluded string   `json:"excluded,omitempty"`
}

type Score struct {
	ProductID string    `json:"product"`
	PersonID  int64     `json:"person_id"`
	Fit       int       `json:"fit"`
	Intent    int       `json:"intent"`
	WhyNow    int       `json:"why_now"`
	Priority  int       `json:"priority"`
	Tier      string    `json:"tier"`
	Eligible  bool      `json:"eligible"`
	Breakdown Breakdown `json:"breakdown"`
}

// Summary is the one-line explanation shown next to a score.
func (s Score) Summary() string {
	out := fmt.Sprintf("%s %d · fit %d", s.Tier, s.Priority, s.Fit)
	if len(s.Breakdown.Fit) > 0 {
		out += " (" + join(s.Breakdown.Fit) + ")"
	}
	out += fmt.Sprintf(" · intent %d", s.Intent)
	if len(s.Breakdown.Intent) > 0 {
		out += " (" + join(s.Breakdown.Intent) + ")"
	}
	if s.WhyNow > 0 {
		out += fmt.Sprintf(" · why now %d (%s)", s.WhyNow, join(s.Breakdown.WhyNow))
	}
	if s.Breakdown.Excluded != "" {
		out += " · excluded: " + s.Breakdown.Excluded
	}
	return out
}

func join(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += "; "
		}
		out += x
	}
	return out
}

func Tier(priority int) string {
	switch {
	case priority >= 70:
		return "A"
	case priority >= 50:
		return "B"
	case priority >= 30:
		return "C"
	}
	return "D"
}

// Compute scores one person for one product at a point in time.
func Compute(ctx context.Context, q store.Q, p *product.Product, personID int64, now time.Time) (Score, error) {
	sc := Score{ProductID: p.ID, PersonID: personID, Eligible: true}

	var personFacts, companyFacts []byte
	var companyID *int64
	var suppressed *string
	if err := q.QueryRow(ctx, `
		SELECT p.facts, COALESCE(c.facts, '{}'), p.company_id, s.reason
		FROM people p LEFT JOIN companies c ON c.id = p.company_id LEFT JOIN suppressions s ON s.person_id = p.id
		WHERE p.id = $1`, personID).Scan(&personFacts, &companyFacts, &companyID, &suppressed); err != nil {
		return sc, err
	}
	facts := map[string]string{}
	json.Unmarshal(companyFacts, &facts)
	pf := map[string]string{}
	json.Unmarshal(personFacts, &pf)
	for k, v := range pf {
		facts[k] = v
	}

	fit := 0
	for _, r := range p.Fit {
		hit := ""
		for _, f := range r.Any {
			if _, ok := facts[f]; ok {
				hit = f
				break
			}
		}
		if hit == "" {
			continue
		}
		label := r.Label
		if label == "" {
			label = hit
		}
		if r.Exclude {
			sc.Eligible = false
			sc.Breakdown.Excluded = label
			continue
		}
		fit += r.Weight
		sc.Breakdown.Fit = append(sc.Breakdown.Fit, fmt.Sprintf("%s +%d", label, r.Weight))
	}
	sc.Fit = min(100, fit)
	if suppressed != nil {
		sc.Eligible = false
		sc.Breakdown.Excluded = "suppressed: " + *suppressed
	}

	rows, err := q.Query(ctx, `
		SELECT type, occurred_at, strength, why_now, person_id = $2 AS own
		FROM signals
		WHERE product_id = $1 AND occurred_at <= $4 AND occurred_at > $4 - interval '120 days'
		  AND (person_id = $2 OR (company_id = $3 AND $3 IS NOT NULL))`, p.ID, personID, companyID, now)
	if err != nil {
		return sc, err
	}
	type contrib struct {
		label string
		v     float64
	}
	var intent, whyNow []contrib
	err = forEach(rows, func(r pgx.Rows) error {
		var typ string
		var at time.Time
		var strength int
		var why bool
		var own *bool
		if err := r.Scan(&typ, &at, &strength, &why, &own); err != nil {
			return err
		}
		w := 1.0
		who := ""
		if own == nil || !*own {
			w, who = companyWeight, "coworker "
		}
		days := now.Sub(at).Hours() / 24
		label := fmt.Sprintf("%s%s %s", who, typ, agoDays(days))
		intent = append(intent, contrib{label, float64(strength) * w * math.Pow(0.5, days/halfLifeDays)})
		if why && now.Sub(at) <= whyNowWindow {
			whyNow = append(whyNow, contrib{label, float64(strength) * w})
		}
		return nil
	})
	if err != nil {
		return sc, err
	}
	total := func(cs []contrib, into *[]string) int {
		sort.Slice(cs, func(i, j int) bool { return cs[i].v > cs[j].v })
		sum := 0.0
		for i, c := range cs {
			sum += c.v
			if i < 3 && math.Round(c.v) >= 1 {
				*into = append(*into, fmt.Sprintf("%s +%.0f", c.label, c.v))
			}
		}
		if len(cs) > 3 {
			*into = append(*into, fmt.Sprintf("%d more", len(cs)-3))
		}
		return min(100, int(math.Round(sum)))
	}
	sc.Intent = total(intent, &sc.Breakdown.Intent)
	sc.WhyNow = total(whyNow, &sc.Breakdown.WhyNow)
	sc.Priority = int(math.Round(0.4*float64(sc.Fit) + 0.4*float64(sc.Intent) + 0.2*float64(sc.WhyNow)))
	sc.Tier = Tier(sc.Priority)
	return sc, nil
}

func agoDays(d float64) string {
	switch {
	case d < 1:
		return "today"
	case d < 2:
		return "1d ago"
	}
	return fmt.Sprintf("%.0fd ago", d)
}

func forEach(rows pgx.Rows, f func(pgx.Rows) error) error {
	defer rows.Close()
	for rows.Next() {
		if err := f(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func Save(ctx context.Context, q store.Q, sc Score, now time.Time) error {
	b, _ := json.Marshal(sc.Breakdown)
	_, err := q.Exec(ctx, `
		INSERT INTO scores (product_id, person_id, fit, intent, why_now, priority, tier, eligible, breakdown, computed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (product_id, person_id) DO UPDATE SET fit = $3, intent = $4, why_now = $5, priority = $6,
			tier = $7, eligible = $8, breakdown = $9, computed_at = $10`,
		sc.ProductID, sc.PersonID, sc.Fit, sc.Intent, sc.WhyNow, sc.Priority, sc.Tier, sc.Eligible, b, now)
	return err
}

// Recompute scores and saves one person for one product.
func Recompute(ctx context.Context, q store.Q, productID string, personID int64, now time.Time) (Score, error) {
	p, err := product.Get(ctx, q, productID)
	if err != nil {
		return Score{}, err
	}
	sc, err := Compute(ctx, q, p, personID, now)
	if err != nil {
		return sc, err
	}
	return sc, Save(ctx, q, sc, now)
}

// Get returns the stored score, or false if none.
func Get(ctx context.Context, q store.Q, productID string, personID int64) (Score, bool, error) {
	sc := Score{ProductID: productID, PersonID: personID}
	var b []byte
	err := q.QueryRow(ctx, `SELECT fit, intent, why_now, priority, tier, eligible, breakdown FROM scores WHERE product_id = $1 AND person_id = $2`,
		productID, personID).Scan(&sc.Fit, &sc.Intent, &sc.WhyNow, &sc.Priority, &sc.Tier, &sc.Eligible, &b)
	if err == pgx.ErrNoRows {
		return sc, false, nil
	}
	json.Unmarshal(b, &sc.Breakdown)
	return sc, err == nil, err
}

// Args asks for one person's score to be recomputed.
type Args struct {
	Product  string `json:"product"`
	PersonID int64  `json:"person_id"`
}

func (Args) Kind() string { return "score" }

func (Args) InsertOpts() river.InsertOpts {
	return river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: time.Minute}}
}

// AfterScore runs after a score is saved (plays hook in here).
type AfterScore func(ctx context.Context, sc Score) error

type Worker struct {
	river.WorkerDefaults[Args]
	Pool  *pgxpool.Pool
	After []AfterScore
}

func (w *Worker) Work(ctx context.Context, job *river.Job[Args]) error {
	sc, err := Recompute(ctx, w.Pool, job.Args.Product, job.Args.PersonID, time.Now())
	if err != nil {
		return err
	}
	for _, f := range w.After {
		if err := f(ctx, sc); err != nil {
			return err
		}
	}
	return nil
}

// DailyArgs recomputes every scored or recently signalled person, since
// intent decays with time.
type DailyArgs struct{}

func (DailyArgs) Kind() string { return "score_daily" }

type DailyWorker struct {
	river.WorkerDefaults[DailyArgs]
	Pool *pgxpool.Pool
}

func (w *DailyWorker) Work(ctx context.Context, _ *river.Job[DailyArgs]) error {
	_, err := RecomputeAll(ctx, w.Pool, "", time.Now())
	return err
}

// RecomputeAll rescores everyone with a score or a signal in the last 120
// days, optionally for one product. It returns how many were scored.
func RecomputeAll(ctx context.Context, pool *pgxpool.Pool, productID string, now time.Time) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT product_id, person_id FROM (
			SELECT product_id, person_id FROM scores
			UNION SELECT product_id, person_id FROM signals WHERE person_id IS NOT NULL AND occurred_at > $2::timestamptz - interval '120 days'
			UNION SELECT product_id, person_id FROM product_people
		) x WHERE $1 = '' OR product_id = $1`, productID, now)
	if err != nil {
		return 0, err
	}
	type key struct {
		product string
		person  int64
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (key, error) {
		var k key
		return k, r.Scan(&k.product, &k.person)
	})
	if err != nil {
		return 0, err
	}
	products := map[string]*product.Product{}
	for _, k := range keys {
		p, ok := products[k.product]
		if !ok {
			if p, err = product.Get(ctx, pool, k.product); err != nil {
				return 0, err
			}
			products[k.product] = p
		}
		sc, err := Compute(ctx, pool, p, k.person, now)
		if err != nil {
			return 0, err
		}
		if err := Save(ctx, pool, sc, now); err != nil {
			return 0, err
		}
	}
	return len(keys), nil
}

// Ranked is a person with their score, for "who should I contact" lists.
type Ranked struct {
	Score
	Name      string `json:"name"`
	Company   string `json:"company,omitempty"`
	Email     string `json:"email,omitempty"`
	GitHub    string `json:"github,omitempty"`
	Contacted bool   `json:"contacted"`
	Enrolled  string `json:"active_sequence,omitempty"`
	Summary   string `json:"why"`
}

// List returns scored people for a product, best first. minTier "B"
// returns A and B; onlyEligible hides excluded and suppressed people.
func List(ctx context.Context, q store.Q, productID, minTier string, onlyEligible bool, limit int) ([]Ranked, error) {
	if minTier == "" {
		minTier = "D"
	}
	rows, err := q.Query(ctx, `
		SELECT s.person_id, s.fit, s.intent, s.why_now, s.priority, s.tier, s.eligible, s.breakdown,
			p.name, COALESCE(NULLIF(c.name, ''), c.domain, c.github_org, ''),
			COALESCE((SELECT value FROM identities WHERE person_id = p.id AND kind = 'email' LIMIT 1), ''),
			COALESCE((SELECT value FROM identities WHERE person_id = p.id AND kind = 'github' LIMIT 1), ''),
			EXISTS (SELECT 1 FROM actions a WHERE a.person_id = p.id AND a.product_id = s.product_id AND a.status = 'sent'),
			COALESCE((SELECT sequence_id FROM enrollments e WHERE e.person_id = p.id AND e.product_id = s.product_id AND e.status = 'active'), '')
		FROM scores s JOIN people p ON p.id = s.person_id LEFT JOIN companies c ON c.id = p.company_id
		WHERE s.product_id = $1 AND s.tier <= $2 AND (s.eligible OR NOT $3)
		ORDER BY s.priority DESC, s.person_id LIMIT $4`, productID, minTier, onlyEligible, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ranked
	for rows.Next() {
		r := Ranked{Score: Score{ProductID: productID}}
		var b []byte
		if err := rows.Scan(&r.PersonID, &r.Fit, &r.Intent, &r.WhyNow, &r.Priority, &r.Tier, &r.Eligible, &b,
			&r.Name, &r.Company, &r.Email, &r.GitHub, &r.Contacted, &r.Enrolled); err != nil {
			return nil, err
		}
		json.Unmarshal(b, &r.Breakdown)
		r.Summary = r.Score.Summary()
		out = append(out, r)
	}
	return out, rows.Err()
}

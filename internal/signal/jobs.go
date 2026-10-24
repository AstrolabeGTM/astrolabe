package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/AstrolabeGTM/astrolabe/internal/counters"
	"github.com/AstrolabeGTM/astrolabe/internal/github"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/reception"
	"github.com/AstrolabeGTM/astrolabe/internal/score"
)

// On the first run a monitor looks back this far; later runs overlap the
// previous one a little so late-indexed results are not missed.
const (
	firstLookback = 7 * 24 * time.Hour
	runOverlap    = time.Hour
	enrichEvery   = 30 * 24 * time.Hour
)

var pendingOnly = []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending,
	rivertype.JobStateRetryable, rivertype.JobStateRunning, rivertype.JobStateScheduled}

// ScoreAndEnrich is the standard ingest hook: rescore the person, and
// enrich them from GitHub if they have a login and haven't been lately.
func (svc *Service) ScoreAndEnrich(ctx context.Context, tx pgx.Tx, s *Signal, r Result) error {
	if r.PersonID == nil {
		return nil
	}
	if _, err := svc.River.InsertTx(ctx, tx, score.Args{Product: s.Product, PersonID: *r.PersonID}, nil); err != nil {
		return err
	}
	var login string
	var enriched *time.Time
	err := tx.QueryRow(ctx, `
		SELECT COALESCE((SELECT value FROM identities WHERE person_id = p.id AND kind = 'github' LIMIT 1), ''), p.enriched_at
		FROM people p WHERE p.id = $1`, *r.PersonID).Scan(&login, &enriched)
	if err != nil {
		return err
	}
	if login != "" && (enriched == nil || time.Since(*enriched) > enrichEvery || s.Repo != "") {
		_, err = svc.River.InsertTx(ctx, tx, EnrichArgs{PersonID: *r.PersonID, Repo: s.Repo}, nil)
	}
	return err
}

type MonitorArgs struct {
	Product string `json:"product"`
	Monitor string `json:"monitor"`
}

func (MonitorArgs) Kind() string { return "monitor" }

func (MonitorArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: pendingOnly}}
}

type MonitorWorker struct {
	river.WorkerDefaults[MonitorArgs]
	S       *Service
	Sources *Sources
}

func (w *MonitorWorker) Timeout(*river.Job[MonitorArgs]) time.Duration { return 10 * time.Minute }

func (w *MonitorWorker) Work(ctx context.Context, job *river.Job[MonitorArgs]) error {
	_, err := w.S.RunMonitor(ctx, w.Sources, job.Args.Product, job.Args.Monitor, time.Now())
	return err
}

// RunMonitor runs one monitor now and ingests what it finds. It returns the
// number of new signals. Partial results are kept even if a source failed.
func (svc *Service) RunMonitor(ctx context.Context, src *Sources, productID, monitorID string, now time.Time) (int, error) {
	p, err := product.Get(ctx, svc.Pool, productID)
	if err != nil {
		return 0, err
	}
	var m *product.Monitor
	for _, x := range p.Monitors {
		if x.ID == monitorID {
			m = x
		}
	}
	if m == nil {
		return 0, fmt.Errorf("product %s has no monitor %s", productID, monitorID)
	}
	if product.CounterMonitor(m.Type) {
		return 0, svc.runCounter(ctx, src, productID, m, now)
	}
	since := now.Add(-firstLookback)
	var last time.Time
	if err := svc.Pool.QueryRow(ctx, `SELECT last_ok_at FROM monitor_runs WHERE product_id = $1 AND monitor_id = $2 AND last_ok_at IS NOT NULL`,
		productID, monitorID).Scan(&last); err == nil {
		since = last.Add(-runOverlap)
	}
	var sigs []Signal
	var runErr error
	n := 0
	if product.FormMonitor(m.Type) {
		if m.Type == "form_webhook" {
			return 0, fmt.Errorf("monitor %s receives form posts at /hooks/forms/%s; it is not polled", monitorID, productID)
		}
		if svc.Forms == nil {
			return 0, errors.New("form intake is not configured in this process")
		}
		n, runErr = svc.Forms.Poll(ctx, p, m, since, now)
		sigs = make([]Signal, n) // only the count matters below
	} else {
		sigs, runErr = src.Run(ctx, productID, m, since)
		if m.Reception {
			if err := svc.fileMentions(ctx, productID, m, sigs); err != nil {
				return 0, fmt.Errorf("reception log: %w", err)
			}
		}
		for i := range sigs {
			res, err := svc.Ingest(ctx, &sigs[i])
			if err != nil {
				return n, fmt.Errorf("ingest %s: %w", sigs[i].DedupeKey, err)
			}
			if !res.Duplicate {
				n++
			}
		}
	}
	errMsg := ""
	if runErr != nil {
		errMsg = runErr.Error()
		slog.Warn("monitor had errors", "product", productID, "monitor", monitorID, "err", runErr)
	}
	// A run with errors still counts as ok if it produced anything, so a
	// single flaky source doesn't make every later run look back a week.
	_, err = svc.Pool.Exec(ctx, `
		INSERT INTO monitor_runs (product_id, monitor_id, last_run_at, last_ok_at, last_error, last_count)
		VALUES ($1, $2, $3::timestamptz, CASE WHEN $4::text = '' OR $5::int > 0 THEN $3::timestamptz END, $4, $5)
		ON CONFLICT (product_id, monitor_id) DO UPDATE SET last_run_at = $3,
			last_ok_at = CASE WHEN $4::text = '' OR $5::int > 0 THEN $3::timestamptz ELSE monitor_runs.last_ok_at END,
			last_error = $4, last_count = $5`, productID, monitorID, now, errMsg, n)
	if err != nil {
		return n, err
	}
	if runErr != nil && len(sigs) == 0 {
		return n, runErr
	}
	return n, nil
}

// DueArgs is the periodic job that queues monitors whose interval has passed.
type DueArgs struct{}

func (DueArgs) Kind() string { return "monitors_due" }

type DueWorker struct {
	river.WorkerDefaults[DueArgs]
	S *Service
}

func (w *DueWorker) Work(ctx context.Context, _ *river.Job[DueArgs]) error {
	rows, err := w.S.Pool.Query(ctx, `SELECT id, config FROM products`)
	if err != nil {
		return err
	}
	type prod struct {
		id  string
		cfg []byte
	}
	prods, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (prod, error) {
		var p prod
		return p, r.Scan(&p.id, &p.cfg)
	})
	if err != nil {
		return err
	}
	for _, pr := range prods {
		var p product.Product
		if err := json.Unmarshal(pr.cfg, &p); err != nil {
			return err
		}
		for _, m := range p.Monitors {
			var last time.Time
			err := w.S.Pool.QueryRow(ctx, `SELECT last_run_at FROM monitor_runs WHERE product_id = $1 AND monitor_id = $2`, p.ID, m.ID).Scan(&last)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if product.PushMonitor(m.Type) || time.Since(last) < m.Interval() {
				continue
			}
			if _, err := w.S.River.Insert(ctx, MonitorArgs{Product: p.ID, Monitor: m.ID}, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- Enrichment (cheapest first: public GitHub data) ----

type EnrichArgs struct {
	PersonID int64  `json:"person_id"`
	Repo     string `json:"repo,omitempty"`
}

func (EnrichArgs) Kind() string { return "enrich" }

func (EnrichArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 5, UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: 24 * time.Hour}}
}

type EnrichWorker struct {
	river.WorkerDefaults[EnrichArgs]
	S      *Service
	GitHub *github.Client
}

func (w *EnrichWorker) Work(ctx context.Context, job *river.Job[EnrichArgs]) error {
	return w.S.Enrich(ctx, w.GitHub, job.Args.PersonID, job.Args.Repo)
}

// Enrich reads the person's public GitHub profile and, if given, the repo
// behind their signal: languages and declared dependencies become facts for
// fit scoring. Then it rescores the person everywhere they appear.
func (svc *Service) Enrich(ctx context.Context, gh *github.Client, personID int64, repo string) error {
	var login string
	err := svc.Pool.QueryRow(ctx, `SELECT value FROM identities WHERE person_id = $1 AND kind = 'github' LIMIT 1`, personID).Scan(&login)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	u, err := gh.User(ctx, login)
	if errors.Is(err, github.ErrNotFound) {
		u = &github.User{Login: login}
	} else if err != nil {
		return err
	}

	sub := Subject{}
	if org := strings.TrimPrefix(strings.Fields(u.Company + " ")[0], "@"); strings.HasPrefix(strings.TrimSpace(u.Company), "@") && org != "" {
		sub.GitHubOrg = strings.ToLower(org)
	}
	if d := blogDomain(u.Blog); d != "" && sub.GitHubOrg != "" {
		// A personal blog is not a company domain; only trust it next to an org.
		sub.CompanyDomain = d
	}
	companyID, err := upsertCompany(ctx, svc.Pool, sub)
	if err != nil {
		return err
	}

	personFacts, companyFacts := map[string]string{}, map[string]string{}
	var repoOwner string
	if repo != "" {
		repoOwner, _, _ = strings.Cut(strings.ToLower(repo), "/")
		facts, err := repoFacts(ctx, gh, repo)
		if err != nil {
			slog.Warn("repo facts", "repo", repo, "err", err)
		}
		// Facts about an org's repo describe the company; a personal repo describes the person.
		if repoOwner != "" && repoOwner != strings.ToLower(login) {
			companyFacts = facts
			if companyID == nil {
				if companyID, err = upsertCompany(ctx, svc.Pool, Subject{GitHubOrg: repoOwner}); err != nil {
					return err
				}
			}
		} else {
			personFacts = facts
		}
	}

	return pgx.BeginFunc(ctx, svc.Pool, func(tx pgx.Tx) error {
		pf, _ := json.Marshal(personFacts)
		if _, err := tx.Exec(ctx, `
			UPDATE people SET
				name = CASE WHEN name = '' THEN $2 ELSE name END,
				first_name = CASE WHEN first_name = '' THEN split_part($2, ' ', 1) ELSE first_name END,
				company_id = COALESCE(company_id, $3),
				facts = facts || $4, enriched_at = now()
			WHERE id = $1`, personID, u.Name, companyID, pf); err != nil {
			return err
		}
		if len(companyFacts) > 0 {
			cf, _ := json.Marshal(companyFacts)
			if _, err := tx.Exec(ctx, `UPDATE companies SET facts = facts || $2 WHERE github_org = $1`, repoOwner, cf); err != nil {
				return err
			}
		}
		// A public profile email is the person's own statement: link it
		// unless it already belongs to someone else.
		if e := people.NormalizeEmail(u.Email); e != "" {
			if err := people.LinkIdentity(ctx, tx, "email", e, personID, people.SourceGitHub, false); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT product_id FROM product_people WHERE person_id = $1`, personID)
		if err != nil {
			return err
		}
		products, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, p := range products {
			if _, err := svc.River.InsertTx(ctx, tx, score.Args{Product: p, PersonID: personID}, nil); err != nil {
				return err
			}
		}
		if svc.AfterEnrich != nil {
			return svc.AfterEnrich(ctx, tx, personID)
		}
		return nil
	})
}

func blogDomain(blog string) string {
	if blog == "" {
		return ""
	}
	if !strings.Contains(blog, "://") {
		blog = "https://" + blog
	}
	u, err := url.Parse(blog)
	if err != nil {
		return ""
	}
	h := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	for _, skip := range []string{"github.io", "github.com", "medium.com", "substack.com", "linkedin.com", "twitter.com", "x.com", "dev.to", "hashnode.dev", "notion.site", "vercel.app", "netlify.app"} {
		if h == skip || strings.HasSuffix(h, "."+skip) {
			return ""
		}
	}
	return h
}

// repoFacts: languages above 10% of code, and dependencies declared in
// package.json, requirements.txt or go.mod.
func repoFacts(ctx context.Context, gh *github.Client, repo string) (map[string]string, error) {
	facts := map[string]string{}
	langs, err := gh.Languages(ctx, repo)
	if err != nil {
		return facts, err
	}
	total := 0
	for _, n := range langs {
		total += n
	}
	for l, n := range langs {
		if total > 0 && n*10 >= total {
			facts["lang:"+strings.ToLower(l)] = repo
		}
	}
	if b, err := gh.File(ctx, repo, "package.json"); err == nil {
		var pkg struct{ Dependencies, DevDependencies map[string]string }
		if json.Unmarshal(b, &pkg) == nil {
			for d := range pkg.Dependencies {
				facts["dep:"+strings.ToLower(d)] = repo
			}
			for d := range pkg.DevDependencies {
				facts["dep:"+strings.ToLower(d)] = repo
			}
		}
	}
	if b, err := gh.File(ctx, repo, "requirements.txt"); err == nil {
		for _, d := range PythonRequirements(b) {
			facts["dep:"+d] = repo
			facts["pip:"+d] = repo
		}
	}
	if b, err := gh.File(ctx, repo, "pyproject.toml"); err == nil {
		for _, d := range PyprojectDeps(b) {
			facts["dep:"+d] = repo
		}
	}
	if b, err := gh.File(ctx, repo, "go.mod"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "require "))
			if len(f) >= 2 && strings.Contains(f[0], ".") && strings.HasPrefix(f[1], "v") {
				facts["go:"+strings.ToLower(f[0])] = repo
			}
		}
	}
	return facts, nil
}

// runCounter takes today's snapshot of a counter monitor's repo. It makes no
// signals and no people; running it again the same day replaces that day's
// numbers. Notes (such as skipped discussions) are kept as the run's message.
func (svc *Service) runCounter(ctx context.Context, src *Sources, productID string, m *product.Monitor, now time.Time) error {
	snap, runErr := counters.Collect(ctx, src.GitHub, m.Repo)
	note := ""
	if runErr == nil {
		runErr = counters.Store(ctx, svc.Pool, productID, m.ID, m.Repo, now, snap)
		note = strings.Join(snap.Notes, "; ")
	}
	errMsg := note
	if runErr != nil {
		errMsg = runErr.Error()
		slog.Warn("counter monitor failed", "product", productID, "monitor", m.ID, "err", runErr)
	}
	_, err := svc.Pool.Exec(ctx, `
		INSERT INTO monitor_runs (product_id, monitor_id, last_run_at, last_ok_at, last_error, last_count)
		VALUES ($1, $2, $3::timestamptz, CASE WHEN $5::boolean THEN NULL ELSE $3::timestamptz END, $4, 0)
		ON CONFLICT (product_id, monitor_id) DO UPDATE SET last_run_at = $3,
			last_ok_at = CASE WHEN $5::boolean THEN monitor_runs.last_ok_at ELSE $3::timestamptz END,
			last_error = $4, last_count = 0`, productID, m.ID, now, errMsg, runErr != nil)
	if runErr != nil {
		return runErr
	}
	return err
}

// fileMentions adds each result of a reception monitor to the reception log,
// once per URL. The results still become signals as usual.
func (svc *Service) fileMentions(ctx context.Context, productID string, m *product.Monitor, sigs []Signal) error {
	for _, s := range sigs {
		if s.EvidenceURL == "" {
			continue
		}
		source, _ := s.Data["source"].(string)
		if source == "" {
			source = "github"
			if strings.HasPrefix(s.Type, "community") {
				source = "community"
			}
		}
		text, _ := s.Data["excerpt"].(string)
		title := s.Title
		if title == "" {
			title = text
		}
		if _, _, err := reception.Add(ctx, svc.Pool, reception.Mention{Product: productID, URL: s.EvidenceURL,
			MentionedAt: s.OccurredAt, Source: source, Title: title,
			AsksQuestion: reception.LooksLikeQuestion(s.Title, text), FoundBy: "monitor:" + m.ID}); err != nil {
			return fmt.Errorf("%s: %w", s.EvidenceURL, err)
		}
	}
	return nil
}

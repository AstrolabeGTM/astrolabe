// Package play turns triggers into actions: a signal, a stage entry or a
// stall (of a person, or of an account in the pipeline) starts a sequence
// or a task for the people a play's filter allows.
// Each trigger fires a play at most once (play_runs), and plays started by
// a trigger are rechecked before every step.
package play

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/score"
)

type Engine struct {
	Pool *pgxpool.Pool
	Out  *outreach.Service
	// OnTask runs for a new task whose play asks for an AI draft.
	OnTask func(ctx context.Context, tx pgx.Tx, taskID int64) error
	Now    func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Run is one trigger for one play.
type Run struct {
	Play     *product.Play
	PersonID *int64
	SignalID *int64
	// AccountID: the pipeline account an account trigger fired for; its
	// tasks are linked to the account.
	AccountID *int64
	Key       string // unique per play: the trigger identity
	Title     string // for task text
}

// Fire applies a play's filter and action once per trigger key and returns
// the outcome ("enrolled", "task", "skipped: …", or "" if already handled).
func (e *Engine) Fire(ctx context.Context, p *product.Product, r Run) (string, error) {
	var outcome string
	err := pgx.BeginFunc(ctx, e.Pool, func(tx pgx.Tx) error {
		var runID int64
		err := tx.QueryRow(ctx, `
			INSERT INTO play_runs (product_id, play_id, person_id, signal_id, trigger_key, outcome, account_id)
			VALUES ($1, $2, $3, $4, $5, 'pending', $6) ON CONFLICT DO NOTHING RETURNING id`,
			p.ID, r.Play.ID, r.PersonID, r.SignalID, r.Key, r.AccountID).Scan(&runID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Already handled. A skip for a score threshold may be re-evaluated
			// (enrichment can raise fit later); anything else stays final.
			var outcome, detail string
			if err := tx.QueryRow(ctx, `
				SELECT id, outcome, detail FROM play_runs WHERE product_id = $1 AND play_id = $2 AND trigger_key = $3 FOR UPDATE`,
				p.ID, r.Play.ID, r.Key).Scan(&runID, &outcome, &detail); err != nil {
				return err
			}
			if !Retryable(outcome, detail) {
				return nil
			}
		} else if err != nil {
			return err
		}
		outcome, err = e.act(ctx, tx, p, r, runID)
		if err != nil {
			return err
		}
		kind, detail, _ := strings.Cut(outcome, ": ")
		_, err = tx.Exec(ctx, `UPDATE play_runs SET outcome = $2, detail = $3 WHERE id = $1`, runID, kind, detail)
		return err
	})
	return outcome, err
}

func (e *Engine) act(ctx context.Context, tx pgx.Tx, p *product.Product, r Run, runID int64) (string, error) {
	pl := r.Play
	if pl.Paused {
		return "skipped: play paused", nil
	}
	if pl.LimitPerDay > 0 {
		var today int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM play_runs WHERE product_id = $1 AND play_id = $2 AND outcome IN ('enrolled', 'task')
			AND created_at > $3::timestamptz - interval '24 hours'`, p.ID, pl.ID, e.now()).Scan(&today); err != nil {
			return "", err
		}
		if today >= pl.LimitPerDay {
			return fmt.Sprintf("skipped: daily limit %d reached", pl.LimitPerDay), nil
		}
	}
	if r.PersonID != nil {
		if reason, err := e.qualifies(ctx, tx, p, pl, *r.PersonID, true); err != nil || reason != "" {
			return "skipped: " + reason, err
		}
	}

	if pl.Action.Task != "" {
		next := pl.Action.Task
		if r.Title != "" {
			next += ": " + r.Title
		}
		id, err := e.Out.CreateTaskTx(ctx, tx, outreach.TaskOpts{Product: p.ID, PersonID: r.PersonID, NextAction: next,
			Due: e.now(), PlayID: pl.ID, SignalID: r.SignalID})
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE play_runs SET task_id = $2 WHERE id = $1`, runID, id); err != nil {
			return "", err
		}
		if r.AccountID != nil {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET account_id = $2 WHERE id = $1`, id, *r.AccountID); err != nil {
				return "", err
			}
		}
		if pl.Action.Draft && e.OnTask != nil {
			if err := e.OnTask(ctx, tx, id); err != nil {
				return "", err
			}
		}
		return "task", nil
	}

	if r.PersonID == nil {
		return "skipped: no identifiable person", nil
	}
	delay, _ := product.AfterDuration(pl.Delay)
	eid, skip, err := e.Out.EnrollTx(ctx, tx, outreach.EnrollOpts{Product: p.ID, Sequence: pl.Action.Sequence, PersonID: *r.PersonID,
		PlayID: pl.ID, SignalID: r.SignalID, Delay: delay, GoalStage: p.Goal(pl).Event, Source: "automation:" + pl.ID})
	if err != nil || skip != "" {
		return "skipped: " + skip, err
	}
	_, err = tx.Exec(ctx, `UPDATE play_runs SET enrollment_id = $2 WHERE id = $1`, runID, eid)
	return "enrolled", err
}

// Retryable reports play-run skips that later data (enrichment, new
// signals) can change: score thresholds.
func Retryable(outcome, detail string) bool {
	if outcome != "skipped" {
		return false
	}
	for _, p := range []string{"fit ", "priority ", "tier "} {
		if strings.HasPrefix(detail, p) {
			return true
		}
	}
	return false
}

// qualifies returns why a person does not qualify for a play, or "".
// atStart applies the entry-only filters (fit, tier, recent contact).
func (e *Engine) qualifies(ctx context.Context, tx pgx.Tx, p *product.Product, pl *product.Play, personID int64, atStart bool) (string, error) {
	sc, err := score.Recompute(ctx, tx, p.ID, personID, e.now())
	if err != nil {
		return "", err
	}
	w := pl.When
	// Exclude rules are about acquisition fit; plays for existing users
	// (stage triggers) ignore them unless they are suppression.
	includeExcluded := w.IncludeExcluded || pl.Trigger.Signal == ""
	if !sc.Eligible && (!includeExcluded || strings.HasPrefix(sc.Breakdown.Excluded, "suppressed")) {
		return "not eligible (" + sc.Breakdown.Excluded + ")", nil
	}
	goal := p.Goal(pl).Event
	var reached []string
	rows, err := tx.Query(ctx, `SELECT DISTINCT stage FROM funnel_events WHERE product_id = $1 AND person_id = $2`, p.ID, personID)
	if err != nil {
		return "", err
	}
	reached, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", err
	}
	for _, st := range reached {
		if st == goal && pl.Trigger.Stage != goal {
			return "already reached " + goal, nil
		}
		if w.NotStage != "" && st == w.NotStage {
			return "already " + st, nil
		}
	}
	var openTasks int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE product_id = $1 AND person_id = $2 AND status = 'open'`, p.ID, personID).Scan(&openTasks); err != nil {
		return "", err
	}
	if openTasks > 0 {
		return "you have an open to-do with them", nil
	}
	if !atStart {
		return "", nil
	}
	switch {
	case w.MinFit > 0 && sc.Fit < w.MinFit:
		return fmt.Sprintf("fit %d < %d", sc.Fit, w.MinFit), nil
	case w.MinPriority > 0 && sc.Priority < w.MinPriority:
		return fmt.Sprintf("priority %d < %d", sc.Priority, w.MinPriority), nil
	case w.MinTier != "" && sc.Tier > strings.ToUpper(w.MinTier):
		return fmt.Sprintf("tier %s below %s", sc.Tier, strings.ToUpper(w.MinTier)), nil
	}
	if w.NotContactedWithin != "" {
		d, _ := product.AfterDuration(w.NotContactedWithin)
		var n int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM actions WHERE product_id = $1 AND person_id = $2 AND status IN ('sent', 'sending', 'unknown')
			AND COALESCE(sent_at, claimed_at) > $3`, p.ID, personID, e.now().Add(-d)).Scan(&n); err != nil {
			return "", err
		}
		if n > 0 {
			return "contacted within " + w.NotContactedWithin, nil
		}
	}
	return "", nil
}

// Recheck is the outreach hook run before each step of a play's
// enrollment: it stops the sequence if the goal was reached, the person
// became ineligible, a stall ended, or I am already talking to them.
func (e *Engine) Recheck(ctx context.Context, tx pgx.Tx, enrollmentID int64) (string, error) {
	var productID, playID string
	var personID int64
	var started time.Time
	if err := tx.QueryRow(ctx, `SELECT product_id, play_id, person_id, started_at FROM enrollments WHERE id = $1`, enrollmentID).
		Scan(&productID, &playID, &personID, &started); err != nil {
		return "", err
	}
	if playID == "" {
		return "", nil
	}
	p, err := product.Get(ctx, tx, productID)
	if err != nil {
		return "", err
	}
	pl := p.Play(playID)
	if pl == nil {
		return "", nil // play removed from the file: let the sequence finish
	}
	if st := pl.Trigger.AccountStuck; st != nil {
		// Still in the same stay in that stage, or the account moved on.
		var still bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM play_runs r JOIN accounts a ON a.id = r.account_id
				WHERE r.enrollment_id = $1 AND a.status = 'open' AND a.stage = $2 AND a.entered_at <= $3)`,
			enrollmentID, st.Stage, started).Scan(&still); err != nil {
			return "", err
		}
		if !still {
			return "account moved past " + st.Stage, nil
		}
	}
	if st := pl.Trigger.StageStuck; st != nil {
		var later int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM funnel_events WHERE product_id = $1 AND person_id = $2 AND stage = ANY($3) AND occurred_at >= $4`,
			productID, personID, laterStages(p.Funnel, st.Stage), started.Add(-time.Minute)).Scan(&later); err != nil {
			return "", err
		}
		if later > 0 {
			return "moved past " + st.Stage, nil
		}
	}
	reason, err := e.qualifies(ctx, tx, p, pl, personID, false)
	if reason != "" {
		reason = "no longer qualifies: " + reason
	}
	return reason, err
}

func laterStages(funnel []string, stage string) []string {
	for i, s := range funnel {
		if s == stage {
			return funnel[i+1:]
		}
	}
	return nil
}

// OnSignal fires every play triggered by a signal's type.
func (e *Engine) OnSignal(ctx context.Context, signalID int64) error {
	var productID, typ, title, url string
	var personID *int64
	if err := e.Pool.QueryRow(ctx, `SELECT product_id, type, person_id, title, evidence_url FROM signals WHERE id = $1`, signalID).
		Scan(&productID, &typ, &personID, &title, &url); err != nil {
		return err
	}
	p, err := product.Get(ctx, e.Pool, productID)
	if err != nil {
		return err
	}
	for _, pl := range p.Plays {
		if pl.Trigger.Signal != typ {
			continue
		}
		t := title
		if url != "" {
			t += " (" + url + ")"
		}
		if _, err := e.Fire(ctx, p, Run{Play: pl, PersonID: personID, SignalID: &signalID, Key: fmt.Sprintf("signal:%d", signalID), Title: t}); err != nil {
			return fmt.Errorf("play %s: %w", pl.ID, err)
		}
	}
	return nil
}

// Scan fires stage and stage_stuck plays for everyone who matches now,
// and account_stage and account_stuck plays for pipeline accounts.
func (e *Engine) Scan(ctx context.Context, p *product.Product) error {
	for _, pl := range p.Plays {
		var q string
		var args []any
		switch {
		case pl.Trigger.AccountStage != "" || pl.Trigger.AccountStuck != nil:
			if err := e.scanAccounts(ctx, p, pl); err != nil {
				return fmt.Errorf("play %s: %w", pl.ID, err)
			}
			continue
		case pl.Trigger.StageStuck != nil:
			d, _ := product.AfterDuration(pl.Trigger.StageStuck.For)
			// Furthest stage is the stuck stage, entered at least d ago, and no
			// activity (stage events or product.* signals) for d either.
			q = `
				WITH fe AS (
					SELECT person_id, stage, min(occurred_at) AS at FROM funnel_events WHERE product_id = $1 GROUP BY 1, 2),
				furthest AS (
					SELECT person_id, max(array_position($2::text[], stage)) AS idx FROM fe GROUP BY 1),
				activity AS (
					SELECT person_id, max(t) AS last FROM (
						SELECT person_id, occurred_at AS t FROM funnel_events WHERE product_id = $1
						UNION ALL SELECT person_id, occurred_at FROM signals WHERE product_id = $1 AND person_id IS NOT NULL AND type LIKE 'product.%'
					) x GROUP BY 1)
				SELECT f.person_id, 'stuck:' || $3 || ':' || f.person_id || ':' || to_char(a.last, 'YYYY-MM-DD"T"HH24:MI:SS')
				FROM furthest f
				JOIN fe ON fe.person_id = f.person_id AND fe.stage = $3
				JOIN activity a ON a.person_id = f.person_id
				WHERE f.idx = array_position($2::text[], $3) AND fe.at <= $4 AND a.last <= $4`
			args = []any{p.ID, p.Funnel, pl.Trigger.StageStuck.Stage, e.now().Add(-d)}
		case pl.Trigger.Stage != "":
			q = `SELECT person_id, 'stage:' || $2 || ':' || person_id FROM funnel_events
				WHERE product_id = $1 AND stage = $2 AND occurred_at > $3 GROUP BY person_id`
			args = []any{p.ID, pl.Trigger.Stage, e.now().Add(-7 * 24 * time.Hour)}
		default:
			continue
		}
		rows, err := e.Pool.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		type hit struct {
			person int64
			key    string
		}
		hits, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (hit, error) {
			var h hit
			return h, r.Scan(&h.person, &h.key)
		})
		if err != nil {
			return err
		}
		for _, h := range hits {
			person := h.person
			if _, err := e.Fire(ctx, p, Run{Play: pl, PersonID: &person, Key: h.key}); err != nil {
				return fmt.Errorf("play %s: %w", pl.ID, err)
			}
		}
	}
	return nil
}

// scanAccounts fires an account_stage or account_stuck play. The trigger
// key is the account's current stay in the stage (its latest stage-history
// row): a play fires once per stay, stays quiet once the account moves on,
// and can fire again if the account later re-enters the stage. A task is
// about the account; a sequence starts for its champion (or first person).
func (e *Engine) scanAccounts(ctx context.Context, p *product.Product, pl *product.Play) error {
	kind, stage, cond := "account_stage", pl.Trigger.AccountStage, `a.entered_at > $3`
	since := e.now().Add(-7 * 24 * time.Hour) // like stage plays: recent entries only
	if st := pl.Trigger.AccountStuck; st != nil {
		d, _ := product.AfterDuration(st.For)
		kind, stage, cond, since = "account_stuck", st.Stage, `a.entered_at <= $3`, e.now().Add(-d)
	}
	rows, err := e.Pool.Query(ctx, `
		SELECT a.id, a.entered_at, COALESCE(NULLIF(c.name, ''), c.domain, '#' || c.id),
			COALESCE((SELECT max(h.id) FROM account_stages h WHERE h.account_id = a.id), 0),
			(SELECT ap.person_id FROM account_people ap WHERE ap.account_id = a.id
				ORDER BY ap.role = 'champion' DESC, ap.added_at, ap.person_id LIMIT 1)
		FROM accounts a JOIN companies c ON c.id = a.company_id
		WHERE a.product_id = $1 AND a.status = 'open' AND a.stage = $2 AND `+cond+`
		ORDER BY a.id`, p.ID, stage, since)
	if err != nil {
		return err
	}
	type hit struct {
		account, stay int64
		entered       time.Time
		name          string
		person        *int64
	}
	hits, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (hit, error) {
		var h hit
		return h, r.Scan(&h.account, &h.entered, &h.name, &h.stay, &h.person)
	})
	if err != nil {
		return err
	}
	for _, h := range hits {
		account := h.account
		r := Run{Play: pl, AccountID: &account, Key: fmt.Sprintf("%s:%s:%d:%d", kind, stage, h.account, h.stay),
			Title: h.name + " entered " + stage}
		if kind == "account_stuck" {
			now := e.now()
			y1, m1, d1 := h.entered.In(now.Location()).Date()
			y2, m2, d2 := now.Date()
			days := int(time.Date(y2, m2, d2, 0, 0, 0, 0, time.UTC).Sub(time.Date(y1, m1, d1, 0, 0, 0, 0, time.UTC)).Hours() / 24)
			r.Title = fmt.Sprintf("%s in %s for %d days", h.name, stage, days)
		}
		if pl.Action.Sequence != "" {
			r.PersonID = h.person
		}
		if _, err := e.Fire(ctx, p, r); err != nil {
			return err
		}
	}
	return nil
}

// ---- jobs ----

var pendingOnly = []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending,
	rivertype.JobStateRetryable, rivertype.JobStateRunning, rivertype.JobStateScheduled}

type SignalArgs struct {
	SignalID int64 `json:"signal_id"`
}

func (SignalArgs) Kind() string { return "play_signal" }

func (SignalArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: pendingOnly}}
}

type SignalWorker struct {
	river.WorkerDefaults[SignalArgs]
	E *Engine
}

func (w *SignalWorker) Work(ctx context.Context, job *river.Job[SignalArgs]) error {
	// Fit comes from enrichment: wait (up to 30 minutes) for a new person's
	// GitHub enrichment so the play doesn't judge them on missing facts.
	var waiting bool
	err := w.E.Pool.QueryRow(ctx, `
		SELECT p.enriched_at IS NULL AND s.created_at > now() - interval '30 minutes'
			AND EXISTS (SELECT 1 FROM identities i WHERE i.person_id = p.id AND i.kind = 'github')
		FROM signals s JOIN people p ON p.id = s.person_id WHERE s.id = $1`, job.Args.SignalID).Scan(&waiting)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if waiting {
		return river.JobSnooze(time.Minute)
	}
	return w.E.OnSignal(ctx, job.Args.SignalID)
}

type ScanArgs struct{}

func (ScanArgs) Kind() string { return "play_scan" }

type ScanWorker struct {
	river.WorkerDefaults[ScanArgs]
	E *Engine
}

func (w *ScanWorker) Work(ctx context.Context, _ *river.Job[ScanArgs]) error {
	rows, err := w.E.Pool.Query(ctx, `SELECT id FROM products`)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		p, err := product.Get(ctx, w.E.Pool, id)
		if err != nil {
			return err
		}
		if err := w.E.Scan(ctx, p); err != nil {
			slog.Error("play scan", "product", id, "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

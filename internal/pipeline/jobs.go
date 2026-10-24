package pipeline

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// Revisit makes one task for each paused or lost account whose revisit
// date has come (once per pause or loss), in every product. It returns how
// many tasks it made.
func Revisit(ctx context.Context, db DB, now time.Time) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, accountSelect+`
			WHERE a.status <> 'open' AND a.revisit_task_id IS NULL AND a.revisit_on <= $1::date
			ORDER BY a.id FOR UPDATE OF a SKIP LOCKED`, day(now))
		if err != nil {
			return err
		}
		due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Account, error) { return scanAccount(r) })
		if err != nil {
			return err
		}
		for _, a := range due {
			text := fmt.Sprintf("Revisit %s (%s at %s, reason %s): reopen it by moving it to a stage, or set a new revisit date",
				a.Company, a.Status, a.Stage, a.Reason)
			var task int64
			if err := tx.QueryRow(ctx, `INSERT INTO tasks (product_id, account_id, next_action, due_at) VALUES ($1, $2, $3, $4) RETURNING id`,
				a.Product, a.ID, text, now).Scan(&task); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE accounts SET revisit_task_id = $2 WHERE id = $1`, a.ID, task); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// WeekStart is Monday 00:00 of t's week in t's location.
func WeekStart(t time.Time) time.Time {
	d := (int(t.Weekday()) + 6) % 7
	y, m, dd := t.AddDate(0, 0, -d).Date()
	return time.Date(y, m, dd, 0, 0, 0, 0, t.Location())
}

// Snapshot saves the product's pipeline for the week (one row per account)
// the first time it runs that week, and reports whether it saved one.
func Snapshot(ctx context.Context, db DB, p *product.Product, now time.Time) (bool, error) {
	if p.Pipeline == nil {
		return false, nil
	}
	week := day(WeekStart(now))
	var done bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pipeline_snapshots WHERE week = $1::date AND product_id = $2)`, week, p.ID).Scan(&done); err != nil || done {
		return false, err
	}
	list, err := List(ctx, db, p, now)
	if err != nil || len(list) == 0 {
		return false, err
	}
	checks := map[int64]map[string]string{}
	rows, err := db.Query(ctx, `
		SELECT ac.account_id, ac.name, ac.answer FROM account_checks ac JOIN accounts a ON a.id = ac.account_id WHERE a.product_id = $1`, p.ID)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id int64
		var name, answer string
		if err := rows.Scan(&id, &name, &answer); err != nil {
			rows.Close()
			return false, err
		}
		if checks[id] == nil {
			checks[id] = map[string]string{}
		}
		checks[id][name] = answer
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	saved := false
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		for _, a := range list {
			c, _ := json.Marshal(checks[a.ID])
			if checks[a.ID] == nil {
				c = []byte("{}")
			}
			tag, err := tx.Exec(ctx, `
				INSERT INTO pipeline_snapshots (week, product_id, account_id, company, domain, status, stage, days_in_stage, stuck,
					next_action, next_due, overdue, reason, revisit_on, checks, taken_at)
				VALUES ($1::date, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, '')::date, $12, $13, NULLIF($14, '')::date, $15, $16)
				ON CONFLICT DO NOTHING`,
				week, p.ID, a.ID, a.Company, a.Domain, a.Status, a.Stage, a.DaysInStage, a.Stuck,
				a.NextAction, a.NextDue, a.Overdue, a.Reason, a.RevisitOn, c, now)
			if err != nil {
				return err
			}
			saved = saved || tag.RowsAffected() > 0
		}
		return nil
	})
	return saved, err
}

// WriteCSV writes every weekly snapshot of a product's pipeline as CSV,
// one row per account per week, with a column per qualification check.
func WriteCSV(ctx context.Context, q store.Q, p *product.Product, w io.Writer) error {
	if err := needPipeline(p); err != nil {
		return err
	}
	var checks []string
	if qf := p.Pipeline.Qualify; qf != nil {
		checks = append(append(checks, qf.Checks...), qf.Record...)
	}
	cw := csv.NewWriter(w)
	header := []string{"week", "account_id", "company", "domain", "status", "stage", "days_in_stage", "stuck",
		"next_action", "next_due", "overdue", "reason", "revisit_on"}
	for _, c := range checks {
		header = append(header, "check_"+c)
	}
	if err := cw.Write(header); err != nil {
		return err
	}
	rows, err := q.Query(ctx, `
		SELECT to_char(week, 'YYYY-MM-DD'), account_id, company, domain, status, stage, days_in_stage, stuck,
			next_action, COALESCE(to_char(next_due, 'YYYY-MM-DD'), ''), overdue, reason, COALESCE(to_char(revisit_on, 'YYYY-MM-DD'), ''), checks
		FROM pipeline_snapshots WHERE product_id = $1
		ORDER BY week, status <> 'open', array_position($2::text[], stage), company, account_id`, p.ID, p.Pipeline.Stages)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var week, company, domain, status, stage, next, due, reason, revisit string
		var id int64
		var days int
		var stuck, overdue bool
		var answers map[string]string
		if err := rows.Scan(&week, &id, &company, &domain, &status, &stage, &days, &stuck, &next, &due, &overdue, &reason, &revisit, &answers); err != nil {
			return err
		}
		rec := []string{week, strconv.FormatInt(id, 10), company, domain, status, stage, strconv.Itoa(days), strconv.FormatBool(stuck),
			next, due, strconv.FormatBool(overdue), reason, revisit}
		for _, c := range checks {
			a := answers[c]
			if a == "" {
				a = "unknown"
			}
			rec = append(rec, a)
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

// ---- job ----

type Args struct{}

func (Args) Kind() string { return "pipeline" }

// Worker runs hourly: revisit tasks that are due, and the weekly snapshot.
type Worker struct {
	river.WorkerDefaults[Args]
	Pool *pgxpool.Pool
}

func (w *Worker) Work(ctx context.Context, _ *river.Job[Args]) error {
	now := time.Now()
	if n, err := Revisit(ctx, w.Pool, now); err != nil {
		return err
	} else if n > 0 {
		slog.Info("pipeline revisit tasks", "count", n)
	}
	products, err := product.All(ctx, w.Pool)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range products {
		if _, err := Snapshot(ctx, w.Pool, p, now); err != nil {
			errs = append(errs, fmt.Errorf("%s snapshot: %w", p.ID, err))
		}
	}
	return errors.Join(errs...)
}

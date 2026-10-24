package forget

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// Forget erases the person with this email address or id. See the package
// comment. A dry run changes nothing. A real run first deletes Netlify
// copies (if a client is given), then erases locally in one transaction; if
// Netlify cannot be reached the local erase still happens and the copies left
// are in the report with their ids.
func Forget(ctx context.Context, pool *pgxpool.Pool, who string, opt Options) (*Report, error) {
	w, err := parse(who)
	if err != nil {
		return nil, err
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	rep := &Report{DryRun: opt.DryRun, Deleted: map[string]int{}, Scrubbed: map[string]int{}, NotCovered: notCovered}

	// Netlify first, from a read of what is there now. A copy deleted here
	// whose local row then fails to delete is harmless: a retry deletes the row.
	t, err := gather(ctx, pool, w)
	if err != nil {
		return nil, err
	}
	remotes, err := netlifyCopies(ctx, pool, t)
	if err != nil {
		return nil, err
	}
	switch {
	case opt.DryRun:
		rep.RemotePlanned = remotes
	case opt.Netlify == nil || opt.Netlify.Token == "":
		for _, r := range remotes {
			r.Error = "ASTROLABE_NETLIFY_TOKEN is not set"
			rep.RemoteLeft = append(rep.RemoteLeft, r)
		}
	default:
		for _, r := range remotes {
			if err := opt.Netlify.DeleteSubmission(ctx, r.ID); err != nil {
				r.Error = err.Error()
				rep.RemoteLeft = append(rep.RemoteLeft, r)
			} else {
				rep.RemoteDeleted = append(rep.RemoteDeleted, r)
			}
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := erase(ctx, tx, w, rep, opt.Now()); err != nil {
		return nil, err
	}
	if opt.DryRun {
		return rep, nil // rolled back by the deferred call
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return rep, nil
}

// gather reads everything that identifies the person.
func gather(ctx context.Context, c store.Q, w who) (*target, error) {
	t := &target{}
	if w.email != "" {
		t.addIdent("email", w.email)
		err := c.QueryRow(ctx, `SELECT person_id FROM identities WHERE kind = 'email' AND value = $1`, w.email).Scan(&t.personID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	} else {
		t.personID = w.personID
		var one int
		if err := c.QueryRow(ctx, `SELECT 1 FROM people WHERE id = $1`, w.personID).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("no person %d", w.personID)
		} else if err != nil {
			return nil, err
		}
	}
	if t.personID != 0 {
		rows, err := c.Query(ctx, `SELECT kind, value FROM identities WHERE person_id = $1`, t.personID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				return nil, err
			}
			t.addIdent(k, v)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		var name, first string
		if err := c.QueryRow(ctx, `SELECT name, first_name FROM people WHERE id = $1`, t.personID).Scan(&name, &first); err != nil {
			return nil, err
		}
		_ = first // a first name alone is too common to search for
		if n := strings.ToLower(strings.TrimSpace(name)); len(n) >= 4 {
			t.terms = append(t.terms, n)
		}
	}
	// Addresses that are not identities but are still theirs: where we wrote
	// to, where they wrote from, list and payment addresses.
	var extra []string
	err := c.QueryRow(ctx, `
		SELECT COALESCE(array_agg(DISTINCT e), '{}') FROM (
			SELECT lower(recipient) e FROM actions WHERE person_id = $1 AND channel = 'email'
			UNION SELECT lower(from_addr) FROM replies WHERE person_id = $1 AND NOT is_bounce
			UNION SELECT lower(email) FROM updates_subscribers WHERE person_id = $1
			UNION SELECT lower(email) FROM payments WHERE person_id = $1
		) x WHERE e <> ''`, t.personID).Scan(&extra)
	if err != nil {
		return nil, err
	}
	for _, e := range extra {
		t.addIdent("email", e)
	}
	for _, i := range t.idents {
		// A short GitHub login would match unrelated words in free text.
		if i[0] == "github" && len(i[1]) < 4 {
			continue
		}
		if i[0] == "email" || i[0] == "github" || i[0] == "phone" {
			t.terms = append(t.terms, i[1])
		}
	}
	return t, nil
}

// submissionMatch is the SQL condition for "this form submission is theirs".
const submissionMatch = `(person_id = $1 OR signal_id = ANY($2)
	OR EXISTS (SELECT 1 FROM unnest($3::text[]) x WHERE strpos(lower(fields::text), x) > 0))`

func netlifyCopies(ctx context.Context, c store.Q, t *target) ([]Remote, error) {
	rows, err := c.Query(ctx, `
		SELECT external_id, monitor_id FROM form_submissions
		WHERE source = 'netlify' AND (person_id = $1
			OR EXISTS (SELECT 1 FROM unnest($2::text[]) x WHERE strpos(lower(fields::text), x) > 0))
		ORDER BY id`, t.personID, t.emails)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Remote
	for rows.Next() {
		r := Remote{Source: "netlify"}
		if err := rows.Scan(&r.ID, &r.Form); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type eraser struct {
	ctx context.Context
	tx  pgx.Tx
	rep *Report
	t   *target
	err error
}

// del runs a DELETE and counts it under table.
func (e *eraser) del(table, sql string, args ...any) {
	if e.err != nil {
		return
	}
	tag, err := e.tx.Exec(e.ctx, sql, args...)
	if err != nil {
		e.err = fmt.Errorf("forget %s: %w", table, err)
		return
	}
	if n := int(tag.RowsAffected()); n > 0 {
		e.rep.Deleted[table] += n
	}
}

func (e *eraser) exec(what, sql string, args ...any) int {
	if e.err != nil {
		return 0
	}
	tag, err := e.tx.Exec(e.ctx, sql, args...)
	if err != nil {
		e.err = fmt.Errorf("forget %s: %w", what, err)
		return 0
	}
	return int(tag.RowsAffected())
}

// ids collects a set of ids.
func (e *eraser) ids(sql string, args ...any) []int64 {
	if e.err != nil {
		return nil
	}
	rows, err := e.tx.Query(e.ctx, sql, args...)
	if err != nil {
		e.err = err
		return nil
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		e.err = err
	}
	if out == nil {
		out = []int64{}
	}
	return out
}

func erase(ctx context.Context, tx pgx.Tx, w who, rep *Report, now time.Time) error {
	// Read again inside the transaction: this is the set that is erased.
	t, err := gather(ctx, tx, w)
	if err != nil {
		return err
	}
	if t.personID != 0 {
		id := t.personID
		rep.PersonID = &id
	}
	e := &eraser{ctx: ctx, tx: tx, rep: rep, t: t}
	pid, emails, terms := t.personID, t.emails, t.terms
	if emails == nil {
		emails = []string{}
	}
	if terms == nil {
		terms = []string{}
	}
	phones := t.phones
	if phones == nil {
		phones = []string{}
	}

	// ---- id sets, read before anything is deleted ----
	actions := e.ids(`SELECT id FROM actions WHERE person_id = $1`, pid)
	enrolls := e.ids(`SELECT id FROM enrollments WHERE person_id = $1`, pid)
	subs := e.ids(`SELECT id FROM updates_subscribers WHERE person_id = $1 OR lower(email) = ANY($2)`, pid, emails)
	signals := e.ids(`
		SELECT id FROM signals WHERE person_id = $1
			OR lower(subject->>'email') = ANY($2) OR lower(subject->>'github') = ANY($3)
			OR id IN (SELECT signal_id FROM identity_reviews WHERE $1 = ANY(candidates) OR resolved_to = $1)`,
		pid, emails, githubs(t))
	replies := e.ids(`SELECT id FROM replies WHERE person_id = $1 OR lower(from_addr) = ANY($2)
		OR action_id = ANY($3) OR enrollment_id = ANY($4)`, pid, emails, actions, enrolls)
	tasks := e.ids(`SELECT id FROM tasks WHERE person_id = $1 OR reply_id = ANY($2) OR enrollment_id = ANY($3) OR signal_id = ANY($4)`,
		pid, replies, enrolls, signals)
	var codes []string
	if e.err == nil {
		rows, err := tx.Query(ctx, `SELECT code FROM links WHERE person_id = $1 OR action_id = ANY($2)`, pid, actions)
		if err != nil {
			return err
		}
		codes, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if codes == nil {
			codes = []string{}
		}
	}
	llmIDs := e.ids(`SELECT llm_call_id FROM briefs WHERE person_id = $1 AND llm_call_id IS NOT NULL`, pid)
	if e.err != nil {
		return e.err
	}

	// ---- themes that may quote their conversations (reported, not touched) ----
	if err := reviewThemes(ctx, tx, pid, rep); err != nil {
		return err
	}

	// ---- rows that must stay for a rolling cap: keep an anonymous count ----
	if n := e.exec("erased_sends", `
		INSERT INTO erased_sends (product_id, counter, counted_at)
		SELECT a.product_id, 'cold', a.claimed_at
		FROM actions a
		JOIN enrollments en ON en.id = a.enrollment_id
		JOIN sequences s ON s.product_id = en.product_id AND s.id = en.sequence_id
		WHERE a.id = ANY($1) AND a.status IN ('sending', 'sent', 'unknown') AND a.kind = 'step'
			AND COALESCE(s.config->>'kind', 'cold') = 'cold' AND a.claimed_at >= $2::timestamptz - interval '24 hours'
			AND NOT (a.channel = 'task' OR a.channel LIKE '%\_task')`, actions, now); n > 0 {
		rep.Scrubbed["erased_sends"] += n
	}
	if n := e.exec("erased_sends", `
		INSERT INTO erased_sends (product_id, counter, counted_at)
		SELECT product_id, 'broadcast', claimed_at FROM updates_messages
		WHERE subscriber_id = ANY($1) AND kind = 'broadcast' AND status IN ('sending', 'sent', 'unknown')
			AND claimed_at > $2::timestamptz - interval '24 hours'`, subs, now); n > 0 {
		rep.Scrubbed["erased_sends"] += n
	}
	e.exec("erased_sends", `DELETE FROM erased_sends WHERE counted_at < $1::timestamptz - interval '2 days'`, now)
	// Automation runs inside a play's daily limit stay, with nothing personal.
	if n := e.exec("play_runs", `
		UPDATE play_runs SET person_id = NULL, signal_id = NULL, enrollment_id = NULL, task_id = NULL, account_id = NULL,
			trigger_key = 'erased:' || id, detail = ''
		WHERE (person_id = $1 OR signal_id = ANY($2) OR enrollment_id = ANY($3) OR task_id = ANY($4))
			AND outcome IN ('enrolled', 'task', 'subscribed') AND created_at > $5::timestamptz - interval '24 hours'`,
		pid, signals, enrolls, tasks, now); n > 0 {
		rep.Scrubbed["play_runs"] += n
	}
	e.del("play_runs", `DELETE FROM play_runs WHERE person_id = $1 OR signal_id = ANY($2) OR enrollment_id = ANY($3) OR task_id = ANY($4)`,
		pid, signals, enrolls, tasks)

	// ---- deletes, children before parents ----
	e.del("river_job", `DELETE FROM river_job WHERE
		(kind = 'score' AND (args->>'person_id')::bigint = $1)
		OR (kind = 'draft_action' AND (args->>'action_id')::bigint = ANY($2))
		OR (kind = 'classify_reply' AND (args->>'reply_id')::bigint = ANY($3))
		OR (kind = 'draft_task' AND (args->>'task_id')::bigint = ANY($4))`, pid, actions, replies, tasks)
	e.exec("accounts", `UPDATE accounts SET revisit_task_id = NULL WHERE revisit_task_id = ANY($1)`, tasks)
	e.exec("content_items", `UPDATE content_items SET task_id = NULL WHERE task_id = ANY($1)`, tasks)
	e.del("tasks", `DELETE FROM tasks WHERE id = ANY($1)`, tasks)

	e.del("clicks", `DELETE FROM clicks WHERE code = ANY($1)`, codes)
	e.del("sandbox_outbox", `DELETE FROM sandbox_outbox WHERE action_id = ANY($1) OR lower(recipient) = ANY($2) OR recipient = ANY($3)`, actions, emails, phones)
	e.del("voice_examples", `DELETE FROM voice_examples WHERE action_id = ANY($1)`, actions)
	e.del("updates_messages", `DELETE FROM updates_messages WHERE subscriber_id = ANY($1)`, subs)
	e.del("updates_subscribers", `DELETE FROM updates_subscribers WHERE id = ANY($1)`, subs)

	e.exec("actions", `UPDATE actions SET reply_id = NULL WHERE reply_id = ANY($1)`, replies)
	e.del("replies", `DELETE FROM replies WHERE id = ANY($1)`, replies)
	e.exec("content_items", `UPDATE content_items SET link_code = NULL WHERE link_code = ANY($1)`, codes)
	e.del("actions", `DELETE FROM actions WHERE id = ANY($1)`, actions)
	e.del("links", `DELETE FROM links WHERE code = ANY($1)`, codes)
	e.exec("enrollments", `UPDATE enrollments SET signal_id = NULL WHERE signal_id = ANY($1) AND person_id <> $2`, signals, pid)
	e.del("enrollments", `DELETE FROM enrollments WHERE id = ANY($1)`, enrolls)

	e.del("identity_reviews", `DELETE FROM identity_reviews WHERE $1 = ANY(candidates) OR resolved_to = $1 OR signal_id = ANY($2)`, pid, signals)
	e.del("form_submissions", `DELETE FROM form_submissions WHERE `+submissionMatch, pid, signals, emails)
	e.del("signals", `DELETE FROM signals WHERE id = ANY($1)`, signals)

	e.del("funnel_events", `DELETE FROM funnel_events WHERE person_id = $1`, pid)
	e.del("scores", `DELETE FROM scores WHERE person_id = $1`, pid)
	e.del("briefs", `DELETE FROM briefs WHERE person_id = $1`, pid)
	e.del("channel_permissions", `DELETE FROM channel_permissions WHERE person_id = $1`, pid)
	e.del("suppressions", `DELETE FROM suppressions WHERE person_id = $1`, pid)
	e.del("product_people", `DELETE FROM product_people WHERE person_id = $1`, pid)
	e.del("account_people", `DELETE FROM account_people WHERE person_id = $1`, pid)
	e.del("pricing_feedback", `DELETE FROM pricing_feedback WHERE person_id = $1`, pid)
	e.del("pmf_answers", `DELETE FROM pmf_answers WHERE person_id = $1`, pid)
	e.del("conversations", `DELETE FROM conversations WHERE person_id = $1`, pid)
	e.del("payments", `DELETE FROM payments WHERE person_id = $1 OR lower(email) = ANY($2)`, pid, emails)
	e.del("assignments", `DELETE FROM assignments WHERE unit = 'person:' || $1::text`, strconv.FormatInt(pid, 10))
	// The logged LLM prompts and outputs: the research brief's call, and any
	// call whose text names them.
	e.del("llm_calls", `DELETE FROM llm_calls WHERE id = ANY($1) OR EXISTS (
		SELECT 1 FROM unnest($2::text[]) x WHERE strpos(lower(prompt), x) > 0 OR strpos(lower(output), x) > 0)`, llmIDs, terms)
	e.del("identities", `DELETE FROM identities WHERE person_id = $1`, pid)
	e.del("people", `DELETE FROM people WHERE id = $1`, pid)

	// ---- free text elsewhere that names them ----
	for _, s := range scrubs {
		for _, col := range s.cols {
			n := e.exec(s.table, fmt.Sprintf(`UPDATE %[1]s SET %[2]s = $2 WHERE %[2]s <> $2 AND EXISTS (
				SELECT 1 FROM unnest($1::text[]) x WHERE strpos(lower(%[2]s), x) > 0)`, s.table, col), terms, Marker)
			if n > 0 {
				e.rep.Scrubbed[s.table+"."+col] += n
			}
		}
	}

	// ---- the tombstone ----
	if e.err == nil {
		for _, id := range t.idents {
			added, err := people.Tombstone(ctx, tx, id[0], id[1])
			if err != nil {
				return err
			}
			if added {
				rep.Tombstones++
			}
		}
	}
	for k, v := range rep.Scrubbed {
		if v == 0 {
			delete(rep.Scrubbed, k)
		}
	}
	return e.err
}

func githubs(t *target) []string {
	out := []string{}
	for _, i := range t.idents {
		if i[0] == "github" {
			out = append(out, i[1])
		}
	}
	return out
}

// scrubs are free-text columns of records that stay (an account, a company, a
// digest), cleared when they name the person.
var scrubs = []struct {
	table string
	cols  []string
}{
	{"accounts", []string{"next_action", "goal", "timing", "baseline", "reason"}},
	{"account_stages", []string{"evidence", "reason"}},
	{"account_checks", []string{"evidence"}},
	{"account_links", []string{"note"}},
	{"pipeline_snapshots", []string{"next_action", "reason"}},
	{"companies", []string{"notes"}},
	{"tasks", []string{"next_action", "draft", "outcome_note"}},
	{"conversations", []string{"summary", "their_words", "problem", "current_approach", "next_step"}},
	{"weekly_themes", []string{"body"}},
	{"digests", []string{"body"}},
}

// reviewThemes lists saved weekly themes written from weeks in which the
// person had a logged conversation: the themes hold quotes without names, so
// they cannot be matched, and are left for the operator to read.
func reviewThemes(ctx context.Context, tx pgx.Tx, pid int64, rep *Report) error {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT w.product_id, w.iso_week FROM weekly_themes w
		JOIN conversations c ON c.product_id = w.product_id AND to_char(c.occurred_on, 'IYYY"-W"IW') = w.iso_week
		WHERE c.person_id = $1 ORDER BY 1, 2`, pid)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p, wk string
		if err := rows.Scan(&p, &wk); err != nil {
			return err
		}
		rep.Review = append(rep.Review, fmt.Sprintf("weekly themes %s %s were written from weeks with their conversation notes and may quote them", p, wk))
	}
	return rows.Err()
}

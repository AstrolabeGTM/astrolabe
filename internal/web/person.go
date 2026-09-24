package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/score"
)

type personSignal struct {
	Product, Type, Title, URL string
	At                        time.Time
}

type personMessage struct {
	ID                                      int64
	Product, Channel, Status, Subject, Body string
	At                                      time.Time
}

type personBrief struct {
	Product, Body string
	At            time.Time
}

func (srv *Server) personPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := pathID(r)
	p, err := people.Get(ctx, srv.S.Pool, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	facts, _ := people.Facts(ctx, srv.S.Pool, id)
	var scores []score.Score
	for _, pr := range srv.Products() {
		if sc, ok, _ := score.Get(ctx, srv.S.Pool, pr.ID, id); ok {
			scores = append(scores, sc)
		}
	}
	rows, _ := srv.S.Pool.Query(ctx, `SELECT product_id, type, title, evidence_url, occurred_at FROM signals WHERE person_id = $1 ORDER BY occurred_at DESC LIMIT 50`, id)
	signals, _ := pgx.CollectRows(rows, func(row pgx.CollectableRow) (personSignal, error) {
		var s personSignal
		return s, row.Scan(&s.Product, &s.Type, &s.Title, &s.URL, &s.At)
	})
	rows, _ = srv.S.Pool.Query(ctx, `SELECT id, product_id, channel, status, subject, body, COALESCE(sent_at, created_at) FROM actions WHERE person_id = $1 ORDER BY id DESC LIMIT 50`, id)
	msgs, _ := pgx.CollectRows(rows, func(row pgx.CollectableRow) (personMessage, error) {
		var m personMessage
		return m, row.Scan(&m.ID, &m.Product, &m.Channel, &m.Status, &m.Subject, &m.Body, &m.At)
	})
	rows, _ = srv.S.Pool.Query(ctx, `SELECT DISTINCT ON (product_id) product_id, body, created_at FROM briefs WHERE person_id = $1 ORDER BY product_id, id DESC`, id)
	briefs, _ := pgx.CollectRows(rows, func(row pgx.CollectableRow) (personBrief, error) {
		var b personBrief
		return b, row.Scan(&b.Product, &b.Body, &b.At)
	})
	srv.render(w, "person", srv.page(r, "people", map[string]any{
		"Person": p, "Facts": facts, "Scores": scores, "Signals": signals, "Messages": msgs, "Briefs": briefs,
		"AI": srv.Writer != nil && srv.Writer.LLM.Enabled(),
	}))
}

func (srv *Server) personBrief(w http.ResponseWriter, r *http.Request) {
	back := fmt.Sprintf("/people/%d", pathID(r))
	if srv.Writer == nil || !srv.Writer.LLM.Enabled() {
		done(w, r, back, "", fmt.Errorf("%w: set ASTROLABE_ANTHROPIC_API_KEY to research people", outreach.ErrInvalid))
		return
	}
	_, err := srv.Writer.Brief(r.Context(), r.FormValue("product"), pathID(r), true)
	done(w, r, back, "Research notes written", err)
}

// ---- plays ----

type playRow struct {
	Product, ID, Trigger, Action string
	Paused                       bool
	Enrolled, Tasks, Skipped     int
}

type runRow struct {
	Product, Play, Outcome, Detail, Person string
	PersonID                               *int64
	At                                     time.Time
}

func (srv *Server) playsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var plays []playRow
	for _, p := range srv.Products() {
		for _, pl := range p.Plays {
			row := playRow{Product: p.ID, ID: pl.ID, Paused: pl.Paused}
			switch {
			case pl.Trigger.Signal != "":
				row.Trigger = "signal " + pl.Trigger.Signal
			case pl.Trigger.Stage != "":
				row.Trigger = "entered " + pl.Trigger.Stage
			case pl.Trigger.StageStuck != nil:
				row.Trigger = fmt.Sprintf("stuck at %s for %s", pl.Trigger.StageStuck.Stage, pl.Trigger.StageStuck.For)
			}
			if pl.Action.Sequence != "" {
				row.Action = "sequence " + pl.Action.Sequence
			} else {
				row.Action = "task: " + pl.Action.Task
			}
			srv.S.Pool.QueryRow(ctx, `
				SELECT count(*) FILTER (WHERE outcome = 'enrolled'), count(*) FILTER (WHERE outcome = 'task'), count(*) FILTER (WHERE outcome = 'skipped')
				FROM play_runs WHERE product_id = $1 AND play_id = $2`, p.ID, pl.ID).Scan(&row.Enrolled, &row.Tasks, &row.Skipped)
			plays = append(plays, row)
		}
	}
	rows, err := srv.S.Pool.Query(ctx, `
		SELECT r.product_id, r.play_id, r.outcome, r.detail, r.person_id, COALESCE(NULLIF(p.name, ''), (SELECT value FROM identities WHERE person_id = p.id LIMIT 1), ''), r.created_at
		FROM play_runs r LEFT JOIN people p ON p.id = r.person_id ORDER BY r.id DESC LIMIT 100`)
	if err != nil {
		fail(w, err)
		return
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (runRow, error) {
		var x runRow
		return x, row.Scan(&x.Product, &x.Play, &x.Outcome, &x.Detail, &x.PersonID, &x.Person, &x.At)
	})
	if err != nil {
		fail(w, err)
		return
	}
	srv.render(w, "automations", srv.page(r, "automations", map[string]any{"Plays": plays, "Runs": runs}))
}

// ---- replies and drafts ----

func (srv *Server) replyAnswer(w http.ResponseWriter, r *http.Request) {
	body := strings.TrimSpace(strings.ReplaceAll(r.FormValue("body"), "\r\n", "\n"))
	if body == "" {
		done(w, r, "", "", fmt.Errorf("%w: write the answer first", outreach.ErrInvalid))
		return
	}
	id, err := srv.S.AnswerReply(r.Context(), pathID(r), body)
	done(w, r, "", fmt.Sprintf("Answer #%d is in drafts; approve it to send", id), err)
}

// actionAIDraft asks Claude to (re)write a draft now.
func (srv *Server) actionAIDraft(w http.ResponseWriter, r *http.Request) {
	if srv.Writer == nil || !srv.Writer.LLM.Enabled() {
		done(w, r, "", "", fmt.Errorf("%w: set ASTROLABE_ANTHROPIC_API_KEY for AI drafts", outreach.ErrInvalid))
		return
	}
	id := pathID(r)
	if _, err := srv.S.Pool.Exec(r.Context(), `UPDATE actions SET ai_state = 'pending' WHERE id = $1 AND status = 'draft'`, id); err != nil {
		done(w, r, "", "", err)
		return
	}
	err := srv.Writer.DraftAction(r.Context(), id)
	done(w, r, "", fmt.Sprintf("Claude drafted #%d", id), err)
}

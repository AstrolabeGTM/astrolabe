package mcp

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type briefArg struct {
	Product string `json:"product"`
	Person  string `json:"person" jsonschema:"person id or email"`
	Refresh bool   `json:"refresh,omitempty" jsonschema:"write a new one even if a recent brief exists"`
}

type actionArg struct {
	ActionID int64 `json:"action_id"`
}

type answerArg struct {
	ReplyID int64  `json:"reply_id"`
	Body    string `json:"body"`
}

type playRunsArg struct {
	Product string `json:"product,omitempty"`
	Play    string `json:"play,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

func (s *Server) aiEnabled() error {
	if s.Writer == nil || !s.Writer.LLM.Enabled() {
		return errors.New("Claude API is not configured (ASTROLABE_ANTHROPIC_API_KEY); you can write the text yourself with write_draft")
	}
	return nil
}

func (s *Server) brief(ctx context.Context, a briefArg) (any, error) {
	if err := s.aiEnabled(); err != nil {
		return nil, err
	}
	p, err := s.resolvePerson(ctx, a.Person)
	if err != nil {
		return nil, err
	}
	b, err := s.Writer.Brief(ctx, a.Product, p.ID, a.Refresh)
	return map[string]any{"brief": b}, err
}

func (s *Server) aiDraft(ctx context.Context, a actionArg) (any, error) {
	if err := s.aiEnabled(); err != nil {
		return nil, err
	}
	if _, err := s.S.Pool.Exec(ctx, `UPDATE actions SET ai_state = 'pending' WHERE id = $1 AND status = 'draft'`, a.ActionID); err != nil {
		return nil, err
	}
	if err := s.Writer.DraftAction(ctx, a.ActionID); err != nil {
		return nil, err
	}
	return s.S.Action(ctx, a.ActionID)
}

func (s *Server) answerReply(ctx context.Context, a answerArg) (any, error) {
	id, err := s.S.AnswerReply(ctx, a.ReplyID, a.Body)
	return map[string]any{"action_id": id, "note": "Saved as a draft; approve it to send."}, err
}

func (s *Server) plays(ctx context.Context, a playRunsArg) (any, error) {
	type playOut struct {
		Product  string `json:"product"`
		ID       string `json:"id"`
		Trigger  any    `json:"trigger"`
		When     any    `json:"when"`
		Action   any    `json:"action"`
		Paused   bool   `json:"paused,omitempty"`
		Enrolled int    `json:"enrolled"`
		Tasks    int    `json:"tasks"`
		Skipped  int    `json:"skipped"`
	}
	var out []playOut
	for _, p := range s.Products() {
		if a.Product != "" && p.ID != a.Product {
			continue
		}
		for _, pl := range p.Plays {
			o := playOut{Product: p.ID, ID: pl.ID, Trigger: pl.Trigger, When: pl.When, Action: pl.Action, Paused: pl.Paused}
			s.S.Pool.QueryRow(ctx, `
				SELECT count(*) FILTER (WHERE outcome = 'enrolled'), count(*) FILTER (WHERE outcome = 'task'), count(*) FILTER (WHERE outcome = 'skipped')
				FROM play_runs WHERE product_id = $1 AND play_id = $2`, p.ID, pl.ID).Scan(&o.Enrolled, &o.Tasks, &o.Skipped)
			out = append(out, o)
		}
	}
	return map[string]any{"plays": out}, nil
}

func (s *Server) playRuns(ctx context.Context, a playRunsArg) (any, error) {
	if a.Limit <= 0 {
		a.Limit = 50
	}
	rows, err := s.S.Pool.Query(ctx, `
		SELECT product_id, play_id, outcome, detail, person_id, created_at FROM play_runs
		WHERE ($1 = '' OR product_id = $1) AND ($2 = '' OR play_id = $2) ORDER BY id DESC LIMIT $3`, a.Product, a.Play, a.Limit)
	if err != nil {
		return nil, err
	}
	type run struct {
		Product  string    `json:"product"`
		Play     string    `json:"play"`
		Outcome  string    `json:"outcome"`
		Detail   string    `json:"detail,omitempty"`
		PersonID *int64    `json:"person_id,omitempty"`
		At       time.Time `json:"at"`
	}
	runs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (run, error) {
		var x run
		return x, r.Scan(&x.Product, &x.Play, &x.Outcome, &x.Detail, &x.PersonID, &x.At)
	})
	return map[string]any{"runs": runs}, err
}

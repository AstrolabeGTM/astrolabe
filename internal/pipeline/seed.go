package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

// SeedAccount is one example account (a demo product's demo-accounts.yaml).
// Days are counted back from the day it loads, so the demo always has
// recent, stuck and overdue accounts.
type SeedAccount struct {
	Company string `yaml:"company"` // domain
	Name    string `yaml:"name"`
	Stage   string `yaml:"stage"`
	// DaysInStage: how long ago it entered Stage (or was paused or lost).
	DaysInStage int    `yaml:"days_in_stage"`
	Evidence    string `yaml:"evidence"`
	NextAction  string `yaml:"next_action"`
	DueInDays   int    `yaml:"due_in_days"` // negative: already overdue
	// Checks: check name -> "yes: one line of evidence".
	Checks map[string]string `yaml:"checks"`
	// People: email -> role. They must already exist (demo-people.csv).
	People   map[string]string `yaml:"people"`
	Goal     string            `yaml:"goal"`
	Timing   string            `yaml:"timing"`
	Baseline string            `yaml:"baseline"`
	Links    []string          `yaml:"links"`
	// Status paused or lost, with a reason and a revisit date.
	Status        string `yaml:"status"`
	Reason        string `yaml:"reason"`
	RevisitInDays int    `yaml:"revisit_in_days"`
}

// SeedFile loads example accounts from a YAML file into an empty pipeline.
// It returns how many it added; it does nothing if the product already has
// accounts.
func SeedFile(ctx context.Context, db DB, p *product.Product, path string, now time.Time) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var list []SeedAccount
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&list); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE product_id = $1`, p.ID).Scan(&n); err != nil || n > 0 {
		return 0, err
	}
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		for _, s := range list {
			if err := seed(ctx, tx, p, s, now); err != nil {
				return fmt.Errorf("%s: %w", s.Company, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(list), nil
}

// seed walks one account through the same functions as everyone else:
// added at the first stage, qualified, then moved (and maybe closed).
func seed(ctx context.Context, tx pgx.Tx, p *product.Product, s SeedAccount, now time.Time) error {
	if err := needPipeline(p); err != nil {
		return err
	}
	days := func(n int) time.Time { return now.AddDate(0, 0, n) }
	entered := days(-s.DaysInStage)
	first := p.Pipeline.Stages[0]
	addAt := entered
	if s.Stage != first || s.Status != "" {
		addAt = entered.AddDate(0, 0, -7)
	}
	id, err := Add(ctx, tx, p, AddOpts{Domain: s.Company, Name: s.Name, Evidence: s.Evidence, NextAction: s.NextAction,
		Due: days(s.DueInDays), Goal: s.Goal, Timing: s.Timing, Baseline: s.Baseline}, addAt)
	if err != nil {
		return err
	}
	for email, role := range s.People {
		who, err := people.FindByEmail(ctx, tx, email)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("no person %s", email)
		} else if err != nil {
			return err
		}
		if err := SetPerson(ctx, tx, p, id, who.ID, role); err != nil {
			return err
		}
	}
	for _, l := range s.Links {
		if err := AddLink(ctx, tx, p, id, l, ""); err != nil {
			return err
		}
	}
	var answers []Answer
	for check, v := range s.Checks {
		a, ev, _ := strings.Cut(v, ":")
		answers = append(answers, Answer{Check: check, Answer: strings.TrimSpace(a), Evidence: strings.TrimSpace(ev)})
	}
	if len(answers) > 0 {
		if err := Qualify(ctx, tx, p, id, answers, addAt); err != nil {
			return err
		}
	}
	if s.Stage != "" && s.Stage != first {
		at := entered
		if s.Status != "" {
			at = addAt.AddDate(0, 0, 3)
		}
		if err := Move(ctx, tx, p, id, MoveOpts{Stage: s.Stage, Evidence: s.Evidence}, at); err != nil {
			return err
		}
	}
	if s.Status != "" {
		return Close(ctx, tx, p, id, CloseOpts{Status: s.Status, Reason: s.Reason, Revisit: days(s.RevisitInDays)}, entered)
	}
	return nil
}

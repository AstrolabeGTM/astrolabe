package notes

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/pipeline"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

// SeedNote is one example conversation (a demo product's
// demo-conversations.yaml). Days are counted back from the day it loads.
type SeedNote struct {
	Person          string    `yaml:"person"`  // email; must already exist (demo-people.csv)
	Account         string    `yaml:"account"` // company domain, optional
	DaysAgo         int       `yaml:"days_ago"`
	Channel         string    `yaml:"channel"`
	Duration        int       `yaml:"duration_minutes"`
	Assisted        bool      `yaml:"assisted"`
	Summary         string    `yaml:"summary"`
	TheirWords      string    `yaml:"their_words"`
	Problem         string    `yaml:"problem"`
	CurrentApproach string    `yaml:"current_approach"`
	NextStep        string    `yaml:"next_step"`
	PMF             *PMF      `yaml:"pmf"`
	Pricing         []Pricing `yaml:"pricing"`
}

// SeedFile loads example conversations into a product that has none yet and
// returns how many it added.
func SeedFile(ctx context.Context, db DB, p *product.Product, path string, now time.Time) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var list []SeedNote
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&list); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE product_id = $1`, p.ID).Scan(&n); err != nil || n > 0 {
		return 0, err
	}
	for _, s := range list {
		who, err := people.FindByEmail(ctx, db, s.Person)
		if err != nil {
			return 0, fmt.Errorf("%s: no person %q", path, s.Person)
		}
		o := LogOpts{PersonID: who.ID, Date: now.AddDate(0, 0, -s.DaysAgo), Channel: s.Channel, Duration: s.Duration, Assisted: s.Assisted,
			Summary: s.Summary, TheirWords: s.TheirWords, Problem: s.Problem, CurrentApproach: s.CurrentApproach, NextStep: s.NextStep,
			PMF: s.PMF, Pricing: s.Pricing}
		if s.Account != "" {
			if o.AccountID, err = pipeline.Find(ctx, db, p, s.Account); err != nil {
				return 0, fmt.Errorf("%s: %w", path, err)
			}
		}
		if _, err := Log(ctx, db, p, o, now); err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
	}
	return len(list), nil
}

package product

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// FitRule adds Weight when a person or their company has any of the facts.
// An Exclude rule makes the person ineligible for acquisition plays.
type FitRule struct {
	Any     []string `yaml:"any" json:"any"`
	Weight  int      `yaml:"weight" json:"weight"`
	Exclude bool     `yaml:"exclude" json:"exclude"`
	Label   string   `yaml:"label" json:"label"`
}

// Monitor produces signals on a schedule; products/<id>/monitors.yaml.
type Monitor struct {
	ID       string `yaml:"id" json:"id"`
	Type     string `yaml:"type" json:"type"`
	Signal   string `yaml:"signal" json:"signal"` // signal type produced (github_repo appends .star/.fork/.issue)
	Strength int    `yaml:"strength" json:"strength"`
	WhyNow   bool   `yaml:"why_now" json:"why_now"`
	Every    string `yaml:"every" json:"every"`

	Query    string   `yaml:"query" json:"query,omitempty"`       // github_search: GitHub issue search syntax
	Repo     string   `yaml:"repo" json:"repo,omitempty"`         // github_repo: owner/name
	Events   []string `yaml:"events" json:"events,omitempty"`     // github_repo: star, fork, issue
	Keywords []string `yaml:"keywords" json:"keywords,omitempty"` // community
	Sources  []string `yaml:"sources" json:"sources,omitempty"`   // community: hn, stackoverflow, rss
	Feeds    []string `yaml:"feeds" json:"feeds,omitempty"`       // community rss feed URLs
}

var MonitorTypes = []string{"github_search", "github_repo", "community"}

// Interval returns how often the monitor runs.
func (m *Monitor) Interval() time.Duration {
	d, err := AfterDuration(m.Every)
	if err != nil || d == 0 {
		return time.Hour
	}
	return d
}

func (p *Product) validateFit() []string {
	var bad []string
	for i, r := range p.Fit {
		if len(r.Any) == 0 {
			bad = append(bad, fmt.Sprintf("fit rule %d: any needs at least one fact", i+1))
		}
		if !r.Exclude && (r.Weight < 1 || r.Weight > 100) {
			bad = append(bad, fmt.Sprintf("fit rule %d: weight must be 1–100", i+1))
		}
		for _, f := range r.Any {
			if !strings.Contains(f, ":") || f != strings.ToLower(f) {
				bad = append(bad, fmt.Sprintf("fit rule %d: fact %q must be lowercase kind:value, e.g. dep:bullmq", i+1, f))
			}
		}
	}
	return bad
}

func (p *Product) validateMonitors() []string {
	var bad []string
	seen := map[string]bool{}
	for i, m := range p.Monitors {
		where := fmt.Sprintf("monitors.yaml #%d (%s)", i+1, m.ID)
		if !idPattern.MatchString(m.ID) || seen[m.ID] {
			bad = append(bad, where+": id must be unique lowercase letters, digits and dashes")
		}
		seen[m.ID] = true
		if !slices.Contains(MonitorTypes, m.Type) {
			bad = append(bad, fmt.Sprintf("%s: type must be one of %v", where, MonitorTypes))
		}
		if m.Signal == "" || strings.ContainsAny(m.Signal, " /") {
			bad = append(bad, where+": signal must be a type name like github.issue_pain")
		}
		if m.Strength < 1 || m.Strength > 100 {
			bad = append(bad, where+": strength must be 1–100")
		}
		if d, err := AfterDuration(m.Every); err != nil || d < 15*time.Minute {
			bad = append(bad, where+": every must be at least 15m (e.g. 1h)")
		}
		switch m.Type {
		case "github_search":
			if m.Query == "" {
				bad = append(bad, where+": query is required")
			}
		case "github_repo":
			if strings.Count(m.Repo, "/") != 1 {
				bad = append(bad, where+": repo must be owner/name")
			}
			for _, e := range m.Events {
				if !slices.Contains([]string{"star", "fork", "issue"}, e) {
					bad = append(bad, where+": events are star, fork, issue")
				}
			}
			if len(m.Events) == 0 {
				bad = append(bad, where+": events needs at least one of star, fork, issue")
			}
		case "community":
			if len(m.Keywords) == 0 {
				bad = append(bad, where+": keywords are required")
			}
			for _, s := range m.Sources {
				if !slices.Contains([]string{"hn", "stackoverflow", "rss"}, s) {
					bad = append(bad, where+": sources are hn, stackoverflow, rss (Reddit blocks unauthenticated search)")
				}
			}
			if slices.Contains(m.Sources, "rss") && len(m.Feeds) == 0 {
				bad = append(bad, where+": rss needs feeds")
			}
		}
	}
	return bad
}

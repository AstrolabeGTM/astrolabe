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
	// Reception also files each result in the reception log as a public
	// mention of the product.
	Reception bool `yaml:"reception" json:"reception,omitempty"`

	// Forms (netlify_forms polls Netlify; form_webhook receives signed posts).
	Site    string            `yaml:"site" json:"site,omitempty"`       // netlify_forms: site id or name
	Form    string            `yaml:"form" json:"form,omitempty"`       // netlify_forms: the form's name
	Fields  map[string]string `yaml:"fields" json:"fields,omitempty"`   // Astrolabe key -> the form's field name
	Account bool              `yaml:"account" json:"account,omitempty"` // open a pipeline account for the company
	Respond string            `yaml:"respond" json:"respond,omitempty"` // response time for the account's next action; default 1d
}

var MonitorTypes = []string{"github_search", "github_repo", "community", "github_release", "netlify_forms", "form_webhook"}

// FormFieldKeys are the keys a form monitor's fields map may set.
var FormFieldKeys = []string{"email", "name", "company", "domain", "role", "source", "campaign", "message"}

// FormMonitor reports monitor types that turn form submissions into signals.
func FormMonitor(t string) bool { return t == "netlify_forms" || t == "form_webhook" }

// PushMonitor reports monitor types that receive data instead of polling,
// so the scheduler never runs them.
func PushMonitor(t string) bool { return t == "form_webhook" }

// CounterMonitor reports monitor types that record aggregate counts, never
// signals about people.
func CounterMonitor(t string) bool { return t == "github_release" }

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
		if !CounterMonitor(m.Type) {
			if m.Signal == "" || strings.ContainsAny(m.Signal, " /") {
				bad = append(bad, where+": signal must be a type name like github.issue_pain")
			}
			if m.Strength < 1 || m.Strength > 100 {
				bad = append(bad, where+": strength must be 1–100")
			}
		}
		if d, err := AfterDuration(m.Every); err != nil || d < 15*time.Minute {
			bad = append(bad, where+": every must be at least 15m (e.g. 1h)")
		}
		if FormMonitor(m.Type) {
			bad = append(bad, p.validateForm(where, m)...)
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
				if !slices.Contains([]string{"star", "fork", "issue", "discussion"}, e) {
					bad = append(bad, where+": events are star, fork, issue, discussion")
				}
			}
			if len(m.Events) == 0 {
				bad = append(bad, where+": events needs at least one of star, fork, issue, discussion")
			}
		case "github_release":
			if strings.Count(m.Repo, "/") != 1 {
				bad = append(bad, where+": repo must be owner/name")
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

func (p *Product) validateForm(where string, m *Monitor) []string {
	var bad []string
	if m.Type == "netlify_forms" && (m.Site == "" || m.Form == "") {
		bad = append(bad, where+": site (a Netlify site id or name) and form (the form's name) are required")
	}
	for k, v := range m.Fields {
		if !slices.Contains(FormFieldKeys, k) {
			bad = append(bad, fmt.Sprintf("%s: fields key %q must be one of %v", where, k, FormFieldKeys))
		}
		if strings.TrimSpace(v) == "" {
			bad = append(bad, fmt.Sprintf("%s: fields.%s needs the form's field name", where, k))
		}
	}
	if m.Respond != "" {
		if _, err := AfterDuration(m.Respond); err != nil {
			bad = append(bad, where+": respond: "+err.Error())
		}
	}
	if m.Account && (p.Pipeline == nil || len(p.Pipeline.Stages) == 0) {
		bad = append(bad, where+": account: true needs a pipeline in product.yaml")
	}
	return bad
}

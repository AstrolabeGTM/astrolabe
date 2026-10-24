package product

import (
	"fmt"
	"slices"
	"strings"
)

// Links controls tracked links in drafts.
type Links struct {
	// Personal swaps the offer link in each draft for a tracked short link
	// unique to that message. Unset means true.
	Personal *bool `yaml:"personal" json:"personal,omitempty"`
}

// PersonalLinks reports whether drafts get a personal tracked link.
func (p *Product) PersonalLinks() bool { return p.Links.Personal == nil || *p.Links.Personal }

// Outreach holds guards that apply to every email, whatever started it.
type Outreach struct {
	// EmailFromGitHub allows email to a person whose only known identity and
	// address came from GitHub. GitHub's Acceptable Use Policies forbid using
	// GitHub data to send unsolicited email, so it is off by default.
	EmailFromGitHub bool `yaml:"email_from_github" json:"email_from_github"`
}

// Pipeline tracks companies (accounts) through deal stages, beside the
// per-person funnel. b2b products only.
type Pipeline struct {
	Stages  []string `yaml:"stages" json:"stages"`
	Qualify *Qualify `yaml:"qualify" json:"qualify,omitempty"`
	// Reasons an account can be paused or lost; DefaultReasons when empty.
	Reasons []string `yaml:"reasons" json:"reasons,omitempty"`
	// StuckAfter: how long an account may sit in one stage before the
	// digest and review list it as stuck (default 14d).
	StuckAfter string `yaml:"stuck_after" json:"stuck_after,omitempty"`
}

// Qualify lists the checks answered yes, no or unknown, each with one line
// of evidence. Entering Gate, or any later stage, needs every one of Checks
// answered yes. Record checks are asked and kept but never block.
type Qualify struct {
	Gate   string   `yaml:"gate" json:"gate"`
	Checks []string `yaml:"checks" json:"checks"`
	Record []string `yaml:"record" json:"record,omitempty"`
}

// ClosedStates are where an account leaves the pipeline, with a reason and
// a revisit date. They are not configurable stages.
var ClosedStates = []string{"paused", "lost"}

// DefaultReasons apply when pipeline.reasons is empty.
var DefaultReasons = []string{"no_fit", "no_timing", "no_reply", "chose_alternative", "no_budget", "other"}

// PipelineReasons returns the configured or default paused/lost reasons.
func (p *Product) PipelineReasons() []string {
	if p.Pipeline == nil || len(p.Pipeline.Reasons) == 0 {
		return DefaultReasons
	}
	return p.Pipeline.Reasons
}

// HasAccountStage reports whether s is a configured pipeline stage.
func (p *Product) HasAccountStage(s string) bool {
	return p.Pipeline != nil && slices.Contains(p.Pipeline.Stages, s)
}

func (p *Product) validatePipeline() []string {
	pl := p.Pipeline
	if pl == nil {
		return nil
	}
	var bad []string
	add := func(format string, a ...any) { bad = append(bad, "pipeline: "+fmt.Sprintf(format, a...)) }
	if p.Audience != "b2b" {
		add("needs audience: b2b (it tracks companies)")
	}
	if len(pl.Stages) < 2 {
		add("stages needs at least two stages")
	}
	validName := func(s string) bool { return idPattern.MatchString(strings.ReplaceAll(s, "_", "-")) }
	seen := map[string]bool{}
	for _, s := range pl.Stages {
		switch {
		case !validName(s):
			add("stage %q must be lowercase letters, digits and underscores", s)
		case slices.Contains(ClosedStates, s):
			add("stage %q is built in (accounts are paused or lost with a reason); remove it", s)
		case seen[s]:
			add("stage %q is listed twice", s)
		}
		seen[s] = true
	}
	if q := pl.Qualify; q != nil {
		if !seen[q.Gate] {
			add("qualify.gate %q must be one of the stages", q.Gate)
		}
		if len(q.Checks) == 0 {
			add("qualify.checks needs at least one check")
		}
		checks := map[string]bool{}
		for _, c := range append(slices.Clone(q.Checks), q.Record...) {
			if !validName(c) || checks[c] {
				add("qualify check %q must be a unique lowercase name", c)
			}
			checks[c] = true
		}
	}
	reasons := map[string]bool{}
	for _, r := range pl.Reasons {
		if !validName(r) || reasons[r] {
			add("reason %q must be a unique lowercase name", r)
		}
		reasons[r] = true
	}
	if pl.StuckAfter != "" {
		if _, err := AfterDuration(pl.StuckAfter); err != nil {
			add("stuck_after: %v", err)
		}
	}
	return bad
}

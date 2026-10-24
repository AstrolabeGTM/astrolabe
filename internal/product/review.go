package product

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Review configures the weekly review (sales: pipeline review, forecast
// call, QBR-lite). Every part is optional; leave the section out for the
// defaults.
type Review struct {
	// Questions the review asks, in order. DefaultReviewQuestions when empty.
	Questions []string `yaml:"questions" json:"questions,omitempty"`
	// Targets for the response-time section (how fast we first answer).
	Targets *ReviewTargets `yaml:"targets" json:"targets,omitempty"`
	// Weekday is the review day (default friday). The digest and the
	// browser app use it to say when the review is due.
	Weekday string `yaml:"weekday" json:"weekday,omitempty"`
}

// ReviewTargets are first-response targets, as durations like 2h or 1d.
type ReviewTargets struct {
	Requests    string `yaml:"requests" json:"requests,omitempty"`
	Replies     string `yaml:"replies" json:"replies,omitempty"`
	Issues      string `yaml:"issues" json:"issues,omitempty"`
	Discussions string `yaml:"discussions" json:"discussions,omitempty"`
}

// DefaultReviewQuestions apply when review.questions is empty.
var DefaultReviewQuestions = []string{
	"How many reached activation, and what did they say?",
	"What changed in reach (counters, sign-ups, mentions), and where did new people say they came from?",
	"How many accounts moved, and where are they stuck?",
	"What did people say that surprised us?",
	"What is the single biggest blocker to the next activation?",
}

// Weekdays are the allowed values of review.weekday.
var Weekdays = []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}

// Default response targets, in the order of ReviewTargets' fields.
const (
	DefaultTargetRequests    = "2h"
	DefaultTargetReplies     = "2h"
	DefaultTargetIssues      = "24h"
	DefaultTargetDiscussions = "24h"
)

// ReviewSettings is the review config with defaults filled in.
type ReviewSettings struct {
	Questions []string
	Weekday   time.Weekday
	// Targets by kind: request, reply, issue, discussion.
	Targets map[string]time.Duration
	// TargetText is the same as written, e.g. "2h".
	TargetText map[string]string
}

// ReviewSettings returns the review config with defaults; products without
// a review: section get the defaults.
func (p *Product) ReviewSettings() ReviewSettings {
	r := Review{}
	if p.Review != nil {
		r = *p.Review
	}
	s := ReviewSettings{Questions: r.Questions, Weekday: time.Friday, Targets: map[string]time.Duration{}, TargetText: map[string]string{}}
	if len(s.Questions) == 0 {
		s.Questions = DefaultReviewQuestions
	}
	if i := slices.Index(Weekdays, strings.ToLower(r.Weekday)); i >= 0 {
		s.Weekday = time.Weekday((i + 1) % 7)
	}
	t := ReviewTargets{}
	if r.Targets != nil {
		t = *r.Targets
	}
	for kind, v := range map[string][2]string{
		"request": {t.Requests, DefaultTargetRequests}, "reply": {t.Replies, DefaultTargetReplies},
		"issue": {t.Issues, DefaultTargetIssues}, "discussion": {t.Discussions, DefaultTargetDiscussions},
	} {
		text := v[0]
		if text == "" {
			text = v[1]
		}
		s.TargetText[kind] = text
		s.Targets[kind], _ = AfterDuration(text)
	}
	return s
}

func (p *Product) validateReview() []string {
	r := p.Review
	if r == nil {
		return nil
	}
	var bad []string
	add := func(format string, a ...any) { bad = append(bad, "review."+fmt.Sprintf(format, a...)) }
	seen := map[string]bool{}
	for i, q := range r.Questions {
		q = strings.TrimSpace(q)
		switch {
		case q == "":
			add("questions[%d] is empty", i)
		case len(q) > 300:
			add("questions[%d] is longer than 300 characters", i)
		case seen[q]:
			add("question %q is listed twice", q)
		}
		seen[q] = true
	}
	if len(r.Questions) > 12 {
		add("questions has %d entries; keep it to 12 or fewer", len(r.Questions))
	}
	if r.Weekday != "" && !slices.Contains(Weekdays, strings.ToLower(r.Weekday)) {
		add("weekday must be one of %v", Weekdays)
	}
	if r.Targets != nil {
		for name, v := range map[string]string{"requests": r.Targets.Requests, "replies": r.Targets.Replies,
			"issues": r.Targets.Issues, "discussions": r.Targets.Discussions} {
			if v == "" {
				continue
			}
			if d, err := AfterDuration(v); err != nil {
				add("targets.%s: %v", name, err)
			} else if d <= 0 {
				add("targets.%s must be more than zero", name)
			}
		}
	}
	return bad
}

package product

import (
	"fmt"
	"slices"
	"strings"
)

// Play is an automation: trigger → filter (when) → action; one entry in
// products/<id>/automations.yaml. ("Play" is the internal name.)
type Play struct {
	ID      string  `yaml:"id" json:"id"`
	Trigger Trigger `yaml:"trigger" json:"trigger"`
	When    When    `yaml:"when" json:"when"`
	Delay   string  `yaml:"delay" json:"delay,omitempty"` // wait before the first step; conditions are rechecked
	Action  Action  `yaml:"action" json:"action"`
	// Success defaults to the product offer's success event and window.
	Success     Success `yaml:"success" json:"success"`
	LimitPerDay int     `yaml:"limit_per_day" json:"limit_per_day,omitempty"`
	Paused      bool    `yaml:"paused" json:"paused,omitempty"`
	// Notes: audience, offer and maximum effort, written before starting.
	Notes string `yaml:"notes" json:"notes,omitempty"`
}

type Trigger struct {
	Signal     string      `yaml:"signal" json:"signal,omitempty"`           // a signal type
	StageStuck *StageStuck `yaml:"stage_stuck" json:"stage_stuck,omitempty"` // furthest stage unchanged for a while
	Stage      string      `yaml:"stage" json:"stage,omitempty"`             // just entered a stage
}

type StageStuck struct {
	Stage string `yaml:"stage" json:"stage"`
	For   string `yaml:"for" json:"for"`
}

// When filters who the play acts on. Zero values mean "no condition".
type When struct {
	MinFit             int    `yaml:"min_fit" json:"min_fit,omitempty"`
	MinPriority        int    `yaml:"min_priority" json:"min_priority,omitempty"`
	MinTier            string `yaml:"min_tier" json:"min_tier,omitempty"`
	NotContactedWithin string `yaml:"not_contacted_within" json:"not_contacted_within,omitempty"`
	NotStage           string `yaml:"not_stage" json:"not_stage,omitempty"` // skip people who reached this stage
	// Cold automations skip excluded (fit exclude rule) people; users
	// plays for existing users can set include_excluded.
	IncludeExcluded bool `yaml:"include_excluded" json:"include_excluded,omitempty"`
}

type Action struct {
	Sequence string `yaml:"sequence" json:"sequence,omitempty"`
	Task     string `yaml:"task" json:"task,omitempty"`
	Draft    bool   `yaml:"draft" json:"draft,omitempty"` // AI drafts text for the task
}

type Success struct {
	Event  string `yaml:"event" json:"event,omitempty"`
	Within string `yaml:"within" json:"within,omitempty"`
}

// Goal returns the play's success stage, defaulting to the offer's.
func (p *Product) Goal(pl *Play) Success {
	g := pl.Success
	if g.Event == "" {
		g.Event = p.Offer.SuccessEvent
	}
	if g.Within == "" {
		g.Within = p.Offer.Window
	}
	return g
}

func (p *Product) Play(id string) *Play {
	for _, pl := range p.Plays {
		if pl.ID == id {
			return pl
		}
	}
	return nil
}

func (p *Product) validatePlays(stages map[string]bool) []string {
	var bad []string
	seen := map[string]bool{}
	for i, pl := range p.Plays {
		where := fmt.Sprintf("automations.yaml #%d (%s)", i+1, pl.ID)
		add := func(msg string) { bad = append(bad, where+": "+msg) }
		if !idPattern.MatchString(pl.ID) || seen[pl.ID] {
			add("id must be unique lowercase letters, digits and dashes")
		}
		seen[pl.ID] = true
		n := 0
		if pl.Trigger.Signal != "" {
			n++
		}
		if pl.Trigger.Stage != "" {
			n++
			if !stages[pl.Trigger.Stage] {
				add(fmt.Sprintf("trigger stage %q is not a funnel stage", pl.Trigger.Stage))
			}
		}
		if st := pl.Trigger.StageStuck; st != nil {
			n++
			if !stages[st.Stage] {
				add(fmt.Sprintf("stage_stuck stage %q is not a funnel stage", st.Stage))
			}
			if _, err := AfterDuration(st.For); err != nil {
				add("stage_stuck.for: " + err.Error())
			}
		}
		if n != 1 {
			add("trigger needs exactly one of signal, stage, stage_stuck")
		}
		if (pl.Action.Sequence == "") == (pl.Action.Task == "") {
			add("action needs exactly one of sequence or task")
		}
		if pl.Action.Sequence != "" && p.Sequences[pl.Action.Sequence] == nil {
			add(fmt.Sprintf("sequence %q does not exist in sequences/", pl.Action.Sequence))
		}
		for field, d := range map[string]string{"delay": pl.Delay, "when.not_contacted_within": pl.When.NotContactedWithin, "success.within": pl.Success.Within} {
			if d != "" {
				if _, err := AfterDuration(d); err != nil {
					add(field + ": " + err.Error())
				}
			}
		}
		if pl.When.MinTier != "" && !slices.Contains([]string{"A", "B", "C", "D"}, strings.ToUpper(pl.When.MinTier)) {
			add("when.min_tier must be A, B, C or D")
		}
		if pl.When.NotStage != "" && !stages[pl.When.NotStage] {
			add(fmt.Sprintf("when.not_stage %q is not a funnel stage", pl.When.NotStage))
		}
		if pl.Success.Event != "" && !stages[pl.Success.Event] {
			add(fmt.Sprintf("success.event %q is not a funnel stage", pl.Success.Event))
		}
	}
	return bad
}

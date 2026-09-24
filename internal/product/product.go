// Package product loads and validates product folders (products/<id>/).
package product

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

type Product struct {
	ID       string   `yaml:"id" json:"id"`
	Name     string   `yaml:"name" json:"name"`
	URL      string   `yaml:"url" json:"url"`
	OneLiner string   `yaml:"one_liner" json:"one_liner"`
	Audience string   `yaml:"audience" json:"audience"`
	Target   Target   `yaml:"target" json:"target"`
	Funnel   []string `yaml:"funnel" json:"funnel"`

	Activation  string      `yaml:"activation" json:"activation"`
	Offer       Offer       `yaml:"offer" json:"offer"`
	Claims      Claims      `yaml:"claims" json:"claims"`
	Voice       string      `yaml:"voice" json:"voice"`
	Sender      Sender      `yaml:"sender" json:"sender"`
	AutoApprove AutoApprove `yaml:"auto_approve" json:"auto_approve"`
	Limits      Limits      `yaml:"limits" json:"limits"`
	// Fit rules score people against facts found by enrichment or import.
	Fit []FitRule `yaml:"fit" json:"fit"`
	// Lifecycle channels (optional).
	WhatsApp *WhatsAppConfig `yaml:"whatsapp" json:"whatsapp,omitempty"`
	// Demo marks example data that only loads in sandbox mode.
	Demo   bool          `yaml:"demo" json:"demo,omitempty"`
	Notify *NotifyConfig `yaml:"notify" json:"notify,omitempty"`

	// Loaded from the folder, not from product.yaml.
	Dir       string               `yaml:"-" json:"-"`
	Hash      string               `yaml:"-" json:"-"`
	Sequences map[string]*Sequence `yaml:"-" json:"-"`
	Monitors  []*Monitor           `yaml:"-" json:"monitors"`
	Plays     []*Play              `yaml:"-" json:"plays"`
	// File contents, so background jobs write from the same text as the files.
	ClaimsText     string `yaml:"-" json:"claims_text"`
	VoiceText      string `yaml:"-" json:"voice_text"`
	ObjectionsText string `yaml:"-" json:"objections_text"`
}

// Target describes who the product is for, in plain sentences (used in
// prompts and by me); fit rules make it machine-checkable.
type Target struct {
	Include []string `yaml:"include" json:"include"`
	Exclude []string `yaml:"exclude" json:"exclude"`
}

type Offer struct {
	Promise      string `yaml:"promise" json:"promise"`
	CTA          string `yaml:"cta" json:"cta"`
	Destination  string `yaml:"destination" json:"destination"`
	SuccessEvent string `yaml:"success_event" json:"success_event"`
	Window       string `yaml:"window" json:"window"`
}

type Claims struct {
	Allowed string   `yaml:"allowed" json:"allowed"`
	Never   []string `yaml:"never" json:"never"`
}

// Sender addresses: cold email from a separate domain, user email from the
// product domain.
type Sender struct {
	Cold  string `yaml:"cold" json:"cold"`
	Users string `yaml:"users" json:"users"`
}

// AutoApprove says which AI drafts go out without me clicking approve:
// none, follow_ups (after I approved the first message) or all. Flagged
// drafts always wait.
type AutoApprove struct {
	Cold  string `yaml:"cold" json:"cold"`
	Users string `yaml:"users" json:"users"`
}

// WhatsAppConfig: the Cloud API phone number id messages are sent from.
type WhatsAppConfig struct {
	PhoneNumberID string `yaml:"phone_number_id" json:"phone_number_id"`
}

// NotifyConfig: the product's endpoint that delivers push and in-app
// messages (signed with ASTROLABE_NOTIFY_SECRET_<PRODUCT>).
type NotifyConfig struct {
	URL string `yaml:"url" json:"url"`
}

type Limits struct {
	ColdPerDay              int `yaml:"cold_per_day" json:"cold_per_day"`
	TouchesPerPersonPerWeek int `yaml:"touches_per_person_per_week" json:"touches_per_person_per_week"`
}

// Sequence is an ordered list of steps; products/<id>/sequences/<name>.yaml.
type Sequence struct {
	ID string `yaml:"-" json:"id"`
	// Kind is cold (people who don't know you yet; sender.cold, counts
	// toward limits.cold_per_day) or users (existing users of the product;
	// sender.users; can use WhatsApp, push and in-app).
	Kind string `yaml:"kind" json:"kind"`
	// ExperimentUnit is person or company (default: company for b2b, so
	// coworkers see the same variant).
	ExperimentUnit string   `yaml:"experiment_unit" json:"experiment_unit,omitempty"`
	Steps          []Step   `yaml:"steps" json:"steps"`
	StopOn         []string `yaml:"stop_on" json:"stop_on"`
}

type Step struct {
	Channel string `yaml:"channel" json:"channel"`
	After   string `yaml:"after" json:"after"` // offset from enrollment start: 0d, 3d, 12h
	Brief   bool   `yaml:"brief" json:"brief"`
	Goal    string `yaml:"goal" json:"goal"`
	// Optional starting text; {{.FirstName}}, {{.Company}} etc. are filled in.
	// Without them the draft is created empty for me or Claude to write.
	// For push/in_app the subject is the notification title.
	Subject string `yaml:"subject" json:"subject"`
	Body    string `yaml:"body" json:"body"`
	// WhatsApp: an approved template, its language and body parameters.
	Template string   `yaml:"template" json:"template,omitempty"`
	Language string   `yaml:"language" json:"language,omitempty"`
	Params   []string `yaml:"params" json:"params,omitempty"`
	// Variants make this step an experiment: each person (or company, see
	// the sequence's experiment_unit) gets one variant, fixed once assigned.
	// A variant overrides the step's subject, body and goal.
	Variants []Variant `yaml:"variants" json:"variants,omitempty"`
}

type Variant struct {
	ID      string `yaml:"id" json:"id"`
	Subject string `yaml:"subject" json:"subject,omitempty"`
	Body    string `yaml:"body" json:"body,omitempty"`
	Goal    string `yaml:"goal" json:"goal,omitempty"`
}

// TemplateData is what step subject/body templates can use.
type TemplateData struct {
	FirstName, Name, Company, Domain, Product, Promise, CTA, Destination string
	// Link is a tracked short link to the offer destination (or the
	// destination itself when no public URL is configured).
	Link string
}

// Render fills a step template. Unknown fields are errors, not blanks.
func Render(text string, d TemplateData) (string, error) {
	if text == "" {
		return "", nil
	}
	t, err := template.New("step").Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	err = t.Execute(&b, d)
	return b.String(), err
}

// Channels a step can use. whatsapp, push, in_app and webhook are for
// users sequences (existing users) only.
var Channels = []string{"email", "linkedin_task", "x_task", "whatsapp", "push", "in_app", "webhook"}

// LifecycleOnly reports channels that must not be used for cold outreach.
func LifecycleOnly(c string) bool {
	return c == "whatsapp" || c == "push" || c == "in_app" || c == "webhook"
}

// ManualChannel reports whether a step is a task I do by hand.
func ManualChannel(c string) bool { return strings.HasSuffix(c, "_task") }

// Stop reasons recorded by the system, usable in stop_on alongside funnel stages.
var systemStops = []string{"replied", "unsubscribed", "bounced"}

var (
	idPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	durationPattern  = regexp.MustCompile(`^(\d+)([dhm])$`)
	autoApproveModes = []string{"none", "follow_ups", "all"}
)

// AfterDuration parses "3d", "12h" or "30m".
func AfterDuration(s string) (time.Duration, error) {
	m := durationPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("duration %q must look like 3d or 12h", s)
	}
	n, _ := strconv.Atoi(m[1])
	if m[2] == "d" {
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.Duration(n) * time.Hour, nil
}

// Load reads and validates one product folder. The error lists every problem.
func Load(dir string) (*Product, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "product.yaml"))
	if err != nil {
		return nil, err
	}
	p := &Product{Dir: dir, Sequences: map[string]*Sequence{}}
	canon, err := normalize(raw, productAliases)
	if err != nil {
		return nil, fmt.Errorf("%s/product.yaml: %w", dir, err)
	}
	if err := decodeStrict(canon, p); err != nil {
		return nil, fmt.Errorf("%s/product.yaml: %w", dir, err)
	}

	h := sha256.New()
	h.Write(raw)
	seqFiles, _ := filepath.Glob(filepath.Join(dir, "sequences", "*.yaml"))
	sort.Strings(seqFiles)
	var problems []string
	for _, f := range seqFiles {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		h.Write([]byte(f))
		h.Write(raw)
		s := &Sequence{ID: strings.TrimSuffix(filepath.Base(f), ".yaml")}
		canon, err := normalize(raw, sequenceAliases)
		if err == nil {
			err = decodeStrict(canon, s)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("sequences/%s.yaml: %v", s.ID, err))
			continue
		}
		p.Sequences[s.ID] = s
	}
	// automations.yaml, or plays.yaml in sales vocabulary; not both.
	autoFile := ""
	for _, name := range []string{"automations.yaml", "plays.yaml"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if autoFile != "" {
			problems = append(problems, fmt.Sprintf("both %s and %s exist; keep one", autoFile, name))
			break
		}
		autoFile = name
		h.Write(raw)
		if err := decodeStrict(raw, &p.Plays); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "monitors.yaml")); err == nil {
		h.Write(raw)
		if err := decodeStrict(raw, &p.Monitors); err != nil {
			problems = append(problems, fmt.Sprintf("monitors.yaml: %v", err))
		}
	}
	read := func(f string) string {
		if f == "" || strings.Contains(f, "..") || filepath.IsAbs(f) {
			return ""
		}
		b, _ := os.ReadFile(filepath.Join(dir, f))
		h.Write(b)
		return string(b)
	}
	p.ClaimsText, p.VoiceText, p.ObjectionsText = read(p.Claims.Allowed), read(p.Voice), read("objections.md")
	p.Hash = hex.EncodeToString(h.Sum(nil))

	problems = append(problems, p.validate()...)
	if len(problems) > 0 {
		return p, &ValidationError{Dir: dir, Problems: problems}
	}
	return p, nil
}

// LoadAll loads every products/<id>/ folder that has a product.yaml.
func LoadAll(root string) ([]*Product, error) {
	dirs, err := filepath.Glob(filepath.Join(root, "*", "product.yaml"))
	if err != nil {
		return nil, err
	}
	var out []*Product
	var errs []error
	for _, f := range dirs {
		p, err := Load(filepath.Dir(f))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, p)
	}
	return out, errors.Join(errs...)
}

type ValidationError struct {
	Dir      string
	Problems []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s is invalid:\n  - %s", e.Dir, strings.Join(e.Problems, "\n  - "))
}

func decodeStrict(raw []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	return dec.Decode(v)
}

func (p *Product) validate() []string {
	var bad []string
	add := func(format string, a ...any) { bad = append(bad, fmt.Sprintf(format, a...)) }

	if !idPattern.MatchString(p.ID) {
		add("id %q must be lowercase letters, digits and dashes", p.ID)
	}
	if base := filepath.Base(p.Dir); p.ID != base {
		add("id %q must match the folder name %q", p.ID, base)
	}
	for field, v := range map[string]string{"name": p.Name, "one_liner": p.OneLiner, "url": p.URL} {
		if strings.TrimSpace(v) == "" {
			add("%s is required", field)
		}
	}
	if p.Audience != "b2b" && p.Audience != "b2c" {
		add("audience must be b2b or b2c")
	}
	if len(p.Target.Include) == 0 {
		add("target.include needs at least one line describing who it's for")
	}

	if len(p.Funnel) == 0 {
		add("funnel needs at least one stage")
	}
	seen := map[string]bool{}
	for _, s := range p.Funnel {
		if !idPattern.MatchString(strings.ReplaceAll(s, "_", "-")) {
			add("funnel stage %q must be lowercase letters, digits and underscores", s)
		}
		if seen[s] {
			add("funnel stage %q is listed twice", s)
		}
		seen[s] = true
	}
	if !seen[p.Activation] {
		add("activation %q must be one of the funnel stages", p.Activation)
	}

	if p.Offer.Promise == "" || p.Offer.CTA == "" {
		add("offer.promise and offer.cta are required")
	}
	if !strings.HasPrefix(p.Offer.Destination, "https://") {
		add("offer.destination must be an https URL")
	}
	if !seen[p.Offer.SuccessEvent] {
		add("offer.success_event %q must be one of the funnel stages", p.Offer.SuccessEvent)
	}
	if _, err := AfterDuration(p.Offer.Window); err != nil {
		add("offer.window: %v", err)
	}

	for field, f := range map[string]string{"claims.allowed": p.Claims.Allowed, "voice": p.Voice} {
		switch {
		case f == "":
			add("%s must name a file in the product folder", field)
		case filepath.IsAbs(f) || strings.Contains(f, ".."):
			add("%s must be a file inside the product folder", field)
		default:
			if b, err := os.ReadFile(filepath.Join(p.Dir, f)); err != nil {
				add("%s: %v", field, err)
			} else if strings.TrimSpace(string(b)) == "" {
				add("%s (%s) is empty", field, f)
			} else if strings.HasPrefix(string(b), "STUB:") {
				add("%s (%s) is still a stub; write it before sending anything", field, f)
			}
		}
	}

	for field, addr := range map[string]string{"sender.cold": p.Sender.Cold, "sender.users": p.Sender.Users} {
		if a, err := mail.ParseAddress(addr); err != nil || a.Address != addr {
			add("%s must be a plain email address", field)
		}
	}
	for field, v := range map[string]*string{"auto_approve.cold": &p.AutoApprove.Cold, "auto_approve.users": &p.AutoApprove.Users} {
		if *v == "" {
			*v = "none"
		}
		if !slices.Contains(autoApproveModes, *v) {
			add("%s must be one of %v", field, autoApproveModes)
		}
	}
	if p.Limits.ColdPerDay < 1 || p.Limits.TouchesPerPersonPerWeek < 1 {
		add("limits.cold_per_day and limits.touches_per_person_per_week must be at least 1")
	}

	bad = append(bad, p.validateFit()...)
	bad = append(bad, p.validateMonitors()...)
	bad = append(bad, p.validatePlays(seen)...)

	ids := make([]string, 0, len(p.Sequences))
	for id := range p.Sequences {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, problem := range p.Sequences[id].validate(p, seen) {
			add("sequences/%s.yaml: %s", id, problem)
		}
	}
	return bad
}

func (s *Sequence) validate(p *Product, stages map[string]bool) []string {
	var bad []string
	if !idPattern.MatchString(s.ID) {
		bad = append(bad, "file name must be lowercase letters, digits and dashes")
	}
	if len(s.Steps) == 0 {
		bad = append(bad, "needs at least one step")
	}
	if s.Kind == "" {
		s.Kind = "cold"
	}
	if s.Kind != "cold" && s.Kind != "users" {
		bad = append(bad, "kind must be cold (people who don't know you) or users (existing users)")
	}
	if s.ExperimentUnit == "" {
		s.ExperimentUnit = "person"
		if p.Audience == "b2b" {
			s.ExperimentUnit = "company"
		}
	}
	if s.ExperimentUnit != "person" && s.ExperimentUnit != "company" {
		bad = append(bad, "experiment_unit must be person or company")
	}
	var prev time.Duration
	for i, st := range s.Steps {
		if !slices.Contains(Channels, st.Channel) {
			bad = append(bad, fmt.Sprintf("step %d: channel %q is not available (have %v)", i+1, st.Channel, Channels))
		}
		d, err := AfterDuration(st.After)
		if err != nil {
			bad = append(bad, fmt.Sprintf("step %d: after: %v", i+1, err))
		} else if d < prev {
			bad = append(bad, fmt.Sprintf("step %d: after must not be earlier than the previous step", i+1))
		} else {
			prev = d
		}
		if st.Goal == "" {
			bad = append(bad, fmt.Sprintf("step %d: goal is required", i+1))
		}
		for field, text := range map[string]string{"subject": st.Subject, "body": st.Body} {
			if _, err := Render(text, TemplateData{}); err != nil {
				bad = append(bad, fmt.Sprintf("step %d: %s template: %v", i+1, field, err))
			}
		}
		if len(st.Variants) == 1 {
			bad = append(bad, fmt.Sprintf("step %d: an experiment needs at least two variants", i+1))
		}
		vids := map[string]bool{}
		for _, v := range st.Variants {
			if !idPattern.MatchString(v.ID) || vids[v.ID] {
				bad = append(bad, fmt.Sprintf("step %d: variant ids must be unique lowercase names", i+1))
			}
			vids[v.ID] = true
			for field, text := range map[string]string{"subject": v.Subject, "body": v.Body} {
				if _, err := Render(text, TemplateData{}); err != nil {
					bad = append(bad, fmt.Sprintf("step %d variant %s: %s template: %v", i+1, v.ID, field, err))
				}
			}
			if i > 0 && st.Channel == "email" && v.Subject != "" {
				bad = append(bad, fmt.Sprintf("step %d variant %s: follow-ups keep the thread subject", i+1, v.ID))
			}
		}
		if LifecycleOnly(st.Channel) && s.Kind != "users" {
			bad = append(bad, fmt.Sprintf("step %d: %s only reaches existing users; set kind: users", i+1, st.Channel))
		}
		switch st.Channel {
		case "whatsapp":
			if st.Template == "" {
				bad = append(bad, fmt.Sprintf("step %d: whatsapp needs an approved template name", i+1))
			}
			if p.WhatsApp == nil || p.WhatsApp.PhoneNumberID == "" {
				bad = append(bad, fmt.Sprintf("step %d: whatsapp needs whatsapp.phone_number_id in product.yaml", i+1))
			}
			for j, prm := range st.Params {
				if _, err := Render(prm, TemplateData{}); err != nil {
					bad = append(bad, fmt.Sprintf("step %d: param %d: %v", i+1, j+1, err))
				}
			}
		case "push", "in_app", "webhook":
			if p.Notify == nil || !strings.HasPrefix(p.Notify.URL, "https://") {
				bad = append(bad, fmt.Sprintf("step %d: %s needs notify.url (https) in product.yaml", i+1, st.Channel))
			}
		}
		if i > 0 && st.Channel == "email" && st.Subject != "" {
			bad = append(bad, fmt.Sprintf("step %d: follow-up emails reply in the same thread, so they cannot set a subject", i+1))
		}
	}
	if !slices.Contains(s.StopOn, "replied") {
		bad = append(bad, "stop_on must include replied")
	}
	for _, r := range s.StopOn {
		if !slices.Contains(systemStops, r) && !stages[r] {
			bad = append(bad, fmt.Sprintf("stop_on %q is neither %v nor a funnel stage", r, systemStops))
		}
	}
	return bad
}

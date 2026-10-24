package product

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Campaign is one planned push (a launch week, a small paid test) with a
// goal, a budget, a landing page and the work to do. products/<id>/campaigns/<id>.yaml
// holds the defaults; budget, dates, stop rule and gate checks are changed at
// runtime and every change is kept (internal/campaign).
type Campaign struct {
	ID       string   `yaml:"-" json:"id"` // file name
	Code     string   `yaml:"code" json:"code"`
	Label    string   `yaml:"label" json:"label"`
	Goal     string   `yaml:"goal" json:"goal"`
	Audience string   `yaml:"audience" json:"audience"`
	Channels []string `yaml:"channels" json:"channels"`
	Offer    string   `yaml:"offer" json:"offer"`
	Landing  string   `yaml:"landing" json:"landing"`
	// SuccessEvent is a funnel stage or a signal type.
	SuccessEvent string      `yaml:"success_event" json:"success_event"`
	Budget       Budget      `yaml:"budget" json:"budget"`
	Start        string      `yaml:"start" json:"start"`
	End          string      `yaml:"end" json:"end,omitempty"`
	StopRule     StopRule    `yaml:"stop_rule" json:"stop_rule"`
	Gate         []string    `yaml:"gate" json:"gate"`
	Tasks        []PostTask  `yaml:"tasks" json:"tasks"`
	Creatives    []*Creative `yaml:"creatives" json:"creatives"`
}

// Money is an amount in minor units (cents). YAML writes it as 200 or 200.50.
type Money int64

func (m *Money) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseMoney(n.Value)
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// ParseMoney reads "200", "200.5" or "1,200.50" without floating point.
func ParseMoney(s string) (Money, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" || s == "-" {
		return 0, fmt.Errorf("%q is not an amount", s)
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 2 || whole == "" && frac == "" {
		return 0, fmt.Errorf("%q is not an amount with at most 2 decimals", s)
	}
	frac += strings.Repeat("0", 2-len(frac))
	w, err1 := strconv.ParseInt(orZero(whole), 10, 64)
	f, err2 := strconv.ParseInt(frac, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("%q is not an amount", s)
	}
	v := w*100 + f
	if neg {
		v = -v
	}
	return Money(v), nil
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// MarshalJSON writes 12.5 style numbers, so reports and the stored config
// read as money rather than cents.
func (m Money) MarshalJSON() ([]byte, error) { return []byte(m.String()), nil }

func (m *Money) UnmarshalJSON(b []byte) error {
	v, err := ParseMoney(string(b))
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// String formats minor units as 12.50.
func (m Money) String() string {
	sign := ""
	v := int64(m)
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

// Budget is the default spend cap; 0 means nothing is planned to be spent.
type Budget struct {
	Cap      Money  `yaml:"cap" json:"cap"`
	Currency string `yaml:"currency" json:"currency"`
}

// StopRule is a plain-text rule and an optional form a machine can check:
// stop when spend has reached spend_pct percent of the cap while success
// events are at most max_events (default 0). Astrolabe only warns; it never
// stops or raises spend by itself.
type StopRule struct {
	Text string     `yaml:"text" json:"text"`
	When *StopCheck `yaml:"when" json:"when,omitempty"`
}

type StopCheck struct {
	SpendPct  int `yaml:"spend_pct" json:"spend_pct"`
	MaxEvents int `yaml:"max_events" json:"max_events"`
}

// PostTask is a launch task, e.g. "post Show HN" on day 0. Day is an offset
// from the campaign start (negative is before). Body is the post, with
// {{link}} where the campaign link goes.
type PostTask struct {
	ID      string `yaml:"id" json:"id"`
	Title   string `yaml:"title" json:"title"`
	Channel string `yaml:"channel" json:"channel"`
	Day     int    `yaml:"day" json:"day"`
	Body    string `yaml:"body" json:"body,omitempty"`
}

// Creative is an HTML template plus copy fields, rendered at each size.
type Creative struct {
	ID       string            `yaml:"id" json:"id"`
	Template string            `yaml:"template" json:"template"`
	Sizes    []string          `yaml:"sizes" json:"sizes"`
	Formats  []string          `yaml:"formats" json:"formats,omitempty"`
	Copy     map[string]string `yaml:"copy" json:"copy"`
	Alt      string            `yaml:"alt" json:"alt"`
	// Source says where numbers in the copy come from (a study, claims.md).
	Source string `yaml:"source" json:"source,omitempty"`

	// Loaded from the folder: the template and the files beside it that it uses.
	TemplateText string            `yaml:"-" json:"template_text"`
	Assets       map[string][]byte `yaml:"-" json:"assets,omitempty"`
}

var (
	codePattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,23}$`)
	sizePattern        = regexp.MustCompile(`^(\d{2,4})x(\d{2,4})$`)
	datePattern        = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	currencyPattern    = regexp.MustCompile(`^[A-Z]{3}$`)
	placeholderPattern = regexp.MustCompile(`\{\{\s*([a-z][a-z0-9_]*)\s*\}\}`)
	assetRef           = regexp.MustCompile(`(?:href|src)="([^"#?:]+)"`)
)

// Placeholders lists the {{names}} a creative template uses, in order.
func Placeholders(tmpl string) []string {
	var out []string
	for _, m := range placeholderPattern.FindAllStringSubmatch(tmpl, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// BuiltinPlaceholders are filled by the renderer, not from copy.
var BuiltinPlaceholders = []string{"width", "height"}

// ParseSize reads "1200x627".
func ParseSize(s string) (w, h int, err error) {
	m := sizePattern.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, fmt.Errorf("size %q must look like 1200x627", s)
	}
	w, _ = strconv.Atoi(m[1])
	h, _ = strconv.Atoi(m[2])
	if w < 100 || h < 100 || w > 4000 || h > 4000 {
		return 0, 0, fmt.Errorf("size %q must be between 100 and 4000 pixels each way", s)
	}
	return w, h, nil
}

// Campaign returns the campaign with this id (file name) or code.
func (p *Product) Campaign(idOrCode string) *Campaign {
	for _, c := range p.Campaigns {
		if c.ID == idOrCode {
			return c
		}
	}
	for _, c := range p.Campaigns {
		if c.Code == idOrCode {
			return c
		}
	}
	return nil
}

// Creative returns a campaign's creative by id.
func (c *Campaign) Creative(id string) *Creative {
	for _, cr := range c.Creatives {
		if cr.ID == id {
			return cr
		}
	}
	return nil
}

// CreativeFormats is the formats to produce: png unless the file asks for more.
func (cr *Creative) CreativeFormats() []string {
	if len(cr.Formats) == 0 {
		return []string{"png"}
	}
	return cr.Formats
}

// loadCampaigns reads campaigns/*.yaml, adds each file and template to the
// folder hash, and returns problems with the files.
func (p *Product) loadCampaigns(hash func(name string, b []byte)) []string {
	files, _ := filepath.Glob(filepath.Join(p.Dir, "campaigns", "*.yaml"))
	sort.Strings(files)
	var bad []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			bad = append(bad, err.Error())
			continue
		}
		hash(f, raw)
		c := &Campaign{ID: strings.TrimSuffix(filepath.Base(f), ".yaml")}
		if err := decodeStrict(raw, c); err != nil {
			bad = append(bad, fmt.Sprintf("campaigns/%s.yaml: %v", c.ID, err))
			continue
		}
		for _, cr := range c.Creatives {
			bad = append(bad, p.loadCreative(c, cr, hash)...)
		}
		p.Campaigns = append(p.Campaigns, c)
	}
	return bad
}

func (p *Product) loadCreative(c *Campaign, cr *Creative, hash func(string, []byte)) []string {
	var bad []string
	where := fmt.Sprintf("campaigns/%s.yaml: creative %q", c.ID, cr.ID)
	if cr.Template == "" || filepath.IsAbs(cr.Template) || strings.Contains(cr.Template, "..") {
		return []string{where + ": template must be a file inside the product folder"}
	}
	path := filepath.Join(p.Dir, cr.Template)
	b, err := os.ReadFile(path)
	if err != nil {
		return []string{where + ": template: " + err.Error()}
	}
	hash(path, b)
	cr.TemplateText = string(b)
	cr.Assets = map[string][]byte{}
	for _, m := range assetRef.FindAllStringSubmatch(cr.TemplateText, -1) {
		ref := m[1]
		if strings.Contains(ref, "//") || filepath.IsAbs(ref) {
			continue // remote or absolute: not ours to upload
		}
		ap := filepath.Join(filepath.Dir(path), ref)
		if rel, err := filepath.Rel(p.Dir, ap); err != nil || strings.HasPrefix(rel, "..") {
			bad = append(bad, fmt.Sprintf("%s: %q reaches outside the product folder", where, ref))
			continue
		}
		ab, err := os.ReadFile(ap)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", where, err))
			continue
		}
		if prev, dup := cr.Assets[filepath.Base(ref)]; dup && string(prev) != string(ab) {
			bad = append(bad, fmt.Sprintf("%s: two different files are both called %s; keep assets beside the template with unique names", where, filepath.Base(ref)))
		}
		hash(ap, ab)
		cr.Assets[filepath.Base(ref)] = ab
	}
	return bad
}

func (p *Product) validateCampaigns() []string {
	var bad []string
	codes := map[string]string{}
	for _, c := range p.Campaigns {
		add := func(format string, a ...any) {
			bad = append(bad, fmt.Sprintf("campaigns/%s.yaml: %s", c.ID, fmt.Sprintf(format, a...)))
		}
		if !idPattern.MatchString(c.ID) {
			add("file name must be lowercase letters, digits and dashes")
		}
		if !codePattern.MatchString(c.Code) {
			add("code %q must be 2-24 lowercase letters, digits, dashes or underscores", c.Code)
		} else if other, dup := codes[c.Code]; dup {
			add("code %q is already used by %s", c.Code, other)
		}
		codes[c.Code] = c.ID
		for field, v := range map[string]string{"label": c.Label, "goal": c.Goal, "audience": c.Audience, "offer": c.Offer} {
			if strings.TrimSpace(v) == "" {
				add("%s is required", field)
			}
		}
		if len(c.Channels) == 0 {
			add("channels needs at least one channel (e.g. hn, x, newsletter, search)")
		}
		if !strings.HasPrefix(c.Landing, "https://") {
			add("landing must be an https URL")
		}
		if c.SuccessEvent == "" {
			add("success_event is required: a funnel stage or a signal type")
		} else if !slices.Contains(p.Funnel, c.SuccessEvent) && !idPattern.MatchString(strings.NewReplacer("_", "-", ".", "-").Replace(c.SuccessEvent)) {
			add("success_event %q must be a funnel stage or a signal type like form.request", c.SuccessEvent)
		}
		if c.Budget.Cap < 0 {
			add("budget.cap must not be negative")
		}
		if !currencyPattern.MatchString(c.Budget.Currency) {
			add("budget.currency %q must be a 3-letter code such as USD", c.Budget.Currency)
		}
		var start, end time.Time
		var err error
		if !datePattern.MatchString(c.Start) {
			add("start must be a date like 2026-12-01")
		} else if start, err = time.Parse("2006-01-02", c.Start); err != nil {
			add("start: %v", err)
		}
		if c.End != "" {
			if !datePattern.MatchString(c.End) {
				add("end must be a date like 2026-12-08")
			} else if end, err = time.Parse("2006-01-02", c.End); err != nil {
				add("end: %v", err)
			} else if !start.IsZero() && end.Before(start) {
				add("end must not be before start")
			}
		}
		if w := c.StopRule.When; w != nil {
			if w.SpendPct < 1 || w.SpendPct > 100 {
				add("stop_rule.when.spend_pct must be 1-100")
			}
			if w.MaxEvents < 0 {
				add("stop_rule.when.max_events must not be negative")
			}
			if strings.TrimSpace(c.StopRule.Text) == "" {
				add("stop_rule.text is required when stop_rule.when is set (say it in words too)")
			}
		}
		seen := map[string]bool{}
		for _, g := range c.Gate {
			if strings.TrimSpace(g) == "" || seen[g] {
				add("gate items must be non-empty and unique")
			}
			seen[g] = true
		}
		tasks := map[string]bool{}
		for _, t := range c.Tasks {
			if !idPattern.MatchString(t.ID) || tasks[t.ID] {
				add("task ids must be unique lowercase names (got %q)", t.ID)
			}
			tasks[t.ID] = true
			if strings.TrimSpace(t.Title) == "" || !idPattern.MatchString(t.Channel) {
				add("task %q needs a title and a lowercase channel (e.g. hn, x, linkedin)", t.ID)
			}
		}
		cids := map[string]bool{}
		for _, cr := range c.Creatives {
			add := func(format string, a ...any) { add("creative %q: "+format, append([]any{cr.ID}, a...)...) }
			if !idPattern.MatchString(cr.ID) || cids[cr.ID] {
				add("ids must be unique lowercase names")
			}
			cids[cr.ID] = true
			if len(cr.Sizes) == 0 {
				add("sizes needs at least one, e.g. 1200x627")
			}
			for _, s := range cr.Sizes {
				if _, _, err := ParseSize(s); err != nil {
					add("%v", err)
				}
			}
			for _, f := range cr.Formats {
				if f != "png" && f != "pdf" {
					add("format %q must be png or pdf", f)
				}
			}
			if strings.TrimSpace(cr.Alt) == "" {
				add("alt text is required")
			}
			if cr.TemplateText == "" {
				continue // problem already reported when loading
			}
			for _, name := range Placeholders(cr.TemplateText) {
				if slices.Contains(BuiltinPlaceholders, name) || name == "alt" {
					continue
				}
				if _, ok := cr.Copy[name]; !ok {
					add("template uses {{%s}} but copy has no %q field", name, name)
				}
			}
			for name := range cr.Copy {
				if !slices.Contains(Placeholders(cr.TemplateText), name) {
					add("copy field %q is not used by the template", name)
				}
			}
		}
	}
	return bad
}

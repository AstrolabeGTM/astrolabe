package product

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Spec is what the setup interview collects for a new product.
type Spec struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	URL           string   `json:"url"`
	OneLiner      string   `json:"one_liner"`
	Audience      string   `json:"audience" jsonschema:"b2b or b2c"`
	TargetInclude []string `json:"target_include" jsonschema:"who it's for, one sentence each"`
	TargetExclude []string `json:"target_exclude,omitempty" jsonschema:"who it's not for"`
	Funnel        []string `json:"funnel" jsonschema:"ordered stage names, e.g. reached, replied, signup, activated, paid"`
	Activation    string   `json:"activation" jsonschema:"the aha-moment stage"`
	Promise       string   `json:"offer_promise"`
	CTA           string   `json:"offer_cta"`
	Destination   string   `json:"offer_destination" jsonschema:"https URL of the next step"`
	SenderCold    string   `json:"sender_cold" jsonschema:"mailbox for cold email, ideally on a separate domain"`
	SenderUsers   string   `json:"sender_users" jsonschema:"mailbox for emails to existing users, on the product domain"`
	Never         []string `json:"never_claims,omitempty"`
	// Claims and voice as agreed with me in the interview. Left empty, the
	// files are written as STUB and the product won't load until filled.
	Claims string `json:"claims,omitempty"`
	Voice  string `json:"voice,omitempty"`
}

// Scaffold writes products/<id>/ with product.yaml, claims.md, voice.md, a
// starter outbound sequence and an empty plays.yaml. It never overwrites.
func Scaffold(root string, s Spec) (string, error) {
	if !idPattern.MatchString(s.ID) {
		return "", fmt.Errorf("id %q must be lowercase letters, digits and dashes", s.ID)
	}
	dir := filepath.Join(root, s.ID)
	if _, err := os.Stat(dir); err == nil {
		return "", fmt.Errorf("%s already exists; edit its files instead", dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	type offer struct {
		Promise      string `yaml:"promise"`
		CTA          string `yaml:"cta"`
		Destination  string `yaml:"destination"`
		SuccessEvent string `yaml:"success_event"`
		Window       string `yaml:"window"`
	}
	doc := map[string]any{
		"id": s.ID, "name": s.Name, "url": s.URL, "one_liner": s.OneLiner, "audience": s.Audience,
		"target":       map[string][]string{"include": s.TargetInclude, "exclude": s.TargetExclude},
		"funnel":       s.Funnel,
		"activation":   s.Activation,
		"offer":        offer{s.Promise, s.CTA, s.Destination, s.Activation, "14d"},
		"claims":       map[string]any{"allowed": "claims.md", "never": append([]string{}, s.Never...)},
		"voice":        "voice.md",
		"sender":       map[string]string{"cold": s.SenderCold, "users": s.SenderUsers},
		"auto_approve": map[string]string{"cold": "none", "users": "none"},
		"limits":       map[string]int{"cold_per_day": 20, "touches_per_person_per_week": 2},
		"fit":          []any{},
	}
	y, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	stopOn := []string{"replied", "unsubscribed", "bounced", s.Activation}
	files := map[string]string{
		"product.yaml":     "# Written by product_setup; edit freely.\n" + string(y),
		"claims.md":        orStub(s.Claims, "STUB: list what is true about "+s.Name+" today, each with a proof link. Drafts may only use these.\n"),
		"voice.md":         orStub(s.Voice, "STUB: tone notes and 3–5 example messages you'd happily send.\n"),
		"automations.yaml": "# Automations: trigger -> when -> action. Add them once monitors or product events exist. See docs/astrolabe.md §6.\n[]\n",
		"sequences/intro.yaml": fmt.Sprintf(`steps:
  - {channel: email, after: 0d, brief: true, goal: "%s"}
  - {channel: email, after: 4d, goal: "share one concrete example"}
  - {channel: email, after: 10d, goal: "polite close-the-loop"}
stop_on: [%s]
`, strings.ReplaceAll(s.CTA, `"`, `'`), strings.Join(stopOn, ", ")),
	}
	if err := os.MkdirAll(filepath.Join(dir, "sequences"), 0o755); err != nil {
		return "", err
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func orStub(text, stub string) string {
	if strings.TrimSpace(text) == "" {
		return stub
	}
	return text
}

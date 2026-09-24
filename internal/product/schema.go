package product

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// JSON Schemas for the product folder files, generated from the yaml tags
// (the keys people actually write). Editors with the YAML language server
// use them for autocomplete, hover help and typo detection. Sales-vocabulary
// aliases are included so either form completes and validates.

// SchemaBaseURL is where the published schemas live.
const SchemaBaseURL = "https://raw.githubusercontent.com/astrolabe-gtm/astrolabe/main/schemas/"

// SchemaFiles maps schema file names to the file each describes.
var SchemaFiles = map[string]string{
	"product.schema.json":     "product.yaml",
	"sequence.schema.json":    "sequences/*.yaml",
	"automations.schema.json": "automations.yaml",
	"monitors.schema.json":    "monitors.yaml",
}

var descriptions = map[string]string{
	"Product.id":                         "Folder name; lowercase letters, digits and dashes.",
	"Product.name":                       "Display name.",
	"Product.url":                        "Product website.",
	"Product.one_liner":                  "What it does, in one sentence.",
	"Product.audience":                   "b2b (companies and the people in them) or b2c (individual users).",
	"Product.target":                     "Who it's for, in plain sentences (sales term: ICP). Used in prompts; fit rules make it machine-checkable.",
	"Product.funnel":                     "Ordered stage names, your product's pipeline (e.g. reached, replied, signup, activated, paid). Product events with a matching type record the stage.",
	"Product.activation":                 "The stage where a user first gets real value (the 'aha' moment). Must be one of the funnel stages.",
	"Product.offer":                      "The one concrete thing every message asks for.",
	"Product.claims":                     "What drafts may say about the product.",
	"Product.voice":                      "Markdown file with tone notes and example messages.",
	"Product.sender":                     "Mailboxes messages come from.",
	"Product.auto_approve":               "Which AI drafts go out without your click (sales term: autonomy). Flagged drafts always wait.",
	"Product.limits":                     "Sending limits.",
	"Product.fit":                        "Scoring rules: each adds its weight if the person or company has any of the facts (e.g. dep:bullmq, lang:typescript).",
	"Product.whatsapp":                   "WhatsApp Cloud API sender, for kind: users sequences.",
	"Product.notify":                     "Your product's endpoint that delivers push/in-app messages (signed requests).",
	"Product.demo":                       "Demo data: only loads in sandbox mode.",
	"Target.include":                     "Who it's for, one sentence each.",
	"Target.exclude":                     "Who it's not for.",
	"Offer.promise":                      "What you promise to help with.",
	"Offer.cta":                          "The call to action every message ends with.",
	"Offer.destination":                  "https URL of the next step (tracked links point here).",
	"Offer.success_event":                "Funnel stage that counts as success for this offer.",
	"Offer.window":                       "How long to wait for success before judging, e.g. 14d.",
	"Claims.allowed":                     "Markdown file listing true claims with proof links; drafts may only use these.",
	"Claims.never":                       "Phrases a draft may never contain.",
	"Sender.cold":                        "Mailbox for cold email (sales: outbound), ideally on a separate domain.",
	"Sender.users":                       "Mailbox for email to existing users (sales: lifecycle).",
	"AutoApprove.cold":                   "none | follow_ups | all (sales: manual | first_step | auto).",
	"AutoApprove.users":                  "none | follow_ups | all (sales: manual | first_step | auto).",
	"Limits.cold_per_day":                "Most cold messages per day for this product (sales: outbound_per_day).",
	"Limits.touches_per_person_per_week": "Most messages one person gets per week, across products.",
	"FitRule.any":                        "Facts as kind:value, e.g. dep:bullmq, lang:go, team:5-50.",
	"FitRule.weight":                     "Points added when any fact matches (1-100).",
	"FitRule.exclude":                    "Matching people are never targeted by cold automations.",
	"FitRule.label":                      "Shown in the score breakdown.",
	"Sequence.kind":                      "cold: people who don't know you yet (sales: outbound). users: existing users (sales: lifecycle).",
	"Sequence.steps":                     "Timed message steps (sales: cadence).",
	"Sequence.stop_on":                   "Stop the sequence on replied, unsubscribed, bounced or any funnel stage.",
	"Sequence.experiment_unit":           "A/B assignment unit: person or company.",
	"Step.channel":                       "email, linkedin_task, x_task (you send by hand); whatsapp, push, in_app, webhook for kind: users.",
	"Step.after":                         "Delay from the sequence start, e.g. 0d, 3d, 12h.",
	"Step.brief":                         "Research the person with Claude before drafting.",
	"Step.goal":                          "What this message should achieve; guides the AI draft.",
	"Step.subject":                       "Optional subject template ({{.FirstName}}, {{.Company}}, {{.Link}}...). Follow-ups keep the thread subject.",
	"Step.body":                          "Optional body template; empty means you or Claude write it.",
	"Step.template":                      "Approved WhatsApp template name.",
	"Step.params":                        "WhatsApp template body parameters (templates allowed).",
	"Step.variants":                      "A/B variants of this step; each overrides subject, body or goal.",
	"Play.id":                            "Automation name (sales term: play).",
	"Play.trigger":                       "When it runs: a signal type, entering a stage, or being stuck at a stage.",
	"Play.when":                          "Filters: who it applies to.",
	"Play.delay":                         "Wait before the first step; conditions are rechecked.",
	"Play.action":                        "Start a sequence, or create a to-do.",
	"Play.success":                       "Stage that counts as success (default: the offer's).",
	"Play.limit_per_day":                 "Most runs per day.",
	"Play.notes":                         "Audience, offer and effort budget, written before starting.",
	"When.min_fit":                       "Only people with at least this fit score.",
	"When.min_tier":                      "Only priority A, B, C or better.",
	"When.not_contacted_within":          "Skip people messaged within this long, e.g. 30d.",
	"When.not_stage":                     "Skip people who reached this stage.",
	"Monitor.type":                       "github_search, github_repo or community.",
	"Monitor.signal":                     "Signal type it produces, e.g. github.issue_pain.",
	"Monitor.strength":                   "How strong each signal is (1-100); 25 is a strong buying signal.",
	"Monitor.why_now":                    "Time-sensitive (counts toward why_now).",
	"Monitor.every":                      "How often to run, e.g. 1h (at least 15m).",
	"Monitor.query":                      "GitHub issue search syntax; one phrase works best.",
	"Monitor.repo":                       "owner/name of a repo to watch.",
	"Monitor.keywords":                   "Phrases to look for.",
	"Monitor.sources":                    "hn, stackoverflow, rss.",
	"Monitor.feeds":                      "RSS/Atom feed URLs.",
}

var enums = map[string][]string{
	"Product.audience":         {"b2b", "b2c"},
	"AutoApprove.cold":         {"none", "follow_ups", "all", "manual", "first_step", "auto"},
	"AutoApprove.users":        {"none", "follow_ups", "all", "manual", "first_step", "auto"},
	"Sequence.kind":            {"cold", "users", "outbound", "lifecycle"},
	"Sequence.experiment_unit": {"person", "company"},
	"Step.channel":             {"email", "linkedin_task", "x_task", "whatsapp", "push", "in_app", "webhook"},
	"When.min_tier":            {"A", "B", "C", "D"},
	"Monitor.type":             {"github_search", "github_repo", "community"},
}

// schemaAliases adds sales-vocabulary keys: struct.alias -> canonical key.
var schemaAliases = map[string]map[string]string{
	"Product":     {"icp": "target", "autonomy": "auto_approve"},
	"Sender":      {"outbound": "cold", "lifecycle": "users"},
	"AutoApprove": {"outbound": "cold", "lifecycle": "users", "content": "cold"},
	"Limits":      {"outbound_per_day": "cold_per_day"},
}

var durationPatternJS = `^\d+[dhm]$`

var patterns = map[string]string{
	"Step.after": durationPatternJS, "Play.delay": durationPatternJS, "Offer.window": durationPatternJS,
	"Monitor.every": durationPatternJS, "When.not_contacted_within": durationPatternJS,
	"Product.id": `^[a-z0-9][a-z0-9-]*$`,
}

func schemaFor(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Int, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schemaFor(t.Elem())}
	case reflect.Struct:
		props := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if tag == "" || tag == "-" || !f.IsExported() {
				continue
			}
			s := schemaFor(f.Type)
			key := t.Name() + "." + tag
			if d := descriptions[key]; d != "" {
				s["description"] = d
			}
			if e := enums[key]; e != nil {
				s["enum"] = e
			}
			if p := patterns[key]; p != "" {
				s["pattern"] = p
			}
			props[tag] = s
		}
		for alias, canon := range schemaAliases[t.Name()] {
			if s, ok := props[canon].(map[string]any); ok {
				c := map[string]any{}
				for k, v := range s {
					c[k] = v
				}
				c["description"] = "Same as " + canon + ". " + stringOr(s["description"])
				props[alias] = c
			}
		}
		return map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	}
	return map[string]any{}
}

func stringOr(v any) string {
	s, _ := v.(string)
	return s
}

// Schemas returns the JSON Schema documents by file name.
func Schemas() map[string][]byte {
	docs := map[string]map[string]any{
		"product.schema.json":     schemaFor(reflect.TypeOf(Product{})),
		"sequence.schema.json":    schemaFor(reflect.TypeOf(Sequence{})),
		"automations.schema.json": schemaFor(reflect.TypeOf([]*Play{})),
		"monitors.schema.json":    schemaFor(reflect.TypeOf([]*Monitor{})),
	}
	docs["product.schema.json"]["required"] = []string{"id", "name", "url", "one_liner", "audience", "funnel", "activation", "offer", "claims", "voice", "sender", "limits"}
	docs["sequence.schema.json"]["required"] = []string{"steps", "stop_on"}
	out := map[string][]byte{}
	names := make([]string, 0, len(docs))
	for n := range docs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		d := docs[n]
		d["$schema"] = "http://json-schema.org/draft-07/schema#"
		d["$id"] = SchemaBaseURL + n
		d["title"] = "Astrolabe " + SchemaFiles[n]
		b, _ := json.MarshalIndent(d, "", "  ")
		out[n] = append(b, '\n')
	}
	return out
}

// SchemaComment is the first line that points an editor at a file's schema,
// relative to where the file sits in a product folder.
func SchemaComment(schemaFile, relPath string) string {
	return "# yaml-language-server: $schema=" + relPath + schemaFile + "\n"
}

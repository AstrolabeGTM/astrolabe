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
const SchemaBaseURL = "https://raw.githubusercontent.com/AstrolabeGTM/astrolabe/main/schemas/"

// SchemaFiles maps schema file names to the file each describes.
var SchemaFiles = map[string]string{
	"product.schema.json":     "product.yaml",
	"sequence.schema.json":    "sequences/*.yaml",
	"automations.schema.json": "automations.yaml",
	"monitors.schema.json":    "monitors.yaml",
	"campaign.schema.json":    "campaigns/*.yaml",
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
	"Product.links":                      "Tracked links in drafts.",
	"Product.outreach":                   "Guards on who may be emailed.",
	"Product.updates":                    "Opt-in email list (sales: newsletter, mailing list). Sign-ups are not added until they confirm, by replying to the confirmation email or, with a public URL, by link.",
	"Product.pipeline":                   "Companies (accounts) through deal stages, beside the per-person funnel (sales: deal pipeline). b2b only.",
	"Campaign.code":                      "Short code for landing-page variants and attribution: a signal or funnel event carrying campaign=<code> counts for this campaign.",
	"Campaign.label":                     "Human name; also shown in campaigns.json for per-campaign landing pages.",
	"Campaign.goal":                      "What this campaign is for, in a sentence.",
	"Campaign.audience":                  "Who it is aimed at (sales: ICP, segment).",
	"Campaign.channels":                  "Where it runs, e.g. hn, x, newsletter, search (sales: media mix).",
	"Campaign.offer":                     "The one thing it asks people to do.",
	"Campaign.landing":                   "https URL of this campaign's landing page (a per-campaign page measures better than shared ones).",
	"Campaign.success_event":             "What counts as a result: a funnel stage (e.g. signup) or a signal type (e.g. form.request).",
	"Campaign.budget":                    "Default spend cap (sales: insertion order, flight budget). Changed at runtime; every change is kept.",
	"Campaign.start":                     "First day, YYYY-MM-DD (sales: flight start). Can be changed at runtime.",
	"Campaign.end":                       "Last day, YYYY-MM-DD; empty means open-ended.",
	"Campaign.stop_rule":                 "When to stop, in words and optionally as a rule a machine can check. Astrolabe warns; it never stops or spends by itself.",
	"Campaign.gate":                      "Checklist that must be fully checked before any spend (sales: pre-flight checklist). Spend before it is complete is allowed but flagged.",
	"Campaign.tasks":                     "Launch tasks, such as posting a Show HN. Each becomes a task with the posted URL recorded when done.",
	"Campaign.creatives":                 "Creatives (ad creative, social cards): an HTML template plus copy fields, rendered at each size.",
	"Budget.cap":                         "Most to spend, as a number such as 200 or 200.50; 0 means nothing is planned to be spent.",
	"Budget.currency":                    "3-letter currency code, e.g. USD. Spend must be recorded in the same currency.",
	"StopRule.text":                      "The rule in plain words.",
	"StopRule.when":                      "Machine-checkable form: spend has reached spend_pct percent of the cap and success events are at most max_events.",
	"StopCheck.spend_pct":                "Percent of the cap spent (1-100).",
	"StopCheck.max_events":               "Most success events so far for the rule to apply (default 0: zero results).",
	"PostTask.id":                        "Unique name, lowercase.",
	"PostTask.title":                     "What to do, e.g. Post Show HN.",
	"PostTask.channel":                   "Where, e.g. hn, x, linkedin, reddit, newsletter.",
	"PostTask.day":                       "Days after the campaign start (negative is before) when it is due.",
	"PostTask.body":                      "The post text. {{link}} becomes the campaign link. Checked against claims; flagged posts wait.",
	"Creative.id":                        "Unique name, lowercase.",
	"Creative.template":                  "HTML file in the product folder with {{field}} placeholders; {{width}} and {{height}} are filled per size. Keep CSS and images beside it.",
	"Creative.sizes":                     "Sizes to render, WIDTHxHEIGHT in pixels, e.g. 1200x627.",
	"Creative.formats":                   "png (default) and/or pdf.",
	"Creative.copy":                      "Text for each placeholder in the template. Checked against claims; an edit withdraws approval.",
	"Creative.alt":                       "Alt text, kept with the image and used as {{alt}}.",
	"Creative.source":                    "Where numbers in the copy come from (a study, claims.md). Numbers found neither here nor in claims.md are flagged.",
	"Links.personal":                     "Swap the offer link in each draft for a personal tracked link (default true). false keeps the plain link.",
	"Outreach.email_from_github":         "Allow email to people known only through GitHub (default false). GitHub's Acceptable Use Policies forbid using GitHub data to send unsolicited email.",
	"Updates.name":                       "List name as subscribers see it. Default: '<product> updates'.",
	"Updates.from":                       "Sender address (default sender.users). Replies to it confirm and unsubscribe, so for via: smtp connect this mailbox.",
	"Updates.via":                        "smtp (the From mailbox, default) or resend (ASTROLABE_RESEND_API_KEY).",
	"Updates.per_day":                    "Most broadcast emails sent in any 24 hours (default 100), so a personal mailbox is not flooded.",
	"Updates.postal_address":             "Your postal address, shown in every message. Commercial email law requires one; no broadcast is sent without it.",
	"Updates.confirm_within":             "How long an unconfirmed sign-up stays valid, e.g. 7d (default).",
	"Updates.confirm_subject":            "Confirmation email subject; may use {{.List}}, {{.Product}}, {{.Email}}, {{.Expires}}.",
	"Updates.confirm_body":               "Confirmation email text. How to confirm and unsubscribe are added for you.",
	"Updates.welcome_subject":            "Welcome email subject, sent once after they confirm.",
	"Updates.welcome_body":               "Welcome email text.",
	"Action.updates":                     "subscribe: ask the person to join the product's updates list (needs an updates: section). They are added only after they confirm.",
	"Pipeline.stages":                    "Ordered account stages, e.g. identified, contacted, qualified, evaluation, won. Paused and lost are built in.",
	"Pipeline.qualify":                   "Checks that gate a stage (sales: BANT, MEDDIC), answered yes, no or unknown with evidence.",
	"Pipeline.reasons":                   "Reasons an account is paused or lost, e.g. no_timing, no_reply.",
	"Pipeline.stuck_after":               "An account in one stage longer than this shows as stuck (default 14d).",
	"Qualify.gate":                       "The stage that needs every check answered yes.",
	"Qualify.checks":                     "Checks that must be yes to enter the gate stage or any later one.",
	"Qualify.record":                     "Checks that are asked and kept but never block.",
	"Target.include":                     "Who it's for, one sentence each.",
	"Target.exclude":                     "Who it's not for.",
	"Offer.promise":                      "What you promise to help with.",
	"Offer.cta":                          "The call to action every message ends with.",
	"Offer.destination":                  "https URL of the next step (tracked links point here).",
	"Offer.success_event":                "Funnel stage that counts as success for this offer.",
	"Offer.window":                       "How long to wait for success before judging, e.g. 14d.",
	"Claims.allowed":                     "Markdown file listing true claims with proof links; drafts may only use these.",
	"Claims.never":                       "Phrases a draft may never contain.",
	"Action.sequence":                    "Start this sequence for the person.",
	"Action.task":                        "Create a to-do with this text.",
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
	"Sequence.stop_on":                   "Stop the sequence on replied, unsubscribed, bounced or any funnel stage. Must include replied unless every step is a task.",
	"Sequence.experiment_unit":           "A/B assignment unit: person or company.",
	"Step.channel":                       "email; linkedin_task, x_task (you send by hand); task (a to-do for you, such as a weekly check-in reminder; the draft text is the note, nothing is sent); whatsapp, push, in_app, webhook for kind: users.",
	"Step.after":                         "Delay from the sequence start, e.g. 0d, 3d, 12h.",
	"Step.brief":                         "Research the person with Claude before drafting.",
	"Step.goal":                          "What this message should achieve; guides the AI draft.",
	"Step.subject":                       "Optional subject template ({{.FirstName}}, {{.Company}}, {{.Link}}...). Follow-ups keep the thread subject.",
	"Step.body":                          "Optional body template; empty means you or Claude write it.",
	"Step.template":                      "Approved WhatsApp template name.",
	"Step.params":                        "WhatsApp template body parameters (templates allowed).",
	"Step.variants":                      "A/B variants of this step; each overrides subject, body or goal.",
	"Play.id":                            "Automation name (sales term: play).",
	"Play.trigger":                       "When it runs: a signal type, entering a stage, or being stuck at a stage (for people, or for accounts with account_stage and account_stuck).",
	"Trigger.account_stage":              "An account just entered this pipeline stage.",
	"Trigger.account_stuck":              "An account sat in a pipeline stage for this long.",
	"Play.when":                          "Filters: who it applies to.",
	"Play.delay":                         "Wait before the first step; conditions are rechecked.",
	"Play.action":                        "Start a sequence, create a to-do, or ask the person to join the updates list.",
	"Play.success":                       "Stage that counts as success (default: the offer's).",
	"Play.limit_per_day":                 "Most runs per day.",
	"Play.notes":                         "Audience, offer and effort budget, written before starting.",
	"When.min_fit":                       "Only people with at least this fit score.",
	"When.min_tier":                      "Only priority A, B, C or better.",
	"When.not_contacted_within":          "Skip people messaged within this long, e.g. 30d.",
	"When.not_stage":                     "Skip people who reached this stage.",
	"Monitor.type":                       "github_search, github_repo, community, github_release (download, star and issue counts; no people), netlify_forms (polls a Netlify form's verified submissions; needs ASTROLABE_NETLIFY_TOKEN), or form_webhook (receives signed POSTs at /hooks/forms/<product>; never polled).",
	"Monitor.reception":                  "Also file each result in the reception log as a public mention.",
	"Monitor.signal":                     "Signal type it produces, e.g. github.issue_pain.",
	"Monitor.strength":                   "How strong each signal is (1-100); 25 is a strong buying signal.",
	"Monitor.why_now":                    "Time-sensitive (counts toward why_now).",
	"Monitor.every":                      "How often to run, e.g. 1h (at least 15m).",
	"Monitor.query":                      "GitHub issue search syntax; one phrase works best.",
	"Monitor.repo":                       "owner/name of a repo to watch.",
	"Monitor.events":                     "For github_repo: star, fork, issue, discussion (discussion needs ASTROLABE_GITHUB_TOKEN).",
	"Monitor.keywords":                   "Phrases to look for.",
	"Monitor.sources":                    "hn, stackoverflow, rss.",
	"Monitor.feeds":                      "RSS/Atom feed URLs.",
	"Monitor.site":                       "For netlify_forms: the Netlify site id or name (like mysite.netlify.app).",
	"Monitor.form":                       "For netlify_forms: the form's name (the name attribute of the HTML form).",
	"Monitor.fields":                     "Form field names by Astrolabe key (email, name, company, domain, role, source, campaign, message). Common names are matched when a key is not set.",
	"Monitor.account":                    "Open a pipeline account for the requester's company in the first stage, with the next action \"Reply to the request\". Needs a pipeline.",
	"Monitor.respond":                    "How soon to reply, e.g. 1d (default); the account's next action is due then.",
}

var enums = map[string][]string{
	"Product.audience":         {"b2b", "b2c"},
	"AutoApprove.cold":         {"none", "follow_ups", "all", "manual", "first_step", "auto"},
	"AutoApprove.users":        {"none", "follow_ups", "all", "manual", "first_step", "auto"},
	"Sequence.kind":            {"cold", "users", "outbound", "lifecycle"},
	"Sequence.experiment_unit": {"person", "company"},
	"Step.channel":             {"email", "linkedin_task", "x_task", "task", "whatsapp", "push", "in_app", "webhook"},
	"When.min_tier":            {"A", "B", "C", "D"},
	"Monitor.type":             {"github_search", "github_repo", "community", "github_release", "netlify_forms", "form_webhook"},
	"Updates.via":              {"smtp", "resend"},
	"Action.updates":           {"subscribe"},
}

// schemaAliases adds sales-vocabulary keys: struct.alias -> canonical key.
var schemaAliases = map[string]map[string]string{
	"Product":     {"icp": "target", "autonomy": "auto_approve"},
	"Sender":      {"outbound": "cold", "lifecycle": "users"},
	"AutoApprove": {"outbound": "cold", "lifecycle": "users", "content": "cold"},
	"Limits":      {"outbound_per_day": "cold_per_day"},
}

var datePatternJS = `^\d{4}-\d{2}-\d{2}$`

var durationPatternJS = `^\d+[dhm]$`

var patterns = map[string]string{
	"Step.after": durationPatternJS, "Play.delay": durationPatternJS, "Offer.window": durationPatternJS,
	"Monitor.every": durationPatternJS, "When.not_contacted_within": durationPatternJS,
	"Campaign.start": datePatternJS, "Campaign.end": datePatternJS, "Budget.currency": `^[A-Z]{3}$`,
	"Campaign.code": `^[a-z0-9][a-z0-9_-]{1,23}$`,
	"Product.id":    `^[a-z0-9][a-z0-9-]*$`, "Pipeline.stuck_after": durationPatternJS,
	"Updates.confirm_within": durationPatternJS,
}

func schemaFor(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Int, reflect.Int64:
		if t == reflect.TypeOf(Money(0)) {
			return map[string]any{"type": "number", "minimum": 0}
		}
		return map[string]any{"type": "integer"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaFor(t.Elem())}
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
		"campaign.schema.json":    schemaFor(reflect.TypeOf(Campaign{})),
	}
	docs["product.schema.json"]["required"] = []string{"id", "name", "url", "one_liner", "audience", "funnel", "activation", "offer", "claims", "voice", "sender", "limits"}
	docs["campaign.schema.json"]["required"] = []string{"code", "label", "goal", "audience", "channels", "offer", "landing", "success_event", "budget", "start"}
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

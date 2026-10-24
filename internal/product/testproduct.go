package product

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProduct writes a valid "demo" product folder (daily limit 3, weekly
// touch cap 2, sequence "intro": email, email follow-up, LinkedIn task) and
// loads it.
func TestProduct(t testing.TB) *Product {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "demo")
	os.MkdirAll(filepath.Join(dir, "sequences"), 0o755)
	os.WriteFile(filepath.Join(dir, "product.yaml"), []byte(testProductYAML), 0o644)
	os.WriteFile(filepath.Join(dir, "claims.md"), []byte("- It does one thing"), 0o644)
	os.WriteFile(filepath.Join(dir, "voice.md"), []byte("Plain."), 0o644)
	os.WriteFile(filepath.Join(dir, "sequences", "intro.yaml"), []byte(testSequenceYAML), 0o644)
	os.WriteFile(filepath.Join(dir, "sequences", "nudge.yaml"), []byte("steps:\n  - {channel: email, after: 0d, goal: nudge, subject: Hi, body: Hello}\nstop_on: [replied]\n"), 0o644)
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const testProductYAML = `id: demo
name: Demo
url: https://demo.example
one_liner: Does one thing well.
audience: b2b
target: {include: [small teams]}
funnel: [reached, replied, installed, activated, paid]
activation: activated
offer: {promise: "Try it on one thing", cta: "Send one thing", destination: "https://demo.example/start", success_event: activated, window: 14d}
claims: {allowed: claims.md, never: ["guaranteed"]}
voice: voice.md
sender: {cold: me@demo-mail.example, users: hello@demo.example}
auto_approve: {cold: none, users: none}
limits: {cold_per_day: 3, touches_per_person_per_week: 2}
`

const testSequenceYAML = `steps:
  - {channel: email, after: 0d, goal: intro, subject: "Hi {{.FirstName}}", body: "Hello {{.FirstName}} at {{.Company}}"}
  - {channel: email, after: 3d, goal: follow up}
  - {channel: linkedin_task, after: 5d, goal: connect}
stop_on: [replied, unsubscribed, bounced, installed]
`

const testCampaignYAML = `code: launch
label: Launch week
goal: Tell people who run CI that Demo exists.
audience: Developers who maintain CI pipelines.
channels: [hn, x]
offer: Try Demo on one pipeline.
landing: https://demo.example/c/launch
success_event: installed
budget: {cap: 200.50, currency: USD}
start: "2026-12-01"
end: "2026-12-08"
stop_rule:
  text: Stop when half the budget has produced zero installs.
  when: {spend_pct: 50, max_events: 0}
gate:
  - Landing page is live
  - Claims checked
tasks:
  - {id: show-hn, title: Post Show HN, channel: hn, day: 0, body: "Show HN: Demo {{link}}"}
creatives:
  - id: card
    template: creative/card.html
    sizes: [1200x630, 1080x1080]
    formats: [png, pdf]
    copy: {headline: Do one thing}
    alt: A card that says do one thing.
`

const testCreativeHTML = `<!doctype html><html><head><link rel="stylesheet" href="creative.css"></head>
<body style="width:{{width}}px;height:{{height}}px"><h1>{{headline}}</h1><img alt="{{alt}}"></body></html>`

// WriteTestCampaign adds the fixture campaign to a product folder.
func WriteTestCampaign(t testing.TB, dir string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "campaigns"), 0o755)
	os.MkdirAll(filepath.Join(dir, "creative"), 0o755)
	os.WriteFile(filepath.Join(dir, "campaigns", "launch-week.yaml"), []byte(testCampaignYAML), 0o644)
	os.WriteFile(filepath.Join(dir, "creative", "card.html"), []byte(testCreativeHTML), 0o644)
	os.WriteFile(filepath.Join(dir, "creative", "creative.css"), []byte("h1{font-size:40px}"), 0o644)
}

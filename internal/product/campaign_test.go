package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestCampaignLoads(t *testing.T) {
	p := TestProduct(t)
	if len(p.Campaigns) != 0 {
		t.Fatal("a product without campaigns/ is fine")
	}
	before := p.Hash
	WriteTestCampaign(t, p.Dir)
	p, err := Load(p.Dir)
	if err != nil {
		t.Fatal(err)
	}
	c := p.Campaign("launch")
	if c == nil || c.ID != "launch-week" || c.Budget.Cap != 20050 || c.Budget.Cap.String() != "200.50" {
		t.Fatalf("%+v", c)
	}
	cr := c.Creative("card")
	if cr == nil || cr.TemplateText == "" || string(cr.Assets["creative.css"]) != "h1{font-size:40px}" {
		t.Fatalf("template and beside-assets must be loaded: %+v", cr)
	}
	if p.Hash == before {
		t.Error("campaigns must be part of the folder hash, or edits never sync")
	}
	h := p.Hash
	os.WriteFile(filepath.Join(p.Dir, "creative", "creative.css"), []byte("h1{font-size:41px}"), 0o644)
	if p2, _ := Load(p.Dir); p2.Hash == h {
		t.Error("changing a template asset must change the hash")
	}
}

func TestCampaignValidation(t *testing.T) {
	p := TestProduct(t)
	WriteTestCampaign(t, p.Dir)
	bad := strings.NewReplacer(
		"code: launch", "code: Launch Week",
		"landing: https://demo.example/c/launch", "landing: http://x",
		`end: "2026-12-08"`, `end: "2026-11-01"`,
		"cap: 200.50, currency: USD", "cap: 200, currency: usd",
		"spend_pct: 50", "spend_pct: 150",
		"sizes: [1200x630, 1080x1080]", "sizes: [50x50]",
		"copy: {headline: Do one thing}", "copy: {extra: x}",
		"alt: A card that says do one thing.", "alt: ''",
	).Replace(testCampaignYAML)
	os.WriteFile(filepath.Join(p.Dir, "campaigns", "launch-week.yaml"), []byte(bad), 0o644)
	_, err := Load(p.Dir)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"code \"Launch Week\"", "landing must be an https", "end must not be before start",
		"spend_pct must be 1-100", "between 100 and 4000", "no \"headline\" field", "not used by the template", "alt text is required", "currency"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	os.WriteFile(filepath.Join(p.Dir, "campaigns", "launch-week.yaml"), []byte(strings.Replace(testCampaignYAML, "cap: 200.50", "cap: 1.234", 1)), 0o644)
	if _, err := Load(p.Dir); err == nil || !strings.Contains(err.Error(), "at most 2 decimals") {
		t.Fatalf("fractions of a cent must be rejected: %v", err)
	}
	os.WriteFile(filepath.Join(p.Dir, "campaigns", "launch-week.yaml"), []byte(testCampaignYAML+"typo: 1\n"), 0o644)
	if _, err := Load(p.Dir); err == nil || !strings.Contains(err.Error(), "typo") {
		t.Fatalf("unknown fields must be rejected: %v", err)
	}
}

func TestCampaignSchema(t *testing.T) {
	if err := validateAgainst(t, "campaign.schema.json", testCampaignYAML); err != nil {
		t.Errorf("fixture: %v", err)
	}
	if err := validateAgainst(t, "campaign.schema.json", testCampaignYAML+"lable: x\n"); err == nil {
		t.Error("a misspelled key must fail validation")
	}
	if err := validateAgainst(t, "campaign.schema.json", strings.Replace(testCampaignYAML, `start: "2026-12-01"`, "start: tomorrow", 1)); err == nil {
		t.Error("a bad date must fail validation")
	}
}

func TestParseMoney(t *testing.T) {
	for in, want := range map[string]Money{"200": 20000, "200.5": 20050, "1,200.05": 120005, "0": 0, ".5": 50, "-3.10": -310} {
		if got, err := ParseMoney(in); err != nil || got != want {
			t.Errorf("%q = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "1.234", "1.2.3"} {
		if _, err := ParseMoney(in); err == nil {
			t.Errorf("%q should fail", in)
		}
	}
}

package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidationCatchesMistakes(t *testing.T) {
	p := TestProduct(t)
	write := func(name, s string) { os.WriteFile(filepath.Join(p.Dir, name), []byte(s), 0o644) }
	write("product.yaml", strings.NewReplacer(
		"activation: activated", "activation: signed_up",
		"success_event: activated", "success_event: nope",
		"cold: me@demo-mail.example", "cold: Me <me@demo-mail.example>",
	).Replace(testProductYAML))
	write("claims.md", "STUB: fill me")
	write("sequences/bad.yaml", "steps:\n  - {channel: sms, after: 2d, goal: x}\n  - {channel: email, after: 1d, goal: y, subject: new, body: 'Hi {{.Firstname}}'}\nstop_on: [installed, nope]\n")

	_, err := Load(p.Dir)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{
		`activation "signed_up"`, `offer.success_event "nope"`, "sender.cold", "still a stub",
		`channel "sms" is not available`, "earlier than the previous step", "cannot set a subject",
		"stop_on must include replied", `stop_on "nope"`, "body template",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}

	write("product.yaml", testProductYAML+"typo_field: 1\n")
	if _, err := Load(p.Dir); err == nil || !strings.Contains(err.Error(), "typo_field") {
		t.Fatalf("unknown fields must be rejected: %v", err)
	}
}

func TestPlayValidation(t *testing.T) {
	p := TestProduct(t)
	os.WriteFile(filepath.Join(p.Dir, "automations.yaml"), []byte(`
- id: ok
  trigger: {signal: github.issue_pain}
  when: {min_fit: 50, not_contacted_within: 30d}
  action: {sequence: intro}
- id: bad
  trigger: {signal: x, stage_stuck: {stage: nope, for: 3x}}
  when: {min_tier: Z, not_stage: gone}
  action: {sequence: missing, task: also}
`), 0o644)
	_, err := Load(p.Dir)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"exactly one of signal, stage, stage_stuck", `stage_stuck stage "nope"`, "stage_stuck.for",
		"exactly one of sequence or task", `sequence "missing"`, "min_tier", `not_stage "gone"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "#1 (ok)") {
		t.Errorf("valid play reported: %v", err)
	}
}

func TestScaffold(t *testing.T) {
	root := t.TempDir()
	spec := Spec{ID: "notely", Name: "Notely", URL: "https://notely.example", OneLiner: "Notes that file themselves.",
		Audience: "b2c", TargetInclude: []string{"students"}, Funnel: []string{"reached", "replied", "signup", "activated", "paid"},
		Activation: "activated", Promise: "Organise one course", CTA: "Try it on one course", Destination: "https://notely.example/start",
		SenderCold: "me@trynotely.example", SenderUsers: "hi@notely.example"}
	dir, err := Scaffold(root, spec)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Load(dir)
	if err == nil || !strings.Contains(err.Error(), "still a stub") {
		t.Fatalf("stub claims must block loading: %v", err)
	}
	if _, err := Scaffold(root, spec); err == nil {
		t.Fatal("overwrote an existing product")
	}
	spec.ID, spec.Claims, spec.Voice = "notely2", "- Sorts notes by course (demo: https://notely.example/demo)", "Friendly, short."
	dir, err = Scaffold(root, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("scaffold with claims should load: %v", err)
	}
}

func TestSalesVocabularyAliases(t *testing.T) {
	canonical := TestProduct(t)
	p := TestProduct(t)
	sales := strings.NewReplacer(
		"target: {include: [small teams]}", "icp: {include: [small teams]}",
		"sender: {cold: me@demo-mail.example, users: hello@demo.example}", "sender: {outbound: me@demo-mail.example, lifecycle: hello@demo.example}",
		"auto_approve: {cold: none, users: none}", "autonomy: {outbound: manual, lifecycle: first_step, content: manual}",
		"limits: {cold_per_day: 3,", "limits: {outbound_per_day: 3,",
	).Replace(testProductYAML)
	os.WriteFile(filepath.Join(p.Dir, "product.yaml"), []byte(sales), 0o644)
	os.WriteFile(filepath.Join(p.Dir, "sequences", "intro.yaml"), []byte("kind: outbound\n"+testSequenceYAML), 0o644)
	os.WriteFile(filepath.Join(p.Dir, "plays.yaml"), []byte("- {id: x, trigger: {signal: s}, action: {sequence: intro}}\n"), 0o644)
	got, err := Load(p.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Target.Include[0] != canonical.Target.Include[0] || got.Sender != canonical.Sender || got.Limits != canonical.Limits ||
		got.AutoApprove.Cold != "none" || got.AutoApprove.Users != "follow_ups" || got.Sequences["intro"].Kind != "cold" || len(got.Plays) != 1 {
		t.Fatalf("aliases not applied: %+v", got)
	}

	// Both names for one setting is an error.
	os.WriteFile(filepath.Join(p.Dir, "product.yaml"), []byte(sales+"target: {include: [x]}\n"), 0o644)
	if _, err := Load(p.Dir); err == nil || !strings.Contains(err.Error(), "icp and target") {
		t.Fatalf("expected a conflict error, got %v", err)
	}
	os.WriteFile(filepath.Join(p.Dir, "product.yaml"), []byte(sales), 0o644)
	os.WriteFile(filepath.Join(p.Dir, "automations.yaml"), []byte("[]\n"), 0o644)
	if _, err := Load(p.Dir); err == nil || !strings.Contains(err.Error(), "keep one") {
		t.Fatalf("expected plays.yaml/automations.yaml conflict, got %v", err)
	}
}

func TestAfterDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"0d": 0, "3d": 72 * time.Hour, "12h": 12 * time.Hour, "30m": 30 * time.Minute, "15m": 15 * time.Minute} {
		got, err := AfterDuration(in)
		if err != nil || got != want {
			t.Errorf("AfterDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := AfterDuration("3w"); err == nil {
		t.Error("3w should be rejected")
	}
}

func TestTaskChannelAndDiscussionEventValidate(t *testing.T) {
	p := TestProduct(t)
	write := func(name, s string) { os.WriteFile(filepath.Join(p.Dir, name), []byte(s), 0o644) }
	write("sequences/checkin.yaml", "steps:\n  - {channel: task, after: 0d, goal: kickoff, body: 'Hi {{.FirstName}}'}\n  - {channel: task, after: 7d, goal: check in}\nstop_on: [replied]\n")
	write("sequences/lifecycle.yaml", "kind: users\nsteps:\n  - {channel: task, after: 1d, goal: nudge}\nstop_on: [replied]\n")
	write("monitors.yaml", "- {id: repo, type: github_repo, repo: me/tool, events: [star, fork, issue, discussion], signal: github.repo, strength: 10, every: 1h}\n")
	if _, err := Load(p.Dir); err != nil {
		t.Fatalf("task steps and the discussion event should validate: %v", err)
	}
	write("sequences/checkin.yaml", "steps:\n  - {channel: task, after: 2d, goal: a}\n  - {channel: task, after: 1d, goal: b, subject: nope}\nstop_on: [replied]\n")
	if _, err := Load(p.Dir); err == nil || !strings.Contains(err.Error(), "earlier than the previous step") {
		t.Fatalf("task steps follow the same ordering rules: %v", err)
	}
	// Only sequences that never message anyone may keep going after a reply.
	write("sequences/checkin.yaml", "steps:\n  - {channel: task, after: 7d, goal: a}\n  - {channel: task, after: 14d, goal: b}\nstop_on: [unsubscribed]\n")
	if _, err := Load(p.Dir); err != nil {
		t.Fatalf("a task-only sequence may leave replied out of stop_on: %v", err)
	}
	write("sequences/checkin.yaml", "steps:\n  - {channel: task, after: 7d, goal: a}\n  - {channel: email, after: 14d, goal: b}\nstop_on: [unsubscribed]\n")
	if _, err := Load(p.Dir); err == nil || !strings.Contains(err.Error(), "stop_on must include replied") {
		t.Fatalf("a sequence with an email step must stop on replied: %v", err)
	}
}

func TestFormMonitorsValidate(t *testing.T) {
	p := TestProduct(t)
	write := func(s string) { os.WriteFile(filepath.Join(p.Dir, "monitors.yaml"), []byte(s), 0o644) }
	write("- {id: req, type: netlify_forms, site: me.netlify.app, form: request, signal: form.request, strength: 40, every: 1h, fields: {source: heard_from}}\n" +
		"- {id: hook, type: form_webhook, signal: form.updates, strength: 5, every: 1h}\n")
	if _, err := Load(p.Dir); err != nil {
		t.Fatalf("valid form monitors: %v", err)
	}
	for name, c := range map[string]struct{ yaml, want string }{
		"no form":     {"- {id: req, type: netlify_forms, site: s, signal: form.request, strength: 40, every: 1h}", "form (the form's name)"},
		"bad key":     {"- {id: h, type: form_webhook, signal: form.x, strength: 5, every: 1h, fields: {phone: tel}}", `fields key "phone"`},
		"no pipeline": {"- {id: h, type: form_webhook, signal: form.x, strength: 5, every: 1h, account: true}", "needs a pipeline"},
		"bad respond": {"- {id: h, type: form_webhook, signal: form.x, strength: 5, every: 1h, respond: soon}", "respond"},
	} {
		write(c.yaml + "\n")
		if _, err := Load(p.Dir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

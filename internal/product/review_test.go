package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewConfigDefaultsAndValidation(t *testing.T) {
	p := TestProduct(t)
	s := p.ReviewSettings()
	if len(s.Questions) != 5 || s.Weekday != time.Friday || s.Targets["request"] != 2*time.Hour || s.Targets["reply"] != 2*time.Hour ||
		s.Targets["issue"] != 24*time.Hour || s.Targets["discussion"] != 24*time.Hour {
		t.Fatalf("defaults: %+v", s)
	}

	load := func(extra string) (*Product, error) {
		raw, _ := os.ReadFile(filepath.Join(p.Dir, "product.yaml"))
		os.WriteFile(filepath.Join(p.Dir, "product.yaml"), append(raw, extra...), 0o644)
		defer os.WriteFile(filepath.Join(p.Dir, "product.yaml"), raw, 0o644)
		return Load(p.Dir)
	}
	got, err := load("review:\n  questions: [\"What changed?\"]\n  weekday: Monday\n  targets: {requests: 30m, issues: 2d}\n")
	if err != nil {
		t.Fatal(err)
	}
	s = got.ReviewSettings()
	if len(s.Questions) != 1 || s.Weekday != time.Monday || s.Targets["request"] != 30*time.Minute || s.Targets["issue"] != 48*time.Hour || s.Targets["reply"] != 2*time.Hour {
		t.Errorf("custom: %+v", s)
	}
	if err := validateAgainst(t, "product.schema.json", testProductYAML+"review:\n  questions: [a]\n  weekday: friday\n  targets: {requests: 2h}\n"); err != nil {
		t.Errorf("schema rejects a valid review: %v", err)
	}
	for yaml, want := range map[string]string{
		"review: {weekday: someday}\n":         "review.weekday",
		"review: {questions: [\"\"]}\n":        "questions[0] is empty",
		"review: {questions: [a, a]}\n":        "listed twice",
		"review: {targets: {replies: soon}}\n": "review.targets.replies",
		"review: {targets: {requests: 0h}}\n":  "more than zero",
		"review: {surprise: true}\n":           "surprise",
	} {
		if _, err := load(yaml); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", yaml, want, err)
		}
	}
	if err := validateAgainst(t, "product.schema.json", testProductYAML+"review: {weekday: someday}\n"); err == nil {
		t.Error("schema accepts a bad weekday")
	}
}

package product

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"
)

// ValidateAgainst checks YAML text against one of the generated schemas.
func validateAgainst(t *testing.T, schemaFile, text string) error {
	t.Helper()
	var s jsonschema.Schema
	if err := json.Unmarshal(Schemas()[schemaFile], &s); err != nil {
		t.Fatal(err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := yaml.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v) // YAML ints -> JSON numbers
	var inst any
	json.Unmarshal(b, &inst)
	return rs.Validate(inst)
}

func TestSchemasAreCommittedAndCurrent(t *testing.T) {
	for name, want := range Schemas() {
		got, err := os.ReadFile(filepath.Join("..", "..", "schemas", name))
		if err != nil || string(got) != string(want) {
			t.Errorf("schemas/%s is missing or stale; run: go run ./cmd/astrolabe schema schemas", name)
		}
	}
}

func TestFixturesValidateAgainstSchemas(t *testing.T) {
	if err := validateAgainst(t, "product.schema.json", testProductYAML); err != nil {
		t.Errorf("product: %v", err)
	}
	if err := validateAgainst(t, "sequence.schema.json", testSequenceYAML); err != nil {
		t.Errorf("sequence: %v", err)
	}
	sales := strings.NewReplacer("target:", "icp:", "auto_approve: {cold: none, users: none}", "autonomy: {outbound: manual, lifecycle: auto}").Replace(testProductYAML)
	if err := validateAgainst(t, "product.schema.json", sales); err != nil {
		t.Errorf("sales vocabulary should validate: %v", err)
	}
	if err := validateAgainst(t, "product.schema.json", testProductYAML+"fitt: []\n"); err == nil {
		t.Error("a misspelled key must fail validation")
	}
	if err := validateAgainst(t, "automations.schema.json", "- {id: a, trigger: {signal: s}, when: {min_fit: 50}, action: {sequence: intro}, delay: 2x}\n"); err == nil {
		t.Error("a bad duration must fail validation")
	}
}

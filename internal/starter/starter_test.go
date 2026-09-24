package starter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

func validate(t *testing.T, schemaFile, file string) {
	t.Helper()
	var s jsonschema.Schema
	json.Unmarshal(product.Schemas()[schemaFile], &s)
	rs, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(file)
	var v any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	b, _ := json.Marshal(v)
	var inst any
	json.Unmarshal(b, &inst)
	if err := rs.Validate(inst); err != nil {
		t.Errorf("%s does not match %s: %v", file, schemaFile, err)
	}
}

func TestEveryTemplate(t *testing.T) {
	for _, name := range TemplateNames {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			files, err := Init(Options{Dir: dir, Template: name, Name: "Hook Check"})
			if err != nil {
				t.Fatal(err)
			}
			id := "hook-check"
			if name == "demo" {
				id = "pipewrench-demo"
			}
			pdir := filepath.Join(dir, "products", id)
			// Every YAML file points at its schema and validates against it.
			filepath.Walk(pdir, func(p string, info os.FileInfo, err error) error {
				rel, _ := filepath.Rel(pdir, p)
				if s := schemaFor(filepath.ToSlash(rel)); s != "" {
					first, _ := os.ReadFile(p)
					if !strings.HasPrefix(string(first), "# yaml-language-server: $schema=") {
						t.Errorf("%s lacks a schema comment", rel)
					}
					ref := strings.TrimPrefix(strings.SplitN(string(first), "\n", 2)[0], "# yaml-language-server: $schema=")
					if _, err := os.Stat(filepath.Join(filepath.Dir(p), ref)); err != nil {
						t.Errorf("%s: schema path %s doesn't resolve", rel, ref)
					}
					validate(t, s, p)
				}
				return nil
			})
			_, err = product.Load(pdir)
			if name == "demo" {
				if err != nil {
					t.Fatalf("demo must load as is: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "still a stub") {
				t.Fatalf("real templates must wait for claims/voice: %v", err)
			} else {
				for _, line := range strings.Split(err.Error(), "\n")[1:] {
					if !strings.Contains(line, "still a stub") {
						t.Errorf("unexpected problem besides the stubs: %s", line)
					}
				}
			}
			for _, want := range []string{".env", "compose.yaml", "schemas/product.schema.json"} {
				found := false
				for _, f := range files {
					found = found || filepath.ToSlash(f) == want
				}
				if !found {
					t.Errorf("%s not written", want)
				}
			}
			// Running again never overwrites.
			env, _ := os.ReadFile(filepath.Join(dir, ".env"))
			if _, err := Init(Options{Dir: dir, Template: name, Name: "Hook Check"}); err == nil {
				t.Error("second init of the same product must fail")
			}
			other := "devtool"
			if name == "devtool" {
				other = "saas"
			}
			if _, err := Init(Options{Dir: dir, Template: other, ID: "second"}); err != nil {
				t.Fatal(err)
			}
			env2, _ := os.ReadFile(filepath.Join(dir, ".env"))
			if string(env) != string(env2) {
				t.Error(".env was overwritten")
			}
		})
	}
}

func TestMessagePlaceholdersSurvive(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(Options{Dir: dir, Template: "demo"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "products", "pipewrench-demo", "sequences", "intro.yaml"))
	if !strings.Contains(string(b), "{{.FirstName}}") || !strings.Contains(string(b), "{{.Link}}") {
		t.Fatalf("message templates were rendered by init:\n%s", b)
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if !strings.Contains(string(env), "ASTROLABE_MODE=sandbox") {
		t.Fatal("demo workspace should start in sandbox mode")
	}
	info, _ := os.Stat(filepath.Join(dir, ".env"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode %v", info.Mode().Perm())
	}
}

// examples/ in the repo is the demo template's output; keep them in sync.
func TestExamplesMatchDemoTemplate(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(Options{Dir: dir, Template: "demo"}); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"products/pipewrench-demo", "schemas"} {
		filepath.Walk(filepath.Join(dir, sub), func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			want, _ := os.ReadFile(p)
			got, err := os.ReadFile(filepath.Join("..", "..", "examples", rel))
			if err != nil || string(got) != string(want) {
				t.Errorf("examples/%s is missing or stale; regenerate with: rm -rf examples && go run ./cmd/astrolabe init examples -template demo, then delete examples/.env, compose.yaml and .gitignore", rel)
			}
			return nil
		})
	}
}

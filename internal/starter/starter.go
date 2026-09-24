// Package starter implements `astrolabe init`: a workspace with a product
// folder from a starter template, JSON schemas for editor autocomplete, and
// a compose file and .env to run it. It never overwrites anything.
package starter

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/astrolabe-gtm/astrolabe/internal/product"
)

//go:embed all:templates
var templates embed.FS

// Templates, with what each is for.
var Templates = map[string]string{
	"demo":    "a fictional product with example people, to try everything in sandbox mode",
	"devtool": "a developer tool: GitHub/HN signals, cold email to engineers, activation help",
	"saas":    "a B2B SaaS: trials, activation, trial-ending and win-back",
	"app":     "a consumer app: product events, push/in-app and email to users",
}

// TemplateNames in display order.
var TemplateNames = []string{"demo", "devtool", "saas", "app"}

type Options struct {
	Dir         string // workspace directory
	Template    string
	ID          string
	Name        string
	URL         string
	OneLiner    string
	SenderCold  string
	SenderUsers string
}

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (o *Options) defaults() error {
	if _, ok := Templates[o.Template]; !ok {
		return fmt.Errorf("unknown template %q (have %s)", o.Template, strings.Join(TemplateNames, ", "))
	}
	if o.Template == "demo" && o.ID == "" {
		o.ID = "pipewrench-demo"
	}
	if o.ID == "" && o.Name != "" {
		o.ID = strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(o.Name), "-"), "-")
	}
	if !idRE.MatchString(o.ID) {
		return fmt.Errorf("product id %q must be lowercase letters, digits and dashes (use -id or -name)", o.ID)
	}
	if o.Name == "" {
		o.Name = strings.ToUpper(o.ID[:1]) + o.ID[1:]
	}
	if o.URL == "" {
		o.URL = "https://" + o.ID + ".example"
	}
	if !strings.HasPrefix(o.URL, "https://") {
		return fmt.Errorf("url must start with https://")
	}
	o.URL = strings.TrimRight(o.URL, "/")
	if o.OneLiner == "" {
		o.OneLiner = "What " + o.Name + " does, in one sentence (edit me)."
	}
	domain := strings.TrimPrefix(o.URL, "https://")
	if o.SenderUsers == "" {
		o.SenderUsers = "hello@" + domain
	}
	if o.SenderCold == "" {
		o.SenderCold = "you@try-" + domain
	}
	return nil
}

// schemaFor returns the schema a file in the product folder should use.
func schemaFor(rel string) string {
	switch {
	case rel == "product.yaml":
		return "product.schema.json"
	case rel == "automations.yaml" || rel == "plays.yaml":
		return "automations.schema.json"
	case rel == "monitors.yaml":
		return "monitors.schema.json"
	case strings.HasPrefix(rel, "sequences/") && strings.HasSuffix(rel, ".yaml"):
		return "sequence.schema.json"
	}
	return ""
}

// Init writes the workspace and returns the files it created.
func Init(o Options) ([]string, error) {
	if err := o.defaults(); err != nil {
		return nil, err
	}
	var written []string
	write := func(rel string, data []byte, mode os.FileMode) error {
		p := filepath.Join(o.Dir, rel)
		if _, err := os.Stat(p); err == nil {
			return nil // never overwrite
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, mode); err != nil {
			return err
		}
		written = append(written, rel)
		return nil
	}

	productDir := filepath.Join("products", o.ID)
	if _, err := os.Stat(filepath.Join(o.Dir, productDir)); err == nil {
		return nil, fmt.Errorf("%s already exists; choose another -id", productDir)
	}
	data := map[string]string{"ID": o.ID, "Name": o.Name, "URL": o.URL, "OneLiner": o.OneLiner,
		"SenderCold": o.SenderCold, "SenderUsers": o.SenderUsers, "EnvID": strings.ToUpper(strings.ReplaceAll(o.ID, "-", "_"))}
	root := "templates/" + o.Template
	err := fs.WalkDir(templates, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, root+"/")
		raw, err := templates.ReadFile(p)
		if err != nil {
			return err
		}
		t, err := template.New(rel).Delims("[[", "]]").Option("missingkey=error").Parse(string(raw))
		if err != nil {
			return err
		}
		var b bytes.Buffer
		if err := t.Execute(&b, data); err != nil {
			return err
		}
		out := b.Bytes()
		if s := schemaFor(rel); s != "" {
			depth := strings.Repeat("../", strings.Count(path.Join(productDir, rel), "/"))
			out = append([]byte(product.SchemaComment(s, depth+"schemas/")), out...)
		}
		return write(filepath.Join(productDir, rel), out, 0o644)
	})
	if err != nil {
		return written, err
	}

	for name, b := range product.Schemas() {
		if err := write(filepath.Join("schemas", name), b, 0o644); err != nil {
			return written, err
		}
	}
	mode := "live"
	if o.Template == "demo" {
		mode = "sandbox"
	}
	if err := write(".env", []byte(envFile(mode)), 0o600); err != nil {
		return written, err
	}
	if err := write("compose.yaml", []byte(ComposeFile), 0o644); err != nil {
		return written, err
	}
	if err := write(".gitignore", []byte(".env\nsecrets/\n"), 0o644); err != nil {
		return written, err
	}
	return written, nil
}

func randomPassword() string {
	b := make([]byte, 18)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func envFile(mode string) string {
	// The uid/gid init runs as (docker run --user) is the one compose runs
	// the server as, so files it writes in ./products belong to you.
	ids := fmt.Sprintf("ASTROLABE_UID=%d\nASTROLABE_GID=%d\n", os.Getuid(), os.Getgid())
	return `# Astrolabe settings. Docs: https://github.com/astrolabe-gtm/astrolabe
` + ids + `
# sandbox: every message goes to a local outbox (no accounts needed).
# live: messages are really sent. A database stays in the mode it started in.
ASTROLABE_MODE=` + mode + `
ASTROLABE_PASSWORD=` + randomPassword() + `

# Optional, add as you need them:
# ASTROLABE_PUBLIC_URL=https://gtm.yourdomain.com      # tracked links and webhooks
# ASTROLABE_ANTHROPIC_API_KEY=                         # AI research, drafts, reply sorting
# ASTROLABE_GITHUB_TOKEN=                              # GitHub monitors (strongly recommended)
# ASTROLABE_DIGEST_TO=you@yourdomain.com               # weekly digest
# ASTROLABE_DIGEST_FROM=you@yourdomain.com
# TZ=UTC
`
}

// ComposeFile runs Astrolabe and Postgres from the published image.
const ComposeFile = `# docker compose up -d, then open http://localhost:8080
services:
  postgres:
    image: postgres:17
    environment:
      POSTGRES_USER: astrolabe
      POSTGRES_PASSWORD: astrolabe
      POSTGRES_DB: astrolabe
    volumes: [pgdata:/var/lib/postgresql/data]
    healthcheck:
      test: ["CMD", "pg_isready", "-U", "astrolabe"]
      interval: 2s
      retries: 30
    restart: unless-stopped

  astrolabe:
    image: ghcr.io/astrolabe-gtm/astrolabe:latest
    user: "${ASTROLABE_UID:-10001}:${ASTROLABE_GID:-10001}"
    env_file: .env
    environment:
      ASTROLABE_DATABASE_URL: postgres://astrolabe:astrolabe@postgres:5432/astrolabe?sslmode=disable
    ports: ["8080:8080"]
    volumes:
      - ./products:/data/products
      - secrets:/data/secrets
    depends_on:
      postgres: {condition: service_healthy}
    restart: unless-stopped

volumes:
  pgdata:
  secrets:
`

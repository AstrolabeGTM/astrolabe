package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AstrolabeGTM/astrolabe/internal/mail"
	"github.com/AstrolabeGTM/astrolabe/internal/sandbox"
	"github.com/AstrolabeGTM/astrolabe/internal/starter"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

func find(checks []Check, area string, status Status, contains string) bool {
	for _, c := range checks {
		if c.Area == area && c.Status == status && strings.Contains(c.Detail+" "+c.Fix, contains) {
			return true
		}
	}
	return false
}

func fails(checks []Check) []string {
	var out []string
	for _, c := range checks {
		if c.Status == Fail {
			out = append(out, c.Area+": "+c.Detail)
		}
	}
	return out
}

func TestDoctor(t *testing.T) {
	ctx := context.Background()
	pool, url := store.TestDBURL(t)
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }

	// A fresh demo workspace in sandbox mode has nothing blocking.
	demo := t.TempDir()
	starter.Init(starter.Options{Dir: demo, Template: "demo"})
	in := Input{DatabaseURL: url, ProductsDir: filepath.Join(demo, "products"), SecretsDir: t.TempDir(), Mode: "sandbox",
		Password: "long-enough-password", Getenv: getenv}
	if f := fails(Run(ctx, in)); len(f) > 0 {
		t.Fatalf("demo in sandbox should pass: %v", f)
	}
	// Listening on this machine only, no password is needed.
	in.Password, in.LocalOnly = "", true
	if !find(Run(ctx, in), "password", OK, "none needed") {
		t.Fatal("no password on 127.0.0.1 should pass")
	}
	in.Password, in.LocalOnly = "long-enough-password", false

	// The same demo in live mode fails, and so does a mode mismatch.
	sandbox.EnsureMode(ctx, pool, "sandbox")
	in.Mode = "live"
	checks := Run(ctx, in)
	if !find(checks, "products", Fail, "demo data") || !find(checks, "mode", Fail, "astrolabe mode set live") {
		t.Fatalf("expected demo and mode failures: %v", fails(checks))
	}
	sandbox.SetMode(ctx, pool, "live")

	// A real product: stub files, then a missing mailbox, then connected.
	ws := t.TempDir()
	starter.Init(starter.Options{Dir: ws, Template: "devtool", Name: "Hook Check", SenderCold: "me@try-hook.example"})
	in.ProductsDir = filepath.Join(ws, "products")
	if !find(Run(ctx, in), "products", Fail, "still a stub") {
		t.Fatal("stub claims should fail")
	}
	pdir := filepath.Join(ws, "products", "hook-check")
	os.WriteFile(filepath.Join(pdir, "claims.md"), []byte("- It works"), 0o644)
	os.WriteFile(filepath.Join(pdir, "voice.md"), []byte("Plain."), 0o644)
	checks = Run(ctx, in)
	if !find(checks, "mailbox", Fail, "astrolabe mailbox add me@try-hook.example") {
		t.Fatalf("missing mailbox should fail with the fix: %v", fails(checks))
	}
	if !find(checks, "github", Warn, "ASTROLABE_GITHUB_TOKEN") || !find(checks, "public url", Warn, "") {
		t.Fatal("expected warnings for GitHub token and public URL")
	}
	if !find(checks, "mailbox", Fail, "hello@hook-check.example") {
		t.Fatal("the users sequence sends email too, so its sender must be connected")
	}
	ms := &mail.Store{Dir: filepath.Join(in.SecretsDir, "mail")}
	ms.Save(&mail.Account{Address: "me@try-hook.example", Provider: "fastmail", Password: "x"})
	ms.Save(&mail.Account{Address: "hello@hook-check.example", Provider: "fastmail", Password: "x"})
	env["ASTROLABE_PUBLIC_URL"] = "http://gtm.example.com"
	checks = Run(ctx, in)
	if f := fails(checks); len(f) != 1 || !strings.Contains(f[0], "not https") {
		t.Fatalf("only the non-https public URL should fail now: %v", f)
	}

	// Opting in to email from GitHub data warns and cites the policy.
	if find(checks, "policy", Warn, "") {
		t.Fatal("no policy warning by default")
	}
	py := filepath.Join(pdir, "product.yaml")
	raw, _ := os.ReadFile(py)
	os.WriteFile(py, append(raw, []byte("\noutreach: {email_from_github: true}\n")...), 0o644)
	if !find(Run(ctx, in), "policy", Warn, "Acceptable Use Policies") {
		t.Fatal("email_from_github: true should warn")
	}

	// No database at all.
	in.DatabaseURL = "postgres://nobody@127.0.0.1:1/none"
	if !find(Run(ctx, in), "database", Fail, "Postgres") {
		t.Fatal("unreachable database should fail")
	}
}

func TestDoctorNetlifyToken(t *testing.T) {
	ctx := context.Background()
	_, url := store.TestDBURL(t)
	env := map[string]string{}
	ws := t.TempDir()
	starter.Init(starter.Options{Dir: ws, Template: "devtool", Name: "Hook Check", SenderCold: "me@try-hook.example"})
	pdir := filepath.Join(ws, "products", "hook-check")
	os.WriteFile(filepath.Join(pdir, "claims.md"), []byte("- It works"), 0o644)
	os.WriteFile(filepath.Join(pdir, "voice.md"), []byte("Plain."), 0o644)
	os.WriteFile(filepath.Join(pdir, "monitors.yaml"), []byte("- {id: requests, type: netlify_forms, site: hook-check.netlify.app, form: request, signal: form.request, strength: 40, every: 1h}\n"), 0o644)
	in := Input{DatabaseURL: url, ProductsDir: filepath.Join(ws, "products"), SecretsDir: t.TempDir(), Mode: "sandbox",
		Password: "long-enough-password", Getenv: func(k string) string { return env[k] }}
	if !find(Run(ctx, in), "netlify", Fail, "ASTROLABE_NETLIFY_TOKEN") {
		t.Fatal("a netlify_forms monitor without a token should fail with the fix")
	}
	env["ASTROLABE_NETLIFY_TOKEN"] = "nfp_test"
	if find(Run(ctx, in), "netlify", Fail, "") || !find(Run(ctx, in), "netlify", OK, "token set") {
		t.Fatal("with a token set, the netlify check passes without -connect")
	}
}

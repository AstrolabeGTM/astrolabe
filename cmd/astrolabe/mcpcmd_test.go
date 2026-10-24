package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AstrolabeGTM/astrolabe/internal/app"
)

func TestMCPToolsDocFresh(t *testing.T) {
	var got bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "tools", "-md"}, &got); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("..", "..", "docs", "mcp-tools.md"))
	if err != nil || !bytes.Equal(got.Bytes(), want) {
		t.Errorf("docs/mcp-tools.md is missing or stale; regenerate with: go run ./cmd/astrolabe mcp tools -md > docs/mcp-tools.md")
	}
}

func TestMCPToken(t *testing.T) {
	t.Setenv("ASTROLABE_MCP_TOKEN", "")
	cfg := app.Config{SecretsDir: filepath.Join(t.TempDir(), "secrets")}
	a, err := mcpToken(cfg)
	if err != nil || len(a) < minMCPTokenLen {
		t.Fatalf("generate: %q %v", a, err)
	}
	if b, _ := mcpToken(cfg); b != a {
		t.Errorf("token not stable: %q vs %q", a, b)
	}
	fn := mcpTokenFunc(cfg)
	if fn() != a {
		t.Errorf("handler token differs from printed token")
	}
	c, err := rotateMCPToken(cfg)
	if err != nil || c == a || fn() != c {
		t.Errorf("rotate: %q %v (handler %q)", c, err, fn())
	}
	if fi, _ := os.Stat(mcpTokenPath(cfg)); fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v", fi.Mode())
	}
	t.Setenv("ASTROLABE_MCP_TOKEN", "env-token-0123456789")
	if e, _ := mcpToken(cfg); e != "env-token-0123456789" || mcpTokenFunc(cfg)() != e {
		t.Errorf("env token not used: %q", e)
	}
	if _, err := rotateMCPToken(cfg); err == nil {
		t.Errorf("rotate should refuse when env is set")
	}
	t.Setenv("ASTROLABE_MCP_TOKEN", "short")
	if _, err := mcpToken(cfg); err == nil {
		t.Errorf("short env token accepted")
	}
}

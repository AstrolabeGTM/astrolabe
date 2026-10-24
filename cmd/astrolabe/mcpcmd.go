package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/AstrolabeGTM/astrolabe/internal/app"
	"github.com/AstrolabeGTM/astrolabe/internal/mcp"
)

// mcpCommand handles `astrolabe mcp [token [-rotate] | tools [-md]]`.
func mcpCommand(ctx context.Context, cfg app.Config, args []string, out io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "token":
			fs := flag.NewFlagSet("mcp token", flag.ContinueOnError)
			rotate := fs.Bool("rotate", false, "replace the saved token with a new one")
			if err := fs.Parse(args[1:]); err != nil {
				return err
			}
			var tok string
			var err error
			if *rotate {
				tok, err = rotateMCPToken(cfg)
			} else {
				tok, err = mcpToken(cfg)
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(out, tok)
			return nil
		case "tools":
			fs := flag.NewFlagSet("mcp tools", flag.ContinueOnError)
			md := fs.Bool("md", false, "Markdown output")
			if err := fs.Parse(args[1:]); err != nil {
				return err
			}
			tools, err := mcp.ListTools(ctx)
			if err != nil {
				return err
			}
			mcp.WriteTools(out, tools, *md)
			return nil
		default:
			return errors.New("usage: astrolabe mcp [token [-rotate] | tools [-md]]")
		}
	}
	a, err := app.Open(ctx, cfg, false)
	if err != nil {
		return err
	}
	defer a.Close()
	if err := a.CheckMigrated(ctx); err != nil {
		return err
	}
	return newMCPServer(ctx, a, cfg, loadProducts(ctx, a)).Run(ctx, version)
}

func newMCPServer(ctx context.Context, a *app.App, cfg app.Config, products *loaded) *mcp.Server {
	return &mcp.Server{S: a.Outreach, Signals: a.Signals, Sources: a.Sources, Writer: a.Writer, Studio: a.Studio, Render: a.Render, Updates: a.Updates, Netlify: a.Forms.Netlify, ProductsDir: cfg.ProductsDir, Mode: a.Cfg.Mode,
		Products: products.Products, Reload: products.Reload, LoadIssues: products.Issues, Mail: a.Mail, Accounts: a.Accounts, Doctor: doctorFor(cfg)}
}

const minMCPTokenLen = 16

func mcpTokenPath(cfg app.Config) string { return filepath.Join(cfg.SecretsDir, "mcp-token") }

// mcpToken returns ASTROLABE_MCP_TOKEN, or the token saved in the secrets
// directory (generated on first use).
func mcpToken(cfg app.Config) (string, error) {
	if t := os.Getenv("ASTROLABE_MCP_TOKEN"); t != "" {
		if len(t) < minMCPTokenLen {
			return "", fmt.Errorf("ASTROLABE_MCP_TOKEN must be at least %d characters", minMCPTokenLen)
		}
		return t, nil
	}
	if b, err := os.ReadFile(mcpTokenPath(cfg)); err == nil && len(strings.TrimSpace(string(b))) >= minMCPTokenLen {
		return strings.TrimSpace(string(b)), nil
	}
	return rotateMCPToken(cfg)
}

func rotateMCPToken(cfg app.Config) (string, error) {
	if os.Getenv("ASTROLABE_MCP_TOKEN") != "" {
		return "", errors.New("ASTROLABE_MCP_TOKEN is set, so it is the token; change it there instead")
	}
	b := make([]byte, 32)
	rand.Read(b)
	tok := base64.RawURLEncoding.EncodeToString(b)
	if err := os.MkdirAll(cfg.SecretsDir, 0o700); err != nil {
		return "", err
	}
	// Write then rename so a running server never reads half a token.
	path := mcpTokenPath(cfg)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("save MCP token: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("save MCP token: %w", err)
	}
	return tok, nil
}

// mcpTokenFunc is what the HTTP handler calls per request: the env var if
// set, else the saved file (so `mcp token -rotate` works without a restart).
func mcpTokenFunc(cfg app.Config) func() string {
	return func() string {
		if t := os.Getenv("ASTROLABE_MCP_TOKEN"); t != "" {
			return t
		}
		b, _ := os.ReadFile(mcpTokenPath(cfg))
		return strings.TrimSpace(string(b))
	}
}

// mountMCP serves MCP over HTTP at /mcp, creating the token if needed.
func mountMCP(mux *http.ServeMux, srv *mcp.Server, cfg app.Config) error {
	if _, err := mcpToken(cfg); err != nil {
		return err
	}
	slog.Info("MCP over HTTP at /mcp (bearer token: astrolabe mcp token)")
	mux.Handle("/mcp", srv.HTTPHandler(version, mcpTokenFunc(cfg)))
	return nil
}

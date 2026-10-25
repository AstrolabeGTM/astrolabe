package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/AstrolabeGTM/astrolabe/internal/app"
	"github.com/AstrolabeGTM/astrolabe/internal/web"
)

// localOnly reports whether only this machine can reach the web app: it
// listens on a loopback address and has no public URL (a reverse proxy in
// front would also send Host: 127.0.0.1).
func localOnly(cfg app.Config) bool {
	return web.Loopback(cfg.Addr) && os.Getenv("ASTROLABE_PUBLIC_URL") == ""
}

// adminPassword returns ASTROLABE_PASSWORD; or "" (no sign-in) when only
// this machine can reach the server; or else a password generated on the
// first start and kept in the secrets folder (printed once, like Jenkins).
func adminPassword(cfg app.Config) (string, error) {
	if cfg.Password != "" {
		if len(cfg.Password) < 10 {
			return "", errors.New("ASTROLABE_PASSWORD must be at least 10 characters")
		}
		return cfg.Password, nil
	}
	if localOnly(cfg) {
		slog.Info("no sign-in: the web app listens on " + cfg.Addr + " only (set ASTROLABE_PASSWORD to require one)")
		return "", nil
	}
	path := filepath.Join(cfg.SecretsDir, "admin-password")
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	b := make([]byte, 18)
	rand.Read(b)
	pw := base64.RawURLEncoding.EncodeToString(b)
	if err := os.MkdirAll(cfg.SecretsDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(pw+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("save generated password: %w", err)
	}
	slog.Warn("no ASTROLABE_PASSWORD set; generated one (saved in " + path + ")")
	fmt.Fprintf(os.Stderr, "\n  Web app password: %s\n\n", pw)
	return pw, nil
}

// sessionKey signs web sessions when there is no password. It is kept in the
// secrets folder so a restart does not end the session in an open tab.
func sessionKey(cfg app.Config) ([]byte, error) {
	path := filepath.Join(cfg.SecretsDir, "session-key")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	b := make([]byte, 32)
	rand.Read(b)
	if err := os.MkdirAll(cfg.SecretsDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, fmt.Errorf("save session key: %w", err)
	}
	return b, nil
}

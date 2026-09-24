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

	"github.com/astrolabe-gtm/astrolabe/internal/app"
)

// adminPassword returns ASTROLABE_PASSWORD, or a password generated on the
// first start and kept in the secrets folder (printed once, like Jenkins).
func adminPassword(cfg app.Config) (string, error) {
	if cfg.Password != "" {
		if len(cfg.Password) < 10 {
			return "", errors.New("ASTROLABE_PASSWORD must be at least 10 characters")
		}
		return cfg.Password, nil
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

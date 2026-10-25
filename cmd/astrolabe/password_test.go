package main

import (
	"testing"

	"github.com/AstrolabeGTM/astrolabe/internal/app"
)

func TestAdminPassword(t *testing.T) {
	// On this machine only: no sign-in.
	if pw, err := adminPassword(app.Config{Addr: "127.0.0.1:8080", SecretsDir: t.TempDir()}); err != nil || pw != "" {
		t.Fatalf("loopback: %q %v", pw, err)
	}
	// Reachable from other machines (Docker listens on 0.0.0.0): one is generated and kept.
	cfg := app.Config{Addr: "0.0.0.0:8080", SecretsDir: t.TempDir()}
	pw, err := adminPassword(cfg)
	if err != nil || len(pw) < 10 {
		t.Fatalf("0.0.0.0: %q %v", pw, err)
	}
	if again, _ := adminPassword(cfg); again != pw {
		t.Fatalf("generated password changed: %q then %q", pw, again)
	}
	// A password that is set is used, wherever the server listens.
	if pw, _ := adminPassword(app.Config{Addr: "127.0.0.1:8080", Password: "long-enough-password"}); pw != "long-enough-password" {
		t.Fatalf("set password: %q", pw)
	}
}

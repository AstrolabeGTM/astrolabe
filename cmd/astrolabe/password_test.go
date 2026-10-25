package main

import (
	"bytes"
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
	// With a public URL a reverse proxy may sit in front (and send Host:
	// 127.0.0.1), so a password is needed even on loopback.
	t.Setenv("ASTROLABE_PUBLIC_URL", "https://gtm.example")
	if pw, _ := adminPassword(app.Config{Addr: "127.0.0.1:8080", SecretsDir: t.TempDir()}); pw == "" {
		t.Fatal("loopback with a public URL got no password")
	}
	// A password that is set is used, wherever the server listens.
	if pw, _ := adminPassword(app.Config{Addr: "127.0.0.1:8080", Password: "long-enough-password"}); pw != "long-enough-password" {
		t.Fatalf("set password: %q", pw)
	}
}

func TestSessionKeyIsKept(t *testing.T) {
	cfg := app.Config{SecretsDir: t.TempDir()}
	a, err := sessionKey(cfg)
	if err != nil || len(a) != 32 {
		t.Fatalf("%x %v", a, err)
	}
	if b, _ := sessionKey(cfg); !bytes.Equal(a, b) {
		t.Fatal("session key changed between starts")
	}
}

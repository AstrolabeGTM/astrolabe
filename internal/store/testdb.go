package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	neturl "net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestDB creates a throwaway, migrated database for one test and drops it
// afterwards. ASTROLABE_TEST_DATABASE_URL points at a server where the user
// may create databases; the test is skipped if none is reachable.
func TestDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool, _ := TestDBURL(t)
	return pool
}

// TestDBURL is TestDB that also returns the database URL.
func TestDBURL(t testing.TB) (*pgxpool.Pool, string) {
	t.Helper()
	admin := os.Getenv("ASTROLABE_TEST_DATABASE_URL")
	if admin == "" {
		admin = "postgres:///postgres?host=/tmp"
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Skipf("no test postgres (%v); set ASTROLABE_TEST_DATABASE_URL", err)
	}
	defer conn.Close(ctx)

	b := make([]byte, 6)
	rand.Read(b)
	name := "astrolabe_test_" + hex.EncodeToString(b)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, err := neturl.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	url := u.String()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool, url
}

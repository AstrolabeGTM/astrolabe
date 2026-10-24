package people

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// ErrForgotten is returned when an identity belongs to a person who was
// erased on request. Callers drop the data instead of creating the person.
var ErrForgotten = errors.New("this person asked to be forgotten")

const saltKey = "forget_salt"

// salt returns this install's tombstone salt, creating it on first use. It
// lives in the settings table, next to the data the tombstones protect: a
// restored backup then brings salt and tombstones back together, which a salt
// in a separate secrets volume could not promise. Hashes are only ever
// compared, never reversed, and a guess needs the salt, so a leaked list of
// hashes alone says nothing about who was erased.
func salt(ctx context.Context, q store.Q) (string, error) {
	var s string
	err := q.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, saltKey).Scan(&s)
	if err == nil {
		return s, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	if _, err := q.Exec(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2) ON CONFLICT DO NOTHING`, saltKey, hex.EncodeToString(b)); err != nil {
		return "", err
	}
	err = q.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, saltKey).Scan(&s)
	return s, err
}

func normalizeIdentity(kind, value string) string {
	value = strings.TrimSpace(value)
	switch kind {
	case "email":
		return NormalizeEmail(value)
	case "github":
		return NormalizeGitHub(value)
	}
	return strings.ToLower(value)
}

// IdentityHash is the tombstone key for an identity.
func IdentityHash(ctx context.Context, q store.Q, kind, value string) (string, error) {
	s, err := salt(ctx, q)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(s + "\n" + kind + ":" + normalizeIdentity(kind, value)))
	return hex.EncodeToString(sum[:]), nil
}

// Tombstone records that an identity belongs to an erased person.
func Tombstone(ctx context.Context, q store.Q, kind, value string) (bool, error) {
	if strings.TrimSpace(value) == "" {
		return false, nil
	}
	h, err := IdentityHash(ctx, q, kind, value)
	if err != nil {
		return false, err
	}
	tag, err := q.Exec(ctx, `INSERT INTO forgotten (hash, kind) VALUES ($1, $2) ON CONFLICT DO NOTHING`, h, kind)
	return tag.RowsAffected() == 1, err
}

// IsForgotten says whether an identity (kind "email", "github", ...) was
// erased on request.
func IsForgotten(ctx context.Context, q store.Q, kind, value string) (bool, error) {
	if strings.TrimSpace(value) == "" {
		return false, nil
	}
	h, err := IdentityHash(ctx, q, kind, value)
	if err != nil {
		return false, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM forgotten WHERE hash = $1`, h).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// AnyForgotten checks (kind, value) pairs and reports whether any was erased.
func AnyForgotten(ctx context.Context, q store.Q, ids [][2]string) (bool, error) {
	for _, id := range ids {
		if ok, err := IsForgotten(ctx, q, id[0], id[1]); err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

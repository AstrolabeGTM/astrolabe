// Package links makes tracked short links (/l/<code>) that redirect with
// UTM tags and turn clicks into signals, so the funnel can attribute them.
package links

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/astrolabe-gtm/astrolabe/internal/store"
)

const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"

func newCode() string {
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

type Link struct {
	Product     string
	Destination string
	Label       string            // what it is for, e.g. "play:pain-issue-outreach" or "content:x:12"
	PersonID    *int64            // set for links sent to one person
	UTM         map[string]string // utm_source, utm_medium, utm_campaign, utm_content
}

// Create stores a link and returns its code.
func Create(ctx context.Context, q store.Q, l Link) (string, error) {
	utm, _ := json.Marshal(l.UTM)
	for range 3 {
		code := newCode()
		tag, err := q.Exec(ctx, `
			INSERT INTO links (code, product_id, destination, label, person_id, utm) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (code) DO NOTHING`, code, l.Product, l.Destination, l.Label, l.PersonID, utm)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 1 {
			return code, nil
		}
	}
	return "", errors.New("could not allocate a link code")
}

// URL is the public short link, or "" without a public base URL.
func URL(base, code string) string {
	if base == "" || code == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/l/" + code
}

// Target is the destination with UTM tags and ref=<code> added, so a signup
// can report which link brought it.
func Target(dest string, utm map[string]string, code string) string {
	u, err := url.Parse(dest)
	if err != nil {
		return dest
	}
	q := u.Query()
	for k, v := range utm {
		if v != "" && q.Get(k) == "" {
			q.Set(k, v)
		}
	}
	q.Set("ref", code)
	u.RawQuery = q.Encode()
	return u.String()
}

// AttributeSignup credits a new user's first touch to the link they came
// from, if their product event carried ref=<code>.
func AttributeSignup(ctx context.Context, q store.Q, productID string, personID int64, code string) error {
	var label string
	err := q.QueryRow(ctx, `SELECT label FROM links WHERE code = $1 AND product_id = $2`, code, productID).Scan(&label)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	// Only replace a source that the signup event itself created.
	_, err = q.Exec(ctx, `UPDATE product_people SET source = 'link:' || $3 WHERE product_id = $1 AND person_id = $2 AND source LIKE 'signal:product.%'`,
		productID, personID, label)
	return err
}

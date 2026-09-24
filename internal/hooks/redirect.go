package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/astrolabe-gtm/astrolabe/internal/links"
	"github.com/astrolabe-gtm/astrolabe/internal/outreach"
	"github.com/astrolabe-gtm/astrolabe/internal/product"
	"github.com/astrolabe-gtm/astrolabe/internal/signal"
)

// Redirector serves /l/{code}.
type Redirector struct {
	Signals *signal.Service
	Out     *outreach.Service
}

func (rd *Redirector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	code := r.PathValue("code")
	var productID, dest, label string
	var personID *int64
	var utmRaw []byte
	err := rd.Out.Pool.QueryRow(ctx, `SELECT product_id, destination, label, person_id, utm FROM links WHERE code = $1`, code).
		Scan(&productID, &dest, &label, &personID, &utmRaw)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var utm map[string]string
	json.Unmarshal(utmRaw, &utm)
	// Redirect first; recording must never break the link.
	http.Redirect(w, r, links.Target(dest, utm, code), http.StatusFound)
	if isBot(r.UserAgent()) {
		return
	}
	go rd.record(context.WithoutCancel(ctx), code, productID, label, personID, r.Referer(), r.UserAgent())
}

// Link scanners in mail servers "click" everything; don't count them.
func isBot(ua string) bool {
	ua = strings.ToLower(ua)
	for _, b := range []string{"bot", "crawler", "spider", "preview", "slurp", "facebookexternalhit", "google-read-aloud", "proofpoint", "mimecast", "barracuda"} {
		if strings.Contains(ua, b) {
			return true
		}
	}
	return ua == ""
}

func (rd *Redirector) record(ctx context.Context, code, productID, label string, personID *int64, referer, ua string) {
	if err := rd.Record(ctx, code, productID, label, personID, referer, ua, time.Now()); err != nil {
		slog.Error("record click", "code", code, "err", err)
	}
}

// Record stores a click; a click on a personal link is a signal for that
// person and their "site_visit" stage if the funnel has one.
func (rd *Redirector) Record(ctx context.Context, code, productID, label string, personID *int64, referer, ua string, at time.Time) error {
	if _, err := rd.Out.Pool.Exec(ctx, `INSERT INTO clicks (code, clicked_at, referer, user_agent) VALUES ($1, $2, $3, $4)`, code, at, referer, ua); err != nil {
		return err
	}
	if personID == nil {
		return nil
	}
	sub := signal.Subject{}
	var email string
	rd.Out.Pool.QueryRow(ctx, `SELECT value FROM identities WHERE person_id = $1 AND kind = 'email' LIMIT 1`, *personID).Scan(&email)
	if email == "" {
		return nil
	}
	sub.Email = email
	if _, err := rd.Signals.Ingest(ctx, &signal.Signal{Product: productID, Type: "link.click", OccurredAt: at, Strength: 15,
		Subject: sub, Title: "clicked " + label, DedupeKey: fmt.Sprintf("click:%s:%s", code, at.Format("2006-01-02"))}); err != nil {
		return err
	}
	p, err := product.Get(ctx, rd.Out.Pool, productID)
	if err != nil {
		return err
	}
	for _, st := range p.Funnel {
		if st == "site_visit" {
			return rd.Out.RecordStage(ctx, productID, *personID, "site_visit", at, "link", fmt.Sprintf("site_visit:%d", *personID))
		}
	}
	return nil
}

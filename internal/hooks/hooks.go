// Package hooks receives signed webhooks: product events from my own apps
// and Stripe events. Every step is idempotent (signal, stage and payment
// dedupe keys), so provider retries and duplicates are harmless.
package hooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/links"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
)

const (
	maxBody   = 1 << 20
	tolerance = 5 * time.Minute
)

type Handler struct {
	Signals *signal.Service
	Out     *outreach.Service
	// Secret returns the signing secret for "events" or "stripe" webhooks of
	// a product, or "" if none is configured (the webhook is then refused).
	Secret func(kind, productID string) string
	Now    func() time.Time
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// EnvSecret reads ASTROLABE_EVENTS_SECRET_<PRODUCT> / ASTROLABE_STRIPE_SECRET_<PRODUCT>.
func EnvSecret(kind, productID string) string {
	name := fmt.Sprintf("ASTROLABE_%s_SECRET_%s", strings.ToUpper(kind), strings.ToUpper(strings.ReplaceAll(productID, "-", "_")))
	return os.Getenv(name)
}

// Verify checks a "t=<unix>,v1=<hex>[,v1=…]" signature header: HMAC-SHA256
// over "<t>.<body>" keyed with the secret, within a 5-minute tolerance. This
// is Stripe's scheme; product events use the same one.
func Verify(header string, body []byte, secret string, now time.Time) error {
	if secret == "" {
		return errors.New("no signing secret configured")
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return errors.New("malformed signature header")
	}
	if d := now.Sub(time.Unix(t, 0)); d > tolerance || d < -tolerance {
		return errors.New("signature timestamp outside tolerance")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(want)) {
			return nil
		}
	}
	return errors.New("signature mismatch")
}

// Sign produces a header for Verify (for tests and for my apps' senders).
func Sign(body []byte, secret string, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request, kind, header string) (string, []byte, bool) {
	productID := r.PathValue("product")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return "", nil, false
	}
	if err := Verify(r.Header.Get(header), body, h.Secret(kind, productID), h.now()); err != nil {
		slog.Warn("webhook refused", "kind", kind, "product", productID, "err", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return "", nil, false
	}
	if _, err := product.Get(r.Context(), h.Out.Pool, productID); err != nil {
		http.Error(w, "unknown product", http.StatusNotFound)
		return "", nil, false
	}
	return productID, body, true
}

func (h *Handler) seen(ctx context.Context, provider, id string) (bool, error) {
	var x int
	err := h.Out.Pool.QueryRow(ctx, `SELECT 1 FROM webhook_events WHERE provider = $1 AND event_id = $2`, provider, id).Scan(&x)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (h *Handler) markSeen(ctx context.Context, provider, id, productID string) error {
	_, err := h.Out.Pool.Exec(ctx, `INSERT INTO webhook_events (provider, event_id, product_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, provider, id, productID)
	return err
}

// ---- product events ----

// Event is what my apps send to /hooks/events/{product}, signed with the
// Astrolabe-Signature header. Send one event or {"events": [...]}.
type Event struct {
	ID         string          `json:"id"`   // unique per event; retries reuse it
	Type       string          `json:"type"` // a funnel stage (e.g. installed) or any activity name
	UserID     string          `json:"user_id"`
	Email      string          `json:"email"`
	Phone      string          `json:"phone"`
	Name       string          `json:"name"`
	OccurredAt time.Time       `json:"occurred_at"`
	Data       map[string]any  `json:"data"`
	Consent    map[string]bool `json:"consent"` // e.g. {"whatsapp": true, "push": true}
}

func (h *Handler) ProductEvents(w http.ResponseWriter, r *http.Request) {
	productID, body, ok := h.read(w, r, "events", "Astrolabe-Signature")
	if !ok {
		return
	}
	var batch struct{ Events []Event }
	if err := json.Unmarshal(body, &batch); err != nil || len(batch.Events) == 0 {
		var one Event
		if err := json.Unmarshal(body, &one); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		batch.Events = []Event{one}
	}
	for _, ev := range batch.Events {
		if err := h.handleEvent(r.Context(), productID, ev); err != nil {
			slog.Error("product event", "product", productID, "id", ev.ID, "err", err)
			code := http.StatusInternalServerError
			if errors.Is(err, errBadEvent) {
				code = http.StatusBadRequest
			}
			http.Error(w, err.Error(), code)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

var errBadEvent = errors.New("bad event")

func (h *Handler) handleEvent(ctx context.Context, productID string, ev Event) error {
	if ev.ID == "" || ev.Type == "" || (ev.UserID == "" && ev.Email == "") {
		return fmt.Errorf("%w: needs id, type and user_id or email", errBadEvent)
	}
	if done, err := h.seen(ctx, "events:"+productID, ev.ID); err != nil || done {
		return err
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = h.now()
	}
	p, err := product.Get(ctx, h.Out.Pool, productID)
	if err != nil {
		return err
	}
	isStage := slices.Contains(p.Funnel, ev.Type)
	res, err := h.Signals.Ingest(ctx, &signal.Signal{
		Product: productID, Type: "product." + ev.Type, OccurredAt: ev.OccurredAt, Strength: 10,
		Subject: signal.Subject{User: ev.UserID, Email: ev.Email, Phone: ev.Phone, Name: ev.Name},
		Title:   ev.Type, Data: ev.Data, DedupeKey: "event:" + ev.ID,
	})
	if err != nil {
		return err
	}
	if res.PersonID == nil {
		if res.Ambiguous {
			// Recorded for review; nothing else can be attributed safely.
			return h.markSeen(ctx, "events:"+productID, ev.ID, productID)
		}
		if !res.Duplicate {
			return fmt.Errorf("%w: could not identify the user", errBadEvent)
		}
		// Duplicate delivery of an event already stored: find its person.
		var pid *int64
		h.Out.Pool.QueryRow(ctx, `SELECT person_id FROM signals WHERE product_id = $1 AND dedupe_key = $2`, productID, "event:"+ev.ID).Scan(&pid)
		if pid == nil {
			return h.markSeen(ctx, "events:"+productID, ev.ID, productID)
		}
		res.PersonID = pid
	}
	for ch, allowed := range ev.Consent {
		if _, err := h.Out.Pool.Exec(ctx, `
			INSERT INTO channel_permissions (product_id, person_id, channel, allowed) VALUES ($1, $2, $3, $4)
			ON CONFLICT (product_id, person_id, channel) DO UPDATE SET allowed = $4, updated_at = now()`,
			productID, *res.PersonID, ch, allowed); err != nil {
			return err
		}
	}
	if ref, _ := ev.Data["ref"].(string); ref != "" {
		if err := links.AttributeSignup(ctx, h.Out.Pool, productID, *res.PersonID, ref); err != nil {
			return err
		}
	}
	if isStage {
		if err := h.Out.RecordStage(ctx, productID, *res.PersonID, ev.Type, ev.OccurredAt, "product", fmt.Sprintf("%s:%d", ev.Type, *res.PersonID)); err != nil {
			return err
		}
	}
	return h.markSeen(ctx, "events:"+productID, ev.ID, productID)
}

// ---- Stripe ----

type stripeEvent struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Created int64  `json:"created"`
	Data    struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

type stripeObject struct {
	ID                string            `json:"id"`
	Customer          string            `json:"customer"`
	CustomerEmail     string            `json:"customer_email"`
	AmountPaid        int64             `json:"amount_paid"`
	AmountTotal       int64             `json:"amount_total"`
	AmountRefunded    int64             `json:"amount_refunded"`
	Currency          string            `json:"currency"`
	Mode              string            `json:"mode"`
	PaymentStatus     string            `json:"payment_status"`
	Status            string            `json:"status"`
	ClientReferenceID string            `json:"client_reference_id"`
	ReceiptEmail      string            `json:"receipt_email"`
	Metadata          map[string]string `json:"metadata"`
	CustomerDetails   struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"customer_details"`
	BillingDetails struct {
		Email string `json:"email"`
	} `json:"billing_details"`
}

func (o *stripeObject) email() string {
	for _, e := range []string{o.CustomerEmail, o.CustomerDetails.Email, o.ReceiptEmail, o.BillingDetails.Email} {
		if e != "" {
			return e
		}
	}
	return ""
}

// userID: my apps pass their user id as client_reference_id or metadata.user_id.
func (o *stripeObject) userID() string {
	if o.Metadata["user_id"] != "" {
		return o.Metadata["user_id"]
	}
	return o.ClientReferenceID
}

func (h *Handler) Stripe(w http.ResponseWriter, r *http.Request) {
	productID, body, ok := h.read(w, r, "stripe", "Stripe-Signature")
	if !ok {
		return
	}
	var ev stripeEvent
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" {
		http.Error(w, "invalid event", http.StatusBadRequest)
		return
	}
	if err := h.handleStripe(r.Context(), productID, ev); err != nil {
		slog.Error("stripe event", "product", productID, "id", ev.ID, "type", ev.Type, "err", err)
		http.Error(w, "processing failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleStripe(ctx context.Context, productID string, ev stripeEvent) error {
	if done, err := h.seen(ctx, "stripe", ev.ID); err != nil || done {
		return err
	}
	var o stripeObject
	if err := json.Unmarshal(ev.Data.Object, &o); err != nil {
		return err
	}
	at := time.Unix(ev.Created, 0).UTC()
	p, err := product.Get(ctx, h.Out.Pool, productID)
	if err != nil {
		return err
	}

	type effect struct {
		signal   string
		strength int
		whyNow   bool
		stage    string
		amount   int64 // cents; negative for refunds
		kind     string
		txID     string
	}
	var e effect
	switch ev.Type {
	case "invoice.paid":
		if o.AmountPaid > 0 {
			e = effect{signal: "stripe.paid", strength: 30, stage: "paid", amount: o.AmountPaid, kind: "payment", txID: o.ID}
		}
	case "checkout.session.completed":
		// Subscriptions are counted from invoice.paid; one-off payments here.
		if o.Mode == "payment" && o.PaymentStatus == "paid" {
			e = effect{signal: "stripe.paid", strength: 30, stage: "paid", amount: o.AmountTotal, kind: "payment", txID: o.ID}
		} else {
			e = effect{signal: "stripe.checkout", strength: 20}
		}
	case "charge.refunded":
		var prior int64
		h.Out.Pool.QueryRow(ctx, `SELECT COALESCE(-sum(amount_cents), 0) FROM payments WHERE provider = 'stripe' AND provider_id LIKE $1`,
			"refund:"+o.ID+":%").Scan(&prior)
		if o.AmountRefunded > prior {
			e = effect{signal: "stripe.refunded", strength: 0, amount: -(o.AmountRefunded - prior), kind: "refund",
				txID: fmt.Sprintf("refund:%s:%d", o.ID, o.AmountRefunded)}
		}
	case "customer.subscription.created":
		if o.Status == "trialing" {
			e = effect{signal: "stripe.trial_started", strength: 25, stage: "trial"}
		}
	case "customer.subscription.trial_will_end":
		e = effect{signal: "stripe.trial_will_end", strength: 20, whyNow: true}
	case "invoice.payment_failed":
		e = effect{signal: "stripe.payment_failed", strength: 20, whyNow: true}
	case "customer.subscription.deleted":
		e = effect{signal: "stripe.churned", strength: 0, stage: "churned"}
	default:
		return h.markSeen(ctx, "stripe", ev.ID, productID)
	}
	if e.signal == "" {
		return h.markSeen(ctx, "stripe", ev.ID, productID)
	}
	res, err := h.Signals.Ingest(ctx, &signal.Signal{
		Product: productID, Type: e.signal, OccurredAt: at, Strength: e.strength, WhyNow: e.whyNow,
		Subject: signal.Subject{Email: o.email(), Stripe: o.Customer, User: o.userID(), Name: o.CustomerDetails.Name},
		Title:   strings.ReplaceAll(ev.Type, ".", " "), DedupeKey: "stripe:" + ev.ID,
		Data: map[string]any{"object": o.ID, "amount_cents": e.amount, "currency": o.Currency},
	})
	if err != nil {
		return err
	}
	personID := res.PersonID
	if personID == nil {
		h.Out.Pool.QueryRow(ctx, `SELECT person_id FROM signals WHERE product_id = $1 AND dedupe_key = $2`, productID, "stripe:"+ev.ID).Scan(&personID)
	}
	if e.kind != "" {
		if _, err := h.Out.Pool.Exec(ctx, `
			INSERT INTO payments (product_id, person_id, provider, provider_id, kind, amount_cents, currency, occurred_at, email)
			VALUES ($1, $2, 'stripe', $3, $4, $5, $6, $7, $8) ON CONFLICT (provider, provider_id) DO NOTHING`,
			productID, personID, e.txID, e.kind, e.amount, strings.ToLower(o.Currency), at, o.email()); err != nil {
			return err
		}
	}
	if e.stage != "" && personID != nil && slices.Contains(p.Funnel, e.stage) {
		if err := h.Out.RecordStage(ctx, productID, *personID, e.stage, at, "stripe", fmt.Sprintf("%s:%d", e.stage, *personID)); err != nil {
			return err
		}
	}
	return h.markSeen(ctx, "stripe", ev.ID, productID)
}

package hooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/astrolabe-gtm/astrolabe/internal/outreach"
	"github.com/astrolabe-gtm/astrolabe/internal/signal"
)

// WhatsAppVerify answers Meta's subscription check (GET with hub.mode,
// hub.verify_token and hub.challenge).
func (h *Handler) WhatsAppVerify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	token := os.Getenv("ASTROLABE_WHATSAPP_VERIFY_TOKEN")
	if token == "" || q.Get("hub.mode") != "subscribe" || !hmac.Equal([]byte(q.Get("hub.verify_token")), []byte(token)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Write([]byte(q.Get("hub.challenge")))
}

// VerifyMeta checks X-Hub-Signature-256: "sha256=" + HMAC-SHA256(body, app secret).
func VerifyMeta(header string, body []byte, appSecret string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok || appSecret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	return hmac.Equal([]byte(sig), []byte(hex.EncodeToString(mac.Sum(nil))))
}

type waPayload struct {
	Entry []struct {
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				Contacts []struct {
					WaID    string `json:"wa_id"`
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
				} `json:"contacts"`
				Messages []struct {
					From      string `json:"from"`
					ID        string `json:"id"`
					Timestamp string `json:"timestamp"`
					Type      string `json:"type"`
					Text      struct {
						Body string `json:"body"`
					} `json:"text"`
					Button struct {
						Text string `json:"text"`
					} `json:"button"`
				} `json:"messages"`
				Statuses []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
					Errors []struct {
						Title string `json:"title"`
					} `json:"errors"`
				} `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// WhatsApp receives messages and delivery statuses. Replies stop the
// sequence like email replies; failed deliveries are recorded on the action.
func (h *Handler) WhatsApp(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !VerifyMeta(r.Header.Get("X-Hub-Signature-256"), body, os.Getenv("ASTROLABE_WHATSAPP_APP_SECRET")) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var p waPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := h.handleWhatsApp(r.Context(), p); err != nil {
		slog.Error("whatsapp webhook", "err", err)
		http.Error(w, "processing failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleWhatsApp(ctx context.Context, p waPayload) error {
	for _, e := range p.Entry {
		for _, c := range e.Changes {
			v := c.Value
			mailbox := "whatsapp:" + v.Metadata.PhoneNumberID
			for _, m := range v.Messages {
				text := m.Text.Body
				if text == "" {
					text = m.Button.Text
				}
				sec, _ := strconv.ParseInt(m.Timestamp, 10, 64)
				phone := signal.NormalizePhone("+" + strings.TrimPrefix(m.From, "+"))
				if _, err := h.Out.RecordReply(ctx, outreach.Inbound{Channel: "whatsapp", Mailbox: mailbox, ProviderID: "wa:" + m.ID,
					From: phone, Subject: "WhatsApp message", Snippet: text, Body: text, ReceivedAt: time.Unix(sec, 0).UTC()}); err != nil {
					return err
				}
			}
			for _, st := range v.Statuses {
				if st.Status != "failed" {
					continue
				}
				reason := "WhatsApp delivery failed"
				if len(st.Errors) > 0 {
					reason += ": " + st.Errors[0].Title
				}
				if _, err := h.Out.Pool.Exec(ctx, `UPDATE actions SET error = $2 WHERE provider_id = $1 AND channel = 'whatsapp'`, st.ID, reason); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

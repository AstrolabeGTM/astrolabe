package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviders(t *testing.T) {
	var got map[string]any
	var headers http.Header
	status := 200
	reply := `{"id":"x1"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		got = nil
		json.NewDecoder(r.Body).Decode(&got)
		got["_path"] = r.URL.Path
		w.WriteHeader(status)
		w.Write([]byte(reply))
	}))
	defer srv.Close()
	ctx := context.Background()

	// Resend: idempotency key per action.
	rs := &Resend{APIKey: "re_k", Base: srv.URL}
	res, err := rs.Send(ctx, Message{From: "hello@p.example", To: "u@x.example", Subject: "Hi", Body: "Text", IdempotencyKey: "astrolabe-action-7"})
	if err != nil || res.ProviderID != "x1" || headers.Get("Idempotency-Key") != "astrolabe-action-7" || headers.Get("Authorization") != "Bearer re_k" {
		t.Fatalf("resend: %v %+v %v", err, res, headers)
	}

	// WhatsApp: template with body parameters from lines.
	reply = `{"messages":[{"id":"wamid.1"}]}`
	wa := &WhatsApp{Token: "t", PhoneNumber: "123", Version: "v23.0", Base: srv.URL}
	res, err = wa.Send(ctx, Message{To: "+919812345678", Subject: "first_lesson", Language: "en", Body: "Asha\nIron condor"})
	b, _ := json.Marshal(got)
	if err != nil || res.ProviderID != "wamid.1" || got["_path"] != "/v23.0/123/messages" || got["to"] != "919812345678" ||
		!strings.Contains(string(b), `"text":"Iron condor"`) || !strings.Contains(string(b), `"name":"first_lesson"`) {
		t.Fatalf("whatsapp: %v %s", err, b)
	}

	// Product notify: signed body.
	reply = `{"id":"n1"}`
	n := &Notify{URL: srv.URL + "/notify", Secret: "s"}
	res, err = n.Send(ctx, Message{Channel: "push", To: "u42", Subject: "Try a strategy", Body: "…", IdempotencyKey: "astrolabe-action-9"})
	if err != nil || res.ProviderID != "n1" || got["user_id"] != "u42" || got["id"] != "astrolabe-action-9" ||
		!strings.HasPrefix(headers.Get("Astrolabe-Signature"), "t=") {
		t.Fatalf("notify: %v %+v %v", err, res, got)
	}

	// Refusals are definite; server errors and conflicts are unknown.
	for code, definite := range map[int]bool{400: true, 401: true, 422: true, 429: true, 409: false, 500: false, 503: false} {
		status = code
		_, err := n.Send(ctx, Message{})
		if err == nil || errors.Is(err, ErrNotSent) != definite {
			t.Errorf("%d: %v (definite=%v)", code, err, definite)
		}
	}
}

func TestListHeaders(t *testing.T) {
	m := Message{From: "hello@demo.example", To: "a@x.example", Subject: "Hi", MessageID: "<1@demo.example>", Body: "b",
		Headers: map[string]string{"List-Unsubscribe": "<mailto:hello@demo.example?subject=unsubscribe%201.ab>, <https://x.example/u/1>",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click", "Auto-Submitted": "auto-generated"}}
	raw, err := BuildMIME(m)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if strings.Count(s, "List-Unsubscribe:") != 1 || !strings.Contains(s, "https://x.example/u/1") ||
		!strings.Contains(s, "List-Unsubscribe-Post: List-Unsubscribe=One-Click") || !strings.Contains(s, "Auto-Submitted: auto-generated") {
		t.Fatalf("headers:\n%s", s)
	}
	m.Headers = map[string]string{"X-Evil": "a\r\nBcc: x@y.example"}
	if _, err := BuildMIME(m); err == nil {
		t.Fatal("header injection")
	}
	// Without extra headers the default mailto header stays.
	m.Headers = nil
	raw, _ = BuildMIME(m)
	if !strings.Contains(string(raw), "List-Unsubscribe: <mailto:hello@demo.example?subject=unsubscribe>") {
		t.Fatal("default header lost")
	}
}

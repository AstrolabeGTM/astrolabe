package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
)

func TestBuildMIMEThreadsAndRejectsHeaderInjection(t *testing.T) {
	raw, err := BuildMIME(channel.Message{From: "me@x.example", To: "you@y.example", Subject: "Re: Héllo",
		Body: "Line one\nLine two", MessageID: "<a@x.example>", InReplyTo: "<prev@x.example>"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"Message-ID: <a@x.example>\r\n", "In-Reply-To: <prev@x.example>\r\n",
		"References: <prev@x.example>\r\n", "Subject: =?utf-8?q?Re:_H=C3=A9llo?=\r\n", "Line one\r\nLine two"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if _, err := BuildMIME(channel.Message{To: "a@b.example\r\nBcc: everyone@x.example"}); err == nil {
		t.Fatal("header injection accepted")
	}
}

func TestSendClassifiesOutcomes(t *testing.T) {
	status := 200
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		if status == 200 {
			w.Write([]byte(`{"id":"m1","threadId":"t1"}`))
		} else {
			w.Write([]byte(`{"error":{"message":"nope"}}`))
		}
	}))
	defer srv.Close()
	mb := NewMailbox("me@x.example", srv.URL, srv.Client())
	msg := channel.Message{From: "me@x.example", To: "you@y.example", Subject: "Hi", Body: "Hello", MessageID: "<a@x.example>", ThreadID: "t0"}

	res, err := mb.Send(context.Background(), msg)
	if err != nil || res.ProviderID != "m1" || res.ThreadID != "t1" || got["threadId"] != "t0" {
		t.Fatalf("200: %+v %v %v", res, err, got)
	}
	raw, _ := base64.URLEncoding.DecodeString(got["raw"])
	if !strings.Contains(string(raw), "To: you@y.example") {
		t.Fatalf("raw message: %s", raw)
	}

	status = 400
	if _, err := mb.Send(context.Background(), msg); !errors.Is(err, channel.ErrNotSent) {
		t.Fatalf("400 must be a definite failure: %v", err)
	}
	status = 503
	if _, err := mb.Send(context.Background(), msg); err == nil || errors.Is(err, channel.ErrNotSent) {
		t.Fatalf("503 must be unknown: %v", err)
	}
}

func TestParseBounce(t *testing.T) {
	enc := func(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }
	msg := &message{ID: "b1", ThreadID: "t9", InternalDate: "1790000000000", Payload: part{
		MimeType: "multipart/report",
		Headers: []struct{ Name, Value string }{
			{"From", "Mail Delivery Subsystem <mailer-daemon@googlemail.com>"},
			{"Subject", "Delivery Status Notification (Failure)"},
			{"Content-Type", "multipart/report; report-type=delivery-status; boundary=x"},
		},
		Parts: []part{
			{MimeType: "text/plain", Body: struct{ Data string }{enc("Address not found")}},
			{MimeType: "message/delivery-status", Body: struct{ Data string }{enc("Final-Recipient: rfc822; Gone@Acme.example\nAction: failed")}},
		},
	}}
	in := parse("me@x.example", msg)
	if !in.Bounce || in.From != "mailer-daemon@googlemail.com" || len(in.FailedRecipients) != 1 || in.FailedRecipients[0] != "gone@acme.example" {
		t.Fatalf("%+v", in)
	}
}

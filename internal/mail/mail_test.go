package mail

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/astrolabe-gtm/astrolabe/internal/channel"
)

// fakeSMTP is a minimal SMTP server. mode: "ok", "reject-rcpt", "drop-after-dot".
func fakeSMTP(t *testing.T, mode string) (int, chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan string, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				say := func(s string) { fmt.Fprintf(c, "%s\r\n", s) }
				say("220 fake")
				var data strings.Builder
				inData := false
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if inData {
						if line == ".\r\n" {
							inData = false
							got <- data.String()
							if mode == "drop-after-dot" {
								return
							}
							say("250 queued")
							continue
						}
						data.WriteString(line)
						continue
					}
					cmd := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case strings.HasPrefix(cmd, "EHLO"):
						say("250-fake")
						say("250 AUTH PLAIN")
					case strings.HasPrefix(cmd, "AUTH"):
						say("235 ok")
					case strings.HasPrefix(cmd, "MAIL"):
						say("250 ok")
					case strings.HasPrefix(cmd, "RCPT"):
						if mode == "reject-rcpt" {
							say("550 no such user")
						} else {
							say("250 ok")
						}
					case cmd == "DATA":
						inData = true
						say("354 go")
					case cmd == "QUIT":
						say("221 bye")
						return
					default:
						say("250 ok")
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

func imapServer(t *testing.T) (int, *imapmemserver.User) {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("me@x.example", "app-pass")
	user.Create("INBOX", nil)
	user.Create("Sent", nil)
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port, user
}

func account(smtpPort, imapPort int) *Account {
	return &Account{Address: "me@x.example", Provider: "custom", Username: "me@x.example", Password: "app-pass",
		SMTPHost: "127.0.0.1", SMTPPort: smtpPort, SMTPTLS: "starttls", IMAPHost: "127.0.0.1", IMAPPort: imapPort,
		SentFolder: "Sent", Insecure: true}
}

func TestSMTPOutcomes(t *testing.T) {
	ctx := context.Background()
	imapPort, user := imapServer(t)
	msg := channel.Message{From: "me@x.example", To: "you@y.example", Subject: "Hi", Body: "Hello", MessageID: "<m1@x.example>"}

	port, got := fakeSMTP(t, "ok")
	s := &Sender{Account: account(port, imapPort)}
	res, err := s.Send(ctx, msg)
	if err != nil || res.ThreadID != "<m1@x.example>" {
		t.Fatalf("send: %+v %v", res, err)
	}
	if body := <-got; !strings.Contains(body, "Message-ID: <m1@x.example>") {
		t.Fatalf("sent: %s", body)
	}
	// A copy was filed in Sent, so an uncertain send can be checked.
	if found, err := FindSent(s.Account, "<m1@x.example>"); err != nil || !found {
		t.Fatalf("FindSent: %v %v", found, err)
	}
	_ = user

	port, _ = fakeSMTP(t, "reject-rcpt")
	if _, err := (&Sender{Account: account(port, imapPort)}).Send(ctx, msg); !errors.Is(err, channel.ErrNotSent) {
		t.Fatalf("rejected recipient must be definite: %v", err)
	}
	port, _ = fakeSMTP(t, "drop-after-dot")
	if _, err := (&Sender{Account: account(port, imapPort)}).Send(ctx, msg); err == nil || errors.Is(err, channel.ErrNotSent) {
		t.Fatalf("lost answer after the message must be unknown: %v", err)
	}
	if _, err := (&Sender{Account: account(1, imapPort)}).Send(ctx, msg); !errors.Is(err, channel.ErrNotSent) {
		t.Fatalf("connection refused must be definite: %v", err)
	}
}

func appendRaw(t *testing.T, a *Account, folder, raw string) {
	t.Helper()
	c, err := dialIMAP(a)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cmd := c.Append(folder, int64(len(raw)), nil)
	cmd.Write([]byte(raw))
	cmd.Close()
	if _, err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestPollRepliesAndBounces(t *testing.T) {
	imapPort, _ := imapServer(t)
	a := account(0, imapPort)
	date := time.Now().Format(time.RFC1123Z)
	appendRaw(t, a, "INBOX", "From: Ana <ana@acme.example>\r\nTo: me@x.example\r\nSubject: Re: Hi\r\nDate: "+date+"\r\nMessage-ID: <r1@acme.example>\r\nIn-Reply-To: <m2@x.example>\r\nReferences: <m1@x.example> <m2@x.example>\r\n\r\nYes please!\r\n\r\nOn Mon, Me wrote:\r\n> Hello\r\n")
	appendRaw(t, a, "INBOX", "From: Mail Delivery Subsystem <mailer-daemon@googlemail.com>\r\nTo: me@x.example\r\nSubject: Delivery Status Notification (Failure)\r\nDate: "+date+"\r\nMessage-ID: <b1@google>\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=XX\r\n\r\n--XX\r\nContent-Type: text/plain\r\n\r\nAddress not found\r\n--XX\r\nContent-Type: message/delivery-status\r\n\r\nFinal-Recipient: rfc822; gone@acme.example\r\nAction: failed\r\n--XX--\r\n")

	msgs, cur, err := Poll(a, Cursor{})
	if err != nil || len(msgs) != 2 {
		t.Fatalf("poll: %v %+v", err, msgs)
	}
	r := msgs[0]
	if r.From != "ana@acme.example" || r.ThreadID != "<m1@x.example>" || len(r.References) != 2 || r.Snippet != "Yes please!" {
		t.Fatalf("reply: %+v", r)
	}
	b := msgs[1]
	if !b.Bounce || len(b.FailedRecipients) != 1 || b.FailedRecipients[0] != "gone@acme.example" {
		t.Fatalf("bounce: %+v", b)
	}
	// Nothing new: the cursor moves past what was read.
	again, cur2, err := Poll(a, cur)
	if err != nil || len(again) != 0 || cur2.LastUID != cur.LastUID {
		t.Fatalf("second poll: %v %d %+v %+v", err, len(again), cur, cur2)
	}
	if !strings.HasPrefix(r.ProviderID, "imap:me@x.example:") || strconv.Itoa(int(cur.LastUID)) == "0" {
		t.Fatalf("ids: %s %+v", r.ProviderID, cur)
	}
}

func TestPresets(t *testing.T) {
	a := &Account{Address: "Me@Gmail.com", Provider: "gmail", Password: "x"}
	if err := a.Apply(); err != nil || a.SMTPHost != "smtp.gmail.com" || !a.SavesSent || a.Username != "me@gmail.com" {
		t.Fatalf("%+v %v", a, err)
	}
	ic := &Account{Address: "jo@icloud.com", Provider: "icloud", Password: "x"}
	if ic.Apply(); ic.Username != "jo@icloud.com" || ic.IMAPUsername != "jo" {
		t.Fatalf("iCloud: SMTP uses the full address, IMAP the name only: %+v", ic)
	}
	zc := &Account{Address: "me@mycompany.com", Provider: "zoho", Password: "x"}
	zp := &Account{Address: "me@zohomail.com", Provider: "zoho", Password: "x"}
	zc.Apply()
	zp.Apply()
	if zc.SMTPHost != "smtppro.zoho.com" || zp.SMTPHost != "smtp.zoho.com" || zp.IMAPHost != "imap.zoho.com" {
		t.Fatalf("zoho hosts: %s / %s %s", zc.SMTPHost, zp.SMTPHost, zp.IMAPHost)
	}
	if err := (&Account{Address: "a@b.c", Provider: "custom", Password: "x"}).Apply(); err == nil {
		t.Fatal("custom without hosts must fail")
	}
}

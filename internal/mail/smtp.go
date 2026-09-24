package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"net/textproto"
	"time"

	"github.com/astrolabe-gtm/astrolabe/internal/channel"
)

// Sender sends through an account's SMTP server.
type Sender struct {
	Account *Account
}

func (s *Sender) connect(ctx context.Context) (*smtp.Client, error) {
	a := s.Account
	d := net.Dialer{Timeout: dialTimeout}
	var conn net.Conn
	var err error
	if a.SMTPTLS == "tls" && !a.Insecure {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: a.tlsConfig(a.SMTPHost)}).DialContext(ctx, "tcp", a.smtpAddr())
	} else {
		conn, err = d.DialContext(ctx, "tcp", a.smtpAddr())
	}
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(2 * time.Minute))
	}
	host := a.SMTPHost
	if a.Insecure {
		host = "localhost" // net/smtp only sends PLAIN auth unencrypted to localhost
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if a.SMTPTLS == "starttls" && !a.Insecure {
		if err := c.StartTLS(a.tlsConfig(a.SMTPHost)); err != nil {
			c.Close()
			return nil, fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if err := c.Auth(smtp.PlainAuth("", a.Username, a.Password, host)); err != nil {
		c.Close()
		return nil, fmt.Errorf("login failed (check the app password): %w", err)
	}
	return c, nil
}

// Test logs in without sending anything.
func (s *Sender) Test(ctx context.Context) error {
	c, err := s.connect(ctx)
	if err != nil {
		return err
	}
	return c.Quit()
}

// Send delivers one message. Any rejection, and any failure before the
// server acknowledged the end of the message, means it was not sent. Only
// losing the server's answer to the final "." leaves delivery unknown.
func (s *Sender) Send(ctx context.Context, m channel.Message) (channel.Result, error) {
	raw, err := channel.BuildMIME(m)
	if err != nil {
		return channel.Result{}, channel.NotSent("build message: %v", err)
	}
	c, err := s.connect(ctx)
	if err != nil {
		return channel.Result{}, channel.NotSent("smtp %s: %v", s.Account.SMTPHost, err)
	}
	defer c.Close()
	step := func(what string, err error) error {
		if err == nil {
			return nil
		}
		return channel.NotSent("smtp %s: %v", what, err)
	}
	if err := step("MAIL FROM", c.Mail(m.From)); err != nil {
		return channel.Result{}, err
	}
	if err := step("RCPT TO", c.Rcpt(m.To)); err != nil {
		return channel.Result{}, err
	}
	w, err := c.Data()
	if err := step("DATA", err); err != nil {
		return channel.Result{}, err
	}
	if _, err := w.Write(raw); err != nil {
		return channel.Result{}, channel.NotSent("smtp writing message: %v", err)
	}
	// Close sends the final "." and reads the server's verdict.
	if err := w.Close(); err != nil {
		var tp *textproto.Error
		if errors.As(err, &tp) {
			return channel.Result{}, channel.NotSent("smtp rejected the message: %v", err)
		}
		return channel.Result{}, fmt.Errorf("smtp: no answer after sending; delivery unknown: %w", err)
	}
	c.Quit()

	// Providers other than Gmail don't file SMTP sends in Sent: add a copy
	// so the conversation is visible there and "Check Sent mail" works.
	if !s.Account.SavesSent {
		if err := appendSent(ctx, s.Account, raw); err != nil {
			slog.Warn("could not save a copy to Sent", "mailbox", s.Account.Address, "err", err)
		}
	}
	thread := m.ThreadID
	if thread == "" {
		thread = m.MessageID
	}
	return channel.Result{ProviderID: m.MessageID, ThreadID: thread}, nil
}

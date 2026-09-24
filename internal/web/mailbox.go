package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/mail"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
)

func (srv *Server) mailStore() *mail.Store {
	if srv.Mail != nil {
		return srv.Mail
	}
	return &mail.Store{Dir: "/nonexistent"}
}

// mailboxAdd tests the login (SMTP and IMAP) and saves the mailbox.
func (srv *Server) mailboxAdd(w http.ResponseWriter, r *http.Request) {
	a := &mail.Account{Address: r.FormValue("address"), Provider: r.FormValue("provider"), Username: strings.TrimSpace(r.FormValue("username")),
		Password: strings.TrimSpace(r.FormValue("password")), SMTPHost: strings.TrimSpace(r.FormValue("smtp_host")),
		SMTPTLS: r.FormValue("smtp_tls"), IMAPHost: strings.TrimSpace(r.FormValue("imap_host")), SentFolder: strings.TrimSpace(r.FormValue("sent_folder"))}
	a.SMTPPort, _ = strconv.Atoi(r.FormValue("smtp_port"))
	a.IMAPPort, _ = strconv.Atoi(r.FormValue("imap_port"))
	bad := func(err error) { done(w, r, "/settings", "", fmt.Errorf("%w: %v", outreach.ErrInvalid, err)) }
	if err := a.Apply(); err != nil {
		bad(err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := (&mail.Sender{Account: a}).Test(ctx); err != nil {
		bad(fmt.Errorf("SMTP login failed, nothing saved: %v", err))
		return
	}
	if err := mail.TestIMAP(a); err != nil {
		bad(fmt.Errorf("IMAP login failed, nothing saved: %v", err))
		return
	}
	done(w, r, "/settings", "Mailbox "+a.Address+" connected (SMTP and IMAP login ok)", srv.mailStore().Save(a))
}

func (srv *Server) mailboxRemove(w http.ResponseWriter, r *http.Request) {
	done(w, r, "/settings", "Mailbox removed", srv.mailStore().Remove(r.FormValue("address")))
}

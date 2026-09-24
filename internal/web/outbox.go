package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/astrolabe-gtm/astrolabe/internal/outreach"
	"github.com/astrolabe-gtm/astrolabe/internal/sandbox"
)

func (srv *Server) outboxPage(w http.ResponseWriter, r *http.Request) {
	if srv.Mode != sandbox.Sandbox {
		http.NotFound(w, r)
		return
	}
	msgs, err := sandbox.Outbox(r.Context(), srv.S.Pool, 100)
	if err != nil {
		fail(w, err)
		return
	}
	srv.render(w, "outbox", srv.page(r, "outbox", map[string]any{"Messages": msgs}))
}

func (srv *Server) outboxReply(w http.ResponseWriter, r *http.Request) {
	if srv.Mode != sandbox.Sandbox {
		http.NotFound(w, r)
		return
	}
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		body = "Thanks, this looks useful. How do I get started?"
	}
	err := sandbox.SimulateReply(r.Context(), srv.S, pathID(r), body)
	if err != nil {
		err = fmt.Errorf("%w: %v", outreach.ErrInvalid, err)
	}
	done(w, r, "/outbox", "Reply delivered: the sequence stopped and the reply is in your Inbox to label", err)
}

func (srv *Server) outboxBounce(w http.ResponseWriter, r *http.Request) {
	if srv.Mode != sandbox.Sandbox {
		http.NotFound(w, r)
		return
	}
	err := sandbox.SimulateBounce(r.Context(), srv.S, pathID(r))
	if err != nil {
		err = fmt.Errorf("%w: %v", outreach.ErrInvalid, err)
	}
	done(w, r, "/outbox", "Bounced: the person is now on do-not-contact", err)
}

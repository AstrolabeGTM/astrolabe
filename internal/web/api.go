package web

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/views"
)

// apiRoute is one JSON endpoint of the browser app. Tool names the MCP tool
// that does the same thing through the same domain function: the browser
// app and MCP have the same capabilities, and a test keeps it that way.
type apiRoute struct {
	Pattern string
	Tool    string
	handle  http.HandlerFunc
}

// apiRoutes is the whole JSON API.
func (srv *Server) apiRoutes() []apiRoute {
	routes := []apiRoute{
		{"GET /api/bootstrap", "status", srv.apiBootstrap},
		{"POST /api/pause", "pause", srv.apiPause},
		{"POST /api/resume", "resume", srv.apiResume},
	}
	return slices.Concat(routes, srv.todayRoutes())
}

func (srv *Server) apiMux() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range srv.apiRoutes() {
		mux.HandleFunc(rt.Pattern, rt.handle)
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusNotFound, errBody("No such API endpoint."))
	})
	return mux
}

// apiGuard requires the login session for every API request. Requests that
// change something must also come from this site and carry the CSRF token
// from /api/bootstrap in X-CSRF-Token.
func (srv *Server) apiGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !srv.validSession(r) {
			reply(w, http.StatusUnauthorized, errBody("Sign in to continue."))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			tok := srv.csrfToken(r)
			if !fromThisSite(r) || tok == "" || !hmac.Equal([]byte(tok), []byte(r.Header.Get("X-CSRF-Token"))) {
				reply(w, http.StatusForbidden, errBody("Refused: this request did not come from the Astrolabe app. Reload the page and try again."))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- helpers ----

// apiErr is a problem the operator can fix, shown as it is.
type apiErr struct {
	status int
	msg    string
}

func (e *apiErr) Error() string { return e.msg }

func badRequest(format string, a ...any) error {
	return &apiErr{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

// invalid marks an error from a domain function as the operator's to fix.
func invalid(err error) error {
	if err == nil {
		return nil
	}
	return &apiErr{http.StatusBadRequest, err.Error()}
}

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("api: encoding the response failed", "err", err)
	}
}

func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		apiFail(w, err)
		return
	}
	reply(w, http.StatusOK, v)
}

func apiFail(w http.ResponseWriter, err error) {
	var ae *apiErr
	switch {
	case errors.As(err, &ae):
		reply(w, ae.status, errBody(ae.msg))
	case errors.Is(err, outreach.ErrStale):
		reply(w, http.StatusConflict, errBody(strings.TrimPrefix(err.Error(), outreach.ErrStale.Error()+": ")))
	case errors.Is(err, outreach.ErrInvalid):
		reply(w, http.StatusBadRequest, errBody(strings.TrimPrefix(err.Error(), outreach.ErrInvalid.Error()+": ")))
	case errors.Is(err, pgx.ErrNoRows):
		reply(w, http.StatusNotFound, errBody("Not found."))
	default:
		slog.Error("api request failed", "err", err)
		reply(w, http.StatusInternalServerError, errBody("Something went wrong: "+err.Error()))
	}
}

// readJSON decodes the request body into v; on failure it has answered.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		reply(w, http.StatusBadRequest, errBody("Invalid request: "+err.Error()))
		return false
	}
	return true
}

// idAndJSON reads the {id} path value and the JSON body; on failure it has
// answered.
func idAndJSON(w http.ResponseWriter, r *http.Request, v any) (int64, bool) {
	id, err := idParam(r)
	if err != nil {
		apiFail(w, err)
		return 0, false
	}
	return id, readJSON(w, r, v)
}

func idParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, badRequest("%q is not an id", r.PathValue("id"))
	}
	return id, nil
}

func (srv *Server) aiEnabled() bool { return srv.Writer != nil && srv.Writer.LLM.Enabled() }

func (srv *Server) loadErrors() []string {
	if srv.LoadErrors == nil {
		return nil
	}
	return srv.LoadErrors()
}

// ---- bootstrap ----

// apiBootstrap is the status every screen shows (mode, products, pauses)
// plus the CSRF token for this session.
func (srv *Server) apiBootstrap(w http.ResponseWriter, r *http.Request) {
	st, err := views.GetStatus(r.Context(), srv.S, srv.Mode, srv.Products(), srv.loadErrors(), srv.aiEnabled())
	if err != nil {
		apiFail(w, err)
		return
	}
	reply(w, http.StatusOK, struct {
		*views.Status
		CSRF string `json:"csrf_token"`
	}{st, srv.csrfToken(r)})
}

// ---- pauses ----

type scopeReq struct {
	Scope string `json:"scope"`
}

func (srv *Server) apiPause(w http.ResponseWriter, r *http.Request) {
	var req scopeReq
	if !readJSON(w, r, &req) {
		return
	}
	respond(w, status("Paused "+req.Scope+". Held sends wait; nothing is dropped."), srv.S.Pause(r.Context(), req.Scope))
}

func (srv *Server) apiResume(w http.ResponseWriter, r *http.Request) {
	var req scopeReq
	if !readJSON(w, r, &req) {
		return
	}
	respond(w, status("Resumed "+req.Scope+"."), srv.S.Resume(r.Context(), req.Scope))
}

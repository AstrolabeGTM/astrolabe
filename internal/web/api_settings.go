package web

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/views"
)

func (srv *Server) settingsRoutes() []apiRoute {
	return []apiRoute{
		{"GET /api/setup", "setup", srv.apiSetup},
		{"GET /api/doctor", "doctor", srv.apiDoctor},
		{"POST /api/products/reload", "reload_products", srv.apiReload},
	}
}

func (srv *Server) apiSetup(w http.ResponseWriter, r *http.Request) {
	s, err := views.GetSetup(r.Context(), srv.S, srv.Mode, srv.Products(), srv.loadIssues(), srv.mailStore(), srv.Accounts)
	respond(w, s, err)
}

// apiDoctor runs the setup checks; connect=1 also tests logins, keys and
// DNS, so the app only asks for it from a button.
func (srv *Server) apiDoctor(w http.ResponseWriter, r *http.Request) {
	if srv.Doctor == nil {
		apiFail(w, badRequest("Doctor is not available in this process; run astrolabe doctor."))
		return
	}
	connect := r.URL.Query().Get("connect") == "1"
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	reply(w, http.StatusOK, map[string]any{"checks": srv.Doctor(ctx, connect), "connect": connect})
}

func (srv *Server) apiReload(w http.ResponseWriter, r *http.Request) {
	if srv.Reload == nil {
		apiFail(w, badRequest("Reloading is not available in this process."))
		return
	}
	err := srv.Reload(r.Context())
	issues := srv.loadIssues()
	if err != nil && len(issues) == 0 {
		apiFail(w, err)
		return
	}
	msg := fmt.Sprintf("Reloaded: %d products loaded.", len(srv.Products()))
	if len(issues) > 0 {
		msg += fmt.Sprintf(" %d did not load; the problems are listed below.", len(issues))
	}
	reply(w, http.StatusOK, map[string]any{"status": msg, "load_problems": issues})
}

package web

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/signal"
)

func (srv *Server) signalRoutes() []apiRoute {
	return []apiRoute{
		{"GET /api/signals", "signals", srv.apiSignals},
		{"POST /api/signals", "add_signal", srv.apiAddSignal},
		{"GET /api/monitors", "monitors", srv.apiMonitors},
		{"POST /api/monitors/run", "run_monitor", srv.apiRunMonitor},
		{"GET /api/reviews", "ambiguous_matches", srv.apiReviews},
		{"POST /api/reviews/{id}/resolve", "resolve_match", srv.apiResolveReview},
	}
}

func (srv *Server) apiSignals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days, _ := strconv.Atoi(q.Get("days"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	feed, err := signal.Recent(r.Context(), srv.S.Pool, signal.Filter{Product: q.Get("product"), Type: q.Get("type"), MinTier: q.Get("tier"),
		Days: min(max(days, 0), 365), Limit: min(max(limit, 0), 500)}, time.Now())
	respond(w, feed, err)
}

func (srv *Server) apiAddSignal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product  string `json:"product"`
		Who      string `json:"who"`
		Type     string `json:"type"`
		Strength int    `json:"strength"`
		WhyNow   bool   `json:"why_now"`
		Note     string `json:"note"`
		URL      string `json:"url"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if _, err := srv.product(req.Product); err != nil {
		apiFail(w, err)
		return
	}
	res, err := srv.Signals.AddManual(r.Context(), signal.Manual{Product: req.Product, Who: req.Who, Type: req.Type, Strength: req.Strength,
		WhyNow: req.WhyNow, Note: req.Note, URL: req.URL}, time.Now())
	if err != nil {
		apiFail(w, invalid(err))
		return
	}
	msg := "Signal added and the person rescored. Automations that trigger on it run within a minute."
	if res.Ambiguous {
		msg = "Signal added, but it matches more than one person: pick one under Ambiguous matches."
	}
	reply(w, http.StatusOK, map[string]any{"status": msg, "signal_id": res.SignalID, "person_id": res.PersonID})
}

func (srv *Server) apiMonitors(w http.ResponseWriter, r *http.Request) {
	list, err := signal.Monitors(r.Context(), srv.S.Pool, srv.Products(), time.Now())
	respond(w, map[string]any{"monitors": list}, err)
}

// apiRunMonitor queues the monitor; the job runs signal.RunMonitor, as the
// MCP tool run_monitor does directly.
func (srv *Server) apiRunMonitor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product string `json:"product"`
		Monitor string `json:"monitor"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	p, err := srv.product(req.Product)
	if err != nil {
		apiFail(w, err)
		return
	}
	found := false
	for _, m := range p.Monitors {
		found = found || m.ID == req.Monitor
	}
	if !found {
		apiFail(w, badRequest("%s has no monitor %q.", p.Name, req.Monitor))
		return
	}
	_, err = srv.Signals.River.Insert(r.Context(), signal.MonitorArgs{Product: req.Product, Monitor: req.Monitor}, nil)
	respond(w, status(fmt.Sprintf("Running %s now. Its result shows here when it finishes, usually within a minute.", req.Monitor)), err)
}

func (srv *Server) apiReviews(w http.ResponseWriter, r *http.Request) {
	list, total, err := signal.OpenReviews(r.Context(), srv.S.Pool, 200)
	respond(w, map[string]any{"reviews": list, "total": total}, err)
}

func (srv *Server) apiResolveReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PersonID int64 `json:"person_id"`
	}
	id, ok := idAndJSON(w, r, &req)
	if !ok {
		return
	}
	if req.PersonID == 0 {
		respond(w, status("Dismissed: the signal stays unattached."), srv.Signals.Dismiss(r.Context(), id))
		return
	}
	respond(w, status("Attached the signal to the person you picked."), invalid(srv.Signals.Resolve(r.Context(), id, req.PersonID)))
}

package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/views"
)

func (srv *Server) peopleRoutes() []apiRoute {
	return []apiRoute{
		{"GET /api/people", "people", srv.apiPeople},
		{"POST /api/people/import", "import_people", srv.apiImportPeople},
		{"POST /api/companies/import", "import_companies", srv.apiImportCompanies},
		{"GET /api/people/{id}", "person", srv.apiPerson},
		{"POST /api/people/{id}/facts", "set_facts", srv.apiSetFacts},
		{"POST /api/people/{id}/do-not-contact", "do_not_contact", srv.apiDoNotContact},
		{"POST /api/people/{id}/research", "research", srv.apiResearch},
		{"POST /api/sequences/start", "start_sequence", srv.apiStartSequence},
	}
}

func (srv *Server) apiPeople(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if _, err := srv.product(q.Get("product")); err != nil {
		apiFail(w, err)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := views.GetPeople(r.Context(), srv.S.Pool, q.Get("product"), q.Get("q"), min(max(limit, 0), 1000))
	respond(w, list, err)
}

type csvReq struct {
	Product string `json:"product"`
	CSV     string `json:"csv"`
}

func (srv *Server) apiImportPeople(w http.ResponseWriter, r *http.Request) {
	var req csvReq
	if !readJSON(w, r, &req) {
		return
	}
	if _, err := srv.product(req.Product); err != nil {
		apiFail(w, err)
		return
	}
	res, err := views.ImportPeople(r.Context(), srv.S.Pool, req.Product, req.CSV)
	msg := fmt.Sprintf("Imported %d rows: %d new, %d updated, %d skipped.", res.Created+res.Updated+len(res.Skipped), res.Created, res.Updated, len(res.Skipped))
	respond(w, map[string]any{"status": msg, "created": res.Created, "updated": res.Updated, "skipped": res.Skipped}, err)
}

func (srv *Server) apiImportCompanies(w http.ResponseWriter, r *http.Request) {
	var req csvReq
	if !readJSON(w, r, &req) {
		return
	}
	p, err := srv.product(req.Product)
	if err != nil {
		apiFail(w, err)
		return
	}
	res, err := views.ImportCompanies(r.Context(), srv.S.Pool, p, req.CSV)
	if err != nil {
		apiFail(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"status": fmt.Sprintf("Imported %d companies: %d new, %d updated, %d skipped.",
		res.Created+res.Updated+len(res.Skipped), res.Created, res.Updated, len(res.Skipped)), "result": res})
}

func (srv *Server) apiPerson(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		apiFail(w, err)
		return
	}
	v, err := views.GetPerson(r.Context(), srv.S, srv.Products(), id, time.Now())
	respond(w, v, err)
}

func (srv *Server) apiSetFacts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Facts string `json:"facts"`
	}
	id, ok := idAndJSON(w, r, &req)
	if !ok {
		return
	}
	facts := views.SplitFacts(req.Facts)
	if len(facts) == 0 {
		apiFail(w, badRequest("Type at least one fact, like dep:bullmq or team:5-50."))
		return
	}
	all, err := views.SetFacts(r.Context(), srv.S.Pool, id, facts)
	respond(w, map[string]any{"status": fmt.Sprintf("Saved %d facts; scores are updated.", len(facts)), "facts": all}, err)
}

func (srv *Server) apiDoNotContact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Allow  bool   `json:"allow"`
		Reason string `json:"reason"`
	}
	id, ok := idAndJSON(w, r, &req)
	if !ok {
		return
	}
	if req.Allow {
		respond(w, status("Removed from the do-not-contact list. Sequences you start again can reach them."), people.Unsuppress(r.Context(), srv.S.Pool, id))
		return
	}
	respond(w, status("On the do-not-contact list: no product will message them, and their unsent messages are held."),
		people.Suppress(r.Context(), srv.S.Pool, id, "manual", strings.TrimSpace(req.Reason)))
}

func (srv *Server) apiResearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product string `json:"product"`
	}
	id, ok := idAndJSON(w, r, &req)
	if !ok {
		return
	}
	if !srv.aiEnabled() {
		apiFail(w, badRequest("Research needs the Claude API: set ASTROLABE_ANTHROPIC_API_KEY."))
		return
	}
	if _, err := srv.product(req.Product); err != nil {
		apiFail(w, err)
		return
	}
	_, err := srv.Writer.Brief(r.Context(), req.Product, id, true)
	respond(w, status("Research notes written."), err)
}

func (srv *Server) apiStartSequence(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product   string  `json:"product"`
		Sequence  string  `json:"sequence"`
		PersonIDs []int64 `json:"person_ids"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if _, err := srv.product(req.Product); err != nil {
		apiFail(w, err)
		return
	}
	res, err := views.StartSequence(r.Context(), srv.S, req.Product, req.Sequence, req.PersonIDs)
	if err != nil {
		apiFail(w, err)
		return
	}
	msg := fmt.Sprintf("Started %s for %d of %d; first drafts are on Today.", req.Sequence, len(res.Enrolled), len(req.PersonIDs))
	if len(res.Enrolled) == 0 {
		msg = fmt.Sprintf("Started %s for nobody: all %d were refused.", req.Sequence, len(req.PersonIDs))
	}
	if res.Skipped == nil {
		res.Skipped = []string{}
	}
	reply(w, http.StatusOK, map[string]any{"status": msg, "started": len(res.Enrolled), "skipped": res.Skipped})
}

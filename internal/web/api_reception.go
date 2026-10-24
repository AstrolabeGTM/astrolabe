package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/reception"
	"github.com/AstrolabeGTM/astrolabe/internal/views"
)

func (srv *Server) receptionRoutes() []apiRoute {
	return []apiRoute{
		{"GET /api/counters", "counters", srv.apiCounters},
		{"GET /api/reception", "reception", srv.apiReception},
		{"POST /api/mentions", "log_mention", srv.apiLogMention},
		{"POST /api/mentions/{id}/tone", "set_tone", srv.apiSetTone},
	}
}

// productsFor is one product, or all when id is "".
func (srv *Server) productsFor(id string) ([]*product.Product, error) {
	if id == "" {
		return srv.Products(), nil
	}
	p, err := srv.product(id)
	if err != nil {
		return nil, err
	}
	return []*product.Product{p}, nil
}

func (srv *Server) apiCounters(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("product")
	ps, err := srv.productsFor(id)
	if err != nil {
		apiFail(w, err)
		return
	}
	out, err := views.Counters(r.Context(), srv.S.Pool, ps, id != "", time.Now())
	respond(w, map[string]any{"counters": out}, err)
}

func (srv *Server) apiReception(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ps, err := srv.productsFor(q.Get("product"))
	if err != nil {
		apiFail(w, err)
		return
	}
	days, _ := strconv.Atoi(q.Get("days"))
	out, err := views.Reception(r.Context(), srv.S.Pool, ps, min(max(days, 0), 365), time.Now(), 200)
	respond(w, map[string]any{"reception": out}, err)
}

func (srv *Server) apiLogMention(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product      string `json:"product"`
		URL          string `json:"url"`
		Title        string `json:"title"`
		Source       string `json:"source"`
		Date         string `json:"date"`
		Tone         string `json:"tone"`
		AsksQuestion bool   `json:"asks_question"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if _, err := srv.product(req.Product); err != nil {
		apiFail(w, err)
		return
	}
	at := time.Now()
	if req.Date != "" {
		var err error
		if at, err = time.Parse("2006-01-02", req.Date); err != nil {
			apiFail(w, badRequest("The date must look like 2026-11-30."))
			return
		}
	}
	id, created, err := reception.Add(r.Context(), srv.S.Pool, reception.Mention{Product: req.Product, URL: req.URL, Title: req.Title,
		Source: req.Source, MentionedAt: at, Tone: req.Tone, AsksQuestion: req.AsksQuestion})
	msg := "Mention logged."
	if !created {
		msg = "That link was already in the log; it is kept once, as it was."
	}
	respond(w, map[string]any{"status": msg, "id": id, "created": created}, invalid(err))
}

func (srv *Server) apiSetTone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tone string `json:"tone"`
	}
	id, ok := idAndJSON(w, r, &req)
	if !ok {
		return
	}
	respond(w, status("Tone set to "+req.Tone+"."), invalid(reception.SetTone(r.Context(), srv.S.Pool, id, req.Tone)))
}

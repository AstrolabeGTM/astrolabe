package web

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/campaign"
)

func (srv *Server) campaignRoutes() []apiRoute {
	return []apiRoute{
		srv.toolRoute("GET /api/campaigns", "campaigns"),
		srv.toolRoute("GET /api/campaigns/json", "campaigns_json"),
		srv.toolRoute("GET /api/campaign", "campaign"),
		srv.toolRoute("GET /api/campaign/history", "campaign_history"),
		srv.toolRoute("GET /api/campaign/report", "campaign_report"),
		srv.toolRoute("POST /api/campaign/set", "set_campaign"),
		srv.toolRoute("POST /api/campaign/gate", "check_gate"),
		srv.toolRoute("POST /api/campaign/spend", "record_spend"),
		srv.toolRoute("POST /api/campaign/spend/import", "import_spend"),
		srv.toolRoute("POST /api/campaign/tasks", "make_campaign_tasks"),
		srv.toolRoute("POST /api/campaign/export", "export_pack"),
		srv.toolRoute("GET /api/campaign/creatives", "creatives"),
		srv.toolRoute("POST /api/campaign/creatives/render", "render_creative"),
		srv.toolRoute("POST /api/campaign/creatives/edit", "edit_creative"),
		srv.toolRoute("POST /api/campaign/creatives/approve", "approve_creative"),
		{"GET /api/campaign/creatives/preview", "creative_preview", srv.apiCreativePreview},
		{"GET /api/campaign/creatives/file", "creatives", srv.apiCreativeFile},
	}
}

// apiCreativePreview serves creative_preview's HTML as a document for a
// sandboxed frame: no scripts, nothing fetched, only shown inside the app.
func (srv *Server) apiCreativePreview(w http.ResponseWriter, r *http.Request) {
	h, ok := srv.Tools["creative_preview"]
	if !ok {
		apiFail(w, badRequest("Previews need the creative_preview tool, which this process does not offer."))
		return
	}
	args, err := queryArgs(r.URL.Query(), h.In)
	if err != nil {
		apiFail(w, badRequest("%v", err))
		return
	}
	out, err := h.Call(r.Context(), args)
	if err != nil {
		toolFail(w, "creative_preview", err)
		return
	}
	doc, _ := out.(map[string]any)["html"].(string)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; font-src data:; frame-ancestors 'self'; sandbox")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(doc))
}

// apiCreativeFile serves one rendered file of a creative (the files the
// creatives tool lists under renders).
func (srv *Server) apiCreativeFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := srv.product(q.Get("product"))
	if err != nil {
		apiFail(w, err)
		return
	}
	c := p.Campaign(q.Get("campaign"))
	if c == nil {
		apiFail(w, badRequest("No campaign %q.", q.Get("campaign")))
		return
	}
	cr := c.Creative(q.Get("creative"))
	if cr == nil {
		apiFail(w, badRequest("No creative %q.", q.Get("creative")))
		return
	}
	format := q.Get("format")
	if format == "" {
		format = "png"
	}
	b, err := campaign.RenderedFile(r.Context(), srv.S.Pool, p, c, cr, q.Get("size"), format)
	if errors.Is(err, pgx.ErrNoRows) {
		apiFail(w, &apiErr{http.StatusNotFound, "Not rendered at that size yet."})
		return
	} else if err != nil {
		apiFail(w, err)
		return
	}
	w.Header().Set("Content-Type", map[string]string{"png": "image/png", "pdf": "application/pdf"}[format])
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Write(b)
}

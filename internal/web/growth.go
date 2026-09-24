package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/content"
	"github.com/AstrolabeGTM/astrolabe/internal/digest"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
)

func (srv *Server) contentPage(w http.ResponseWriter, r *http.Request) {
	items, err := srv.Studio.List(r.Context(), r.FormValue("product"), 100)
	if err != nil {
		fail(w, err)
		return
	}
	var platforms []string
	for p := range content.Platforms {
		platforms = append(platforms, p)
	}
	srv.render(w, "content", srv.page(r, "content", map[string]any{
		"Items": items, "Platforms": platforms, "Launch": content.LaunchTargets, "AI": srv.Studio.LLM.Enabled(),
	}))
}

func (srv *Server) contentCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	platforms := r.Form["platform"]
	launch := r.FormValue("launch") == "on"
	if launch && len(platforms) == 0 {
		platforms = content.LaunchTargets
	}
	ids, err := srv.Studio.Repurpose(r.Context(), r.FormValue("product"), strings.TrimSpace(r.FormValue("batch")),
		r.FormValue("source"), platforms, launch)
	if err != nil {
		err = fmt.Errorf("%w: %v", outreach.ErrInvalid, err)
	}
	done(w, r, "/content", fmt.Sprintf("Drafted %d posts", len(ids)), err)
}

func (srv *Server) contentMark(w http.ResponseWriter, r *http.Request) {
	err := srv.Studio.Mark(r.Context(), pathID(r), r.FormValue("status"), strings.TrimSpace(r.FormValue("url")))
	if err != nil {
		err = fmt.Errorf("%w: %v", outreach.ErrInvalid, err)
	}
	done(w, r, "/content", "Updated", err)
}

func (srv *Server) digestPage(w http.ResponseWriter, r *http.Request) {
	body, at, err := digest.Latest(r.Context(), srv.S.Pool)
	if err != nil {
		fail(w, err)
		return
	}
	preview := ""
	if r.URL.Query().Has("preview") {
		d, err := digest.Build(r.Context(), srv.S.Pool, srv.Products(), time.Now())
		if err != nil {
			fail(w, err)
			return
		}
		preview = d.Text()
	}
	srv.render(w, "digest", srv.page(r, "digest", map[string]any{"Body": body, "At": at, "Preview": preview}))
}

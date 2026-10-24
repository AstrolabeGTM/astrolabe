package updates

import (
	"errors"
	"fmt"
	"html"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

// Routes registers the two public endpoints, for installs with
// ASTROLABE_PUBLIC_URL. They need no login: the signed token in the link is
// the credential. A GET only shows a button, because mail scanners open
// every link in a message; the POST does the work (it is also what a mail
// provider sends for one-click unsubscribe, RFC 8058).
func (s *Service) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /updates/confirm/{token}", s.page("confirm"))
	mux.HandleFunc("POST /updates/confirm/{token}", s.act("confirm"))
	mux.HandleFunc("GET /updates/unsubscribe/{token}", s.page("unsubscribe"))
	mux.HandleFunc("POST /updates/unsubscribe/{token}", s.act("unsubscribe"))
}

func write(w http.ResponseWriter, status int, title, text, button string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	form := ""
	if button != "" {
		form = `<form method="post"><button type="submit">` + html.EscapeString(button) + `</button></form>`
	}
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>
<body style="font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:3rem auto;padding:0 1rem"><h1 style="font-size:1.25rem">%s</h1><p>%s</p>%s</body>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(text), form)
}

func (s *Service) listName(r *http.Request, id int64) string {
	var pid string
	if s.Pool.QueryRow(r.Context(), `SELECT product_id FROM updates_subscribers WHERE id = $1`, id).Scan(&pid) != nil {
		return ""
	}
	if p, err := product.Get(r.Context(), s.Pool, pid); err == nil {
		if u, ok := p.UpdatesSettings(); ok {
			return u.Name
		}
	}
	return ""
}

func (s *Service) page(purpose string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.parseToken(r.Context(), purpose, r.PathValue("token"))
		if !ok {
			write(w, http.StatusNotFound, "Link not recognised", "This link is not valid. If you copied it, check that none of it is missing.", "")
			return
		}
		name := s.listName(r, id)
		if name == "" {
			write(w, http.StatusNotFound, "Link not recognised", "This list no longer exists.", "")
			return
		}
		if purpose == "confirm" {
			write(w, http.StatusOK, "Confirm: "+name, "Press the button to confirm that you want to receive "+name+".", "Confirm")
			return
		}
		write(w, http.StatusOK, "Unsubscribe", "Press the button to stop receiving "+name+".", "Unsubscribe")
	}
}

func (s *Service) act(purpose string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.parseToken(r.Context(), purpose, r.PathValue("token"))
		if !ok {
			write(w, http.StatusNotFound, "Link not recognised", "This link is not valid.", "")
			return
		}
		name := s.listName(r, id)
		var err error
		if purpose == "confirm" {
			var tx pgx.Tx
			if tx, err = s.Pool.Begin(r.Context()); err == nil {
				defer tx.Rollback(r.Context())
				if _, err = s.Confirm(r.Context(), tx, id, "link"); err == nil {
					err = tx.Commit(r.Context())
				}
			}
			switch {
			case errors.Is(err, ErrExpired):
				write(w, http.StatusGone, "This request has expired", "The confirmation request is too old. Please sign up again.", "")
			case err != nil:
				write(w, http.StatusInternalServerError, "Something went wrong", "Please try again.", "")
			default:
				write(w, http.StatusOK, "You're confirmed", "You are now subscribed to "+name+". You can unsubscribe from any message.", "")
			}
			return
		}
		via := "link"
		if r.PostFormValue("List-Unsubscribe") == "One-Click" {
			via = "one-click"
		}
		if err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
			_, err := s.Unsubscribe(r.Context(), tx, id, via, false)
			return err
		}); err != nil {
			write(w, http.StatusInternalServerError, "Something went wrong", "Please try again.", "")
			return
		}
		write(w, http.StatusOK, "You're unsubscribed", "You will not get any more "+name+".", "")
	}
}

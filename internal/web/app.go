package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// The browser app: plain HTML, CSS and ES modules embedded in the binary,
// with no build step. It talks to the JSON API under /api/.
//
//go:embed app
var appFS embed.FS

// appHandler serves /app/ (index.html) and its files under /app/.
func appHandler() http.Handler {
	static, err := fs.Sub(appFS, "app")
	if err != nil {
		panic(err)
	}
	index, err := appFS.ReadFile("app/index.html")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/app/", http.FileServer(http.FS(static)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/app/" || r.URL.Path == "/app/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			if r.Method == http.MethodGet {
				w.Write(index)
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r) // no directory listings
			return
		}
		// Revalidate on every load: the files change with each release.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

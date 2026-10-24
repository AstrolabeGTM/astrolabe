package render

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRenderSendsViewportAndFiles(t *testing.T) {
	var gotPath string
	fields := map[string]string{}
	files := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		for k, v := range r.MultipartForm.Value {
			fields[k] = v[0]
		}
		for _, fh := range r.MultipartForm.File["files"] {
			f, _ := fh.Open()
			b, _ := io.ReadAll(f)
			files[fh.Filename] = string(b)
		}
		w.Write([]byte("\x89PNG fake"))
	}))
	defer srv.Close()
	c := New(srv.URL + "/")
	out, err := c.Render(context.Background(), Page{HTML: "<html><head></head><body>hi</body></html>", Assets: map[string][]byte{"a.css": []byte("b{}")}}, 1200, 627, "png")
	if err != nil || !strings.HasPrefix(string(out), "\x89PNG") {
		t.Fatalf("%v %q", err, out)
	}
	if gotPath != "/forms/chromium/screenshot/html" || fields["width"] != "1200" || fields["height"] != "627" || fields["format"] != "png" || fields["clip"] != "true" {
		t.Errorf("%s %v", gotPath, fields)
	}
	if !strings.Contains(files["index.html"], "@page{size:1200px 627px") || files["a.css"] != "b{}" {
		t.Errorf("files: %v", files)
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte("%PDF-1.4"))
	}))
	defer srv2.Close()
	if _, err := New(srv2.URL).Render(context.Background(), Page{HTML: "x"}, 100, 100, "pdf"); err != nil || gotPath != "/forms/chromium/convert/html" {
		t.Errorf("%v %s", err, gotPath)
	}
}

func TestRenderErrorsAreClear(t *testing.T) {
	if _, err := New("").Render(context.Background(), Page{}, 100, 100, "png"); err == nil || !strings.Contains(err.Error(), "docker compose --profile creative up") {
		t.Errorf("no renderer: %v", err)
	}
	if _, err := New("http://127.0.0.1:1").Render(context.Background(), Page{}, 100, 100, "png"); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("unreachable: %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }))
	defer bad.Close()
	if _, err := New(bad.URL).Render(context.Background(), Page{HTML: "x"}, 100, 100, "png"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("500: %v", err)
	}
	notPNG := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) }))
	defer notPNG.Close()
	if _, err := New(notPNG.URL).Render(context.Background(), Page{HTML: "x"}, 100, 100, "png"); err == nil {
		t.Error("a non-PNG answer must be an error")
	}
	if _, err := New("http://x").Render(context.Background(), Page{}, 100, 100, "gif"); err == nil {
		t.Error("bad format")
	}
}

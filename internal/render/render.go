// Package render turns an HTML page into a PNG or PDF at an exact pixel size
// by calling an optional headless-Chromium sidecar over HTTP. It speaks
// Gotenberg 8's API (gotenberg/gotenberg:8): POST /forms/chromium/screenshot/html
// for PNG and /forms/chromium/convert/html for PDF. No image library is
// linked into Astrolabe, and without ASTROLABE_RENDER_URL nothing here runs.
package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HowToEnable is what to tell someone who needs the renderer.
const HowToEnable = "Rendering needs the optional Chromium sidecar: run `docker compose --profile creative up -d` and set ASTROLABE_RENDER_URL=http://gotenberg:3000 (see docs/setup.md). Copy, checks and approval work without it."

// ErrNoRenderer is returned when no renderer is configured.
var ErrNoRenderer = errors.New(HowToEnable)

type Client struct {
	URL  string
	HTTP *http.Client
}

// New returns a client for the sidecar at url; with an empty url the client
// is disabled and Render says so plainly.
func New(url string) *Client {
	return &Client{URL: strings.TrimRight(url, "/"), HTTP: &http.Client{Timeout: 90 * time.Second}}
}

// Enabled reports whether a renderer is configured.
func (c *Client) Enabled() bool { return c != nil && c.URL != "" }

// Page is one HTML document and the files it uses (CSS, images, fonts),
// which sit beside it by base name.
type Page struct {
	HTML   string
	Assets map[string][]byte
}

// Render returns the page as "png" or "pdf" at exactly width x height pixels.
func (c *Client) Render(ctx context.Context, pg Page, width, height int, format string) ([]byte, error) {
	if !c.Enabled() {
		return nil, ErrNoRenderer
	}
	var path string
	fields := map[string]string{}
	switch format {
	case "png":
		path = "/forms/chromium/screenshot/html"
		// clip makes the image exactly the viewport; without it Chromium adds a few pixels.
		fields = map[string]string{"width": strconv.Itoa(width), "height": strconv.Itoa(height), "format": "png", "clip": "true"}
	case "pdf":
		path = "/forms/chromium/convert/html"
		fields = map[string]string{"preferCssPageSize": "true", "printBackground": "true"}
	default:
		return nil, fmt.Errorf("format %q must be png or pdf", format)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part := func(name string, data []byte) error {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files"; filename=%q`, name))
		h.Set("Content-Type", "application/octet-stream")
		w, err := mw.CreatePart(h)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	if err := part("index.html", []byte(Fit(pg.HTML, width, height))); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(pg.Assets))
	for n := range pg.Assets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := part(n, pg.Assets[n]); err != nil {
			return nil, err
		}
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := mw.WriteField(k, fields[k]); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+path, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the renderer at %s is not reachable (%v). %s", c.URL, err, HowToEnable)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("the renderer answered %d: %s", resp.StatusCode, msg)
	}
	magic := map[string]string{"png": "\x89PNG", "pdf": "%PDF"}[format]
	if !bytes.HasPrefix(out, []byte(magic)) {
		return nil, fmt.Errorf("the renderer answered 200 but not with a %s file", format)
	}
	return out, nil
}

// Fit pins the page to the viewport so the output is exactly one page of the
// requested size: no margin, no scroll, and a print page of the same size.
func Fit(html string, width, height int) string {
	style := fmt.Sprintf(`<style>@page{size:%dpx %dpx;margin:0}html{width:%dpx;height:%dpx;overflow:hidden}body{margin:0;width:%dpx;height:%dpx;overflow:hidden}</style>`,
		width, height, width, height, width, height)
	if i := strings.Index(strings.ToLower(html), "</head>"); i >= 0 {
		return html[:i] + style + html[i:]
	}
	return style + html
}

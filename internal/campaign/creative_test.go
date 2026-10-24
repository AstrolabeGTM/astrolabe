package campaign

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/content"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/render"
)

// fakeRenderer answers like Gotenberg: a "PNG" that records the viewport and
// the HTML it was asked to render.
func fakeRenderer(t *testing.T) (*render.Client, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1 << 20)
		f, _ := r.MultipartForm.File["files"][0].Open()
		buf := make([]byte, 4096)
		n, _ := f.Read(buf)
		mu.Lock()
		seen = append(seen, string(buf[:n]))
		mu.Unlock()
		if r.URL.Path == "/forms/chromium/convert/html" {
			fmt.Fprintf(w, "%%PDF-fake %s", string(buf[:n]))
			return
		}
		fmt.Fprintf(w, "\x89PNG %sx%s %s", r.MultipartForm.Value["width"][0], r.MultipartForm.Value["height"][0], string(buf[:n]))
	}))
	t.Cleanup(srv.Close)
	return render.New(srv.URL), &seen
}

func TestCreativeRendersTwoSizesAndEditWithdrawsApproval(t *testing.T) {
	ctx := context.Background()
	pool, p, c := fixture(t)
	cr := c.Creative("card")
	rc, seen := fakeRenderer(t)

	v, err := RenderCreative(ctx, pool, rc, p, c, cr, "me")
	if err != nil {
		t.Fatal(err)
	}
	// Two sizes x (png, pdf) = four files, each at its own size.
	if len(v.Renders) != 4 || len(v.Missing) != 0 {
		t.Fatalf("%+v", v.Renders)
	}
	png1, _ := RenderedFile(ctx, pool, p, c, cr, "1200x630", "png")
	png2, _ := RenderedFile(ctx, pool, p, c, cr, "1080x1080", "png")
	if !strings.HasPrefix(string(png1), "\x89PNG 1200x630 ") || !strings.HasPrefix(string(png2), "\x89PNG 1080x1080 ") {
		t.Errorf("sizes: %.20q %.20q", png1, png2)
	}
	if !strings.Contains(string(png1), "width:1200px;height:630px") || !strings.Contains(string(png1), "<h1>Do one thing</h1>") ||
		!strings.Contains(string(png1), `alt="A card that says do one thing."`) || !strings.Contains(string(png1), `href="creative.css"`) {
		t.Errorf("template not filled: %s", png1)
	}
	if len(*seen) != 4 {
		t.Errorf("renderer calls: %d", len(*seen))
	}

	v, err = ApproveCreative(ctx, pool, p, c, cr, "me")
	if err != nil || !v.Approved || v.ApprovedBy != "me" {
		t.Fatalf("%v %+v", err, v)
	}
	hash := v.Hash

	// A no-op edit changes nothing.
	same := "Do one thing"
	if v, _ = EditCreative(ctx, pool, p, c, cr, "me", map[string]string{"headline": same}, nil); !v.Approved {
		t.Error("setting a field to what it is must not withdraw approval")
	}
	// Any real edit withdraws approval and drops the stale files.
	v, err = EditCreative(ctx, pool, p, c, cr, "me", map[string]string{"headline": "Do one thing, well"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Approved || len(v.Renders) != 0 || v.Hash == hash || v.Version != 3 || v.Status != "not approved" {
		t.Errorf("edit must withdraw approval: %+v", v)
	}
	if _, err := EditCreative(ctx, pool, p, c, cr, "me", map[string]string{"nope": "x"}, nil); err == nil {
		t.Error("unknown field")
	}
	// Alt text edits count too.
	RenderCreative(ctx, pool, rc, p, c, cr, "me")
	ApproveCreative(ctx, pool, p, c, cr, "me")
	alt := "New alt"
	if v, _ = EditCreative(ctx, pool, p, c, cr, "me", nil, &alt); v.Approved {
		t.Error("alt edit must withdraw approval")
	}
	// Re-rendering withdraws approval too: the files are new.
	RenderCreative(ctx, pool, rc, p, c, cr, "me")
	v, _ = ApproveCreative(ctx, pool, p, c, cr, "me")
	if !v.Approved {
		t.Fatal("approve again")
	}
	if v, _ = RenderCreative(ctx, pool, rc, p, c, cr, "me"); v.Approved {
		t.Error("a re-render must withdraw approval")
	}
	// A changed template on disk also voids an approval (the hash no longer matches).
	ApproveCreative(ctx, pool, p, c, cr, "me")
	cr2 := *cr
	cr2.TemplateText += "<!-- changed -->"
	if v, _ = View(ctx, pool, p, c, &cr2); v.Approved || !strings.Contains(v.Status, "withdrawn") {
		t.Errorf("template change: %+v", v)
	}
}

func TestWithoutRendererCopyStillWorks(t *testing.T) {
	ctx := context.Background()
	pool, p, c := fixture(t)
	cr := c.Creative("card")
	_, err := RenderCreative(ctx, pool, render.New(""), p, c, cr, "me")
	if err == nil || !strings.Contains(err.Error(), "docker compose --profile creative up") || !strings.Contains(err.Error(), "ASTROLABE_RENDER_URL") {
		t.Fatalf("must say plainly what is needed: %v", err)
	}
	v, err := ApproveCreative(ctx, pool, p, c, cr, "me")
	if err != nil || !v.Approved || !strings.Contains(v.Status, "copy only") {
		t.Fatalf("copy-only approval: %v %+v", err, v)
	}
	v, _ = EditCreative(ctx, pool, p, c, cr, "me", map[string]string{"headline": "Changed"}, nil)
	if v.Approved {
		t.Error("edit withdraws copy-only approval too")
	}
	// An unreachable renderer is also plain.
	if _, err := RenderCreative(ctx, pool, render.New("http://127.0.0.1:1"), p, c, cr, "me"); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("%v", err)
	}
}

func TestFlaggedCopyWaits(t *testing.T) {
	ctx := context.Background()
	pool, p, c := fixture(t)
	cr := c.Creative("card")
	rc, _ := fakeRenderer(t)
	for _, tc := range []struct{ copy, flag string }{
		{"Guaranteed to work", "never-claim: guaranteed"},
		{"Cuts CI time by 40%", "number without a source: 40%"},
		{"Fill me [Name]", "placeholder"},
		{"   ", "is empty"},
	} {
		v, err := EditCreative(ctx, pool, p, c, cr, "me", map[string]string{"headline": tc.copy}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(v.Flags, "|"), tc.flag) {
			t.Errorf("%q: flags %v, want %q", tc.copy, v.Flags, tc.flag)
		}
		if _, err := ApproveCreative(ctx, pool, p, c, cr, "me"); err == nil || !strings.Contains(err.Error(), "waits") {
			t.Errorf("%q must not be approvable: %v", tc.copy, err)
		}
		// Rendering a preview is allowed; export is not.
		RenderCreative(ctx, pool, rc, p, c, cr, "me")
		if _, err := ExportPack(ctx, pool, p, c, t.TempDir(), "me"); err == nil {
			t.Errorf("%q: export must refuse", tc.copy)
		}
	}
	// A number that comes with its source passes: the source line vouches for it.
	cr3 := *cr
	cr3.Source = "the 2026 study, 40% of runs"
	EditCreative(ctx, pool, p, c, cr, "me", map[string]string{"headline": "Cuts CI time by 40%"}, nil)
	if v, _ := View(ctx, pool, p, c, &cr3); len(v.Flags) != 0 {
		t.Errorf("sourced number flagged: %v", v.Flags)
	}
}

func TestEscapesCopyInTemplate(t *testing.T) {
	_, p, c := fixture(t)
	cr := c.Creative("card")
	out, err := Fill(cr, map[string]string{"headline": `<script>x</script> & "q"`}, "a", 100, 100)
	_ = p
	if err != nil || strings.Contains(out, "<script>") || !strings.Contains(out, "&lt;script&gt;") {
		t.Errorf("%v %s", err, out)
	}
	if _, err := Fill(cr, map[string]string{}, "a", 100, 100); err == nil {
		t.Error("an unfilled placeholder must be an error")
	}
}

func TestExportPack(t *testing.T) {
	ctx := context.Background()
	pool, p, c := fixture(t)
	cr := c.Creative("card")
	rc, _ := fakeRenderer(t)
	dir := filepath.Join(t.TempDir(), "pack")
	if _, err := ExportPack(ctx, pool, p, c, dir, "me"); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("unapproved: %v", err)
	}
	RenderCreative(ctx, pool, rc, p, c, cr, "me")
	if _, err := ApproveCreative(ctx, pool, p, c, cr, "me"); err != nil {
		t.Fatal(err)
	}
	res, err := ExportPack(ctx, pool, p, c, dir, "me")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"card/1200x630.png", "card/1080x1080.png", "card/1200x630.pdf", "card/alt.txt", "copy.md", "landing.txt", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s (have %v)", f, res.Files)
		}
	}
	md, _ := os.ReadFile(filepath.Join(dir, "copy.md"))
	if !strings.Contains(string(md), "Alt text: A card that says do one thing.") || !strings.Contains(string(md), "Show HN: Demo https://demo.example/c/launch") {
		t.Errorf("copy.md:\n%s", md)
	}
	// The exported file is the approved one, byte for byte.
	want, _ := RenderedFile(ctx, pool, p, c, cr, "1200x630", "png")
	got, _ := os.ReadFile(filepath.Join(dir, "card/1200x630.png"))
	if string(got) != string(want) {
		t.Error("export must use the stored bytes")
	}
}

func TestPostingTasks(t *testing.T) {
	ctx := context.Background()
	pool, p, c := fixture(t)
	out := &outreach.Service{Pool: pool}
	now := day("2026-11-30")
	res, err := CreateTasks(ctx, pool, out, "https://gtm.example", p, c, now)
	if err != nil || len(res.Created) != 1 || len(res.Held) != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	po := res.Created[0]
	if po.Due != "2026-12-01" || !strings.HasPrefix(po.Body, "Show HN: Demo https://gtm.example/l/") {
		t.Errorf("%+v", po)
	}
	// Idempotent.
	if res, _ = CreateTasks(ctx, pool, out, "https://gtm.example", p, c, now); len(res.Created) != 0 || res.Existing != 1 {
		t.Errorf("second run: %+v", res)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE next_action LIKE 'Post Show HN%'`).Scan(&n)
	if n != 1 {
		t.Errorf("tasks: %d", n)
	}
	// The existing content_mark records the posted URL and closes the task.
	studio := &content.Studio{Pool: pool}
	if err := studio.Mark(ctx, *po.ContentID, "posted", "https://news.ycombinator.com/item?id=1"); err != nil {
		t.Fatal(err)
	}
	posts, _, _ := Posts(ctx, pool, p, c)
	if posts[0].Status != "posted" || posts[0].PostedURL != "https://news.ycombinator.com/item?id=1" {
		t.Errorf("%+v", posts[0])
	}
	var status string
	pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id = $1`, *po.TaskID).Scan(&status)
	if status != "done" {
		t.Errorf("task %s", status)
	}
	r, _ := BuildReport(ctx, pool, p, c, time.Now())
	found := false
	for _, ct := range r.Counts {
		if strings.HasPrefix(ct.Label, "posting tasks posted") && ct.N == 1 && ct.Of != nil && *ct.Of == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("report counts: %+v", r.Counts)
	}
}

func TestFlaggedPostIsHeldNotMade(t *testing.T) {
	ctx := context.Background()
	pool, p, c := fixture(t)
	c.Tasks[0].Body = "Show HN: Demo is guaranteed to cut CI time by 90%"
	res, err := CreateTasks(ctx, pool, &outreach.Service{Pool: pool}, "", p, c, day("2026-11-30"))
	if err != nil || len(res.Created) != 0 || len(res.Held) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if f := strings.Join(res.Held[0].Flags, "|"); !strings.Contains(f, "guaranteed") || !strings.Contains(f, "90%") {
		t.Errorf("%v", f)
	}
}

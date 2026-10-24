package campaign

import (
	"bytes"
	"context"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/render"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// Renders the demo creative with a real Gotenberg:
//
//	docker run --rm -p 3000:3000 gotenberg/gotenberg:8
//	ASTROLABE_RENDER_TEST_URL=http://localhost:3000 go test ./internal/campaign -run RealRenderer -v
func TestRealRenderer(t *testing.T) {
	url := os.Getenv("ASTROLABE_RENDER_TEST_URL")
	if url == "" {
		t.Skip("set ASTROLABE_RENDER_TEST_URL to a Gotenberg 8 to run this")
	}
	ctx := context.Background()
	p, err := product.Load(filepath.Join("..", "..", "examples", "products", "pipewrench-demo"))
	if err != nil {
		t.Fatal(err)
	}
	c := p.Campaign("launch")
	cr := c.Creative("social-card")
	cr.Formats = []string{"png", "pdf"}
	pool := store.TestDB(t)
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	v, err := RenderCreative(ctx, pool, render.New(url), p, c, cr, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Renders) != 4 {
		t.Fatalf("%+v", v.Renders)
	}
	for size, want := range map[string][2]int{"1200x630": {1200, 630}, "1080x1080": {1080, 1080}} {
		b, err := RenderedFile(ctx, pool, p, c, cr, size, "png")
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(b))
		if err != nil || cfg.Width != want[0] || cfg.Height != want[1] {
			t.Errorf("%s: %v %+v", size, err, cfg)
		}
		if dir := os.Getenv("ASTROLABE_RENDER_TEST_OUT"); dir != "" {
			os.WriteFile(filepath.Join(dir, "card-"+size+".png"), b, 0o644)
		}
		pdf, _ := RenderedFile(ctx, pool, p, c, cr, size, "pdf")
		if n := len(regexp.MustCompile(`/Type\s*/Page\b`).FindAll(pdf, -1)); n != 1 {
			t.Errorf("%s pdf has %d pages", size, n)
		}
	}
	if v, err = ApproveCreative(ctx, pool, p, c, cr, "test"); err != nil || !v.Approved {
		t.Fatalf("%v %+v", err, v)
	}
}

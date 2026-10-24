package campaign

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/draft"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/render"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// RenderInfo describes one stored rendered file.
type RenderInfo struct {
	Size     string    `json:"size"`
	Format   string    `json:"format"`
	Bytes    int       `json:"bytes"`
	SHA256   string    `json:"sha256"`
	Rendered time.Time `json:"rendered_at"`
}

// CreativeView is a creative as it is now: effective copy, check flags,
// rendered files and whether the approval still holds.
type CreativeView struct {
	Campaign string            `json:"campaign"`
	ID       string            `json:"id"`
	Version  int               `json:"version"`
	Copy     map[string]string `json:"copy"`
	Alt      string            `json:"alt"`
	Source   string            `json:"source,omitempty"`
	Sizes    []string          `json:"sizes"`
	Formats  []string          `json:"formats"`
	// Flags come from the claims check on the copy; flagged copy cannot be approved.
	Flags    []string     `json:"flags"`
	Renders  []RenderInfo `json:"renders"`
	Missing  []string     `json:"not_rendered,omitempty"`
	Approved bool         `json:"approved"`
	// ApprovedBy and ApprovedAt describe the approval that holds now.
	ApprovedBy string     `json:"approved_by,omitempty"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
	// Status says in words why it is or is not approved.
	Status string `json:"status"`
	// Hash covers the exact copy, template and rendered files.
	Hash string `json:"hash"`
}

type creativeRow struct {
	overrides map[string]string
	alt       *string
	version   int
	hash      *string
	by        *string
	at        *time.Time
}

func readCreative(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign, cr *product.Creative) (creativeRow, error) {
	r := creativeRow{overrides: map[string]string{}, version: 1}
	var raw []byte
	err := q.QueryRow(ctx, `
		SELECT copy_overrides, alt_override, version, approved_hash, approved_by, approved_at
		FROM creatives WHERE product_id = $1 AND campaign_id = $2 AND creative_id = $3`, p.ID, c.ID, cr.ID).
		Scan(&raw, &r.alt, &r.version, &r.hash, &r.by, &r.at)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(raw, &r.overrides)
}

func (r creativeRow) effective(cr *product.Creative) (map[string]string, string) {
	cp := map[string]string{}
	for k, v := range cr.Copy {
		cp[k] = v
	}
	for k, v := range r.overrides {
		cp[k] = v
	}
	alt := cr.Alt
	if r.alt != nil {
		alt = *r.alt
	}
	return cp, alt
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// contentHash covers everything that makes the creative what it is, except
// the rendered files: copy, alt, sizes, formats, template and its assets.
func contentHash(cr *product.Creative, cp map[string]string, alt string) string {
	assets := map[string]string{}
	for n, b := range cr.Assets {
		assets[n] = sum(b)
	}
	b, _ := json.Marshal(map[string]any{"copy": cp, "alt": alt, "sizes": cr.Sizes, "formats": cr.CreativeFormats(),
		"template": cr.TemplateText, "assets": assets, "source": cr.Source})
	return sum(b)
}

func fullHash(content string, renders []RenderInfo) string {
	var b strings.Builder
	b.WriteString(content)
	for _, r := range renders { // already sorted
		fmt.Fprintf(&b, "\n%s:%s:%s", r.Size, r.Format, r.SHA256)
	}
	return sum([]byte(b.String()))
}

func listRenders(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign, cr *product.Creative) ([]RenderInfo, error) {
	rows, err := q.Query(ctx, `
		SELECT size, format, octet_length(content), sha256, rendered_at FROM creative_renders
		WHERE product_id = $1 AND campaign_id = $2 AND creative_id = $3 ORDER BY size, format`, p.ID, c.ID, cr.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (RenderInfo, error) {
		var i RenderInfo
		err := r.Scan(&i.Size, &i.Format, &i.Bytes, &i.SHA256, &i.Rendered)
		return i, err
	})
}

// CopyFlags runs the claims check on a creative's copy and alt text, exactly
// like an email draft (never-claims, leftover placeholders, numbers without a
// source), plus empty fields.
func CopyFlags(p *product.Product, cr *product.Creative, cp map[string]string, alt string) []string {
	names := make([]string, 0, len(cp))
	for k := range cp {
		names = append(names, k)
	}
	sort.Strings(names)
	var parts, flags []string
	for _, k := range names {
		if strings.TrimSpace(cp[k]) == "" {
			flags = append(flags, fmt.Sprintf("copy field %q is empty", k))
		}
		parts = append(parts, cp[k])
	}
	parts = append(parts, alt)
	return append(flags, draft.CheckCopy(p, strings.Join(parts, "\n"), cr.Source)...)
}

// View builds the current view of one creative.
func View(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign, cr *product.Creative) (*CreativeView, error) {
	row, err := readCreative(ctx, q, p, c, cr)
	if err != nil {
		return nil, err
	}
	renders, err := listRenders(ctx, q, p, c, cr)
	if err != nil {
		return nil, err
	}
	cp, alt := row.effective(cr)
	v := &CreativeView{Campaign: c.ID, ID: cr.ID, Version: row.version, Copy: cp, Alt: alt, Source: cr.Source, Sizes: cr.Sizes,
		Formats: cr.CreativeFormats(), Flags: CopyFlags(p, cr, cp, alt), Renders: renders}
	if v.Flags == nil {
		v.Flags = []string{}
	}
	if v.Renders == nil {
		v.Renders = []RenderInfo{}
	}
	have := map[string]bool{}
	for _, r := range renders {
		have[r.Size+"."+r.Format] = true
	}
	for _, s := range cr.Sizes {
		for _, f := range cr.CreativeFormats() {
			if !have[s+"."+f] {
				v.Missing = append(v.Missing, s+" "+f)
			}
		}
	}
	v.Hash = fullHash(contentHash(cr, cp, alt), renders)
	switch {
	case row.hash == nil:
		v.Status = "not approved"
	case *row.hash != v.Hash:
		v.Status = "approval withdrawn: the template or files changed since " + row.by2() + " approved it"
	case len(v.Flags) > 0:
		v.Status = "flagged: " + strings.Join(v.Flags, "; ")
	default:
		v.Approved, v.ApprovedBy, v.ApprovedAt = true, *row.by, row.at
		v.Status = "approved"
		if len(renders) == 0 {
			v.Status = "approved (copy only: no rendered files yet)"
		}
	}
	if !v.Approved && len(v.Flags) > 0 && row.hash == nil {
		v.Status = "flagged, waiting: " + strings.Join(v.Flags, "; ")
	}
	return v, nil
}

func (r creativeRow) by2() string {
	if r.by != nil {
		return *r.by
	}
	return "someone"
}

// Creatives lists the views of every creative in a campaign.
func Creatives(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign) ([]*CreativeView, error) {
	out := []*CreativeView{}
	for _, cr := range c.Creatives {
		v, err := View(ctx, q, p, c, cr)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func ensure(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign, cr *product.Creative) error {
	_, err := q.Exec(ctx, `INSERT INTO creatives (product_id, campaign_id, creative_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, p.ID, c.ID, cr.ID)
	return err
}

// EditCreative changes copy fields (and optionally the alt text). Any real
// change withdraws the approval and deletes the rendered files, because they
// no longer show the copy. A change that sets a field to what it already is
// does nothing.
func EditCreative(ctx context.Context, pool *pgxpool.Pool, p *product.Product, c *product.Campaign, cr *product.Creative, by string, fields map[string]string, alt *string) (*CreativeView, error) {
	if strings.TrimSpace(by) == "" {
		return nil, errors.New("say who is editing it")
	}
	for k := range fields {
		if _, ok := cr.Copy[k]; !ok {
			return nil, fmt.Errorf("the template has no copy field %q (fields: %s)", k, strings.Join(sortedKeys(cr.Copy), ", "))
		}
	}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := ensure(ctx, tx, p, c, cr); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM creatives WHERE product_id = $1 AND campaign_id = $2 AND creative_id = $3 FOR UPDATE`, p.ID, c.ID, cr.ID); err != nil {
			return err
		}
		row, err := readCreative(ctx, tx, p, c, cr)
		if err != nil {
			return err
		}
		cp, curAlt := row.effective(cr)
		changed := false
		for k, v := range fields {
			if cp[k] != v {
				row.overrides[k] = v
				changed = true
			}
		}
		if alt != nil && *alt != curAlt {
			row.alt, changed = alt, true
		}
		if !changed {
			return nil
		}
		raw, _ := json.Marshal(row.overrides)
		if _, err := tx.Exec(ctx, `
			UPDATE creatives SET copy_overrides = $4, alt_override = $5, version = version + 1,
				approved_hash = NULL, approved_by = NULL, approved_at = NULL, updated_by = $6, updated_at = now()
			WHERE product_id = $1 AND campaign_id = $2 AND creative_id = $3`, p.ID, c.ID, cr.ID, raw, row.alt, by); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM creative_renders WHERE product_id = $1 AND campaign_id = $2 AND creative_id = $3`, p.ID, c.ID, cr.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return View(ctx, pool, p, c, cr)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	placeholderRE = regexp.MustCompile(`\{\{\s*([a-z][a-z0-9_]*)\s*\}\}`)
	assetRefRE    = regexp.MustCompile(`((?:href|src)=")([^"#?:]+)(")`)
)

// Fill puts the copy into the template for one size. Values are HTML-escaped;
// a placeholder without a value is an error. Relative file references are
// rewritten to bare file names, because the renderer receives the template
// and its files side by side.
func Fill(cr *product.Creative, cp map[string]string, alt string, width, height int) (string, error) {
	var missing []string
	out := placeholderRE.ReplaceAllStringFunc(cr.TemplateText, func(m string) string {
		name := placeholderRE.FindStringSubmatch(m)[1]
		switch name {
		case "width":
			return strconv.Itoa(width)
		case "height":
			return strconv.Itoa(height)
		case "alt":
			return html.EscapeString(alt)
		}
		v, ok := cp[name]
		if !ok {
			missing = append(missing, name)
		}
		return html.EscapeString(v)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("the template needs copy for: %s", strings.Join(missing, ", "))
	}
	out = assetRefRE.ReplaceAllStringFunc(out, func(m string) string {
		g := assetRefRE.FindStringSubmatch(m)
		if _, ok := cr.Assets[filepath.Base(g[2])]; !ok {
			return m
		}
		return g[1] + filepath.Base(g[2]) + g[3]
	})
	return out, nil
}

// RenderCreative renders the creative at every size and format and stores the
// files. It needs the renderer sidecar and says so plainly if there is none.
// Rendering again replaces the files and withdraws any approval, since the
// new files are not the ones that were approved.
func RenderCreative(ctx context.Context, pool *pgxpool.Pool, rc *render.Client, p *product.Product, c *product.Campaign, cr *product.Creative, by string) (*CreativeView, error) {
	if !rc.Enabled() {
		return nil, render.ErrNoRenderer
	}
	if strings.TrimSpace(by) == "" {
		return nil, errors.New("say who is rendering it")
	}
	row, err := readCreative(ctx, pool, p, c, cr)
	if err != nil {
		return nil, err
	}
	cp, alt := row.effective(cr)
	type out struct {
		size, format string
		data         []byte
	}
	var results []out
	for _, size := range cr.Sizes {
		w, h, err := product.ParseSize(size)
		if err != nil {
			return nil, err
		}
		page, err := Fill(cr, cp, alt, w, h)
		if err != nil {
			return nil, err
		}
		for _, format := range cr.CreativeFormats() {
			data, err := rc.Render(ctx, render.Page{HTML: page, Assets: cr.Assets}, w, h, format)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", size, format, err)
			}
			results = append(results, out{size, format, data})
		}
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := ensure(ctx, tx, p, c, cr); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM creative_renders WHERE product_id = $1 AND campaign_id = $2 AND creative_id = $3`, p.ID, c.ID, cr.ID); err != nil {
			return err
		}
		for _, r := range results {
			if _, err := tx.Exec(ctx, `
				INSERT INTO creative_renders (product_id, campaign_id, creative_id, size, format, content, sha256)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, p.ID, c.ID, cr.ID, r.size, r.format, r.data, sum(r.data)); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `
			UPDATE creatives SET approved_hash = NULL, approved_by = NULL, approved_at = NULL, version = version + 1, updated_by = $4, updated_at = now()
			WHERE product_id = $1 AND campaign_id = $2 AND creative_id = $3`, p.ID, c.ID, cr.ID, by)
		return err
	})
	if err != nil {
		return nil, err
	}
	return View(ctx, pool, p, c, cr)
}

// ApproveCreative approves exactly what View shows now: the copy, the
// template and every rendered file, by hash. Flagged copy cannot be approved.
// All sizes must be rendered, or none (an approval of the copy alone, which
// is what you want when there is no renderer).
func ApproveCreative(ctx context.Context, pool *pgxpool.Pool, p *product.Product, c *product.Campaign, cr *product.Creative, by string) (*CreativeView, error) {
	if strings.TrimSpace(by) == "" {
		return nil, errors.New("say who is approving it")
	}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := ensure(ctx, tx, p, c, cr); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM creatives WHERE product_id = $1 AND campaign_id = $2 AND creative_id = $3 FOR UPDATE`, p.ID, c.ID, cr.ID); err != nil {
			return err
		}
		v, err := View(ctx, tx, p, c, cr)
		if err != nil {
			return err
		}
		if len(v.Flags) > 0 {
			return fmt.Errorf("flagged copy waits for a fix, not an approval: %s", strings.Join(v.Flags, "; "))
		}
		if len(v.Renders) > 0 && len(v.Missing) > 0 {
			return fmt.Errorf("some sizes are not rendered (%s); render again so the approval covers every file", strings.Join(v.Missing, ", "))
		}
		_, err = tx.Exec(ctx, `
			UPDATE creatives SET approved_hash = $4, approved_by = $5, approved_at = now()
			WHERE product_id = $1 AND campaign_id = $2 AND creative_id = $3`, p.ID, c.ID, cr.ID, v.Hash, by)
		return err
	})
	if err != nil {
		return nil, err
	}
	return View(ctx, pool, p, c, cr)
}

// RenderedFile returns a stored rendered file's bytes.
func RenderedFile(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign, cr *product.Creative, size, format string) ([]byte, error) {
	var b []byte
	err := q.QueryRow(ctx, `SELECT content FROM creative_renders WHERE product_id = $1 AND campaign_id = $2 AND creative_id = $3 AND size = $4 AND format = $5`,
		p.ID, c.ID, cr.ID, size, format).Scan(&b)
	return b, err
}

// ---- export ----

// PackResult says what was written.
type PackResult struct {
	Dir   string   `json:"dir"`
	Files []string `json:"files"`
	Note  string   `json:"note"`
}

// ExportPack writes an approved pack for posting by hand: the landing page,
// the copy and alt text, the posts, and each creative's rendered files (the
// stored bytes that were approved; nothing is rendered again). It refuses if
// any creative is not approved or any post is flagged. Nothing is posted and
// no ad platform is contacted.
func ExportPack(ctx context.Context, pool *pgxpool.Pool, p *product.Product, c *product.Campaign, dir, by string) (*PackResult, error) {
	if dir == "" {
		return nil, errors.New("no folder to write the pack to")
	}
	var notReady []string
	views := map[string]*CreativeView{}
	for _, cr := range c.Creatives {
		v, err := View(ctx, pool, p, c, cr)
		if err != nil {
			return nil, err
		}
		views[cr.ID] = v
		if !v.Approved {
			notReady = append(notReady, fmt.Sprintf("creative %s: %s", cr.ID, v.Status))
		}
	}
	posts, held, err := Posts(ctx, pool, p, c)
	if err != nil {
		return nil, err
	}
	for _, h := range held {
		notReady = append(notReady, fmt.Sprintf("post %s: flagged: %s", h.Task, strings.Join(h.Flags, "; ")))
	}
	if len(notReady) > 0 {
		return nil, fmt.Errorf("the pack is not ready, and flagged or unapproved items wait:\n  - %s", strings.Join(notReady, "\n  - "))
	}
	res := &PackResult{Dir: dir, Note: "Post these by hand. Astrolabe posts nothing and has no ad-platform connection."}
	write := func(rel string, data []byte) error {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
		res.Files = append(res.Files, rel)
		return nil
	}
	manifest := map[string]any{"campaign": c.ID, "code": c.Code, "exported_by": by, "exported_at": time.Now().UTC().Format(time.RFC3339), "landing": c.Landing}
	creatives := map[string]any{}
	var md strings.Builder
	fmt.Fprintf(&md, "# %s (%s)\n\nLanding page: %s\n\n", c.Label, c.Code, c.Landing)
	for _, cr := range c.Creatives {
		v := views[cr.ID]
		fmt.Fprintf(&md, "## Creative: %s\n\nApproved by %s. Hash %s.\n\n", cr.ID, v.ApprovedBy, v.Hash)
		for _, k := range sortedKeys(v.Copy) {
			fmt.Fprintf(&md, "- %s: %s\n", k, v.Copy[k])
		}
		fmt.Fprintf(&md, "\nAlt text: %s\n\n", v.Alt)
		if err := write(filepath.Join(cr.ID, "alt.txt"), []byte(v.Alt+"\n")); err != nil {
			return nil, err
		}
		files := map[string]string{}
		for _, r := range v.Renders {
			b, err := RenderedFile(ctx, pool, p, c, cr, r.Size, r.Format)
			if err != nil {
				return nil, err
			}
			name := filepath.Join(cr.ID, r.Size+"."+r.Format)
			if err := write(name, b); err != nil {
				return nil, err
			}
			files[name] = r.SHA256
		}
		if len(v.Renders) == 0 {
			md.WriteString("(Copy only: no rendered files were approved.)\n\n")
		}
		creatives[cr.ID] = map[string]any{"hash": v.Hash, "approved_by": v.ApprovedBy, "files": files}
	}
	if len(posts) > 0 {
		md.WriteString("## Posts\n\n")
		for _, po := range posts {
			fmt.Fprintf(&md, "### %s (%s, day %+d)\n\n%s\n\n", po.Title, po.Channel, po.Day, po.Body)
		}
	}
	manifest["creatives"] = creatives
	if err := write("copy.md", []byte(md.String())); err != nil {
		return nil, err
	}
	if err := write("landing.txt", []byte(c.Landing+"\n")); err != nil {
		return nil, err
	}
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	if err := write("manifest.json", append(mb, '\n')); err != nil {
		return nil, err
	}
	return res, nil
}

// ---- preview ----

var (
	stylesheetRE = regexp.MustCompile(`<link[^>]*rel="stylesheet"[^>]*href="([^"#?:]+)"[^>]*>|<link[^>]*href="([^"#?:]+)"[^>]*rel="stylesheet"[^>]*>`)
	srcRE        = regexp.MustCompile(`(src=")([^"#?:]+)(")`)
)

// Preview is the creative filled with its copy at one size, as a single
// HTML document: its stylesheets inlined and its images as data URIs, so
// it can be shown without the renderer. It is what a render would draw.
func Preview(cr *product.Creative, cp map[string]string, alt string, size string) (string, int, int, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return "", 0, 0, err
	}
	doc, err := Fill(cr, cp, alt, w, h)
	if err != nil {
		return "", 0, 0, err
	}
	doc = stylesheetRE.ReplaceAllStringFunc(doc, func(m string) string {
		g := stylesheetRE.FindStringSubmatch(m)
		name := filepath.Base(g[1] + g[2])
		css, ok := cr.Assets[name]
		if !ok {
			return m
		}
		return "<style>\n" + strings.ReplaceAll(string(css), "</style", "<\\/style") + "\n</style>"
	})
	doc = srcRE.ReplaceAllStringFunc(doc, func(m string) string {
		g := srcRE.FindStringSubmatch(m)
		b, ok := cr.Assets[filepath.Base(g[2])]
		if !ok {
			return m
		}
		return g[1] + "data:" + http.DetectContentType(b) + ";base64," + base64.StdEncoding.EncodeToString(b) + g[3]
	})
	return doc, w, h, nil
}

func parseSize(size string) (int, int, error) {
	ws, hs, ok := strings.Cut(size, "x")
	w, err1 := strconv.Atoi(ws)
	h, err2 := strconv.Atoi(hs)
	if !ok || err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("size must look like 1200x630, got %q", size)
	}
	return w, h, nil
}

// Package content turns release notes or posts into per-platform drafts
// with tracked links, and launches into a checklist of tasks. Nothing is
// posted automatically: I post by hand and mark it done.
package content

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/links"
	"github.com/AstrolabeGTM/astrolabe/internal/llm"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

// Platforms and what a good post looks like on each.
var Platforms = map[string]string{
	"x":           "an X post under 270 characters, plain, one concrete point, the link at the end",
	"linkedin":    "a LinkedIn post of 80–150 words, first line is the hook, no hashtag spam, the link at the end",
	"hn":          "a Show HN title (\"Show HN: …\", under 80 chars) then a blank line and a 100–200 word first comment from the maker: what it does, why, what's rough, the link",
	"reddit":      "a Reddit post for a relevant technical subreddit: a title line, a blank line, then a helpful 100–200 word body that would be welcome even without the link",
	"newsletter":  "a newsletter section of 100–180 words with a short heading line",
	"producthunt": "a Product Hunt tagline (under 60 chars), a blank line, then a 2–3 sentence maker comment",
	"directories": "a 1-sentence and a 3-sentence description for product directories",
}

// LaunchTargets are the usual places for a launch.
var LaunchTargets = []string{"hn", "producthunt", "reddit", "x", "linkedin", "newsletter", "directories"}

type Studio struct {
	Pool      *pgxpool.Pool
	LLM       *llm.Client
	Out       *outreach.Service
	PublicURL string
}

type Item struct {
	ID        int64      `json:"id"`
	Product   string     `json:"product"`
	Batch     string     `json:"batch"`
	Platform  string     `json:"platform"`
	Body      string     `json:"body"`
	Link      string     `json:"link,omitempty"`
	Status    string     `json:"status"`
	PostedURL string     `json:"posted_url,omitempty"`
	Clicks    int        `json:"clicks"`
	TaskID    *int64     `json:"task_id,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	PostedAt  *time.Time `json:"posted_at,omitempty"`
}

// Repurpose writes a draft for each platform from a source text. With
// launch=true each draft also becomes a task due today.
func (s *Studio) Repurpose(ctx context.Context, productID, batch, source string, platforms []string, launch bool) ([]int64, error) {
	if !s.LLM.Enabled() {
		return nil, errors.New("content drafting needs the Claude API (ASTROLABE_ANTHROPIC_API_KEY)")
	}
	if len(platforms) == 0 {
		platforms = []string{"x", "linkedin", "hn", "newsletter"}
	}
	for _, pl := range platforms {
		if _, ok := Platforms[pl]; !ok {
			return nil, fmt.Errorf("unknown platform %q", pl)
		}
	}
	p, err := product.Get(ctx, s.Pool, productID)
	if err != nil {
		return nil, err
	}
	var specs strings.Builder
	for _, pl := range platforms {
		fmt.Fprintf(&specs, "- %s: %s\n", pl, Platforms[pl])
	}
	res, err := s.LLM.Complete(ctx, llm.Request{Purpose: "content", Product: productID, MaxTokens: 2500,
		System: "You turn a founder's source material into platform-native posts they publish themselves. Specific, honest, no hype, no emoji walls. Only product claims from CLAIMS. Where the link goes, write {{LINK}} exactly.",
		Prompt: fmt.Sprintf("PRODUCT: %s — %s\nCLAIMS:\n%s\nNEVER SAY: %s\nVOICE:\n%s\n\nSOURCE (%s):\n%s\n\nWrite one post per platform:\n%s\nReturn JSON: {\"items\":[{\"platform\":\"x\",\"text\":\"...\"}, ...]}",
			p.Name, p.OneLiner, p.ClaimsText, strings.Join(p.Claims.Never, "; "), p.VoiceText, batch, source, specs.String())})
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []struct{ Platform, Text string }
	}
	if err := llm.JSON(res.Text, &out); err != nil {
		return nil, err
	}
	var ids []int64
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		for _, it := range out.Items {
			if !slices.Contains(platforms, it.Platform) || strings.TrimSpace(it.Text) == "" {
				continue
			}
			url, code := p.Offer.Destination, (*string)(nil)
			if s.PublicURL != "" && p.URL != "" {
				c, err := links.Create(ctx, tx, links.Link{Product: productID, Destination: p.URL,
					Label: fmt.Sprintf("content:%s:%s", it.Platform, batch),
					UTM:   map[string]string{"utm_source": it.Platform, "utm_medium": "social", "utm_campaign": batch}})
				if err != nil {
					return err
				}
				url, code = links.URL(s.PublicURL, c), &c
			} else if p.URL != "" {
				url = p.URL
			}
			body := strings.ReplaceAll(it.Text, "{{LINK}}", url)
			var id int64
			if err := tx.QueryRow(ctx, `INSERT INTO content_items (product_id, batch, platform, body, link_code) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
				productID, batch, it.Platform, body, code).Scan(&id); err != nil {
				return err
			}
			if launch {
				tid, err := s.Out.CreateTaskTx(ctx, tx, outreach.TaskOpts{Product: productID,
					NextAction: fmt.Sprintf("Post on %s: %s (content #%d)", it.Platform, batch, id), Due: time.Now()})
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE content_items SET task_id = $2 WHERE id = $1`, id, tid); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE tasks SET draft = $2 WHERE id = $1`, tid, body); err != nil {
					return err
				}
			}
			ids = append(ids, id)
		}
		return nil
	})
	return ids, err
}

// List returns recent content items with click counts.
func (s *Studio) List(ctx context.Context, productID string, limit int) ([]Item, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT c.id, c.product_id, c.batch, c.platform, c.body, COALESCE(c.link_code, ''), c.status, c.posted_url,
			(SELECT count(*) FROM clicks k WHERE k.code = c.link_code), c.task_id, c.created_at, c.posted_at
		FROM content_items c WHERE $1 = '' OR c.product_id = $1 ORDER BY c.id DESC LIMIT $2`, productID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Item, error) {
		var it Item
		var code string
		err := r.Scan(&it.ID, &it.Product, &it.Batch, &it.Platform, &it.Body, &code, &it.Status, &it.PostedURL, &it.Clicks, &it.TaskID, &it.CreatedAt, &it.PostedAt)
		it.Link = links.URL(s.PublicURL, code)
		return it, err
	})
}

// Mark records a post as posted (with its URL) or skipped, closing its task.
func (s *Studio) Mark(ctx context.Context, id int64, status, postedURL string) error {
	if status != "posted" && status != "skipped" {
		return fmt.Errorf("status must be posted or skipped")
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var taskID *int64
		if err := tx.QueryRow(ctx, `
			UPDATE content_items SET status = $2, posted_url = $3, posted_at = CASE WHEN $2 = 'posted' THEN now() END
			WHERE id = $1 RETURNING task_id`, id, status, postedURL).Scan(&taskID); err != nil {
			return fmt.Errorf("no content item %d", id)
		}
		if taskID != nil {
			_, err := tx.Exec(ctx, `UPDATE tasks SET status = 'done', outcome = 'other', outcome_note = $2, closed_at = now() WHERE id = $1 AND status = 'open'`,
				*taskID, status+" "+postedURL)
			return err
		}
		return nil
	})
}

package campaign

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/draft"
	"github.com/AstrolabeGTM/astrolabe/internal/links"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// Post is a campaign's launch task with its text as it will be posted.
type Post struct {
	Task      string `json:"task"`
	Title     string `json:"title"`
	Channel   string `json:"channel"`
	Day       int    `json:"day"`
	Body      string `json:"body,omitempty"`
	Due       string `json:"due"`
	ContentID *int64 `json:"content_id,omitempty"`
	TaskID    *int64 `json:"task_id,omitempty"`
	Status    string `json:"status"` // not made, draft, posted, skipped
	PostedURL string `json:"posted_url,omitempty"`
}

// Held is a post that waits because the claims check flagged it.
type Held struct {
	Task  string   `json:"task"`
	Flags []string `json:"flags"`
}

func due(s Settings, day int, loc *time.Location) time.Time {
	start, _ := time.ParseInLocation(dateLayout, s.Start, loc)
	return start.AddDate(0, 0, day)
}

// Posts lists the campaign's launch tasks: made ones with their status and
// posted URL, others from the YAML. Posts whose text fails the claims check
// are returned in held.
func Posts(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign) ([]Post, []Held, error) {
	s, err := Effective(ctx, q, p, c)
	if err != nil {
		return nil, nil, err
	}
	made := map[string]Post{}
	rows, err := q.Query(ctx, `SELECT campaign_task, id, task_id, body, status, posted_url FROM content_items WHERE product_id = $1 AND campaign_id = $2 AND campaign_task IS NOT NULL`, p.ID, c.ID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var task string
		var po Post
		var id int64
		if err := rows.Scan(&task, &id, &po.TaskID, &po.Body, &po.Status, &po.PostedURL); err != nil {
			return nil, nil, err
		}
		po.ContentID = &id
		made[task] = po
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var posts []Post
	var held []Held
	for _, t := range c.Tasks {
		po, ok := made[t.ID]
		if !ok {
			po = Post{Status: "not made", Body: strings.ReplaceAll(t.Body, "{{link}}", c.Landing)}
		}
		po.Task, po.Title, po.Channel, po.Day = t.ID, t.Title, t.Channel, t.Day
		po.Due = due(s, t.Day, time.Local).Format(dateLayout)
		if po.Body != "" {
			if flags := draft.CheckCopy(p, po.Body, ""); len(flags) > 0 {
				held = append(held, Held{Task: t.ID, Flags: flags})
			}
		}
		posts = append(posts, po)
	}
	return posts, held, nil
}

// TasksResult says what CreateTasks did.
type TasksResult struct {
	Created  []Post `json:"created"`
	Existing int    `json:"already_made"`
	Held     []Held `json:"held_by_the_claims_check,omitempty"`
}

// CreateTasks turns the campaign's launch tasks into content items and tasks
// in the existing launch-task machinery, due on start + day. Mark one done
// with content_mark (posted, with its URL), which closes its task. Running it
// again only adds tasks that are missing, and posts flagged by the claims
// check are held back, not made. With a public URL, the {{link}} in a post is
// a tracked short link for the campaign; otherwise it is the landing page.
func CreateTasks(ctx context.Context, pool *pgxpool.Pool, out *outreach.Service, publicURL string, p *product.Product, c *product.Campaign, now time.Time) (*TasksResult, error) {
	s, err := Effective(ctx, pool, p, c)
	if err != nil {
		return nil, err
	}
	res := &TasksResult{}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, t := range c.Tasks {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM content_items WHERE product_id = $1 AND campaign_id = $2 AND campaign_task = $3)`, p.ID, c.ID, t.ID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				res.Existing++
				continue
			}
			if t.Body != "" {
				if flags := draft.CheckCopy(p, strings.ReplaceAll(t.Body, "{{link}}", c.Landing), ""); len(flags) > 0 {
					res.Held = append(res.Held, Held{Task: t.ID, Flags: flags})
					continue
				}
			}
			link, code := c.Landing, (*string)(nil)
			if publicURL != "" {
				lc, err := links.Create(ctx, tx, links.Link{Product: p.ID, Destination: c.Landing, Label: fmt.Sprintf("campaign:%s:%s", c.Code, t.ID),
					UTM: map[string]string{"utm_source": t.Channel, "utm_medium": "campaign", "utm_campaign": c.Code}})
				if err != nil {
					return err
				}
				link, code = links.URL(publicURL, lc), &lc
			}
			body := strings.ReplaceAll(t.Body, "{{link}}", link)
			var id int64
			if err := tx.QueryRow(ctx, `
				INSERT INTO content_items (product_id, batch, platform, body, link_code, campaign_id, campaign_task)
				VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`, p.ID, "campaign:"+c.Code, t.Channel, body, code, c.ID, t.ID).Scan(&id); err != nil {
				return err
			}
			dueAt := due(s, t.Day, now.Location())
			tid, err := out.CreateTaskTx(ctx, tx, outreach.TaskOpts{Product: p.ID, Due: dueAt,
				NextAction: fmt.Sprintf("%s (campaign %s, %s; content #%d)", t.Title, c.Label, t.Channel, id)})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE content_items SET task_id = $2 WHERE id = $1`, id, tid); err != nil {
				return err
			}
			if body != "" {
				if _, err := tx.Exec(ctx, `UPDATE tasks SET draft = $2 WHERE id = $1`, tid, body); err != nil {
					return err
				}
			}
			res.Created = append(res.Created, Post{Task: t.ID, Title: t.Title, Channel: t.Channel, Day: t.Day, Body: body,
				Due: dueAt.Format(dateLayout), ContentID: &id, TaskID: &tid, Status: "draft"})
		}
		return nil
	})
	return res, err
}

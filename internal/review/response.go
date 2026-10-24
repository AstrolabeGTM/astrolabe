package review

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

// ResponseTimes: for inbound items that arrived this week, the time from
// arrival to our first response. A first response is a message we sent to
// that person, or the related task closed, whichever came first. The
// clock is wall-clock time (nights and weekends count), so a target of 2h
// means two real hours.
type ResponseTimes struct {
	Basis string         `json:"basis"`
	Kinds []ResponseKind `json:"by_kind"`
	// Slowest items across all kinds (answered or not), longest first.
	Slowest []ResponseItem `json:"slowest"`
}

// ResponseKind counts one kind of inbound item against its target.
// Within + Over + Waiting = Total. Over includes answered late and not
// yet answered past the target; Waiting is not yet answered and still
// inside the target.
type ResponseKind struct {
	Kind     string `json:"kind"` // request, reply, issue or discussion
	Target   string `json:"target"`
	Total    int    `json:"total"`
	Answered int    `json:"answered"`
	Within   int    `json:"within_target"`
	Over     int    `json:"over_target"`
	Waiting  int    `json:"waiting_inside_target"`
}

type ResponseItem struct {
	Kind        string     `json:"kind"`
	Ref         string     `json:"ref"`
	Label       string     `json:"label"`
	ArrivedAt   time.Time  `json:"arrived_at"`
	RespondedAt *time.Time `json:"first_response_at,omitempty"`
	Response    string     `json:"first_response,omitempty"` // message or task
	Hours       float64    `json:"hours"`                    // arrival to response, or to now when unanswered
	Status      string     `json:"status"`                   // within, over or waiting
}

type inbound struct {
	kind, ref, label string
	at               time.Time
	person, account  *int64
	reply            *int64
}

func responseTimes(ctx context.Context, pool *pgxpool.Pool, p *product.Product, from, to, now time.Time) (ResponseTimes, error) {
	out := ResponseTimes{Basis: "wall-clock hours from arrival to first response (a message sent to the person, or the related task closed)", Slowest: []ResponseItem{}}
	set := p.ReviewSettings()
	var items []inbound

	rows, err := pool.Query(ctx, `
		SELECT f.id, f.submitted_at, COALESCE(NULLIF(pe.name, ''), NULLIF(f.fields->>'email', ''), 'request #' || f.id), f.person_id, f.account_id
		FROM form_submissions f LEFT JOIN people pe ON pe.id = f.person_id
		WHERE f.product_id = $1 AND f.submitted_at >= $2 AND f.submitted_at < $3 ORDER BY f.submitted_at`, p.ID, from, to)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		it := inbound{kind: "request"}
		var id int64
		if err := rows.Scan(&id, &it.at, &it.label, &it.person, &it.account); err != nil {
			rows.Close()
			return out, err
		}
		it.ref = fmt.Sprintf("form_submission:%d", id)
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	rows, err = pool.Query(ctx, `
		SELECT r.id, r.received_at, COALESCE(NULLIF(pe.name, ''), r.from_addr), r.person_id
		FROM replies r LEFT JOIN people pe ON pe.id = r.person_id
		WHERE r.product_id = $1 AND r.received_at >= $2 AND r.received_at < $3 AND r.classification IN ('interested', 'question') AND NOT r.is_bounce
		ORDER BY r.received_at`, p.ID, from, to)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		it := inbound{kind: "reply"}
		var id int64
		if err := rows.Scan(&id, &it.at, &it.label, &it.person); err != nil {
			rows.Close()
			return out, err
		}
		it.ref = fmt.Sprintf("reply:%d", id)
		it.reply = &id
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	// New GitHub issues (and discussions): by monitor event type (.issue,
	// .discussion) or an issue found by a github_search monitor.
	rows, err = pool.Query(ctx, `
		SELECT s.id, s.occurred_at, CASE WHEN s.type LIKE '%.discussion' THEN 'discussion' ELSE 'issue' END,
			COALESCE(NULLIF(s.title, ''), s.type), s.person_id
		FROM signals s
		WHERE s.product_id = $1 AND s.occurred_at >= $2 AND s.occurred_at < $3
			AND (s.type ~ '\.(issue|discussion)$' OR s.data->>'kind' = 'issue')
		ORDER BY s.occurred_at`, p.ID, from, to)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		it := inbound{}
		var id int64
		if err := rows.Scan(&id, &it.at, &it.kind, &it.label, &it.person); err != nil {
			rows.Close()
			return out, err
		}
		it.ref = fmt.Sprintf("signal:%d", id)
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	kinds := []string{"request", "reply", "issue", "discussion"}
	by := map[string]*ResponseKind{}
	for _, k := range kinds {
		by[k] = &ResponseKind{Kind: k, Target: set.TargetText[k]}
	}
	var all []ResponseItem
	for _, it := range items {
		at, how, err := firstResponse(ctx, pool, p.ID, it)
		if err != nil {
			return out, err
		}
		target := set.Targets[it.kind]
		ri := ResponseItem{Kind: it.kind, Ref: it.ref, Label: it.label, ArrivedAt: it.at.UTC(), Response: how}
		k := by[it.kind]
		k.Total++
		if at != nil {
			ri.RespondedAt = at
			ri.Hours = roundHours(at.Sub(it.at))
			k.Answered++
			if at.Sub(it.at) <= target {
				ri.Status = "within"
				k.Within++
			} else {
				ri.Status, k.Over = "over", k.Over+1
			}
		} else {
			wait := now.Sub(it.at)
			ri.Hours = roundHours(wait)
			if wait > target {
				ri.Status, k.Over = "over", k.Over+1
			} else {
				ri.Status, k.Waiting = "waiting", k.Waiting+1
			}
		}
		all = append(all, ri)
	}
	for _, k := range kinds {
		out.Kinds = append(out.Kinds, *by[k])
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Hours > all[j].Hours })
	if len(all) > 5 {
		all = all[:5]
	}
	if all != nil {
		out.Slowest = all
	}
	return out, nil
}

func roundHours(d time.Duration) float64 {
	if d < 0 {
		d = 0
	}
	return float64(int(d.Hours()*10+0.5)) / 10
}

// firstResponse finds the earliest message sent to the person, or task
// closed for them (their account's tasks, or the task made from the
// reply), at or after the item arrived.
func firstResponse(ctx context.Context, pool *pgxpool.Pool, productID string, it inbound) (*time.Time, string, error) {
	var at *time.Time
	var how *string
	err := pool.QueryRow(ctx, `
		SELECT t, k FROM (
			SELECT sent_at AS t, 'message' AS k FROM actions
				WHERE product_id = $1 AND $2::bigint IS NOT NULL AND person_id = $2 AND status = 'sent' AND sent_at >= $4
			UNION ALL
			SELECT closed_at, 'task' FROM tasks
				WHERE product_id = $1 AND status = 'done' AND closed_at >= $4
					AND (reply_id = $5 OR (created_at >= $4 AND (person_id = $2 OR account_id = $3)))
		) x WHERE t IS NOT NULL ORDER BY t LIMIT 1`, productID, it.person, it.account, it.at, it.reply).Scan(&at, &how)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", nil
		}
		return nil, "", err
	}
	if at != nil {
		u := at.UTC()
		at = &u
	}
	h := ""
	if how != nil {
		h = *how
	}
	return at, h, nil
}

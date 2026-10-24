package views

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/pipeline"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

// Response targets: how long each kind of item may wait on the operator.
// They are constants for now; this is the one place to make them settings.
const (
	// ReplyTarget: people who wrote back get an answer within two hours.
	ReplyTarget = 2 * time.Hour
	// ApprovalTarget: a draft waits at most a day for a decision.
	ApprovalTarget = 24 * time.Hour
	// HeldTarget: a held, blocked, failed or unknown send gets a decision
	// within a day.
	HeldTarget = 24 * time.Hour
	// NearShare: an item is near its target once it has waited this share
	// of it.
	NearShare = 0.75
	// TaskSoon: a task due within this long is near its due time.
	TaskSoon = 2 * time.Hour
	// todayLimit caps how many items of each kind are listed; counts are
	// always complete.
	todayLimit = 200
)

// Targets are the response targets, in seconds, as the screen shows them.
type Targets struct {
	ReplySeconds    int64   `json:"reply_seconds"`
	ApprovalSeconds int64   `json:"approval_seconds"`
	HeldSeconds     int64   `json:"held_seconds"`
	NearShare       float64 `json:"near_share"`
	TaskSoonSeconds int64   `json:"task_soon_seconds"`
}

// Item is one thing waiting on the operator: a reply to label, a draft to
// approve, a held send to decide on, a task that is due, or a queued send.
type Item struct {
	// Kind is reply, approval, held, task, queued, account (an overdue
	// next action) or revisit (a paused or lost account to look at again).
	Kind string `json:"kind"`
	// Key is unique on the page, e.g. "reply:12".
	Key string `json:"key"`
	// Since is when the wait started; Due is when it passes its target (for
	// a task, its due time).
	Since time.Time `json:"since"`
	Due   time.Time `json:"due"`
	// AgeSeconds is how long it has waited (for a task, how long since it
	// was due; negative before then).
	AgeSeconds int64 `json:"age_seconds"`
	// TargetSeconds is the response target for this kind; 0 for tasks
	// (their due time is the target) and queued sends (nothing to decide).
	TargetSeconds int64 `json:"target_seconds"`
	// State is past (target missed), near (close to it) or ok.
	State  string           `json:"state"`
	Reply  *outreach.Reply  `json:"reply,omitempty"`
	Action *outreach.Action `json:"action,omitempty"`
	Task   *outreach.Task   `json:"task,omitempty"`
	// Account is set for account and revisit items.
	Account *pipeline.Account `json:"account,omitempty"`
}

// Count is how many items of a kind wait, and how many are past or near
// their target. Listed is how many are in Items (the rest beyond the limit).
type Count struct {
	Total  int `json:"total"`
	Listed int `json:"listed"`
	Past   int `json:"past"`
	Near   int `json:"near"`
}

// Today is everything waiting on the operator, most urgent first.
type Today struct {
	Now     time.Time        `json:"now"`
	Product string           `json:"product,omitempty"`
	Targets Targets          `json:"targets"`
	Items   []Item           `json:"items"`
	Counts  map[string]Count `json:"counts"`
	// LaterTasks counts open tasks due after today; they are not listed.
	LaterTasks int `json:"later_tasks"`
	// OpenAccounts and ClosedAccounts are what the account items are out
	// of: open, and paused or lost, pipeline accounts.
	OpenAccounts   int      `json:"open_accounts"`
	ClosedAccounts int      `json:"paused_or_lost_accounts"`
	ReplyLabels    []string `json:"reply_labels"`
	Outcomes       []string `json:"task_outcomes"`
	Note           string   `json:"note"`
}

var stateRank = map[string]int{"past": 0, "near": 1, "ok": 2}

func timed(kind, key string, since time.Time, target time.Duration, now time.Time) Item {
	it := Item{Kind: kind, Key: key, Since: since, Due: since.Add(target), AgeSeconds: int64(now.Sub(since).Seconds()),
		TargetSeconds: int64(target.Seconds()), State: "ok"}
	switch age := now.Sub(since); {
	case target <= 0:
	case age >= target:
		it.State = "past"
	case float64(age) >= NearShare*float64(target):
		it.State = "near"
	}
	return it
}

// isHeld reports an approved message the send check is holding back (a
// pause or a limit); the reason is in its error.
func isHeld(a *outreach.Action) bool { return strings.HasPrefix(a.Error, "held: ") }

// GetToday lists what waits on the operator for one product, or all when
// product is "". Replies wait ReplyTarget, drafts ApprovalTarget, held sends
// HeldTarget; tasks are listed when due by the end of today.
func GetToday(ctx context.Context, out *outreach.Service, products []*product.Product, productID string, now time.Time) (*Today, error) {
	t := &Today{Now: now, Product: productID, Items: []Item{}, Counts: map[string]Count{},
		Targets:     Targets{int64(ReplyTarget.Seconds()), int64(ApprovalTarget.Seconds()), int64(HeldTarget.Seconds()), NearShare, int64(TaskSoon.Seconds())},
		ReplyLabels: outreach.Classifications, Outcomes: outreach.Outcomes,
		Note: "Approve sends exactly the text shown, from that sender to that recipient. Editing a draft withdraws any approval; approve the saved text again."}

	replies, total, err := out.RepliesToLabel(ctx, productID, todayLimit)
	if err != nil {
		return nil, err
	}
	t.Counts["reply"] = Count{Total: total}
	for _, r := range replies {
		it := timed("reply", "reply:"+itoa(r.ID), r.ReceivedAt, ReplyTarget, now)
		it.Reply = r
		t.Items = append(t.Items, it)
	}

	drafts, err := out.Actions(ctx, productID, "draft")
	if err != nil {
		return nil, err
	}
	for _, a := range drafts {
		it := timed("approval", "action:"+itoa(a.ID), a.CreatedAt, ApprovalTarget, now)
		it.Action = a
		t.Items = append(t.Items, it)
	}
	sends, err := out.Actions(ctx, productID, "unknown", "failed", "blocked", "approved", "sending")
	if err != nil {
		return nil, err
	}
	for _, a := range sends {
		kind, target := "held", HeldTarget
		if (a.Status == "approved" && !isHeld(a)) || a.Status == "sending" {
			kind, target = "queued", 0
		}
		it := timed(kind, "action:"+itoa(a.ID), a.UpdatedAt, target, now)
		it.Action = a
		t.Items = append(t.Items, it)
	}
	var drafted, held, queued int
	if err := out.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'draft'),
			count(*) FILTER (WHERE status IN ('unknown', 'failed', 'blocked') OR (status = 'approved' AND error LIKE 'held: %')),
			count(*) FILTER (WHERE status = 'sending' OR (status = 'approved' AND error NOT LIKE 'held: %'))
		FROM actions WHERE ($1 = '' OR product_id = $1)`, productID).Scan(&drafted, &held, &queued); err != nil {
		return nil, err
	}
	t.Counts["approval"], t.Counts["held"], t.Counts["queued"] = Count{Total: drafted}, Count{Total: held}, Count{Total: queued}

	y, m, d := now.In(time.Local).Date()
	endOfDay := time.Date(y, m, d, 0, 0, 0, 0, time.Local).AddDate(0, 0, 1)
	var dueToday int
	if err := out.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE due_at < $1), count(*) FILTER (WHERE due_at >= $1)
		FROM tasks WHERE status = 'open' AND ($2 = '' OR product_id = $2)`, endOfDay, productID).Scan(&dueToday, &t.LaterTasks); err != nil {
		return nil, err
	}
	t.Counts["task"] = Count{Total: dueToday}
	tasks, err := out.Tasks(ctx, true, 1000)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, task := range tasks {
		if !task.DueAt.Before(endOfDay) || (productID != "" && task.ProductID != productID) || n >= todayLimit {
			continue
		}
		n++
		it := Item{Kind: "task", Key: "task:" + itoa(task.ID), Since: task.DueAt, Due: task.DueAt,
			AgeSeconds: int64(now.Sub(task.DueAt).Seconds()), State: "ok", Task: task}
		switch {
		case !now.Before(task.DueAt):
			it.State = "past"
		case task.DueAt.Sub(now) <= TaskSoon:
			it.State = "near"
		}
		t.Items = append(t.Items, it)
	}

	if err := t.accounts(ctx, out, products, productID, now); err != nil {
		return nil, err
	}

	// Most urgent first: past target, then near it, then the rest; within
	// each, whatever passes (or passed) its target first.
	slices.SortStableFunc(t.Items, func(a, b Item) int {
		if r := stateRank[a.State] - stateRank[b.State]; r != 0 {
			return r
		}
		return a.Due.Compare(b.Due)
	})
	for _, it := range t.Items {
		c := t.Counts[it.Kind]
		c.Listed++
		switch it.State {
		case "past":
			c.Past++
		case "near":
			c.Near++
		}
		t.Counts[it.Kind] = c
	}
	return t, nil
}

// accounts adds pipeline accounts whose next action is overdue, and paused
// or lost accounts whose revisit date has come and that have no revisit
// task yet (once the task exists it is listed as a task).
func (t *Today) accounts(ctx context.Context, out *outreach.Service, products []*product.Product, productID string, now time.Time) error {
	t.Counts["account"], t.Counts["revisit"] = Count{}, Count{}
	for _, p := range products {
		if p.Pipeline == nil || (productID != "" && p.ID != productID) {
			continue
		}
		b, err := pipeline.BuildBoard(ctx, out.Pool, p, now)
		if err != nil {
			return err
		}
		t.OpenAccounts += len(b.Open)
		t.ClosedAccounts += b.Paused + b.Lost
		y, m, d := now.In(time.Local).Date()
		today := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
		day := func(s string) time.Time {
			v, _ := time.ParseInLocation("2006-01-02", s, time.Local)
			return v
		}
		for _, a := range b.Overdue {
			due := day(a.NextDue)
			t.Items = append(t.Items, Item{Kind: "account", Key: "account:" + itoa(a.ID), Since: due, Due: due,
				AgeSeconds: int64(now.Sub(due).Seconds()), State: "past", Account: a})
			c := t.Counts["account"]
			c.Total++
			t.Counts["account"] = c
		}
		rows, err := out.Pool.Query(ctx, `SELECT id FROM accounts WHERE product_id = $1 AND revisit_task_id IS NOT NULL`, p.ID)
		if err != nil {
			return err
		}
		tasked := map[int64]bool{}
		if err := forRows(rows, func(r pgx.Rows) error {
			var id int64
			err := r.Scan(&id)
			tasked[id] = true
			return err
		}); err != nil {
			return err
		}
		for _, a := range b.RevisitDue {
			if tasked[a.ID] {
				continue
			}
			on := day(a.RevisitOn)
			state := "near"
			if on.Before(today) {
				state = "past"
			}
			t.Items = append(t.Items, Item{Kind: "revisit", Key: "revisit:" + itoa(a.ID), Since: on, Due: on,
				AgeSeconds: int64(now.Sub(on).Seconds()), State: state, Account: a})
			c := t.Counts["revisit"]
			c.Total++
			t.Counts["revisit"] = c
		}
	}
	return nil
}

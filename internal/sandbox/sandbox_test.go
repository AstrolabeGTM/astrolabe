package sandbox_test

import (
	"context"
	"errors"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/sandbox"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

func TestModeIsFixedPerDatabase(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	if err := sandbox.EnsureMode(ctx, pool, "sandbox"); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.EnsureMode(ctx, pool, "live"); !errors.Is(err, sandbox.ErrModeMismatch) {
		t.Fatalf("switching to live must be refused: %v", err)
	}
	if err := sandbox.SetMode(ctx, pool, "live"); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.EnsureMode(ctx, pool, "live"); err != nil {
		t.Fatal(err)
	}
}

func TestWholeLoopInSandbox(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	product.Sync(ctx, pool, []*product.Product{p})
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	box := &sandbox.Sender{Pool: pool}
	out := &outreach.Service{Pool: pool, River: rc, Senders: func(channel.Message) (channel.Sender, error) { return box, nil }}

	people.Import(ctx, pool, "demo", []people.Row{{Email: "ana@acme.example", Name: "Ana"}, {Email: "bo@acme.example", Name: "Bo"}})
	out.Enroll(ctx, "demo", "intro", []int64{1, 2})
	out.Tick(ctx)
	drafts, _ := out.Actions(ctx, "demo", "draft")
	for _, d := range drafts {
		if err := out.Approve(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		if res, err := out.SendAction(ctx, d.ID); err != nil || res.Status != "sent" {
			t.Fatalf("send: %+v %v", res, err)
		}
	}
	msgs, _ := sandbox.Outbox(ctx, pool, 10)
	if len(msgs) != 2 || msgs[0].MessageID == "" {
		t.Fatalf("outbox: %+v", msgs)
	}
	byTo := map[string]sandbox.Message{}
	for _, m := range msgs {
		byTo[m.To] = m
	}
	// Ana replies, Bo bounces: both go through the real reply path.
	if err := sandbox.SimulateReply(ctx, out, byTo["ana@acme.example"].ID, "Sounds useful, how do I start?"); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.SimulateBounce(ctx, out, byTo["bo@acme.example"].ID); err != nil {
		t.Fatal(err)
	}
	var stopped []string
	rows, _ := pool.Query(ctx, `SELECT stop_reason FROM enrollments ORDER BY person_id`)
	for rows.Next() {
		var r string
		rows.Scan(&r)
		stopped = append(stopped, r)
	}
	if len(stopped) != 2 || stopped[0] != "replied" || stopped[1] != "bounced" {
		t.Fatalf("stops: %v", stopped)
	}
	replies, _ := out.Replies(ctx, true, 10)
	if len(replies) != 1 || replies[0].PersonName != "Ana" {
		t.Fatalf("replies to label: %+v", replies)
	}
}

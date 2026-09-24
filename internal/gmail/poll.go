package gmail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/astrolabe-gtm/astrolabe/internal/outreach"
)

type PollArgs struct{}

func (PollArgs) Kind() string { return "gmail_poll" }

// PollWorker reads every authorised mailbox for replies and bounces.
type PollWorker struct {
	river.WorkerDefaults[PollArgs]
	Accounts *Accounts
	S        *outreach.Service
}

// Overlap re-reads a window before the last poll so late-indexed messages
// are not missed; RecordReply ignores ones already stored.
const pollOverlap = time.Hour

func (w *PollWorker) Work(ctx context.Context, _ *river.Job[PollArgs]) error {
	var errs []error
	for _, addr := range w.Accounts.Addresses() {
		if err := w.pollOne(ctx, addr); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", addr, err))
		}
	}
	return errors.Join(errs...)
}

func (w *PollWorker) pollOne(ctx context.Context, addr string) error {
	mb, err := w.Accounts.Mailbox(addr)
	if err != nil {
		return err
	}
	started := time.Now()
	since := started.Add(-48 * time.Hour)
	var last time.Time
	err = w.S.Pool.QueryRow(ctx, `SELECT last_poll_at FROM mailbox_sync WHERE mailbox = $1`, addr).Scan(&last)
	if err == nil {
		since = last.Add(-pollOverlap)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	msgs, err := mb.Poll(ctx, since)
	if err != nil {
		return err
	}
	n := 0
	for _, m := range msgs {
		ok, err := w.S.RecordReply(ctx, m)
		if err != nil {
			return err
		}
		if ok {
			n++
		}
	}
	if n > 0 {
		slog.Info("new replies", "mailbox", addr, "count", n)
	}
	_, err = w.S.Pool.Exec(ctx, `
		INSERT INTO mailbox_sync (mailbox, last_poll_at) VALUES ($1, $2)
		ON CONFLICT (mailbox) DO UPDATE SET last_poll_at = $2`, addr, started)
	return err
}

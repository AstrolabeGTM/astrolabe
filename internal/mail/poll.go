package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
)

type PollArgs struct{}

func (PollArgs) Kind() string { return "imap_poll" }

// PollWorker reads new inbox mail for every SMTP/IMAP mailbox.
type PollWorker struct {
	river.WorkerDefaults[PollArgs]
	Store *Store
	S     *outreach.Service
}

func (w *PollWorker) Work(ctx context.Context, _ *river.Job[PollArgs]) error {
	accounts, err := w.Store.List()
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range accounts {
		if err := w.pollOne(ctx, a); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Address, err))
		}
	}
	return errors.Join(errs...)
}

func (w *PollWorker) pollOne(ctx context.Context, a *Account) error {
	key := "imap:" + a.Address
	var cur Cursor
	var validity, last *int64
	err := w.S.Pool.QueryRow(ctx, `SELECT uid_validity, last_uid FROM mailbox_sync WHERE mailbox = $1`, key).Scan(&validity, &last)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if validity != nil && last != nil {
		cur = Cursor{UIDValidity: uint32(*validity), LastUID: uint32(*last)}
	}
	msgs, next, err := Poll(a, cur)
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
		slog.Info("new replies", "mailbox", a.Address, "count", n)
	}
	_, err = w.S.Pool.Exec(ctx, `
		INSERT INTO mailbox_sync (mailbox, last_poll_at, uid_validity, last_uid) VALUES ($1, now(), $2, $3)
		ON CONFLICT (mailbox) DO UPDATE SET last_poll_at = now(), uid_validity = $2, last_uid = $3`,
		key, int64(next.UIDValidity), int64(next.LastUID))
	return err
}

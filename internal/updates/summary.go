package updates

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Summary is the list's week: size changes and broadcast status, for the
// weekly digest.
type Summary struct {
	Name            string       `json:"name"`
	Pending         int          `json:"pending"`
	Confirmed       int          `json:"confirmed"`
	Unsubscribed    int          `json:"unsubscribed"`
	NewConfirmed    int          `json:"new_confirmed"`
	NewUnsubscribed int          `json:"new_unsubscribed"`
	Broadcasts      []*Broadcast `json:"broadcasts,omitempty"`
}

// WeekSummary covers the 7 days before now. Broadcasts are those created or
// approved in that time, plus any still sending.
func WeekSummary(ctx context.Context, pool *pgxpool.Pool, productID, name string, now time.Time) (*Summary, error) {
	s := &Service{Pool: pool, Now: func() time.Time { return now }}
	c, err := s.Counts(ctx, productID)
	if err != nil {
		return nil, err
	}
	sum := &Summary{Name: name, Pending: c.Pending, Confirmed: c.Confirmed, Unsubscribed: c.Unsubscribed}
	week := now.AddDate(0, 0, -7)
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE confirmed_at >= $2 AND status = 'confirmed'), count(*) FILTER (WHERE unsubscribed_at >= $2)
		FROM updates_subscribers WHERE product_id = $1`, productID, week).Scan(&sum.NewConfirmed, &sum.NewUnsubscribed); err != nil {
		return nil, err
	}
	all, err := s.Broadcasts(ctx, productID, 10)
	if err != nil {
		return nil, err
	}
	for _, b := range all {
		if b.CreatedAt.After(week) || (b.ApprovedAt != nil && b.ApprovedAt.After(week)) || b.State == "sending" {
			sum.Broadcasts = append(sum.Broadcasts, b)
		}
	}
	return sum, nil
}

// Empty reports a list with nothing to show.
func (m *Summary) Empty() bool {
	return m.Pending+m.Confirmed+m.Unsubscribed == 0 && len(m.Broadcasts) == 0
}

// Text renders the summary for the digest.
func (m *Summary) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d confirmed (+%d this week, %d unsubscribed), %d waiting to confirm\n", m.Name, m.Confirmed, m.NewConfirmed, m.NewUnsubscribed, m.Pending)
	for _, x := range m.Broadcasts {
		fmt.Fprintf(&b, "  Broadcast #%d %q: %s", x.ID, x.Subject, x.State)
		if x.Sent+x.Queued+x.Unknown+x.Failed > 0 {
			fmt.Fprintf(&b, " (%d sent, %d to go", x.Sent, x.Queued)
			if x.Unknown > 0 {
				fmt.Fprintf(&b, ", %d unknown: check the mailbox", x.Unknown)
			}
			b.WriteString(")")
		}
		if x.Note != "" {
			b.WriteString(" - " + x.Note)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Package campaign runs campaigns: the settings an operator changes at
// runtime (budget cap, dates, stop rule, gate checks) with a history of every
// change, spend records, creatives with approval, posting tasks and the
// report. The YAML in products/<id>/campaigns/ gives the defaults. Nothing
// here posts anything, talks to an ad platform or raises spend by itself.
// All functions are plain Go over the database so the browser app can reuse
// them.
package campaign

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

const dateLayout = "2006-01-02"

// Settings are the effective values: a runtime override if one was set,
// otherwise the YAML default.
type Settings struct {
	Cap      product.Money      `json:"budget_cap"`
	Currency string             `json:"currency"`
	Start    string             `json:"start"`
	End      string             `json:"end,omitempty"`
	StopText string             `json:"stop_rule_text,omitempty"`
	StopWhen *product.StopCheck `json:"stop_rule_when,omitempty"`
}

// StopDescription is the stop rule for people: its text and, if there is one, the checkable form.
func (s Settings) StopDescription() string {
	out := s.StopText
	if s.StopWhen != nil {
		out += fmt.Sprintf(" [checked: spend at %d%% of the cap with %d or fewer success events]", s.StopWhen.SpendPct, s.StopWhen.MaxEvents)
	}
	return strings.TrimSpace(out)
}

// Started reports whether the start date has come.
func (s Settings) Started(now time.Time) bool { return s.Start <= now.Format(dateLayout) }

// Ended reports whether the end date has passed.
func (s Settings) Ended(now time.Time) bool { return s.End != "" && s.End < now.Format(dateLayout) }

// Running is started and not ended.
func (s Settings) Running(now time.Time) bool { return s.Started(now) && !s.Ended(now) }

// NotEnded includes campaigns that have not started yet (their landing pages
// need to exist before the first day).
func (s Settings) NotEnded(now time.Time) bool { return !s.Ended(now) }

type override struct {
	cap         *int64
	currency    *string
	start, end  *time.Time
	endCleared  bool
	stopText    *string
	pct, maxEv  *int
	stopCleared bool
}

func readOverride(ctx context.Context, q store.Q, productID, campaignID string) (override, error) {
	var o override
	err := q.QueryRow(ctx, `
		SELECT budget_cap, currency, start_date, end_date, end_cleared, stop_text, stop_spend_pct, stop_max_events, stop_cleared
		FROM campaign_settings WHERE product_id = $1 AND campaign_id = $2`, productID, campaignID).
		Scan(&o.cap, &o.currency, &o.start, &o.end, &o.endCleared, &o.stopText, &o.pct, &o.maxEv, &o.stopCleared)
	if errors.Is(err, pgx.ErrNoRows) {
		return override{}, nil
	}
	return o, err
}

func (o override) apply(c *product.Campaign) Settings {
	s := Settings{Cap: c.Budget.Cap, Currency: c.Budget.Currency, Start: c.Start, End: c.End, StopText: c.StopRule.Text}
	if c.StopRule.When != nil {
		w := *c.StopRule.When
		s.StopWhen = &w
	}
	if o.cap != nil {
		s.Cap = product.Money(*o.cap)
	}
	if o.currency != nil {
		s.Currency = *o.currency
	}
	if o.start != nil {
		s.Start = o.start.Format(dateLayout)
	}
	if o.endCleared {
		s.End = ""
	} else if o.end != nil {
		s.End = o.end.Format(dateLayout)
	}
	if o.stopText != nil {
		s.StopText = *o.stopText
	}
	if o.stopCleared {
		s.StopWhen = nil
	} else if o.pct != nil {
		w := product.StopCheck{SpendPct: *o.pct}
		if s.StopWhen != nil {
			w.MaxEvents = s.StopWhen.MaxEvents
		}
		if o.maxEv != nil {
			w.MaxEvents = *o.maxEv
		}
		s.StopWhen = &w
	} else if o.maxEv != nil && s.StopWhen != nil {
		s.StopWhen.MaxEvents = *o.maxEv
	}
	return s
}

// Effective returns a campaign's settings with runtime changes applied.
func Effective(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign) (Settings, error) {
	o, err := readOverride(ctx, q, p.ID, c.ID)
	if err != nil {
		return Settings{}, err
	}
	return o.apply(c), nil
}

// Change is a set of new values; nil fields are left alone.
type Change struct {
	BudgetCap *product.Money `json:"budget_cap,omitempty"`
	Currency  *string        `json:"currency,omitempty"`
	Start     *string        `json:"start,omitempty"`
	End       *string        `json:"end,omitempty"` // "" removes the end date
	StopText  *string        `json:"stop_rule_text,omitempty"`
	// StopSpendPct and StopMaxEvents set the checkable form of the stop rule;
	// ClearStopRule removes it.
	StopSpendPct  *int   `json:"stop_spend_pct,omitempty"`
	StopMaxEvents *int   `json:"stop_max_events,omitempty"`
	ClearStopRule bool   `json:"clear_stop_rule,omitempty"`
	Note          string `json:"note,omitempty"`
}

// HistoryEntry is one change, kept forever.
type HistoryEntry struct {
	ID        int64     `json:"id"`
	Field     string    `json:"field"`
	Old       string    `json:"old"`
	New       string    `json:"new"`
	ChangedBy string    `json:"changed_by"`
	Note      string    `json:"note,omitempty"`
	At        time.Time `json:"at"`
}

var currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

// Set changes budget, dates and stop rule. Every field that actually changes
// is recorded in the history with who changed it; setting a value to what it
// already is records nothing. The cap is only ever changed by a call like
// this one, never by the system.
func Set(ctx context.Context, pool *pgxpool.Pool, p *product.Product, c *product.Campaign, by string, ch Change) ([]HistoryEntry, error) {
	if strings.TrimSpace(by) == "" {
		return nil, errors.New("say who is changing it")
	}
	var recorded []HistoryEntry
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO campaign_settings (product_id, campaign_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, p.ID, c.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM campaign_settings WHERE product_id = $1 AND campaign_id = $2 FOR UPDATE`, p.ID, c.ID); err != nil {
			return err
		}
		o, err := readOverride(ctx, tx, p.ID, c.ID)
		if err != nil {
			return err
		}
		before := o.apply(c)
		next := o
		if ch.BudgetCap != nil {
			if *ch.BudgetCap < 0 {
				return errors.New("budget cap must not be negative")
			}
			v := int64(*ch.BudgetCap)
			next.cap = &v
		}
		if ch.Currency != nil {
			if !currencyRE.MatchString(*ch.Currency) {
				return fmt.Errorf("currency %q must be a 3-letter code such as USD", *ch.Currency)
			}
			if *ch.Currency != before.Currency {
				var n int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM campaign_spend WHERE product_id = $1 AND campaign_id = $2`, p.ID, c.ID).Scan(&n); err != nil {
					return err
				}
				if n > 0 {
					return fmt.Errorf("%d spend entries are already recorded in %s; Astrolabe does not convert currencies", n, before.Currency)
				}
			}
			next.currency = ch.Currency
		}
		parse := func(name, v string) (*time.Time, error) {
			t, err := time.Parse(dateLayout, v)
			if err != nil {
				return nil, fmt.Errorf("%s must be a date like 2026-12-01", name)
			}
			return &t, nil
		}
		if ch.Start != nil {
			if next.start, err = parse("start", *ch.Start); err != nil {
				return err
			}
		}
		if ch.End != nil {
			if *ch.End == "" {
				next.end, next.endCleared = nil, true
			} else {
				if next.end, err = parse("end", *ch.End); err != nil {
					return err
				}
				next.endCleared = false
			}
		}
		if ch.StopText != nil {
			next.stopText = ch.StopText
		}
		if ch.ClearStopRule && (ch.StopSpendPct != nil || ch.StopMaxEvents != nil) {
			return errors.New("clear_stop_rule cannot be combined with a new rule")
		}
		if ch.ClearStopRule {
			next.stopCleared, next.pct, next.maxEv = true, nil, nil
		}
		if ch.StopSpendPct != nil {
			if *ch.StopSpendPct < 1 || *ch.StopSpendPct > 100 {
				return errors.New("stop_spend_pct must be 1-100")
			}
			next.pct, next.stopCleared = ch.StopSpendPct, false
			if next.maxEv == nil {
				z := 0
				if before.StopWhen != nil {
					z = before.StopWhen.MaxEvents
				}
				next.maxEv = &z
			}
		}
		if ch.StopMaxEvents != nil {
			if *ch.StopMaxEvents < 0 {
				return errors.New("stop_max_events must not be negative")
			}
			next.maxEv = ch.StopMaxEvents
			if next.pct == nil && before.StopWhen == nil {
				return errors.New("stop_max_events needs a stop_spend_pct (there is no checkable stop rule yet)")
			}
			next.pct = firstInt(next.pct, before.StopWhen)
		}
		after := next.apply(c)
		if after.End != "" && after.End < after.Start {
			return errors.New("end must not be before start")
		}
		if _, err := tx.Exec(ctx, `
			UPDATE campaign_settings SET budget_cap = $3, currency = $4, start_date = $5, end_date = $6, end_cleared = $7,
				stop_text = $8, stop_spend_pct = $9, stop_max_events = $10, stop_cleared = $11, updated_at = now()
			WHERE product_id = $1 AND campaign_id = $2`, p.ID, c.ID,
			next.cap, next.currency, next.start, next.end, next.endCleared, next.stopText, next.pct, next.maxEv, next.stopCleared); err != nil {
			return err
		}
		diffs := []struct{ field, old, new string }{
			{"budget_cap", before.Cap.String(), after.Cap.String()},
			{"currency", before.Currency, after.Currency},
			{"start", before.Start, after.Start},
			{"end", before.End, after.End},
			{"stop_rule", before.StopDescription(), after.StopDescription()},
		}
		for _, d := range diffs {
			if d.old == d.new {
				continue
			}
			h := HistoryEntry{Field: d.field, Old: d.old, New: d.new, ChangedBy: by, Note: ch.Note}
			if err := tx.QueryRow(ctx, `
				INSERT INTO campaign_history (product_id, campaign_id, field, old_value, new_value, changed_by, note)
				VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, changed_at`,
				p.ID, c.ID, d.field, d.old, d.new, by, ch.Note).Scan(&h.ID, &h.At); err != nil {
				return err
			}
			recorded = append(recorded, h)
		}
		return nil
	})
	return recorded, err
}

func firstInt(set *int, w *product.StopCheck) *int {
	if set != nil {
		return set
	}
	v := w.SpendPct
	return &v
}

// History lists changes to settings and gate checks, oldest first.
func History(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign) ([]HistoryEntry, error) {
	rows, err := q.Query(ctx, `
		SELECT id, field, old_value, new_value, changed_by, note, changed_at FROM campaign_history
		WHERE product_id = $1 AND campaign_id = $2 ORDER BY id`, p.ID, c.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (HistoryEntry, error) {
		var h HistoryEntry
		err := r.Scan(&h.ID, &h.Field, &h.Old, &h.New, &h.ChangedBy, &h.Note, &h.At)
		return h, err
	})
}

// ---- gate ----

type GateItem struct {
	Item      string     `json:"item"`
	Checked   bool       `json:"checked"`
	CheckedBy string     `json:"checked_by,omitempty"`
	Note      string     `json:"note,omitempty"`
	At        *time.Time `json:"checked_at,omitempty"`
}

// Gate lists the gate items with their state, and whether all are checked.
// A campaign with no gate items has an empty, complete gate.
func Gate(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign) ([]GateItem, bool, error) {
	rows, err := q.Query(ctx, `SELECT item, checked_by, note, checked_at FROM campaign_gate WHERE product_id = $1 AND campaign_id = $2`, p.ID, c.ID)
	if err != nil {
		return nil, false, err
	}
	checked := map[string]GateItem{}
	for rows.Next() {
		var g GateItem
		var at time.Time
		if err := rows.Scan(&g.Item, &g.CheckedBy, &g.Note, &at); err != nil {
			return nil, false, err
		}
		g.Checked, g.At = true, &at
		checked[g.Item] = g
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	out := make([]GateItem, 0, len(c.Gate))
	all := true
	for _, item := range c.Gate {
		g, ok := checked[item]
		if !ok {
			g = GateItem{Item: item}
			all = false
		}
		out = append(out, g)
	}
	return out, all, nil
}

// GateItemFor finds a gate item by its exact text or its 1-based number.
func GateItemFor(c *product.Campaign, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	for _, g := range c.Gate {
		if g == ref {
			return g, nil
		}
	}
	var n int
	if _, err := fmt.Sscanf(ref, "%d", &n); err == nil && fmt.Sprint(n) == ref && n >= 1 && n <= len(c.Gate) {
		return c.Gate[n-1], nil
	}
	return "", fmt.Errorf("no gate item %q (items: %q)", ref, c.Gate)
}

// CheckGate checks or unchecks one gate item and records who did it.
func CheckGate(ctx context.Context, pool *pgxpool.Pool, p *product.Product, c *product.Campaign, ref string, checked bool, by, note string) (GateItem, error) {
	item, err := GateItemFor(c, ref)
	if err != nil {
		return GateItem{}, err
	}
	if strings.TrimSpace(by) == "" {
		return GateItem{}, errors.New("say who is checking it")
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var was bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM campaign_gate WHERE product_id = $1 AND campaign_id = $2 AND item = $3)`, p.ID, c.ID, item).Scan(&was); err != nil {
			return err
		}
		if checked {
			if _, err := tx.Exec(ctx, `
				INSERT INTO campaign_gate (product_id, campaign_id, item, checked_by, note) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (product_id, campaign_id, item) DO UPDATE SET checked_by = $4, note = $5, checked_at = now()`,
				p.ID, c.ID, item, by, note); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `DELETE FROM campaign_gate WHERE product_id = $1 AND campaign_id = $2 AND item = $3`, p.ID, c.ID, item); err != nil {
			return err
		}
		if was == checked {
			return nil // nothing changed; the note on a re-check is kept above
		}
		return recordGate(ctx, tx, p.ID, c.ID, item, was, checked, by, note)
	})
	if err != nil {
		return GateItem{}, err
	}
	items, _, err := Gate(ctx, pool, p, c)
	for _, g := range items {
		if g.Item == item {
			return g, err
		}
	}
	return GateItem{}, err
}

func recordGate(ctx context.Context, tx pgx.Tx, productID, campaignID, item string, was, now bool, by, note string) error {
	state := func(b bool) string {
		if b {
			return "checked"
		}
		return "unchecked"
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO campaign_history (product_id, campaign_id, field, old_value, new_value, changed_by, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, productID, campaignID, "gate:"+item, state(was), state(now), by, note)
	return err
}

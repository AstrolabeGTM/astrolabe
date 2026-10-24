package campaign

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// Spend is one line of money spent, as entered or imported.
type Spend struct {
	ID          int64         `json:"id"`
	Date        string        `json:"date"`
	Amount      product.Money `json:"amount"`
	Currency    string        `json:"currency"`
	Platform    string        `json:"platform"`
	Note        string        `json:"note,omitempty"`
	Clicks      *int64        `json:"clicks,omitempty"` // the platform's own count, as entered
	Impressions *int64        `json:"impressions,omitempty"`
	Source      string        `json:"source"`
	// BeforeGate is true when this was recorded while the gate was not fully
	// checked. It is a fact about the moment of recording and never changes.
	BeforeGate bool   `json:"before_gate"`
	CreatedBy  string `json:"created_by"`
}

// SpendInput is a spend entry made by hand.
type SpendInput struct {
	Date        string        `json:"date"` // YYYY-MM-DD
	Amount      product.Money `json:"amount"`
	Platform    string        `json:"platform"`
	Note        string        `json:"note,omitempty"`
	Clicks      *int64        `json:"clicks,omitempty"`
	Impressions *int64        `json:"impressions,omitempty"`
}

// SpendResult is what was recorded and what to be careful about now.
type SpendResult struct {
	Recorded []Spend  `json:"recorded"`
	Warnings []string `json:"warnings"`
}

// RecordSpend records spend entered by hand. Spend on a campaign whose gate
// is not fully checked is allowed but flagged on the entry and in the warnings.
func RecordSpend(ctx context.Context, pool *pgxpool.Pool, p *product.Product, c *product.Campaign, by string, in SpendInput, now time.Time) (*SpendResult, error) {
	return record(ctx, pool, p, c, by, []SpendInput{in}, "manual", now)
}

func record(ctx context.Context, pool *pgxpool.Pool, p *product.Product, c *product.Campaign, by string, ins []SpendInput, source string, now time.Time) (*SpendResult, error) {
	if strings.TrimSpace(by) == "" {
		return nil, errors.New("say who is recording it")
	}
	res := &SpendResult{}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		s, err := Effective(ctx, tx, p, c)
		if err != nil {
			return err
		}
		_, gateOK, err := Gate(ctx, tx, p, c)
		if err != nil {
			return err
		}
		for _, in := range ins {
			if _, err := time.Parse(dateLayout, in.Date); err != nil {
				return fmt.Errorf("date %q must look like 2026-12-02", in.Date)
			}
			if in.Amount < 0 {
				return errors.New("amount must not be negative")
			}
			in.Platform = strings.ToLower(strings.TrimSpace(in.Platform))
			if in.Platform == "" {
				return errors.New("say which platform the spend was on (e.g. linkedin, reddit, newsletter)")
			}
			sp := Spend{Date: in.Date, Amount: in.Amount, Currency: s.Currency, Platform: in.Platform, Note: in.Note,
				Clicks: in.Clicks, Impressions: in.Impressions, Source: source, BeforeGate: !gateOK, CreatedBy: by}
			q := `
				INSERT INTO campaign_spend (product_id, campaign_id, spent_on, amount, currency, platform, note, clicks, impressions, source, gate_complete, created_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`
			if source == "csv" {
				q += ` ON CONFLICT (product_id, campaign_id, platform, spent_on) WHERE source = 'csv'
					DO UPDATE SET amount = EXCLUDED.amount, clicks = EXCLUDED.clicks, impressions = EXCLUDED.impressions, note = EXCLUDED.note,
						gate_complete = campaign_spend.gate_complete AND EXCLUDED.gate_complete, created_by = EXCLUDED.created_by`
			}
			q += ` RETURNING id`
			if err := tx.QueryRow(ctx, q, p.ID, c.ID, in.Date, int64(in.Amount), s.Currency, in.Platform, in.Note, in.Clicks, in.Impressions, source, gateOK, by).Scan(&sp.ID); err != nil {
				return err
			}
			res.Recorded = append(res.Recorded, sp)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	st, err := Check(ctx, pool, p, c, now)
	if err != nil {
		return nil, err
	}
	res.Warnings = st.Warnings
	return res, nil
}

// SpendList returns spend entries, newest first.
func SpendList(ctx context.Context, q store.Q, p *product.Product, c *product.Campaign, limit int) ([]Spend, error) {
	rows, err := q.Query(ctx, `
		SELECT id, to_char(spent_on, 'YYYY-MM-DD'), amount, currency, platform, note, clicks, impressions, source, NOT gate_complete, created_by
		FROM campaign_spend WHERE product_id = $1 AND campaign_id = $2 ORDER BY spent_on DESC, id DESC LIMIT $3`, p.ID, c.ID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Spend, error) {
		var s Spend
		var amt int64
		err := r.Scan(&s.ID, &s.Date, &amt, &s.Currency, &s.Platform, &s.Note, &s.Clicks, &s.Impressions, &s.Source, &s.BeforeGate, &s.CreatedBy)
		s.Amount = product.Money(amt)
		return s, err
	})
}

// ImportResult says what a CSV import did.
type ImportResult struct {
	Rows     int      `json:"rows"`
	Recorded int      `json:"recorded"`
	Skipped  []string `json:"skipped,omitempty"`
	Total    string   `json:"total"`
	Warnings []string `json:"warnings"`
}

var csvAliases = map[string][]string{
	"date":        {"date", "day", "reporting starts", "start date", "spent on"},
	"amount":      {"amount", "spend", "cost", "amount spent", "total spent", "total spend"},
	"clicks":      {"clicks", "link clicks", "total clicks"},
	"impressions": {"impressions", "impr.", "impr"},
	"platform":    {"platform", "network", "source"},
	"note":        {"note", "notes", "campaign name", "campaign"},
}

var dateFormats = []string{dateLayout, "Jan 2, 2006", "2 Jan 2006", "January 2, 2006", "2 January 2006"}

// ImportSpend reads a platform's spend and clicks CSV (a header row with date,
// spend/cost/amount and optionally clicks, impressions, platform). Rows for
// the same platform and day are added together; importing the same file
// again replaces those days instead of adding to them. Amounts may carry a
// currency symbol; the campaign's currency is assumed. A row that cannot be
// read is skipped and reported; the rest are imported.
func ImportSpend(ctx context.Context, pool *pgxpool.Pool, p *product.Product, c *product.Campaign, by, defaultPlatform string, r io.Reader, now time.Time) (*ImportResult, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	recs, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read CSV: %w", err)
	}
	if len(recs) < 2 {
		return nil, errors.New("the CSV needs a header row and at least one row")
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		h = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\xef\xbb\xbf")))
		for canon, names := range csvAliases {
			for _, n := range names {
				if h == n {
					if _, taken := col[canon]; !taken {
						col[canon] = i
					}
				}
			}
		}
	}
	for _, need := range []string{"date", "amount"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("the CSV needs a %q column (accepted: %s)", need, strings.Join(csvAliases[need], ", "))
		}
	}
	if _, ok := col["platform"]; !ok && strings.TrimSpace(defaultPlatform) == "" {
		return nil, errors.New("the CSV has no platform column; say which platform it is from")
	}
	res := &ImportResult{}
	type key struct{ platform, date string }
	agg := map[key]*SpendInput{}
	var order []key
	for n, rec := range recs[1:] {
		line := n + 2
		get := func(name string) string {
			if i, ok := col[name]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		if get("date") == "" && get("amount") == "" {
			continue // blank row
		}
		res.Rows++
		date, derr := parseDate(get("date"))
		amt, aerr := parseAmount(get("amount"))
		platform := strings.ToLower(get("platform"))
		if platform == "" {
			platform = strings.ToLower(strings.TrimSpace(defaultPlatform))
		}
		switch {
		case derr != nil:
			res.Skipped = append(res.Skipped, fmt.Sprintf("line %d: %v", line, derr))
			continue
		case aerr != nil:
			res.Skipped = append(res.Skipped, fmt.Sprintf("line %d: %v", line, aerr))
			continue
		case platform == "":
			res.Skipped = append(res.Skipped, fmt.Sprintf("line %d: no platform", line))
			continue
		}
		in := SpendInput{Date: date, Amount: amt, Platform: platform}
		var bad error
		for name, dst := range map[string]**int64{"clicks": &in.Clicks, "impressions": &in.Impressions} {
			if v := get(name); v != "" {
				n, err := strconv.ParseInt(strings.ReplaceAll(v, ",", ""), 10, 64)
				if err != nil || n < 0 {
					bad = fmt.Errorf("line %d: %s %q is not a count", line, name, v)
					break
				}
				*dst = &n
			}
		}
		if bad != nil {
			res.Skipped = append(res.Skipped, bad.Error())
			continue
		}
		k := key{platform, date}
		if prev, ok := agg[k]; ok {
			prev.Amount += in.Amount
			prev.Clicks, prev.Impressions = addCount(prev.Clicks, in.Clicks), addCount(prev.Impressions, in.Impressions)
			continue
		}
		agg[k] = &in
		order = append(order, k)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].date != order[j].date {
			return order[i].date < order[j].date
		}
		return order[i].platform < order[j].platform
	})
	var ins []SpendInput
	var total product.Money
	for _, k := range order {
		ins = append(ins, *agg[k])
		total += agg[k].Amount
	}
	res.Total = total.String()
	if len(ins) > 0 {
		out, err := record(ctx, pool, p, c, by, ins, "csv", now)
		if err != nil {
			return nil, err
		}
		res.Recorded, res.Warnings = len(out.Recorded), out.Warnings
	}
	return res, nil
}

func addCount(a, b *int64) *int64 {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	v := *a + *b
	return &v
}

func parseDate(s string) (string, error) {
	for _, f := range dateFormats {
		if t, err := time.Parse(f, s); err == nil {
			return t.Format(dateLayout), nil
		}
	}
	return "", fmt.Errorf("date %q: use YYYY-MM-DD (day/month order of 02/03/2026 is ambiguous)", s)
}

func parseAmount(s string) (product.Money, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimFunc(s, func(r rune) bool { return !(r >= '0' && r <= '9') && r != '.' && r != '-' })
	m, err := product.ParseMoney(s)
	if err != nil {
		return 0, fmt.Errorf("amount: %w", err)
	}
	return m, nil
}

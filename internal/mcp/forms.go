package mcp

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type formSubmissionsArg struct {
	Product string `json:"product" jsonschema:"product id"`
	Monitor string `json:"monitor,omitempty" jsonschema:"only this form monitor"`
	Days    int    `json:"days,omitempty" jsonschema:"look back this many days (default 30)"`
	Limit   int    `json:"limit,omitempty" jsonschema:"default 50"`
}

// formSubmission is one form request with who sent it and where it went.
type formSubmission struct {
	ID             int64          `json:"id"`
	Monitor        string         `json:"monitor"`
	Source         string         `json:"source"`
	SubmittedAt    time.Time      `json:"submitted_at"`
	Person         *formPerson    `json:"person,omitempty"`
	Company        string         `json:"company,omitempty"`
	Account        *formAccount   `json:"account,omitempty"`
	ReportedSource string         `json:"reported_source,omitempty"`
	Campaign       string         `json:"campaign,omitempty"`
	SignalID       *int64         `json:"signal_id,omitempty"`
	Fields         map[string]any `json:"fields"`
}

type formPerson struct {
	ID    int64  `json:"id"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

type formAccount struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Stage      string `json:"stage"`
	NextAction string `json:"next_action,omitempty"`
	NextDue    string `json:"next_due,omitempty"`
}

func (s *Server) formSubmissions(ctx context.Context, a formSubmissionsArg) (any, error) {
	if _, err := s.find(a.Product); err != nil {
		return nil, err
	}
	if a.Days <= 0 {
		a.Days = 30
	}
	if a.Limit <= 0 {
		a.Limit = 50
	}
	rows, err := s.S.Pool.Query(ctx, `
		SELECT f.id, f.monitor_id, f.source, f.submitted_at, f.reported_source, f.campaign, f.signal_id, f.fields,
			p.id, COALESCE(p.name, ''), COALESCE((SELECT value FROM identities WHERE person_id = p.id AND kind = 'email' ORDER BY value LIMIT 1), ''),
			COALESCE(NULLIF(c.name, ''), c.domain, ''),
			a.id, COALESCE(a.status, ''), COALESCE(a.stage, ''), COALESCE(a.next_action, ''), COALESCE(to_char(a.next_due, 'YYYY-MM-DD'), '')
		FROM form_submissions f
		LEFT JOIN people p ON p.id = f.person_id
		LEFT JOIN companies c ON c.id = f.company_id
		LEFT JOIN accounts a ON a.id = f.account_id
		WHERE f.product_id = $1 AND ($2 = '' OR f.monitor_id = $2) AND f.submitted_at > now() - make_interval(days => $3)
		ORDER BY f.submitted_at DESC, f.id DESC LIMIT $4`, a.Product, a.Monitor, a.Days, a.Limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (formSubmission, error) {
		var f formSubmission
		var pid, aid *int64
		var pname, pemail, astatus, astage, anext, adue string
		if err := r.Scan(&f.ID, &f.Monitor, &f.Source, &f.SubmittedAt, &f.ReportedSource, &f.Campaign, &f.SignalID, &f.Fields,
			&pid, &pname, &pemail, &f.Company, &aid, &astatus, &astage, &anext, &adue); err != nil {
			return f, err
		}
		if pid != nil {
			f.Person = &formPerson{ID: *pid, Name: pname, Email: pemail}
		}
		if aid != nil {
			f.Account = &formAccount{ID: *aid, Status: astatus, Stage: astage, NextAction: anext, NextDue: adue}
		}
		return f, nil
	})
	if out == nil {
		out = []formSubmission{}
	}
	return map[string]any{"submissions": out}, err
}

// reportedSources is what a person said, per product, about how they heard
// of it; the person view shows it beside the first-touch source.
func (s *Server) reportedSources(ctx context.Context, personID int64) ([]map[string]any, error) {
	rows, err := s.S.Pool.Query(ctx, `
		SELECT product_id, source, reported_source FROM product_people
		WHERE person_id = $1 AND reported_source <> '' ORDER BY product_id`, personID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (map[string]any, error) {
		var prod, first, said string
		err := r.Scan(&prod, &first, &said)
		return map[string]any{"product": prod, "self_reported": said, "first_touch": first}, err
	})
}

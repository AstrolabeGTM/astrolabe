package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/astrolabe-gtm/astrolabe/internal/outreach"
	"github.com/astrolabe-gtm/astrolabe/internal/people"
	"github.com/astrolabe-gtm/astrolabe/internal/score"
	"github.com/astrolabe-gtm/astrolabe/internal/signal"
)

type feedItem struct {
	ID          int64
	Product     string
	Type        string
	OccurredAt  time.Time
	Title       string
	EvidenceURL string
	Strength    int
	WhyNow      bool
	PersonID    *int64
	PersonName  string
	Company     string
	Tier        string
	Priority    *int
	Summary     string
}

type review struct {
	ID         int64
	SignalID   int64
	Title      string
	Evidence   string
	Candidates []candidate
}

type candidate struct {
	ID    int64
	Label string
}

type monitorStatus struct {
	Product, ID, Type, Every, Error string
	LastRun                         *time.Time
	LastCount                       int
}

func (srv *Server) signalsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	prod := r.FormValue("product")
	typ := r.FormValue("type")
	tier := r.FormValue("tier")
	days, _ := strconv.Atoi(r.FormValue("days"))
	if days <= 0 {
		days = 14
	}
	rows, err := srv.S.Pool.Query(ctx, `
		SELECT s.id, s.product_id, s.type, s.occurred_at, s.title, s.evidence_url, s.strength, s.why_now, s.person_id,
			COALESCE(NULLIF(p.name, ''), (SELECT value FROM identities WHERE person_id = p.id LIMIT 1), ''),
			COALESCE(NULLIF(c.name, ''), c.domain, c.github_org, ''),
			COALESCE(sc.tier, ''), sc.priority, COALESCE(sc.fit, 0), COALESCE(sc.intent, 0), COALESCE(sc.why_now, 0), sc.breakdown
		FROM signals s
		LEFT JOIN people p ON p.id = s.person_id
		LEFT JOIN companies c ON c.id = s.company_id
		LEFT JOIN scores sc ON sc.product_id = s.product_id AND sc.person_id = s.person_id
		WHERE s.occurred_at > now() - make_interval(days => $4)
		  AND ($1 = '' OR s.product_id = $1) AND ($2 = '' OR s.type = $2) AND ($3 = '' OR sc.tier <= $3)
		ORDER BY s.occurred_at DESC LIMIT 300`, prod, typ, tier, days)
	if err != nil {
		fail(w, err)
		return
	}
	feed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (feedItem, error) {
		var f feedItem
		var bd []byte
		var sc score.Score
		err := row.Scan(&f.ID, &f.Product, &f.Type, &f.OccurredAt, &f.Title, &f.EvidenceURL, &f.Strength, &f.WhyNow, &f.PersonID,
			&f.PersonName, &f.Company, &f.Tier, &f.Priority, &sc.Fit, &sc.Intent, &sc.WhyNow, &bd)
		if err == nil && f.Priority != nil {
			sc.Tier, sc.Priority = f.Tier, *f.Priority
			json.Unmarshal(bd, &sc.Breakdown)
			f.Summary = sc.Summary()
		}
		return f, err
	})
	if err != nil {
		fail(w, err)
		return
	}

	rows, err = srv.S.Pool.Query(ctx, `
		SELECT r.id, r.signal_id, s.title, s.evidence_url, r.candidates FROM identity_reviews r JOIN signals s ON s.id = r.signal_id
		WHERE r.status = 'open' ORDER BY r.id LIMIT 50`)
	if err != nil {
		fail(w, err)
		return
	}
	type rawReview struct {
		review
		ids []int64
	}
	raws, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (rawReview, error) {
		var rv rawReview
		return rv, row.Scan(&rv.ID, &rv.SignalID, &rv.Title, &rv.Evidence, &rv.ids)
	})
	if err != nil {
		fail(w, err)
		return
	}
	var reviews []review
	for _, rv := range raws {
		for _, id := range rv.ids {
			p, err := people.Get(ctx, srv.S.Pool, id)
			if err != nil {
				continue
			}
			label := strings.TrimSpace(fmt.Sprintf("%s %s %s", p.Name, p.Email, p.GitHub))
			rv.Candidates = append(rv.Candidates, candidate{id, label})
		}
		reviews = append(reviews, rv.review)
	}

	var monitors []monitorStatus
	for _, p := range srv.Products() {
		for _, m := range p.Monitors {
			ms := monitorStatus{Product: p.ID, ID: m.ID, Type: m.Type, Every: m.Every}
			srv.S.Pool.QueryRow(ctx, `SELECT last_run_at, last_error, last_count FROM monitor_runs WHERE product_id = $1 AND monitor_id = $2`,
				p.ID, m.ID).Scan(&ms.LastRun, &ms.Error, &ms.LastCount)
			monitors = append(monitors, ms)
		}
	}
	var types []string
	trows, err := srv.S.Pool.Query(ctx, `SELECT DISTINCT type FROM signals ORDER BY type`)
	if err == nil {
		types, _ = pgx.CollectRows(trows, pgx.RowTo[string])
	}
	srv.render(w, "signals", srv.page(r, "signals", map[string]any{
		"Feed": feed, "Reviews": reviews, "Monitors": monitors, "Types": types,
		"Product": prod, "Type": typ, "Tier": tier, "Days": days,
	}))
}

func (srv *Server) signalAdd(w http.ResponseWriter, r *http.Request) {
	strength, _ := strconv.Atoi(r.FormValue("strength"))
	who := strings.TrimSpace(r.FormValue("who"))
	sub := signal.Subject{}
	switch {
	case strings.Contains(who, "@"):
		sub.Email = who
	case who != "":
		sub.GitHub = who
	}
	note := strings.TrimSpace(r.FormValue("note"))
	sig := &signal.Signal{Product: r.FormValue("product"), Type: "manual." + strings.TrimPrefix(r.FormValue("type"), "manual."),
		OccurredAt: time.Now(), Strength: strength, WhyNow: r.FormValue("why_now") == "on", Title: note,
		EvidenceURL: r.FormValue("url"), Subject: sub, DedupeKey: fmt.Sprintf("manual:%d", time.Now().UnixNano())}
	_, err := srv.Signals.Ingest(r.Context(), sig)
	if err != nil {
		err = fmt.Errorf("%w: %v", outreach.ErrInvalid, err)
	}
	done(w, r, "/signals?product="+url.QueryEscape(sig.Product), "Signal added", err)
}

func (srv *Server) reviewResolve(w http.ResponseWriter, r *http.Request) {
	person, _ := strconv.ParseInt(r.FormValue("person"), 10, 64)
	err := srv.Signals.Resolve(r.Context(), pathID(r), person)
	if err != nil {
		err = fmt.Errorf("%w: %v", outreach.ErrInvalid, err)
	}
	done(w, r, "/signals", "Signal attached", err)
}

func (srv *Server) reviewDismiss(w http.ResponseWriter, r *http.Request) {
	done(w, r, "/signals", "Dismissed", srv.Signals.Dismiss(r.Context(), pathID(r)))
}

func (srv *Server) monitorRun(w http.ResponseWriter, r *http.Request) {
	_, err := srv.Signals.River.Insert(r.Context(), signal.MonitorArgs{Product: r.FormValue("product"), Monitor: r.FormValue("monitor")}, nil)
	done(w, r, "/signals", "Monitor queued; refresh in a minute", err)
}

func (srv *Server) personFacts(w http.ResponseWriter, r *http.Request) {
	facts := strings.FieldsFunc(r.FormValue("facts"), func(c rune) bool { return c == ',' || c == ';' || c == ' ' })
	err := people.SetFacts(r.Context(), srv.S.Pool, pathID(r), facts, "manual")
	if err == nil {
		_, err = score.RecomputeAll(r.Context(), srv.S.Pool, "", time.Now())
	} else {
		err = fmt.Errorf("%w: %v", outreach.ErrInvalid, err)
	}
	done(w, r, "", "Facts saved and scores updated", err)
}

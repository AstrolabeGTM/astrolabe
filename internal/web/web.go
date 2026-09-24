// Package web is the operator's web app: inbox, people, funnel, settings.
// Server-rendered HTML with plain forms; every POST redirects back.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/content"
	"github.com/AstrolabeGTM/astrolabe/internal/draft"
	"github.com/AstrolabeGTM/astrolabe/internal/experiment"
	"github.com/AstrolabeGTM/astrolabe/internal/funnel"
	"github.com/AstrolabeGTM/astrolabe/internal/gmail"
	"github.com/AstrolabeGTM/astrolabe/internal/hooks"
	"github.com/AstrolabeGTM/astrolabe/internal/mail"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/sandbox"
	"github.com/AstrolabeGTM/astrolabe/internal/score"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
)

//go:embed templates/*.html
var templateFS embed.FS

type Server struct {
	S        *outreach.Service
	Signals  *signal.Service
	Writer   *draft.Writer // nil or disabled without an API key
	Hooks    *hooks.Handler
	Studio   *content.Studio
	Mode     string // sandbox or live
	Mail     *mail.Store
	Accounts *gmail.Accounts
	Password string
	// Products returns the currently loaded products (reloadable).
	Products func() []*product.Product
	// Reload re-reads product folders.
	Reload func(context.Context) error

	tmpl *template.Template
}

func (srv *Server) Handler() http.Handler {
	srv.tmpl = template.Must(template.New("").Funcs(template.FuncMap{
		"ago":     ago,
		"date":    func(t time.Time) string { return t.Local().Format("Mon 2 Jan 15:04") },
		"deref":   func(p *int64) int64 { return *p },
		"inc":     func(i int) int { return i + 1 },
		"list":    func(xs ...string) []string { return xs },
		"money":   func(c int64) string { return fmt.Sprintf("%.2f", float64(c)/100) },
		"mul100":  func(f float64) float64 { return f * 100 },
		"sandbox": func() bool { return srv.Mode == sandbox.Sandbox },
		"derefF": func(p *float64) float64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"deref32": func(p *int) int {
			if p == nil {
				return 0
			}
			return *p
		},
		"replace": strings.ReplaceAll,
		"has":     func(list []string, s string) bool { return slices.Contains(list, s) },
		"pct": func(n, max int) int {
			if max == 0 {
				return 0
			}
			return 100 * n / max
		},
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
	}).ParseFS(templateFS, "templates/*.html"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", srv.loginPage)
	mux.HandleFunc("POST /login", srv.login)
	mux.HandleFunc("POST /logout", srv.logout)
	// Webhooks authenticate by signature, not session.
	if srv.Hooks != nil {
		mux.HandleFunc("POST /hooks/stripe/{product}", srv.Hooks.Stripe)
		mux.HandleFunc("POST /hooks/events/{product}", srv.Hooks.ProductEvents)
		mux.HandleFunc("GET /hooks/whatsapp", srv.Hooks.WhatsAppVerify)
		mux.Handle("GET /l/{code}", &hooks.Redirector{Signals: srv.Signals, Out: srv.S})
		mux.HandleFunc("POST /hooks/whatsapp", srv.Hooks.WhatsApp)
	}

	app := http.NewServeMux()
	app.HandleFunc("GET /{$}", srv.inbox)
	app.HandleFunc("POST /actions/{id}/save", srv.actionSave)
	app.HandleFunc("POST /actions/{id}/approve", srv.actionApprove)
	app.HandleFunc("POST /actions/{id}/skip", srv.actionSkip)
	app.HandleFunc("POST /actions/{id}/resolve", srv.actionResolve)
	app.HandleFunc("POST /actions/{id}/check", srv.actionCheck)
	app.HandleFunc("POST /replies/{id}/classify", srv.replyClassify)
	app.HandleFunc("POST /tasks", srv.taskCreate)
	app.HandleFunc("POST /tasks/{id}/close", srv.taskClose)
	app.HandleFunc("GET /people", srv.peoplePage)
	app.HandleFunc("POST /people/import", srv.peopleImport)
	app.HandleFunc("POST /people/start", srv.peopleEnroll)
	app.HandleFunc("POST /people/{id}/do-not-contact", srv.personSuppress)
	app.HandleFunc("POST /people/{id}/allow-contact", srv.personUnsuppress)
	app.HandleFunc("GET /signals", srv.signalsPage)
	app.HandleFunc("POST /signals", srv.signalAdd)
	app.HandleFunc("POST /signals/reviews/{id}/resolve", srv.reviewResolve)
	app.HandleFunc("POST /signals/reviews/{id}/dismiss", srv.reviewDismiss)
	app.HandleFunc("POST /monitors/run", srv.monitorRun)
	app.HandleFunc("POST /people/{id}/facts", srv.personFacts)
	app.HandleFunc("GET /people/{id}", srv.personPage)
	app.HandleFunc("POST /people/{id}/research", srv.personBrief)
	app.HandleFunc("GET /automations", srv.playsPage)
	app.HandleFunc("GET /content", srv.contentPage)
	app.HandleFunc("POST /content", srv.contentCreate)
	app.HandleFunc("POST /content/{id}/mark", srv.contentMark)
	app.HandleFunc("GET /digest", srv.digestPage)
	app.HandleFunc("GET /outbox", srv.outboxPage)
	app.HandleFunc("POST /outbox/{id}/reply", srv.outboxReply)
	app.HandleFunc("POST /outbox/{id}/bounce", srv.outboxBounce)
	app.HandleFunc("POST /replies/{id}/answer", srv.replyAnswer)
	app.HandleFunc("POST /actions/{id}/ai", srv.actionAIDraft)
	app.HandleFunc("GET /funnel", srv.funnelPage)
	app.HandleFunc("POST /funnel/stage", srv.stageMark)
	app.HandleFunc("GET /settings", srv.settingsPage)
	app.HandleFunc("POST /pauses", srv.pauseToggle)
	app.HandleFunc("POST /products/reload", srv.reload)
	app.HandleFunc("POST /mailboxes", srv.mailboxAdd)
	app.HandleFunc("POST /mailboxes/remove", srv.mailboxRemove)
	mux.Handle("/", srv.requireLogin(sameOrigin(app)))
	return securityHeaders(mux)
}

// ---- auth ----

const cookieName = "astrolabe_session"

func (srv *Server) sessionKey() []byte {
	k := sha256.Sum256([]byte("astrolabe-session\x00" + srv.Password))
	return k[:]
}

func (srv *Server) sign(exp int64) string {
	m := hmac.New(sha256.New, srv.sessionKey())
	fmt.Fprint(m, exp)
	return fmt.Sprintf("%d.%s", exp, hex.EncodeToString(m.Sum(nil)))
}

func (srv *Server) validSession(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	expStr, _, ok := strings.Cut(c.Value, ".")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if !ok || err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(srv.sign(exp)))
}

func (srv *Server) requireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !srv.validSession(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin rejects cross-site form posts (with SameSite=Strict cookies this
// is belt and braces).
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (srv *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	srv.render(w, "login", map[string]any{"Failed": r.URL.Query().Has("failed")})
}

func (srv *Server) login(w http.ResponseWriter, r *http.Request) {
	time.Sleep(300 * time.Millisecond) // slows guessing
	if subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(srv.Password)) != 1 {
		http.Redirect(w, r, "/login?failed", http.StatusSeeOther)
		return
	}
	exp := time.Now().Add(30 * 24 * time.Hour).Unix()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: srv.sign(exp), Path: "/", HttpOnly: true,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", SameSite: http.SameSiteStrictMode, MaxAge: 30 * 24 * 3600})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (srv *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- helpers ----

func (srv *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := srv.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render", "template", name, "err", err)
	}
}

func (srv *Server) page(r *http.Request, active string, data map[string]any) map[string]any {
	data["Active"] = active
	data["Flash"] = r.URL.Query().Get("msg")
	data["Error"] = r.URL.Query().Get("err")
	data["Products"] = srv.Products()
	return data
}

// done redirects back with a message, or with the error if there was one.
func done(w http.ResponseWriter, r *http.Request, to string, msg string, err error) {
	if to == "" {
		to = r.FormValue("back")
	}
	if to == "" || !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") {
		to = "/"
	}
	u, _ := url.Parse(to)
	q := u.Query()
	q.Del("msg")
	q.Del("err")
	if err != nil {
		if !errors.Is(err, outreach.ErrInvalid) {
			slog.Error("request failed", "path", r.URL.Path, "err", err)
		}
		q.Set("err", err.Error())
	} else if msg != "" {
		q.Set("msg", msg)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func fail(w http.ResponseWriter, err error) {
	slog.Error("page failed", "err", err)
	http.Error(w, "Something went wrong: "+err.Error(), http.StatusInternalServerError)
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 0:
		return "in " + dur(-d)
	case d < time.Minute:
		return "just now"
	default:
		return dur(d) + " ago"
	}
}

func dur(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// ---- inbox ----

func (srv *Server) inbox(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	prod := r.URL.Query().Get("product")
	replies, err := srv.S.Replies(ctx, true, 50)
	if err != nil {
		fail(w, err)
		return
	}
	tasks, err := srv.S.Tasks(ctx, true, 100)
	if err != nil {
		fail(w, err)
		return
	}
	attention, err := srv.S.Actions(ctx, prod, "unknown", "failed", "blocked")
	if err != nil {
		fail(w, err)
		return
	}
	drafts, err := srv.S.Actions(ctx, prod, "draft")
	if err != nil {
		fail(w, err)
		return
	}
	queued, err := srv.S.Actions(ctx, prod, "approved", "sending")
	if err != nil {
		fail(w, err)
		return
	}
	pauses, _ := srv.S.Pauses(ctx)
	srv.render(w, "inbox", srv.page(r, "inbox", map[string]any{
		"Replies": replies, "Tasks": tasks, "Attention": attention, "Drafts": drafts, "Queued": queued,
		"Pauses": pauses, "Classes": outreach.Classifications, "Outcomes": outreach.Outcomes, "Product": prod,
		"Now": time.Now(),
	}))
}

func (srv *Server) actionSave(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	err := srv.S.Edit(r.Context(), id, strings.TrimSpace(r.FormValue("subject")), strings.TrimSpace(strings.ReplaceAll(r.FormValue("body"), "\r\n", "\n")))
	if err == nil && r.FormValue("then") == "approve" {
		err = srv.S.Approve(r.Context(), id)
		done(w, r, "", fmt.Sprintf("Approved #%d", id), err)
		return
	}
	done(w, r, "", fmt.Sprintf("Saved #%d", id), err)
}

func (srv *Server) actionApprove(w http.ResponseWriter, r *http.Request) {
	done(w, r, "", fmt.Sprintf("Approved #%d", pathID(r)), srv.S.Approve(r.Context(), pathID(r)))
}

func (srv *Server) actionSkip(w http.ResponseWriter, r *http.Request) {
	done(w, r, "", fmt.Sprintf("Skipped #%d", pathID(r)), srv.S.Skip(r.Context(), pathID(r)))
}

func (srv *Server) actionResolve(w http.ResponseWriter, r *http.Request) {
	sent := r.FormValue("sent") == "yes"
	msg := "Recorded as sent"
	if !sent {
		msg = "Returned to drafts; approve again to send"
	}
	done(w, r, "", msg, srv.S.Resolve(r.Context(), pathID(r), sent, nil))
}

// actionCheck looks for an uncertain send in the mailbox's Sent folder.
func (srv *Server) actionCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := srv.S.Action(ctx, pathID(r))
	if err != nil {
		done(w, r, "", "", err)
		return
	}
	if a.MessageID == "" {
		done(w, r, "", "", fmt.Errorf("%w: no Message-ID recorded; check the mailbox by hand", outreach.ErrInvalid))
		return
	}
	if acct, _ := srv.mailStore().Get(a.Sender); acct != nil {
		found, err := mail.FindSent(acct, a.MessageID)
		if err != nil {
			done(w, r, "", "", err)
			return
		}
		if found {
			done(w, r, "", fmt.Sprintf("Found #%d in %s; recorded as sent", a.ID, acct.SentFolder), srv.S.Resolve(ctx, a.ID, true, &channel.Result{ProviderID: a.MessageID, ThreadID: a.MessageID}))
			return
		}
		note := ""
		if !acct.SavesSent {
			note = " This provider doesn't file SMTP sends in Sent by itself, so a missing copy isn't proof; check with the recipient if it matters."
		}
		done(w, r, "", fmt.Sprintf("#%d is not in %s.%s", a.ID, acct.SentFolder, note), nil)
		return
	}
	mb, err := srv.Accounts.Mailbox(a.Sender)
	if err != nil {
		done(w, r, "", "", err)
		return
	}
	found, err := mb.FindSent(ctx, a.MessageID)
	if err != nil {
		done(w, r, "", "", err)
		return
	}
	if found.ProviderID != "" {
		done(w, r, "", fmt.Sprintf("Found #%d in Sent mail; recorded as sent", a.ID), srv.S.Resolve(ctx, a.ID, true, &found))
		return
	}
	// Not found by Message-ID. Gmail may not keep the id we set, so this is
	// not proof it wasn't sent: show what was sent to them around then.
	since := a.CreatedAt.Add(-time.Hour)
	subjects, err := mb.SentTo(ctx, a.Recipient, since)
	if err != nil {
		done(w, r, "", "", err)
		return
	}
	msg := fmt.Sprintf("#%d was not found by Message-ID. Nothing else was sent to %s since %s either, so it most likely did not go out; check by hand before returning it to drafts.",
		a.ID, a.Recipient, since.Local().Format("2 Jan 15:04"))
	if len(subjects) > 0 {
		msg = fmt.Sprintf("#%d was not found by Message-ID, but Sent mail has %d message(s) to %s since %s: %q. If one is this message, choose \"It was sent\".",
			a.ID, len(subjects), a.Recipient, since.Local().Format("2 Jan 15:04"), subjects)
	}
	done(w, r, "", msg, nil)
}

func (srv *Server) replyClassify(w http.ResponseWriter, r *http.Request) {
	var remind *time.Time
	if v := r.FormValue("remind"); v != "" {
		if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
			remind = &t
		}
	}
	class := r.FormValue("class")
	done(w, r, "", "Marked "+strings.ReplaceAll(class, "_", " "), srv.S.Classify(r.Context(), pathID(r), class, remind))
}

func (srv *Server) taskCreate(w http.ResponseWriter, r *http.Request) {
	var person *int64
	if id, err := strconv.ParseInt(r.FormValue("person_id"), 10, 64); err == nil {
		person = &id
	}
	due := time.Now()
	if t, err := time.ParseInLocation("2006-01-02", r.FormValue("due"), time.Local); err == nil {
		due = t.Add(9 * time.Hour)
	}
	_, err := srv.S.CreateTask(r.Context(), r.FormValue("product"), person, r.FormValue("next_action"), due)
	done(w, r, "", "Task added", err)
}

func (srv *Server) taskClose(w http.ResponseWriter, r *http.Request) {
	var minutes *int
	if m, err := strconv.Atoi(r.FormValue("minutes")); err == nil {
		minutes = &m
	}
	done(w, r, "", "Task closed", srv.S.CloseTask(r.Context(), pathID(r), r.FormValue("outcome"), r.FormValue("note"), minutes))
}

// ---- people ----

func (srv *Server) currentProduct(r *http.Request) *product.Product {
	ps := srv.Products()
	id := r.FormValue("product")
	for _, p := range ps {
		if p.ID == id {
			return p
		}
	}
	if len(ps) > 0 {
		return ps[0]
	}
	return nil
}

func (srv *Server) peoplePage(w http.ResponseWriter, r *http.Request) {
	p := srv.currentProduct(r)
	data := srv.page(r, "people", map[string]any{"Product": p, "Q": r.FormValue("q")})
	if p != nil {
		list, err := people.List(r.Context(), srv.S.Pool, p.ID, r.FormValue("q"), 500)
		if err != nil {
			fail(w, err)
			return
		}
		enrolled, err := enrollmentsByPerson(r.Context(), srv.S, p.ID)
		if err != nil {
			fail(w, err)
			return
		}
		ranked, err := score.List(r.Context(), srv.S.Pool, p.ID, "D", false, 5000)
		if err != nil {
			fail(w, err)
			return
		}
		scores := map[int64]*score.Ranked{}
		for i := range ranked {
			scores[ranked[i].PersonID] = &ranked[i]
		}
		prio := func(id int64) int {
			if s, ok := scores[id]; ok {
				return s.Priority
			}
			return -1
		}
		slices.SortStableFunc(list, func(a, b *people.Person) int { return prio(b.ID) - prio(a.ID) })
		data["Scores"] = scores
		var seqs []string
		for id := range p.Sequences {
			seqs = append(seqs, id)
		}
		slices.Sort(seqs)
		data["People"], data["Enrolled"], data["Sequences"] = list, enrolled, seqs
	}
	srv.render(w, "people", data)
}

func enrollmentsByPerson(ctx context.Context, s *outreach.Service, productID string) (map[int64]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT person_id, string_agg(sequence_id || ' (' ||
			CASE WHEN status = 'active' THEN 'step ' || (next_step + 1)
			ELSE status || CASE WHEN stop_reason <> '' THEN ': ' || stop_reason ELSE '' END END || ')', ', ')
		FROM enrollments WHERE product_id = $1 GROUP BY person_id`, productID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var s string
		if err := rows.Scan(&id, &s); err != nil {
			return nil, err
		}
		out[id] = s
	}
	return out, rows.Err()
}

func (srv *Server) peopleImport(w http.ResponseWriter, r *http.Request) {
	back := "/people?product=" + url.QueryEscape(r.FormValue("product"))
	var rows []people.Row
	var err error
	if f, _, ferr := r.FormFile("file"); ferr == nil {
		defer f.Close()
		rows, err = people.ReadCSV(f)
	} else {
		rows, err = people.ReadCSV(strings.NewReader(r.FormValue("csv")))
	}
	if err != nil {
		done(w, r, back, "", fmt.Errorf("%w: %v", outreach.ErrInvalid, err))
		return
	}
	res, err := people.Import(r.Context(), srv.S.Pool, r.FormValue("product"), rows)
	if err == nil {
		_, err = score.RecomputeAll(r.Context(), srv.S.Pool, r.FormValue("product"), time.Now())
	}
	msg := fmt.Sprintf("Imported: %d new, %d updated", res.Created, res.Updated)
	if len(res.Skipped) > 0 {
		msg += fmt.Sprintf("; skipped %d: %s", len(res.Skipped), strings.Join(res.Skipped, "; "))
	}
	done(w, r, back, msg, err)
}

func (srv *Server) peopleEnroll(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	back := "/people?product=" + url.QueryEscape(r.FormValue("product"))
	var ids []int64
	for _, v := range r.Form["person"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		done(w, r, back, "", fmt.Errorf("%w: select people first", outreach.ErrInvalid))
		return
	}
	res, err := srv.S.Enroll(r.Context(), r.FormValue("product"), r.FormValue("sequence"), ids)
	if err == nil {
		_, err = srv.S.Tick(r.Context()) // draft first steps now rather than within the minute
	}
	msg := fmt.Sprintf("Started the sequence for %d; first drafts are in the inbox", len(res.Enrolled))
	if len(res.Skipped) > 0 {
		msg += "; skipped: " + strings.Join(res.Skipped, "; ")
	}
	done(w, r, back, msg, err)
}

func (srv *Server) personSuppress(w http.ResponseWriter, r *http.Request) {
	done(w, r, "", "Added to do-not-contact; no product will message them", people.Suppress(r.Context(), srv.S.Pool, pathID(r), "manual", r.FormValue("detail")))
}

func (srv *Server) personUnsuppress(w http.ResponseWriter, r *http.Request) {
	done(w, r, "", "Removed from do-not-contact", people.Unsuppress(r.Context(), srv.S.Pool, pathID(r)))
}

// ---- funnel ----

func (srv *Server) funnelPage(w http.ResponseWriter, r *http.Request) {
	p := srv.currentProduct(r)
	days, _ := strconv.Atoi(r.FormValue("days"))
	if days <= 0 {
		days = 90
	}
	data := srv.page(r, "funnel", map[string]any{"Product": p, "Days": days})
	if p != nil {
		rep, err := funnel.BuildReport(r.Context(), srv.S.Pool, p, time.Now().AddDate(0, 0, -days), time.Now())
		if err != nil {
			fail(w, err)
			return
		}
		max := 1
		for _, s := range rep.Stages {
			if s.People > max {
				max = s.People
			}
		}
		exps, err := experiment.Report(r.Context(), srv.S.Pool, p)
		if err != nil {
			fail(w, err)
			return
		}
		data["R"], data["Max"], data["Experiments"] = rep, max, exps
	}
	srv.render(w, "funnel", data)
}

func (srv *Server) stageMark(w http.ResponseWriter, r *http.Request) {
	prod := r.FormValue("product")
	back := r.FormValue("back")
	if back == "" {
		back = "/funnel?product=" + url.QueryEscape(prod)
	}
	who := strings.TrimSpace(r.FormValue("person"))
	var personID int64
	if id, err := strconv.ParseInt(who, 10, 64); err == nil {
		personID = id
	} else if p, err := people.FindByEmail(r.Context(), srv.S.Pool, who); err == nil {
		personID = p.ID
	} else {
		done(w, r, back, "", fmt.Errorf("%w: no person with id or email %q", outreach.ErrInvalid, who))
		return
	}
	stage := r.FormValue("stage")
	done(w, r, back, fmt.Sprintf("Recorded %s for person %d", stage, personID), srv.S.MarkStage(r.Context(), prod, personID, stage, time.Now()))
}

// ---- settings ----

func (srv *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	pauses, err := srv.S.Pauses(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	type box struct {
		Address, Product, Status string
	}
	var boxes []box
	authorised := srv.Accounts.Addresses()
	seen := map[string]bool{}
	for _, p := range srv.Products() {
		for _, addr := range []string{p.Sender.Cold, p.Sender.Users} {
			addr = strings.ToLower(addr)
			if addr == "" || seen[addr] {
				continue
			}
			seen[addr] = true
			st := "not set up"
			if acct, _ := srv.mailStore().Get(addr); acct != nil {
				st = "ready (" + acct.Provider + ", SMTP/IMAP)"
			} else if slices.Contains(authorised, addr) {
				st = "ready (Gmail OAuth)"
			}
			if srv.Mode == sandbox.Sandbox {
				st += " · sandbox: not used"
			}
			boxes = append(boxes, box{addr, p.ID, st})
		}
	}
	type hook struct {
		Product, Kind, Path, Env string
		Set                      bool
	}
	var hookList []hook
	for _, p := range srv.Products() {
		env := func(kind string) string {
			return "ASTROLABE_" + strings.ToUpper(kind) + "_SECRET_" + strings.ToUpper(strings.ReplaceAll(p.ID, "-", "_"))
		}
		hookList = append(hookList,
			hook{p.ID, "product events", "/hooks/events/" + p.ID, env("events"), hooks.EnvSecret("events", p.ID) != ""},
			hook{p.ID, "Stripe", "/hooks/stripe/" + p.ID, env("stripe"), hooks.EnvSecret("stripe", p.ID) != ""})
	}
	var presets []map[string]string
	for _, name := range mail.PresetNames() {
		presets = append(presets, map[string]string{"ID": name, "Name": mail.Presets[name].Name, "Help": mail.Presets[name].Help})
	}
	srv.render(w, "settings", srv.page(r, "settings", map[string]any{"Pauses": pauses, "Mailboxes": boxes, "Hooks": hookList, "Presets": presets}))
}

func (srv *Server) pauseToggle(w http.ResponseWriter, r *http.Request) {
	scope := r.FormValue("scope")
	if r.FormValue("op") == "resume" {
		done(w, r, "", "Resumed "+scope, srv.S.Resume(r.Context(), scope))
		return
	}
	done(w, r, "", "Paused "+scope, srv.S.Pause(r.Context(), scope))
}

func (srv *Server) reload(w http.ResponseWriter, r *http.Request) {
	err := srv.Reload(r.Context())
	if err != nil {
		err = fmt.Errorf("%w: %v", outreach.ErrInvalid, err)
	}
	done(w, r, "/settings", "Products reloaded", err)
}

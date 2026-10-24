package web

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/sentmail"
	"github.com/AstrolabeGTM/astrolabe/internal/views"
)

func (srv *Server) todayRoutes() []apiRoute {
	return []apiRoute{
		{"GET /api/today", "today", srv.apiToday},
		{"POST /api/actions/{id}/approve", "approve", srv.apiApprove},
		{"POST /api/actions/{id}/edit", "write_draft", srv.apiEdit},
		{"POST /api/actions/{id}/skip", "skip", srv.apiSkip},
		{"POST /api/actions/{id}/resolve", "resolve_send", srv.apiResolve},
		{"POST /api/actions/{id}/check-sent", "check_sent", srv.apiCheckSent},
		{"POST /api/actions/{id}/ai-draft", "ai_draft", srv.apiAIDraft},
		{"POST /api/replies/{id}/label", "label_reply", srv.apiLabel},
		{"POST /api/replies/{id}/answer", "answer_reply", srv.apiAnswer},
		{"POST /api/tasks", "add_task", srv.apiAddTask},
		{"POST /api/tasks/{id}/close", "close_task", srv.apiCloseTask},
	}
}

func (srv *Server) product(id string) (*product.Product, error) {
	for _, p := range srv.Products() {
		if p.ID == id {
			return p, nil
		}
	}
	if id == "" {
		return nil, badRequest("Choose a product.")
	}
	return nil, badRequest("No loaded product %q.", id)
}

func status(msg string) map[string]string { return map[string]string{"status": msg} }

func (srv *Server) apiToday(w http.ResponseWriter, r *http.Request) {
	t, err := views.GetToday(r.Context(), srv.S, r.URL.Query().Get("product"), time.Now())
	respond(w, t, err)
}

func (srv *Server) apiApprove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ContentHash string `json:"content_hash"`
	}
	id, ok := idAndJSON(w, r, &req)
	if !ok {
		return
	}
	msg := fmt.Sprintf("Approved #%d.", id)
	if srv.Mode == "sandbox" {
		msg += " It goes to the sandbox outbox, not to a real person."
	}
	respond(w, status(msg), srv.S.ApproveSeen(r.Context(), id, req.ContentHash))
}

func (srv *Server) apiEdit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
		Approve bool   `json:"approve"`
	}
	id, ok := idAndJSON(w, r, &req)
	if !ok {
		return
	}
	ctx := r.Context()
	cur, err := srv.S.Action(ctx, id)
	if err != nil {
		apiFail(w, err)
		return
	}
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		subject = cur.Subject
	}
	body := strings.TrimSpace(strings.ReplaceAll(req.Body, "\r\n", "\n"))
	if err := srv.S.Edit(ctx, id, subject, body); err != nil {
		apiFail(w, err)
		return
	}
	if !req.Approve {
		respond(w, status(fmt.Sprintf("Saved #%d. It needs approval before it is sent.", id)), nil)
		return
	}
	// Approve exactly the text just saved.
	err = srv.S.ApproveSeen(ctx, id, outreach.ContentHash(cur.Channel, cur.Sender, cur.Recipient, subject, body))
	respond(w, status(fmt.Sprintf("Saved and approved #%d.", id)), err)
}

func (srv *Server) apiSkip(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err == nil {
		err = srv.S.Skip(r.Context(), id)
	}
	respond(w, status(fmt.Sprintf("Skipped #%d; the sequence moves on to its next step.", id)), err)
}

func (srv *Server) apiResolve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Sent bool `json:"sent"`
	}
	id, ok := idAndJSON(w, r, &req)
	if !ok {
		return
	}
	msg := fmt.Sprintf("Recorded #%d as sent.", id)
	if !req.Sent {
		msg = fmt.Sprintf("#%d is back in drafts; approve it again to send.", id)
	}
	respond(w, status(msg), srv.S.Resolve(r.Context(), id, req.Sent, nil))
}

func (srv *Server) apiCheckSent(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		apiFail(w, err)
		return
	}
	res, err := sentmail.Check(r.Context(), srv.S, srv.mailStore(), srv.Accounts, id)
	respond(w, res, err)
}

func (srv *Server) apiAIDraft(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err == nil && srv.Writer == nil {
		err = badRequest("Set ASTROLABE_ANTHROPIC_API_KEY for AI drafts, or write the text yourself.")
	}
	if err == nil {
		err = srv.Writer.Redraft(r.Context(), id)
	}
	respond(w, status(fmt.Sprintf("Claude rewrote #%d. Read it before approving.", id)), err)
}

func (srv *Server) apiLabel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Class    string `json:"class"`
		RemindOn string `json:"remind_on"`
	}
	id, ok := idAndJSON(w, r, &req)
	if !ok {
		return
	}
	var remind *time.Time
	if req.RemindOn != "" {
		t, err := time.ParseInLocation("2006-01-02", req.RemindOn, time.Local)
		if err != nil {
			apiFail(w, badRequest("The reminder date must look like 2026-11-30."))
			return
		}
		t = t.Add(9 * time.Hour)
		remind = &t
	}
	msg := map[string]string{
		"interested": "Labelled interested; a task to reply is due now.", "question": "Labelled a question; a task to answer it is due now.",
		"not_now": "Labelled not now; a reminder task is set.", "no": "Labelled no; they are on the do-not-contact list.",
		"unsubscribe": "Unsubscribed; they are on the do-not-contact list.", "out_of_office": "Labelled out of office.",
	}[req.Class]
	respond(w, status(msg), srv.S.Classify(r.Context(), id, req.Class, remind))
}

func (srv *Server) apiAnswer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body string `json:"body"`
	}
	id, ok := idAndJSON(w, r, &req)
	if !ok {
		return
	}
	body := strings.TrimSpace(strings.ReplaceAll(req.Body, "\r\n", "\n"))
	if body == "" {
		apiFail(w, badRequest("Write the answer first."))
		return
	}
	aid, err := srv.S.AnswerReply(r.Context(), id, body)
	respond(w, map[string]any{"status": fmt.Sprintf("Your answer is draft #%d; approve it to send.", aid), "action_id": aid}, err)
}

func (srv *Server) apiAddTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product    string `json:"product"`
		PersonID   int64  `json:"person_id"`
		NextAction string `json:"next_action"`
		Due        string `json:"due"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if _, err := srv.product(req.Product); err != nil {
		apiFail(w, err)
		return
	}
	due := time.Now()
	if req.Due != "" {
		t, err := time.ParseInLocation("2006-01-02", req.Due, time.Local)
		if err != nil {
			apiFail(w, badRequest("The due date must look like 2026-11-30."))
			return
		}
		due = t.Add(9 * time.Hour)
	}
	var person *int64
	if req.PersonID > 0 {
		person = &req.PersonID
	}
	id, err := srv.S.CreateTask(r.Context(), req.Product, person, req.NextAction, due)
	respond(w, map[string]any{"status": "Task added.", "task_id": id}, err)
}

func (srv *Server) apiCloseTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Outcome string `json:"outcome"`
		Note    string `json:"note"`
		Minutes *int   `json:"minutes"`
	}
	id, ok := idAndJSON(w, r, &req)
	if !ok {
		return
	}
	if !slices.Contains(outreach.Outcomes, req.Outcome) {
		apiFail(w, badRequest("Pick an outcome."))
		return
	}
	respond(w, status("Task closed: "+strings.ReplaceAll(req.Outcome, "_", " ")+"."), srv.S.CloseTask(r.Context(), id, req.Outcome, strings.TrimSpace(req.Note), req.Minutes))
}

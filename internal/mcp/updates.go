package mcp

import (
	"context"
	"errors"
	"maps"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AstrolabeGTM/astrolabe/internal/updates"
)

// updatesInstructions extends the server instructions for the updates list.
const updatesInstructions = `

Updates list (a product with updates: in product.yaml): an opt-in email list. Sales term -> Astrolabe term: newsletter/mailing list/nurture list/drip list -> updates list; subscribe/opt in -> updates_add (the person gets a confirmation email and is NOT a subscriber until they confirm: double opt-in); blast/campaign send/issue -> broadcast; unsubscribe -> updates_unsubscribe (list only; it is not do_not_contact). A broadcast is drafted (from content posted in a period, or written by hand), the operator reads the exact subject and body, and only then broadcast_approve; any edit withdraws the approval. If broadcast_approve is refused, tell the operator why (a flagged claim, a missing postal address) rather than retrying. Sending goes only to confirmed subscribers, once each, within per_day.`

var updatesTerms = map[string]string{
	"updates_subscribers": "newsletter subscribers, mailing list size, list health, opt-ins",
	"updates_add":         "add a subscriber, opt-in, double opt-in sign-up, nurture list enrolment",
	"updates_unsubscribe": "remove from the newsletter, opt out of the list (not DNC)",
	"updates_export":      "export the mailing list, subscriber CSV",
	"broadcast_create":    "draft a newsletter, write an issue, email blast, content roundup",
	"broadcast_edit":      "edit the newsletter copy",
	"broadcast_approve":   "send the newsletter, approve the email blast",
	"broadcast_cancel":    "stop a send, pull a campaign",
	"broadcast_status":    "newsletter delivery status, campaign send report",
}

func init() { maps.Copy(salesTerms, updatesTerms) }

func (s *Server) registerUpdates(srv *sdk.Server) {
	register(srv, "updates_subscribers", "The updates list (opt-in email list) of a product with counts: pending (asked, not yet confirmed), confirmed, unsubscribed, expired and bounced, plus subscribers (filter by status). Only confirmed people receive broadcasts.", s.updatesList)
	register(srv, "updates_add", "Ask someone to join a product's updates list, by hand (for example they asked in person). Finds or creates the person by email and sends a confirmation email; they are NOT subscribed until they reply to it (or open its link when a public URL is set), and an unconfirmed request expires. Refused for people on do-not-contact.", s.updatesAdd)
	register(srv, "updates_unsubscribe", "Take someone off the updates list (by email) at once; nothing queued for them goes out. This is the list only: it does not put them on do-not-contact.", s.updatesUnsubscribe)
	register(srv, "broadcast_create", "Draft a broadcast (one email to the whole updates list). Give subject and text, or a period (this-month, last-month, YYYY-MM or 30d) to build it from the content posted in that time (content_mark). Nothing is sent: review the returned text and flags, edit it, then broadcast_approve.", s.broadcastCreate)
	register(srv, "broadcast_edit", "Change a draft broadcast's subject or text. Editing an approved broadcast withdraws its approval; one that has started sending cannot be edited.", s.broadcastEdit)
	register(srv, "broadcast_approve", "Approve a broadcast exactly as saved and queue it for everyone confirmed now. Refused when it has flags (a never-claim, a placeholder) or the product has no updates.postal_address. Show the operator the exact subject and text first. Sending then runs in batches within updates.per_day, once per subscriber.", s.broadcastApprove)
	register(srv, "broadcast_cancel", "Stop a broadcast: whatever is still queued is not sent. What went out stays recorded.", s.broadcastCancel)
	register(srv, "broadcast_status", "Broadcasts of a product, or one by id: state (draft, approved, sending, sent, cancelled), how many were sent, are still queued, unknown (the provider's answer was lost; never resent, check the mailbox), failed or skipped (unsubscribed since), and what is holding it (daily cap, pause, missing postal address).", s.broadcastStatus)
	register(srv, "updates_export", "Confirmed subscribers of a product as CSV (email, name, confirmed_at, confirmed_via, source).", s.updatesExport)
}

func (s *Server) lists() (*updates.Service, error) {
	if s.Updates == nil {
		return nil, errors.New("the updates list is not configured")
	}
	return s.Updates, nil
}

type updatesListArg struct {
	Product string `json:"product"`
	Status  string `json:"status,omitempty" jsonschema:"pending, confirmed, unsubscribed, expired or bounced; omit for the counts and the latest requests"`
	Limit   int    `json:"limit,omitempty"`
}

func (s *Server) updatesList(ctx context.Context, a updatesListArg) (any, error) {
	u, err := s.lists()
	if err != nil {
		return nil, err
	}
	if _, err := s.find(a.Product); err != nil {
		return nil, err
	}
	c, err := u.Counts(ctx, a.Product)
	if err != nil {
		return nil, err
	}
	list, err := u.List(ctx, a.Product, a.Status, a.Limit)
	return map[string]any{"counts": c, "subscribers": list}, err
}

type updatesAddArg struct {
	Product string `json:"product"`
	Email   string `json:"email"`
	Name    string `json:"name,omitempty"`
}

func (s *Server) updatesAdd(ctx context.Context, a updatesAddArg) (any, error) {
	u, err := s.lists()
	if err != nil {
		return nil, err
	}
	res, err := u.AddByEmail(ctx, a.Product, a.Email, a.Name, "manual")
	if err != nil {
		return nil, err
	}
	note := "A confirmation email is queued. They are not subscribed until they confirm."
	if res.State != "pending" {
		note = res.State
	}
	return map[string]any{"subscriber": res.Subscriber, "state": res.State, "note": note}, nil
}

type updatesUnsubArg struct {
	Product string `json:"product"`
	Email   string `json:"email"`
}

func (s *Server) updatesUnsubscribe(ctx context.Context, a updatesUnsubArg) (any, error) {
	u, err := s.lists()
	if err != nil {
		return nil, err
	}
	changed, err := u.UnsubscribeByEmail(ctx, a.Product, a.Email)
	if err != nil {
		return nil, err
	}
	return map[string]any{"unsubscribed": changed}, nil
}

type broadcastCreateArg struct {
	Product string `json:"product"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty" jsonschema:"the text; leave empty to build it from content posted in the period"`
	Period  string `json:"period,omitempty" jsonschema:"this-month, last-month, YYYY-MM or a window like 30d"`
}

func (s *Server) broadcastCreate(ctx context.Context, a broadcastCreateArg) (any, error) {
	u, err := s.lists()
	if err != nil {
		return nil, err
	}
	b, err := u.CreateBroadcast(ctx, updates.CreateOpts{Product: a.Product, Subject: a.Subject, Body: a.Body, Period: a.Period})
	if err != nil {
		return nil, err
	}
	return map[string]any{"broadcast": b, "note": "A draft. Nothing is sent until the operator reads it and you call broadcast_approve."}, nil
}

type broadcastEditArg struct {
	ID      int64   `json:"id"`
	Subject *string `json:"subject,omitempty"`
	Body    *string `json:"body,omitempty"`
}

func (s *Server) broadcastEdit(ctx context.Context, a broadcastEditArg) (any, error) {
	u, err := s.lists()
	if err != nil {
		return nil, err
	}
	b, err := u.EditBroadcast(ctx, a.ID, a.Subject, a.Body)
	return map[string]any{"broadcast": b}, err
}

type broadcastIDArg struct {
	ID int64 `json:"id"`
}

func (s *Server) broadcastApprove(ctx context.Context, a broadcastIDArg) (any, error) {
	u, err := s.lists()
	if err != nil {
		return nil, err
	}
	b, err := u.ApproveBroadcast(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"broadcast": b, "queued_for": b.Queued}, nil
}

func (s *Server) broadcastCancel(ctx context.Context, a broadcastIDArg) (any, error) {
	u, err := s.lists()
	if err != nil {
		return nil, err
	}
	b, err := u.CancelBroadcast(ctx, a.ID)
	return map[string]any{"broadcast": b}, err
}

type broadcastStatusArg struct {
	Product string `json:"product,omitempty"`
	ID      int64  `json:"id,omitempty" jsonschema:"one broadcast; omit to list the product's latest"`
}

func (s *Server) broadcastStatus(ctx context.Context, a broadcastStatusArg) (any, error) {
	u, err := s.lists()
	if err != nil {
		return nil, err
	}
	if a.ID != 0 {
		b, err := u.Broadcast(ctx, a.ID)
		return map[string]any{"broadcast": b}, err
	}
	if strings.TrimSpace(a.Product) == "" {
		return nil, errors.New("give a product, or the id of a broadcast")
	}
	list, err := u.Broadcasts(ctx, a.Product, 20)
	return map[string]any{"broadcasts": list}, err
}

func (s *Server) updatesExport(ctx context.Context, a productArg) (any, error) {
	u, err := s.lists()
	if err != nil {
		return nil, err
	}
	c, err := u.Counts(ctx, a.Product)
	if err != nil {
		return nil, err
	}
	csv, err := u.ExportCSV(ctx, a.Product)
	return map[string]any{"count": c.Confirmed, "csv": csv}, err
}

// Package sentmail settles a send whose delivery is unknown by looking for
// it in the sending mailbox's Sent folder. The classic inbox, the browser
// app and the MCP tool check_sent all use Check.
package sentmail

import (
	"context"
	"fmt"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/gmail"
	"github.com/AstrolabeGTM/astrolabe/internal/mail"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
)

// Result says what the lookup found. When Found, the action has been
// recorded as sent; otherwise nothing changed and Message says what to do.
type Result struct {
	Found   bool   `json:"found"`
	Message string `json:"message"`
}

// Check looks for an action in its sender's Sent mail by Message-ID, through
// the SMTP/IMAP mailbox if one is set up, else Gmail.
func Check(ctx context.Context, out *outreach.Service, store *mail.Store, accounts *gmail.Accounts, id int64) (Result, error) {
	a, err := out.Action(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if a.Status != "unknown" && a.Status != "failed" {
		return Result{}, fmt.Errorf("%w: action %d is %s; only unknown or failed sends need checking", outreach.ErrInvalid, id, a.Status)
	}
	if a.MessageID == "" {
		return Result{}, fmt.Errorf("%w: no Message-ID recorded; check the mailbox by hand", outreach.ErrInvalid)
	}
	if store != nil {
		acct, err := store.Get(a.Sender)
		if err != nil {
			return Result{}, err
		}
		if acct != nil {
			found, err := mail.FindSent(acct, a.MessageID)
			if err != nil {
				return Result{}, err
			}
			if found {
				err := out.Resolve(ctx, a.ID, true, &channel.Result{ProviderID: a.MessageID, ThreadID: a.MessageID})
				return Result{true, fmt.Sprintf("Found #%d in %s; recorded as sent.", a.ID, acct.SentFolder)}, err
			}
			note := ""
			if !acct.SavesSent {
				note = " This provider doesn't file SMTP sends in Sent by itself, so a missing copy isn't proof; check with the recipient if it matters."
			}
			return Result{false, fmt.Sprintf("#%d is not in %s.%s", a.ID, acct.SentFolder, note)}, nil
		}
	}
	if accounts == nil {
		return Result{}, fmt.Errorf("%w: no mailbox is set up for %s", outreach.ErrInvalid, a.Sender)
	}
	mb, err := accounts.Mailbox(a.Sender)
	if err != nil {
		return Result{}, err
	}
	found, err := mb.FindSent(ctx, a.MessageID)
	if err != nil {
		return Result{}, err
	}
	if found.ProviderID != "" {
		err := out.Resolve(ctx, a.ID, true, &found)
		return Result{true, fmt.Sprintf("Found #%d in Sent mail; recorded as sent.", a.ID)}, err
	}
	// Not found by Message-ID. Gmail may not keep the id we set, so this is
	// not proof it wasn't sent: say what was sent to them around then.
	since := a.CreatedAt.Add(-time.Hour)
	subjects, err := mb.SentTo(ctx, a.Recipient, since)
	if err != nil {
		return Result{}, err
	}
	msg := fmt.Sprintf("#%d was not found by Message-ID. Nothing else was sent to %s since %s either, so it most likely did not go out; check by hand before returning it to drafts.",
		a.ID, a.Recipient, since.Local().Format("2 Jan 15:04"))
	if len(subjects) > 0 {
		msg = fmt.Sprintf("#%d was not found by Message-ID, but Sent mail has %d message(s) to %s since %s: %q. If one is this message, choose \"It was sent\".",
			a.ID, len(subjects), a.Recipient, since.Local().Format("2 Jan 15:04"), subjects)
	}
	return Result{false, msg}, nil
}

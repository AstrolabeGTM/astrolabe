package updates

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
)

var subjectToken = regexp.MustCompile(`(?i)\bunsubscribe\s+(\d+\.[0-9a-f]{24})\b`)

// Words that leave the list. "stop all" also leaves everything: it puts the
// person on the global do-not-contact list, as it does for outreach.
var (
	leaveWords    = map[string]bool{"stop": true, "unsubscribe": true, "unsubscribe me": true, "remove me": true, "stop emailing me": true}
	leaveAllWords = map[string]bool{"stop all": true, "unsubscribe all": true}
	// In answer to a confirmation request, these first words decline.
	declineFirst = map[string]bool{"no": true, "nope": true, "stop": true, "unsubscribe": true, "cancel": true, "remove": true}
)

// HandleInbound is the outreach.Service.OnInbound hook. It claims replies
// to the list's own messages:
//   - a reply to the confirmation confirms (unless it says no);
//   - "unsubscribe" or "stop" in a reply, or the mailto: unsubscribe a mail
//     client sends (its subject carries a signed token), leaves the list;
//   - a bounce marks the address undeliverable;
//   - an auto-reply is ignored (it must never confirm anyone).
//
// Anything else (a person answering a broadcast, say) is left for the
// operator's inbox, and a reply that does not reference a list message is
// not touched here at all.
func (s *Service) HandleInbound(ctx context.Context, tx pgx.Tx, in outreach.Inbound, replyID int64) (bool, error) {
	if in.Channel != "" && in.Channel != "email" {
		return false, nil
	}
	var (
		subID, personID int64
		kind, productID string
		email, status   string
		viaToken        bool
	)
	if len(in.References) > 0 {
		err := tx.QueryRow(ctx, `
			SELECT m.subscriber_id, m.kind, x.product_id, x.email, x.status, x.person_id
			FROM updates_messages m JOIN updates_subscribers x ON x.id = m.subscriber_id
			WHERE m.message_id = ANY($1) AND m.message_id <> '' ORDER BY m.id DESC LIMIT 1`, in.References).
			Scan(&subID, &kind, &productID, &email, &status, &personID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	}
	if subID == 0 {
		// A mailto: unsubscribe: a new message whose subject has our token.
		if m := subjectToken.FindStringSubmatch(in.Subject); m != nil {
			if id, ok := s.parseToken(ctx, "unsubscribe", m[1]); ok {
				err := tx.QueryRow(ctx, `SELECT product_id, email, status, person_id FROM updates_subscribers WHERE id = $1`, id).
					Scan(&productID, &email, &status, &personID)
				if err == nil {
					subID, viaToken, kind = id, true, "unsubscribe"
				} else if !errors.Is(err, pgx.ErrNoRows) {
					return false, err
				}
			}
		}
	}
	if subID == 0 {
		return false, nil
	}
	_, u, err := settings(ctx, tx, productID)
	if err != nil {
		if errors.Is(err, ErrRefused) {
			return false, nil
		}
		return false, err
	}
	if !viaToken && !strings.EqualFold(in.Mailbox, u.From) {
		return false, nil
	}
	label := func(class string) error {
		_, err := tx.Exec(ctx, `
			UPDATE replies SET product_id = $2, person_id = $3, classification = $4, classified_at = $5 WHERE id = $1 AND classification IN ('', $4)`,
			replyID, productID, personID, class, s.now())
		return err
	}

	// Left for the operator, but still tied to the list so it is never
	// mistaken for an answer to outreach.
	visible := func(own bool) (bool, error) {
		var pid any
		if own {
			pid = personID
		}
		_, err := tx.Exec(ctx, `UPDATE replies SET product_id = $2, person_id = $3 WHERE id = $1`, replyID, productID, pid)
		return true, err
	}
	switch {
	case in.Bounce:
		if _, err := tx.Exec(ctx, `UPDATE updates_subscribers SET status = 'bounced', expires_at = NULL WHERE id = $1 AND status IN ('pending', 'confirmed')`, subID); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE updates_messages SET status = 'skipped', error = 'address bounced' WHERE subscriber_id = $1 AND status = 'queued'`, subID); err != nil {
			return false, err
		}
		_, err := tx.Exec(ctx, `UPDATE replies SET product_id = $2, person_id = $3 WHERE id = $1`, replyID, productID, personID)
		return true, err
	case in.Automatic():
		return true, label("out_of_office")
	}
	// Only the sender's own address counts (a mailto: unsubscribe carries a
	// token that proves it instead).
	if !viaToken && people.NormalizeEmail(in.From) != email {
		return visible(false)
	}

	line := firstLine(in.Body)
	if line == "" {
		line = normalize(in.Subject)
		line = strings.TrimSpace(strings.TrimPrefix(line, "re:"))
	}
	first, _, _ := strings.Cut(line, " ")
	switch {
	case leaveAllWords[line]:
		if _, err := s.Unsubscribe(ctx, tx, subID, "reply", true); err != nil {
			return false, err
		}
		return true, label("unsubscribe")
	case leaveWords[line] || viaToken || kind == "confirm" && declineFirst[first]:
		if _, err := s.Unsubscribe(ctx, tx, subID, "reply", false); err != nil {
			return false, err
		}
		return true, label("unsubscribe")
	case kind == "confirm" && status == "pending":
		if ok, err := s.Confirm(ctx, tx, subID, "reply"); errors.Is(err, ErrExpired) {
			return visible(true) // too late: the operator sees the reply
		} else if err != nil {
			return false, err
		} else if ok {
			// Writing to us from this address is giving it to us.
			if err := people.LinkIdentity(ctx, tx, "email", email, personID, people.SourceReply, true); err != nil {
				return false, err
			}
		}
		return true, label("list")
	}
	return visible(true)
}

// firstLine is the first line of a reply that is not empty or quoted, in
// lower case without trailing punctuation.
func firstLine(body string) string {
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, ">") {
			continue
		}
		return normalize(l)
	}
	return ""
}

func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimRight(s, ".!,; ")
	return strings.Join(strings.Fields(s), " ")
}

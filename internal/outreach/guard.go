package outreach

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// githubPolicyReason is what the operator sees when the guard refuses.
const githubPolicyReason = "only known from GitHub: GitHub's Acceptable Use Policies (section 7) forbid using GitHub data to send unsolicited email, so no email goes to them unless they give an address themselves (a form, a reply, an import or a manual entry) or the product sets outreach.email_from_github: true"

// HasEmailStep reports whether any step of a sequence sends email.
func HasEmailStep(seq *product.Sequence) bool {
	for _, st := range seq.Steps {
		if st.Channel == "email" {
			return true
		}
	}
	return false
}

// MayEmail reports whether a person may be sent email for a product. It is
// the one place the GitHub-only policy lives: enrolment, the send
// transaction and answers to replies all ask it, and so should anything that
// sends email later (such as a broadcast to an updates list).
//
// A person is GitHub-only when GitHub is all we know of them: they have a
// GitHub login or a GitHub-sourced email, and no email identity from any
// other source. A GitHub-only person may not be emailed unless the product
// sets outreach.email_from_github. When ok is false, reason says why, in
// words fit to show the operator. People with no GitHub trace at all pass
// here; a missing address is reported where the address is needed.
func MayEmail(ctx context.Context, q store.Q, productID string, personID int64) (ok bool, reason string, err error) {
	refused, err := EmailRefusals(ctx, q, productID, []int64{personID})
	if err != nil {
		return false, "", err
	}
	if r, no := refused[personID]; no {
		return false, r, nil
	}
	return true, "", nil
}

// EmailRefusals is MayEmail for many people at once: it returns the reason
// for each person who may not be emailed for the product; everyone else
// may be. Lists such as the People screen use it.
func EmailRefusals(ctx context.Context, q store.Q, productID string, personIDs []int64) (map[int64]string, error) {
	refused := map[int64]string{}
	prod, err := product.Get(ctx, q, productID)
	if err != nil {
		return nil, err
	}
	if prod.Outreach.EmailFromGitHub || len(personIDs) == 0 {
		return refused, nil
	}
	rows, err := q.Query(ctx, `
		SELECT person_id FROM identities
		WHERE person_id = ANY($1) AND kind IN ('email', 'github')
		GROUP BY person_id
		HAVING NOT COALESCE(bool_or(kind = 'email' AND source <> 'github'), false)
		   AND COALESCE(bool_or(source = 'github'), false)`, personIDs)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	for _, id := range ids {
		refused[id] = githubPolicyReason
	}
	return refused, err
}

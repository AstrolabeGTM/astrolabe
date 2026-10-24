package outreach

import (
	"context"

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
	prod, err := product.Get(ctx, q, productID)
	if err != nil {
		return false, "", err
	}
	if prod.Outreach.EmailFromGitHub {
		return true, "", nil
	}
	var own, fromGitHub bool
	if err := q.QueryRow(ctx, `
		SELECT
			COALESCE(bool_or(kind = 'email' AND source <> 'github'), false),
			COALESCE(bool_or(source = 'github'), false)
		FROM identities WHERE person_id = $1 AND kind IN ('email', 'github')`, personID).Scan(&own, &fromGitHub); err != nil {
		return false, "", err
	}
	if own || !fromGitHub {
		return true, "", nil
	}
	return false, githubPolicyReason, nil
}

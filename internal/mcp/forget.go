package mcp

import (
	"context"
	"errors"
	"maps"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AstrolabeGTM/astrolabe/internal/forget"
)

const forgetInstructions = `

Erasure requests: forget_person deletes everything about one person (by email or id) and remembers only a salted hash so they are never recontacted or recreated. Call it first without confirm to get the counts per table, show them to the operator, and call it again with confirm: true only on their say-so. It cannot be undone. Copies Astrolabe cannot reach (your mailbox, backups, Netlify when it has no token) are listed in the answer: tell the operator exactly what remains.`

func init() {
	maps.Copy(salesTerms, map[string]string{
		"forget_person": "GDPR erasure, right to be forgotten, data deletion request, delete my data, CCPA delete, privacy request, remove a contact, erase a lead",
	})
}

type forgetArg struct {
	Person  string `json:"person" jsonschema:"email address or person id"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"true to really erase; without it nothing is deleted and you get a dry-run summary"`
}

func (s *Server) registerForget(srv *sdk.Server) {
	register(srv, "forget_person", "Erase everything about one person on request (email or id): the person, addresses, messages sent and received, replies, research notes, scores, tasks, call notes, survey and pricing answers, form submissions (also at Netlify, through its API), list subscriptions and messages, funnel events, tracked links and clicks, payments rows and their mentions in free text; the account and company stay. A salted hash of their addresses and GitHub login is kept so a later monitor run, form, import or reply is dropped and they are never contacted; if they write again through a form an operator task is opened. Without confirm: true it deletes nothing and returns counts per table. With it: the counts, plus any copies left that must be deleted by hand (Netlify submission ids) and places Astrolabe cannot reach. Cannot be undone.", s.forgetPerson)
}

func (s *Server) forgetPerson(ctx context.Context, a forgetArg) (any, error) {
	if a.Person == "" {
		return nil, errors.New("person is required: an email address or a person id")
	}
	rep, err := forget.Forget(ctx, s.S.Pool, a.Person, forget.Options{DryRun: !a.Confirm, Netlify: s.Netlify})
	if err != nil {
		return nil, err
	}
	out := map[string]any{"report": rep}
	if rep.DryRun {
		out["note"] = "Nothing was deleted. Show the operator these counts; call again with confirm: true to erase. This cannot be undone."
	}
	return out, nil
}

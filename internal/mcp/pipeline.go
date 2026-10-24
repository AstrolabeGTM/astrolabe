package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AstrolabeGTM/astrolabe/internal/notes"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/pipeline"
	"github.com/AstrolabeGTM/astrolabe/internal/play"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

// pipelineInstructions extends the server instructions for the account
// pipeline.
const pipelineInstructions = `

Account pipeline (b2b products with pipeline: in product.yaml): companies (accounts) move through deal stages beside the per-person funnel. Sales term -> Astrolabe term: deal/opportunity -> account; deal stage -> pipeline stage (move_stage); BANT/MEDDIC/qualification criteria -> qualify checks; next step -> next action; closed-lost/on hold/nurture -> close_account (lost or paused). Every move needs one line of evidence; the gate stage and later ones need every gating check answered yes, so if move_stage is refused, tell the operator which checks fail rather than retrying.`

var pipelineTerms = map[string]string{
	"pipeline":        "deal pipeline, opportunities, pipeline review, deal board",
	"account":         "account/deal/opportunity record",
	"add_account":     "create a deal or opportunity, add a target account",
	"move_stage":      "advance a deal, change the opportunity stage",
	"qualify":         "BANT, MEDDIC, qualification criteria, discovery answers",
	"set_next_action": "next step, follow-up date",
	"close_account":   "close-lost, closed lost, deal on hold, nurture",
	"update_account":  "deal notes, pain, compelling event, status quo, stakeholders, champion, economic buyer",
	"pipeline_export": "pipeline report, deal export, weekly pipeline snapshot",
}

func init() { maps.Copy(salesTerms, pipelineTerms) }

func (s *Server) registerPipeline(srv *sdk.Server) {
	register(srv, "pipeline", "The account pipeline of a b2b product (pipeline: in product.yaml): open accounts per stage, paused and lost counts, each account's days in its stage and its one next action with the due date, plus stuck accounts (longer than stuck_after in one stage), overdue next actions, and paused or lost accounts whose revisit date has come.", s.pipelineBoard)
	register(srv, "account", "Everything about one account (id, company domain or company name): stage and days in it, next action, qualification checks with evidence (and which gating checks fail), people and their roles, evidence links, stage history, goal, timing, baseline and open tasks.", s.account)
	register(srv, "add_account", "Put a company into a product's pipeline: company domain (or name), one line of evidence for why it belongs (a named person and a specific need or signal), and its one next action with a due date. Starts at the first stage unless stage is given; the qualify gate and later stages are refused until the checks are yes. Optional: people with roles (champion, buyer, user), goal, timing, baseline.", s.addAccount)
	register(srv, "move_stage", "Move an account to another pipeline stage with one line of evidence (what happened). Entering the qualify gate stage or any later one is refused, with the reason, unless every gating check is yes. Also reopens a paused or lost account (then next_action and due are required). Give the next action for the new stage too; without one the current one stays.", s.moveStage)
	register(srv, "qualify", "Answer an account's qualification checks: yes, no or unknown, each with one line of evidence (required for yes and no). Gating checks must all be yes to enter the gate stage; record-only checks are kept but never block.", s.qualify)
	register(srv, "set_next_action", "Set an open account's one next action and its due date (YYYY-MM-DD, default today). It replaces the current one: an open account always has exactly one.", s.setNextAction)
	register(srv, "close_account", "Pause an account or mark it lost, with a reason from the product's list and a revisit date (YYYY-MM-DD). A task to look at it again appears on that date. Its next action is dropped; move_stage reopens it.", s.closeAccount)
	register(srv, "update_account", "Edit an account: goal (what they want to achieve), timing (what is driving the timing), baseline (how they do it today), people with roles (person id or email; remove=true takes one off; an unknown email is added as a new person) and evidence links.", s.updateAccount)
	register(srv, "pipeline_export", "The pipeline as CSV: one row per account per week from the weekly snapshots (this week's is taken now if missing), with status, stage, days in stage, stuck, next action and due, overdue, reason, revisit date and each check's answer.", s.pipelineExport)
}

type accountArg struct {
	Product string `json:"product"`
	Account string `json:"account" jsonschema:"account id, company domain or company name"`
}

type memberArg struct {
	Person string `json:"person" jsonschema:"person id or email"`
	Role   string `json:"role,omitempty" jsonschema:"champion, buyer, user, ... (default contact)"`
	Remove bool   `json:"remove,omitempty" jsonschema:"take this person off the account"`
}

type linkArg struct {
	URL  string `json:"url"`
	Note string `json:"note,omitempty"`
}

type addAccountArg struct {
	Product    string      `json:"product"`
	Company    string      `json:"company" jsonschema:"company domain (acme.example), or its name if it has no domain"`
	Name       string      `json:"name,omitempty" jsonschema:"company display name"`
	Stage      string      `json:"stage,omitempty" jsonschema:"default: the first stage"`
	Evidence   string      `json:"evidence" jsonschema:"one line: why this company belongs in the pipeline"`
	NextAction string      `json:"next_action"`
	Due        string      `json:"due,omitempty" jsonschema:"YYYY-MM-DD; default today"`
	People     []memberArg `json:"people,omitempty"`
	Goal       string      `json:"goal,omitempty" jsonschema:"what they want to achieve"`
	Timing     string      `json:"timing,omitempty" jsonschema:"what is driving the timing"`
	Baseline   string      `json:"baseline,omitempty" jsonschema:"how they do it today"`
}

type moveArg struct {
	Product    string `json:"product"`
	Account    string `json:"account" jsonschema:"account id, company domain or company name"`
	Stage      string `json:"stage"`
	Evidence   string `json:"evidence" jsonschema:"one line: what happened that justifies the move"`
	NextAction string `json:"next_action,omitempty" jsonschema:"the next action in the new stage (required when reopening)"`
	Due        string `json:"due,omitempty" jsonschema:"YYYY-MM-DD; default today"`
}

type qualifyArg struct {
	Product string            `json:"product"`
	Account string            `json:"account" jsonschema:"account id, company domain or company name"`
	Answers []pipeline.Answer `json:"answers"`
}

type nextActionArg struct {
	Product    string `json:"product"`
	Account    string `json:"account" jsonschema:"account id, company domain or company name"`
	NextAction string `json:"next_action"`
	Due        string `json:"due,omitempty" jsonschema:"YYYY-MM-DD; default today"`
}

type closeAccountArg struct {
	Product string `json:"product"`
	Account string `json:"account" jsonschema:"account id, company domain or company name"`
	Status  string `json:"status" jsonschema:"paused or lost"`
	Reason  string `json:"reason" jsonschema:"one of the product's pipeline reasons"`
	Revisit string `json:"revisit" jsonschema:"YYYY-MM-DD: when to look again"`
	Note    string `json:"note,omitempty" jsonschema:"one line"`
}

type updateAccountArg struct {
	Product  string      `json:"product"`
	Account  string      `json:"account" jsonschema:"account id, company domain or company name"`
	Goal     *string     `json:"goal,omitempty" jsonschema:"what they want to achieve"`
	Timing   *string     `json:"timing,omitempty" jsonschema:"what is driving the timing"`
	Baseline *string     `json:"baseline,omitempty" jsonschema:"how they do it today"`
	People   []memberArg `json:"people,omitempty"`
	Links    []linkArg   `json:"links,omitempty"`
}

// parseDay reads YYYY-MM-DD in local time; empty means def.
func parseDay(field, s string, def time.Time) (time.Time, error) {
	if s == "" {
		return def, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be YYYY-MM-DD", field)
	}
	return t, nil
}

// pipelineProduct finds a product and checks that it has a pipeline.
func (s *Server) pipelineProduct(id string) (*product.Product, error) {
	p, err := s.find(id)
	if err != nil {
		return nil, err
	}
	if p.Pipeline == nil {
		return nil, fmt.Errorf("%s has no account pipeline; add pipeline: (stages, qualify) to its product.yaml", p.ID)
	}
	return p, nil
}

func (s *Server) findAccount(ctx context.Context, a accountArg) (*product.Product, int64, error) {
	p, err := s.pipelineProduct(a.Product)
	if err != nil {
		return nil, 0, err
	}
	id, err := pipeline.Find(ctx, s.S.Pool, p, a.Account)
	return p, id, err
}

func (s *Server) accountDetail(ctx context.Context, p *product.Product, id int64) (any, error) {
	return pipeline.Get(ctx, s.S.Pool, p, id, time.Now())
}

// scanSoon asks the server's workers to run automations now, so account
// triggers don't wait for the hourly scan.
func (s *Server) scanSoon(ctx context.Context) {
	if s.S.River == nil {
		return
	}
	if _, err := s.S.River.Insert(ctx, play.ScanArgs{}, nil); err != nil {
		slog.Warn("queue automation scan", "err", err)
	}
}

func (s *Server) pipelineBoard(ctx context.Context, a productArg) (any, error) {
	p, err := s.pipelineProduct(a.Product)
	if err != nil {
		return nil, err
	}
	return pipeline.BuildBoard(ctx, s.S.Pool, p, time.Now())
}

func (s *Server) account(ctx context.Context, a accountArg) (any, error) {
	p, id, err := s.findAccount(ctx, a)
	if err != nil {
		return nil, err
	}
	d, err := pipeline.Get(ctx, s.S.Pool, p, id, time.Now())
	if err != nil {
		return nil, err
	}
	list, err := notes.List(ctx, s.S.Pool, notes.ListOpts{Product: p.ID, AccountID: id}, time.Now())
	if err != nil {
		return nil, err
	}
	return struct {
		*pipeline.Detail
		Conversations []*notes.Note `json:"conversations"`
	}{d, list}, nil
}

func (s *Server) addAccount(ctx context.Context, a addAccountArg) (any, error) {
	p, err := s.pipelineProduct(a.Product)
	if err != nil {
		return nil, err
	}
	due, err := parseDay("due", a.Due, time.Now())
	if err != nil {
		return nil, err
	}
	o := pipeline.AddOpts{Name: a.Name, Stage: a.Stage, Evidence: a.Evidence, NextAction: a.NextAction, Due: due,
		Goal: a.Goal, Timing: a.Timing, Baseline: a.Baseline}
	if c := strings.TrimSpace(a.Company); strings.Contains(c, ".") && !strings.Contains(c, " ") {
		o.Domain = c
	} else if o.Name == "" {
		o.Name = c
	}
	id, err := pipeline.Add(ctx, s.S.Pool, p, o, time.Now())
	if err != nil {
		return nil, err
	}
	if err := s.setPeople(ctx, p, id, a.People); err != nil {
		return nil, fmt.Errorf("account %d was added, but: %w", id, err)
	}
	s.scanSoon(ctx)
	return s.accountDetail(ctx, p, id)
}

func (s *Server) setPeople(ctx context.Context, p *product.Product, id int64, list []memberArg) error {
	for _, m := range list {
		who, err := s.resolvePerson(ctx, m.Person)
		if err != nil && strings.Contains(m.Person, "@") && !m.Remove {
			if _, ierr := people.Import(ctx, s.S.Pool, p.ID, []people.Row{{Email: m.Person}}); ierr != nil {
				return ierr
			}
			who, err = s.resolvePerson(ctx, m.Person)
		}
		if err != nil {
			return err
		}
		if m.Remove {
			err = pipeline.RemovePerson(ctx, s.S.Pool, p, id, who.ID)
		} else {
			err = pipeline.SetPerson(ctx, s.S.Pool, p, id, who.ID, m.Role)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) moveStage(ctx context.Context, a moveArg) (any, error) {
	p, id, err := s.findAccount(ctx, accountArg{a.Product, a.Account})
	if err != nil {
		return nil, err
	}
	o := pipeline.MoveOpts{Stage: a.Stage, Evidence: a.Evidence, NextAction: a.NextAction}
	if a.NextAction != "" {
		if o.Due, err = parseDay("due", a.Due, time.Now()); err != nil {
			return nil, err
		}
	}
	if err := pipeline.Move(ctx, s.S.Pool, p, id, o, time.Now()); err != nil {
		return nil, err
	}
	s.scanSoon(ctx)
	return s.accountDetail(ctx, p, id)
}

func (s *Server) qualify(ctx context.Context, a qualifyArg) (any, error) {
	p, id, err := s.findAccount(ctx, accountArg{a.Product, a.Account})
	if err != nil {
		return nil, err
	}
	if err := pipeline.Qualify(ctx, s.S.Pool, p, id, a.Answers, time.Now()); err != nil {
		return nil, err
	}
	return s.accountDetail(ctx, p, id)
}

func (s *Server) setNextAction(ctx context.Context, a nextActionArg) (any, error) {
	p, id, err := s.findAccount(ctx, accountArg{a.Product, a.Account})
	if err != nil {
		return nil, err
	}
	due, err := parseDay("due", a.Due, time.Now())
	if err != nil {
		return nil, err
	}
	if err := pipeline.SetNextAction(ctx, s.S.Pool, p, id, a.NextAction, due); err != nil {
		return nil, err
	}
	return s.accountDetail(ctx, p, id)
}

func (s *Server) closeAccount(ctx context.Context, a closeAccountArg) (any, error) {
	p, id, err := s.findAccount(ctx, accountArg{a.Product, a.Account})
	if err != nil {
		return nil, err
	}
	if a.Revisit == "" {
		return nil, errors.New("revisit (YYYY-MM-DD) is required: when to look at this account again")
	}
	revisit, err := parseDay("revisit", a.Revisit, time.Time{})
	if err != nil {
		return nil, err
	}
	if err := pipeline.Close(ctx, s.S.Pool, p, id, pipeline.CloseOpts{Status: a.Status, Reason: a.Reason, Revisit: revisit, Note: a.Note}, time.Now()); err != nil {
		return nil, err
	}
	return s.accountDetail(ctx, p, id)
}

func (s *Server) updateAccount(ctx context.Context, a updateAccountArg) (any, error) {
	p, id, err := s.findAccount(ctx, accountArg{a.Product, a.Account})
	if err != nil {
		return nil, err
	}
	if a.Goal != nil || a.Timing != nil || a.Baseline != nil {
		if err := pipeline.Update(ctx, s.S.Pool, p, id, pipeline.UpdateOpts{Goal: a.Goal, Timing: a.Timing, Baseline: a.Baseline}); err != nil {
			return nil, err
		}
	}
	if err := s.setPeople(ctx, p, id, a.People); err != nil {
		return nil, err
	}
	for _, l := range a.Links {
		if err := pipeline.AddLink(ctx, s.S.Pool, p, id, l.URL, l.Note); err != nil {
			return nil, err
		}
	}
	return s.accountDetail(ctx, p, id)
}

func (s *Server) pipelineExport(ctx context.Context, a productArg) (any, error) {
	p, err := s.pipelineProduct(a.Product)
	if err != nil {
		return nil, err
	}
	if _, err := pipeline.Snapshot(ctx, s.S.Pool, p, time.Now()); err != nil {
		return nil, err
	}
	var b strings.Builder
	if err := pipeline.WriteCSV(ctx, s.S.Pool, p, &b); err != nil {
		return nil, err
	}
	return map[string]any{"csv": b.String(), "note": "One row per account per weekly snapshot (taken Mondays, or the first hour the server runs that week)."}, nil
}

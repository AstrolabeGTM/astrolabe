package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AstrolabeGTM/astrolabe/internal/review"
)

const reviewInstructions = `

Weekly review: weekly_review builds one page of numbers for an ISO week (reach, users, accounts, response times, themes, campaigns, experiments) and the review's questions; every figure is a count with its denominator and window. The friday_review prompt walks the operator through the questions. save_review records their answers and a keep, change or stop decision per running experiment and campaign, and freezes the week's numbers so a past week never drifts. reviews lists saved weeks. Never turn counts into percentages without the counts.`

var reviewTerms = map[string]string{
	"weekly_review": "pipeline review, forecast call, QBR-lite, Friday review, weekly business review, WBR, weekly metrics review",
	"save_review":   "log the review decisions, review notes, action items from the forecast call, QBR notes",
	"reviews":       "past pipeline reviews, review archive, previous forecast calls",
}

func init() { maps.Copy(salesTerms, reviewTerms) }

func (s *Server) registerReview(srv *sdk.Server) {
	register(srv, "weekly_review", "Build the weekly review for a product and ISO week (default this week, like 2026-W43): one page of counts, each with its denominator and window, that answers the Friday questions. It covers reach (public counters, updates list, mentions by tone, form requests by the source people reported, new signals by type), users (people entering each funnel stage this week, a fixed four-week cohort's stage-to-stage counts, PMF survey counts so far, assisted versus unassisted activations), accounts (entries per stage, median days in each stage, stuck accounts and overdue next actions, paused and lost reasons counted, advocates; products with a pipeline), response times (inbound form requests, interested or question replies and new GitHub issues or discussions: how many were answered within target, over target or are still waiting, and the slowest; wall-clock hours), the week's themes and pricing feedback, running campaigns, running experiments with their verdict, and the review's questions with any saved answers. A saved week returns the frozen snapshot with the saved answers and decisions; fresh=true adds a new build beside it.", s.weeklyReview)
	register(srv, "save_review", "Save the review: answers to the review's questions (by number or exact text; an empty answer clears one) and a keep, change or stop decision with a note for each running experiment (its key) and campaign (its id or code). Saving again merges into what was saved. The first save freezes the week's numbers as built then; refresh_snapshot=true rebuilds them. Only on the operator's say-so: these are their answers.", s.saveReview)
	register(srv, "reviews", "Saved weekly reviews for a product, newest week first, with how many questions were answered and decisions made. Fetch one with weekly_review.", s.savedReviews)

	srv.AddPrompt(&sdk.Prompt{
		Name:        "friday_review",
		Description: "Walk through the weekly review (pipeline review, forecast call, QBR-lite): the week's numbers, then each question in turn, then keep, change or stop for each running experiment and campaign, then save_review.",
		Arguments: []*sdk.PromptArgument{
			{Name: "product", Description: "product id", Required: true},
			{Name: "week", Description: "ISO week like 2026-W43; default this week"},
		},
	}, s.fridayReviewPrompt)
}

type weeklyReviewArg struct {
	Product string `json:"product" jsonschema:"product id"`
	Week    string `json:"week,omitempty" jsonschema:"ISO week like 2026-W43; default this week"`
	Fresh   bool   `json:"fresh,omitempty" jsonschema:"for a saved week, also build the week again from today's data"`
}

func (s *Server) weeklyReview(ctx context.Context, a weeklyReviewArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if a.Week != "" {
		if saved, err := review.Get(ctx, s.S.Pool, p, a.Week); err != nil {
			return nil, err
		} else if saved != nil {
			out := map[string]any{"saved": true, "saved_review": saved}
			if a.Fresh {
				if out["fresh_build"], err = review.BuildAt(ctx, s.S.Pool, p, a.Week, now); err != nil {
					return nil, err
				}
			}
			return out, nil
		}
	}
	if a.Week == "" {
		// The current week may be saved too.
		if saved, err := review.Get(ctx, s.S.Pool, p, weekOf(now)); err != nil {
			return nil, err
		} else if saved != nil {
			out := map[string]any{"saved": true, "saved_review": saved}
			if a.Fresh {
				if out["fresh_build"], err = review.BuildAt(ctx, s.S.Pool, p, "", now); err != nil {
					return nil, err
				}
			}
			return out, nil
		}
	}
	r, err := review.BuildAt(ctx, s.S.Pool, p, a.Week, now)
	if err != nil {
		return nil, err
	}
	return map[string]any{"saved": false, "review": r}, nil
}

type saveReviewArg struct {
	Product   string            `json:"product" jsonschema:"product id"`
	Week      string            `json:"week,omitempty" jsonschema:"ISO week like 2026-W43; default this week"`
	Answers   []review.Answer   `json:"answers,omitempty"`
	Decisions []review.Decision `json:"decisions,omitempty"`
	Refresh   bool              `json:"refresh_snapshot,omitempty" jsonschema:"rebuild the frozen numbers from today's data (normally the first save's numbers stay)"`
}

func (s *Server) saveReview(ctx context.Context, a saveReviewArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	return review.Save(ctx, s.S.Pool, p, a.Week, review.SaveInput{Answers: a.Answers, Decisions: a.Decisions, Refresh: a.Refresh}, time.Now())
}

type reviewsArg struct {
	Product string `json:"product" jsonschema:"product id"`
	Limit   int    `json:"limit,omitempty"`
}

func (s *Server) savedReviews(ctx context.Context, a reviewsArg) (any, error) {
	p, err := s.find(a.Product)
	if err != nil {
		return nil, err
	}
	l, err := review.List(ctx, s.S.Pool, p, a.Limit)
	if err != nil {
		return nil, err
	}
	return map[string]any{"product": p.ID, "reviews": l}, nil
}

func weekOf(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

const fridayInstructions = `Walk the operator through this week's review.
1. Start with two or three sentences: what stands out in the numbers. Give every figure as a count with its denominator ("3 of 7"), never a bare percentage. Say where a number is partial (the week is not over) or where a response-time item is still waiting.
2. Ask the questions one at a time, in order. Before each, show the numbers that bear on it (activation and what people said for 1; counters, sign-ups, mentions and reported sources for 2; accounts moved and stuck for 3; themes and quotes for 4). Draft a short answer from the numbers if you can, and let the operator edit it. Use their words for what people said.
3. For each running experiment and campaign, state its verdict or status and ask keep, change or stop, with a one-line note. Do not decide for them.
4. When they have answered, call save_review with their answers (by question number) and decisions. Saving freezes this week's numbers. Skip any question they want to leave open.`

func (s *Server) fridayReviewPrompt(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
	args := req.Params.Arguments
	p, err := s.find(args["product"])
	if err != nil {
		return nil, err
	}
	now := time.Now()
	week := args["week"]
	if week == "" {
		week = weekOf(now)
	}
	var body []byte
	if saved, err := review.Get(ctx, s.S.Pool, p, week); err != nil {
		return nil, err
	} else if saved != nil {
		body, err = json.MarshalIndent(saved, "", "  ")
		if err != nil {
			return nil, err
		}
	} else {
		r, err := review.BuildAt(ctx, s.S.Pool, p, week, now)
		if err != nil {
			return nil, err
		}
		if body, err = json.MarshalIndent(r, "", "  "); err != nil {
			return nil, err
		}
	}
	var qs []string
	for i, q := range p.ReviewSettings().Questions {
		qs = append(qs, fmt.Sprintf("%d. %s", i+1, q))
	}
	text := fmt.Sprintf("Weekly review for %s, %s.\n\n%s\n\nThe questions:\n%s\n\nThe week's review (JSON):\n%s", p.Name, week, fridayInstructions, strings.Join(qs, "\n"), body)
	return &sdk.GetPromptResult{
		Description: "Weekly review of " + week + " for " + p.ID,
		Messages:    []*sdk.PromptMessage{{Role: "user", Content: &sdk.TextContent{Text: text}}},
	}, nil
}

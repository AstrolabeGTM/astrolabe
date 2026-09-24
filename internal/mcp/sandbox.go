package mcp

import (
	"context"

	"github.com/AstrolabeGTM/astrolabe/internal/sandbox"
)

type outboxArg struct {
	OutboxID int64  `json:"outbox_id"`
	Body     string `json:"body,omitempty" jsonschema:"their reply text (default: an interested reply)"`
}

func (s *Server) outbox(ctx context.Context, _ noArgs) (any, error) {
	msgs, err := sandbox.Outbox(ctx, s.S.Pool, 50)
	return map[string]any{"mode": "sandbox", "messages": msgs}, err
}

func (s *Server) simulateReply(ctx context.Context, a outboxArg) (any, error) {
	if a.Body == "" {
		a.Body = "Thanks, this looks useful. How do I get started?"
	}
	if err := sandbox.SimulateReply(ctx, s.S, a.OutboxID, a.Body); err != nil {
		return nil, err
	}
	return map[string]string{"status": "ok", "note": "The sequence stopped; the reply is in the inbox to label."}, nil
}

func (s *Server) simulateBounce(ctx context.Context, a outboxArg) (any, error) {
	return ok(sandbox.SimulateBounce(ctx, s.S, a.OutboxID))
}

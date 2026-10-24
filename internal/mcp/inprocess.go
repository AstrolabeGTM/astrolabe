package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Handler is one tool as a plain function, for callers in the same process
// (the browser app's JSON API). It runs exactly the code the MCP tool runs,
// so a screen and Claude can never disagree.
type Handler struct {
	// In is the tool's argument type (a struct with json tags).
	In reflect.Type
	// Call decodes args (a JSON object; unknown fields are refused) into In
	// and runs the tool.
	Call func(ctx context.Context, args json.RawMessage) (any, error)
}

// capturing holds, for a server being built by Handlers, where register
// records each tool.
var capturing sync.Map // *sdk.Server -> map[string]Handler

// Handlers returns every tool this server offers, by name.
func (s *Server) Handlers() map[string]Handler {
	out := map[string]Handler{}
	s.build("in-process", out)
	return out
}

// record keeps a tool's handler if its server is being built by Handlers.
func record[In any](srv *sdk.Server, name string, f func(context.Context, In) (any, error)) {
	m, ok := capturing.Load(srv)
	if !ok {
		return
	}
	m.(map[string]Handler)[name] = Handler{
		In: reflect.TypeFor[In](),
		Call: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in In
			if len(bytes.TrimSpace(args)) > 0 && string(bytes.TrimSpace(args)) != "null" {
				d := json.NewDecoder(bytes.NewReader(args))
				d.DisallowUnknownFields()
				if err := d.Decode(&in); err != nil {
					return nil, fmt.Errorf("%s: invalid arguments: %w", name, err)
				}
			}
			return f(ctx, in)
		},
	}
}

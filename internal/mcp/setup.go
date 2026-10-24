package mcp

import (
	"context"
	"errors"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/views"
)

func (s *Server) productSetup(ctx context.Context, a product.Spec) (any, error) {
	if s.ProductsDir == "" {
		return nil, errors.New("products folder is not configured")
	}
	dir, err := product.Scaffold(s.ProductsDir, a)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"folder": dir}
	if _, err := product.Load(dir); err != nil {
		res["status"] = "written, not loaded yet"
		res["fix"] = err.Error()
		return res, nil
	}
	if s.Reload != nil {
		if err := s.Reload(ctx); err != nil {
			res["reload_warning"] = err.Error()
		}
	}
	res["status"] = "loaded; add fit rules, monitors.yaml and plays.yaml next"
	return res, nil
}

func (s *Server) loadIssues() []views.LoadIssue {
	if s.LoadIssues == nil {
		return nil
	}
	return s.LoadIssues()
}

func (s *Server) status(ctx context.Context, _ noArgs) (any, error) {
	mode := s.Mode
	if mode == "" {
		mode = "live"
	}
	return views.GetStatus(ctx, s.S, mode, s.Products(), s.loadIssues(), s.aiEnabled() == nil)
}

func (s *Server) setup(ctx context.Context, _ noArgs) (any, error) {
	mode := s.Mode
	if mode == "" {
		mode = "live"
	}
	return views.GetSetup(ctx, s.S, mode, s.Products(), s.loadIssues(), s.Mail, s.Accounts)
}

type doctorArg struct {
	Connect bool `json:"connect,omitempty" jsonschema:"also test mailbox logins, the GitHub token, the Claude key and DNS (slower; sends nothing)"`
}

func (s *Server) doctor(ctx context.Context, a doctorArg) (any, error) {
	if s.Doctor == nil {
		return nil, errors.New("doctor is not available in this process")
	}
	return map[string]any{"checks": s.Doctor(ctx, a.Connect)}, nil
}

func (s *Server) reloadProducts(ctx context.Context, _ noArgs) (any, error) {
	if s.Reload == nil {
		return nil, errors.New("reloading is not available in this process")
	}
	err := s.Reload(ctx)
	res := map[string]any{"loaded": views.Summarize(s.Products()), "load_problems": s.loadIssues()}
	if err != nil && len(s.loadIssues()) == 0 {
		return nil, err
	}
	return res, nil
}

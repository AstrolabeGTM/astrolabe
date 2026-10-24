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

func (s *Server) loadErrors() []string {
	if s.LoadErrors == nil {
		return nil
	}
	return s.LoadErrors()
}

func (s *Server) status(ctx context.Context, _ noArgs) (any, error) {
	mode := s.Mode
	if mode == "" {
		mode = "live"
	}
	return views.GetStatus(ctx, s.S, mode, s.Products(), s.loadErrors(), s.aiEnabled() == nil)
}

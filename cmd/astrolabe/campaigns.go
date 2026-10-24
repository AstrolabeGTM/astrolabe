package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/app"
	"github.com/AstrolabeGTM/astrolabe/internal/campaign"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

const campaignsUsage = `usage:
  astrolabe campaigns export [-product <id>] [-o campaigns.json]
        the landing-page variants of campaigns that have not ended, as [{"code","label"}]
        for a website to prerender per-campaign landing pages
  astrolabe campaigns report <product> <campaign>
        spend, clicks and attributed results for the campaign window`

// campaignsCmd handles `astrolabe campaigns export|report`.
func campaignsCmd(ctx context.Context, cfg app.Config, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(campaignsUsage)
	}
	fs := flag.NewFlagSet("campaigns "+args[0], flag.ContinueOnError)
	productID := fs.String("product", "", "limit to one product")
	outFile := fs.String("o", "", "write to this file instead of standard output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	a, err := app.Open(ctx, cfg, false)
	if err != nil {
		return err
	}
	defer a.Close()
	if err := a.CheckMigrated(ctx); err != nil {
		return err
	}
	products, err := a.LoadProducts(ctx)
	if err != nil {
		slog.Warn("some products were not loaded", "err", err)
	}
	switch args[0] {
	case "export":
		if fs.NArg() != 0 {
			return errors.New(campaignsUsage)
		}
		var ps []*product.Product
		for _, p := range products {
			if *productID == "" || p.ID == *productID {
				ps = append(ps, p)
			}
		}
		if *productID != "" && len(ps) == 0 {
			return fmt.Errorf("no valid product %q", *productID)
		}
		v, err := campaign.Variants(ctx, a.Pool, ps, time.Now())
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(v, "", "  ")
		b = append(b, '\n')
		if *outFile != "" {
			return os.WriteFile(*outFile, b, 0o644)
		}
		_, err = out.Write(b)
		return err
	case "report":
		if fs.NArg() != 2 {
			return errors.New(campaignsUsage)
		}
		for _, p := range products {
			if p.ID != fs.Arg(0) {
				continue
			}
			c := p.Campaign(fs.Arg(1))
			if c == nil {
				return fmt.Errorf("%s has no campaign %q", p.ID, fs.Arg(1))
			}
			r, err := campaign.BuildReport(ctx, a.Pool, p, c, time.Now())
			if err != nil {
				return err
			}
			fmt.Fprint(out, r.Text())
			return nil
		}
		return fmt.Errorf("no valid product %q", fs.Arg(0))
	}
	return errors.New(campaignsUsage)
}

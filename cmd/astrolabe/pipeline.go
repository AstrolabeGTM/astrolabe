package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/app"
	"github.com/AstrolabeGTM/astrolabe/internal/pipeline"
)

// pipelineCmd: astrolabe pipeline export <product> [file.csv]. This week's
// snapshot is taken first if the server hasn't taken it yet.
func pipelineCmd(ctx context.Context, cfg app.Config, args []string, out io.Writer) error {
	if len(args) < 2 || len(args) > 3 || args[0] != "export" {
		return errors.New("usage: astrolabe pipeline export <product> [file.csv]")
	}
	a, err := app.Open(ctx, cfg, false)
	if err != nil {
		return err
	}
	defer a.Close()
	if err := a.CheckMigrated(ctx); err != nil {
		return err
	}
	products, loadErr := a.LoadProducts(ctx)
	if loadErr != nil {
		slog.Warn("some products were not loaded", "err", loadErr)
	}
	for _, p := range products {
		if p.ID != args[1] {
			continue
		}
		if p.Pipeline == nil {
			return fmt.Errorf("%s has no pipeline in product.yaml", p.ID)
		}
		if _, err := pipeline.Snapshot(ctx, a.Pool, p, time.Now()); err != nil {
			return err
		}
		if len(args) == 3 {
			f, err := os.Create(args[2])
			if err != nil {
				return err
			}
			if err := pipeline.WriteCSV(ctx, a.Pool, p, f); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			fmt.Fprintln(out, "wrote", args[2])
			return nil
		}
		return pipeline.WriteCSV(ctx, a.Pool, p, out)
	}
	return fmt.Errorf("product %q is not loaded (is it valid? run astrolabe product validate)", args[1])
}

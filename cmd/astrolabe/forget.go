package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/AstrolabeGTM/astrolabe/internal/app"
	"github.com/AstrolabeGTM/astrolabe/internal/forget"
)

// forgetCmd is `astrolabe forget <email|id> [-yes]`: show what would be
// erased, then erase after a yes (or with -yes).
func forgetCmd(ctx context.Context, cfg app.Config, args []string, in io.Reader, out io.Writer) error {
	var who string
	yes := false
	for _, a := range args {
		switch {
		case a == "-yes" || a == "--yes" || a == "-y":
			yes = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %s\nusage: astrolabe forget <email or person id> [-yes]", a)
		case who == "":
			who = a
		default:
			return errors.New("usage: astrolabe forget <email or person id> [-yes]")
		}
	}
	if who == "" {
		return errors.New("usage: astrolabe forget <email or person id> [-yes]")
	}
	a, err := app.Open(ctx, cfg, false)
	if err != nil {
		return err
	}
	defer a.Close()
	if err := a.CheckMigrated(ctx); err != nil {
		return err
	}
	opt := forget.Options{Netlify: a.Forms.Netlify}
	if !yes {
		opt.DryRun = true
		rep, err := forget.Forget(ctx, a.Pool, who, opt)
		if err != nil {
			return err
		}
		fmt.Fprint(out, rep.Summary())
		fmt.Fprint(out, "\nThis cannot be undone. Erase them now? Type yes: ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if strings.ToLower(strings.TrimSpace(line)) != "yes" {
			fmt.Fprintln(out, "\nNothing was deleted.")
			return nil
		}
		opt.DryRun = false
	}
	rep, err := forget.Forget(ctx, a.Pool, who, opt)
	if err != nil {
		return err
	}
	fmt.Fprint(out, rep.Summary())
	if len(rep.RemoteLeft) > 0 {
		return fmt.Errorf("erased here, but %d copies remain at the source (listed above)", len(rep.RemoteLeft))
	}
	return nil
}

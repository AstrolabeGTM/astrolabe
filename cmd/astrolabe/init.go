package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AstrolabeGTM/astrolabe/internal/starter"
)

func initCmd(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	o := starter.Options{}
	fs.StringVar(&o.Template, "template", "", "demo, devtool, saas or app")
	fs.StringVar(&o.Name, "name", "", "product name")
	fs.StringVar(&o.ID, "id", "", "folder name (default: from the name)")
	fs.StringVar(&o.URL, "url", "", "product website (https://...)")
	fs.StringVar(&o.OneLiner, "one-liner", "", "what it does, in one sentence")
	fs.StringVar(&o.SenderCold, "sender-cold", "", "mailbox for cold email")
	fs.StringVar(&o.SenderUsers, "sender-users", "", "mailbox for email to users")
	// Allow "init [dir] -flags" as well as "init -flags [dir]".
	o.Dir = "."
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		o.Dir, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		o.Dir = fs.Arg(0)
	}

	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		interactive = true
	}
	r := bufio.NewReader(in)
	ask := func(q, def string) string {
		if !interactive {
			return def
		}
		fmt.Fprintf(out, "%s [%s]: ", q, def)
		line, _ := r.ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
		return def
	}
	if o.Template == "" {
		if interactive {
			fmt.Fprintln(out, "Templates:")
			for _, t := range starter.TemplateNames {
				fmt.Fprintf(out, "  %-8s %s\n", t, starter.Templates[t])
			}
		}
		o.Template = ask("Template", "demo")
	}
	if o.Template != "demo" && o.Name == "" && o.ID == "" {
		o.Name = ask("Product name", "")
		if o.Name == "" {
			return fmt.Errorf("give the product a name: astrolabe init -template %s -name \"My Product\"", o.Template)
		}
		if o.URL == "" {
			o.URL = ask("Website", "")
		}
	}
	files, err := starter.Init(o)
	for _, f := range files {
		fmt.Fprintln(out, "  created", f)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	if o.Template == "demo" {
		fmt.Fprintln(out, `Next: docker compose up -d, open http://localhost:8080 and sign in with ASTROLABE_PASSWORD from .env.
The demo runs in sandbox mode: nothing is really sent; try People → Start sequence, approve in the Inbox,
then reply from the Outbox.`)
	} else {
		fmt.Fprintf(out, `Next:
  1. Write products/%[1]s/claims.md and voice.md (the product won't load while they start with STUB:).
  2. Edit monitors.yaml and automations.yaml; your editor autocompletes them from schemas/.
  3. astrolabe doctor   (checks what's missing)
`, idFrom(files))
	}
	return nil
}

func idFrom(files []string) string {
	for _, f := range files {
		parts := strings.Split(filepath.ToSlash(f), "/")
		if len(parts) > 2 && parts[0] == "products" {
			return parts[1]
		}
	}
	return "<id>"
}

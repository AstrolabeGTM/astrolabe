// Command astrolabe is the whole system: web app, job workers, MCP server
// and admin commands in one binary.
package main

import (
	_ "time/tzdata" // TZ works in minimal containers

	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/astrolabe-gtm/astrolabe/internal/app"
	"github.com/astrolabe-gtm/astrolabe/internal/doctor"
	"github.com/astrolabe-gtm/astrolabe/internal/hooks"
	"github.com/astrolabe-gtm/astrolabe/internal/mcp"
	"github.com/astrolabe-gtm/astrolabe/internal/people"
	"github.com/astrolabe-gtm/astrolabe/internal/product"
	"github.com/astrolabe-gtm/astrolabe/internal/sandbox"
	"github.com/astrolabe-gtm/astrolabe/internal/score"
	"github.com/astrolabe-gtm/astrolabe/internal/store"
	"github.com/astrolabe-gtm/astrolabe/internal/web"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usage = `astrolabe — go-to-market for one operator, many products

Commands:
  init [dir] [-template demo|devtool|saas|app] [-name ...]
                                  create a workspace: product folder, schemas, compose.yaml, .env
  serve                           web app + workers (sending, reply polling)
  migrate                         create or update the database schema
  product validate [dir...]       check product folders (default: all in $ASTROLABE_PRODUCTS)
  import -product <id> <file.csv> import people (columns: email, github, name, first_name,
                                  title, company, company_domain, notes)
  monitor run <product> <monitor> run one monitor now
  score [product]                 recompute scores
  mode [set sandbox|live]         show or deliberately switch this database's mode
  schema [dir]                    write JSON schemas for the YAML files (editor autocomplete)
  doctor [-connect]               check the setup and say what to fix (-connect also tests logins, keys, DNS)
  mailbox add|list|test|remove    set up a sending mailbox with SMTP/IMAP and an app password (simplest)
  gmail auth <address>            or: authorise a Gmail/Workspace mailbox with OAuth
  mcp                             MCP server on stdio, for Claude Desktop / Claude Code

Environment:
  ASTROLABE_DATABASE_URL          Postgres URL (default postgres:///astrolabe?host=/tmp)
  ASTROLABE_PRODUCTS              products folder (default ./products)
  ASTROLABE_ADDR                  listen address (default 127.0.0.1:8080)
  ASTROLABE_PASSWORD              web app password (required for serve)
  ASTROLABE_SECRETS_DIR           mailbox tokens (default ~/.config/astrolabe)
  ASTROLABE_GOOGLE_CLIENT_ID      Google OAuth client (Desktop app) for Gmail
  ASTROLABE_GOOGLE_CLIENT_SECRET
  ASTROLABE_GITHUB_TOKEN          GitHub token for monitors/enrichment (5,000 req/h instead of 60)
  ASTROLABE_STACKEXCHANGE_KEY     optional Stack Exchange key
  ASTROLABE_EVENTS_SECRET_<PRODUCT> signing secret for POST /hooks/events/<product>
  ASTROLABE_STRIPE_SECRET_<PRODUCT> Stripe endpoint secret (whsec_…) for POST /hooks/stripe/<product>
  ASTROLABE_MODE                  sandbox (local outbox, no accounts needed) or live (default)
  ASTROLABE_PUBLIC_URL            public https URL of this server (tracked links /l/…, webhooks)
  ASTROLABE_RESEND_API_KEY        Resend key for lifecycle email (optional)
  ASTROLABE_WHATSAPP_TOKEN / _APP_SECRET / _VERIFY_TOKEN / _API_VERSION   WhatsApp Cloud API
  ASTROLABE_NOTIFY_SECRET_<PRODUCT> signs push/in-app messages sent to the product's notify.url
  ASTROLABE_DIGEST_TO / _FROM     weekly digest recipient and sender mailbox
  ASTROLABE_ANTHROPIC_API_KEY     Claude API key for briefs, drafts and reply sorting (optional)
  ASTROLABE_MODEL_WRITER / _FAST  models (default claude-sonnet-5 / claude-haiku-4-5-20251001)
  ASTROLABE_LLM_PRICES            e.g. "claude-sonnet-5=3:15" (USD per M input:output tokens) for spend
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	cfg := app.ConfigFromEnv()
	if len(args) == 0 {
		fmt.Fprint(out, usage)
		return nil
	}
	switch args[0] {
	case "serve":
		return serve(ctx, cfg)
	case "migrate":
		pool, err := store.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer pool.Close()
		applied, err := store.Migrate(ctx, pool)
		if err != nil {
			return err
		}
		if len(applied) == 0 {
			fmt.Fprintln(out, "Database is up to date.")
		}
		for _, m := range applied {
			fmt.Fprintln(out, "applied", m)
		}
		return nil
	case "product":
		if len(args) < 2 || args[1] != "validate" {
			return errors.New("usage: astrolabe product validate [dir...]")
		}
		return validate(cfg, args[2:], out)
	case "import":
		return importCSV(ctx, cfg, args[1:], out)
	case "gmail":
		if len(args) != 3 || args[1] != "auth" {
			return errors.New("usage: astrolabe gmail auth <address>")
		}
		a, err := app.Open(ctx, cfg, false)
		if err != nil {
			return err
		}
		defer a.Close()
		if cfg.GoogleID == "" || cfg.GoogleSecret == "" {
			return errors.New("set ASTROLABE_GOOGLE_CLIENT_ID and ASTROLABE_GOOGLE_CLIENT_SECRET (a Google Cloud OAuth client of type Desktop app)")
		}
		err = a.Accounts.Authorize(ctx, args[2], func(url string) {
			fmt.Fprintf(out, "Sign in as %s here:\n\n  %s\n\nWaiting for Google…\n", args[2], url)
		})
		if err == nil {
			fmt.Fprintf(out, "Authorised %s.\n", args[2])
		}
		return err
	case "mcp":
		a, err := app.Open(ctx, cfg, false)
		if err != nil {
			return err
		}
		defer a.Close()
		if err := a.CheckMigrated(ctx); err != nil {
			return err
		}
		products := loadProducts(ctx, a)
		srv := &mcp.Server{S: a.Outreach, Signals: a.Signals, Sources: a.Sources, Writer: a.Writer, Studio: a.Studio, ProductsDir: cfg.ProductsDir, Mode: a.Cfg.Mode,
			Products: func() []*product.Product { return *products.Load() },
			Reload: func(ctx context.Context) error {
				list, err := a.LoadProducts(ctx)
				products.Store(&list)
				return err
			}}
		return srv.Run(ctx, version)
	case "monitor":
		if len(args) != 4 || args[1] != "run" {
			return errors.New("usage: astrolabe monitor run <product> <monitor>")
		}
		a, err := app.Open(ctx, cfg, false)
		if err != nil {
			return err
		}
		defer a.Close()
		if _, err := a.LoadProducts(ctx); err != nil {
			slog.Warn("some products were not loaded", "err", err)
		}
		n, err := a.Signals.RunMonitor(ctx, a.Sources, args[2], args[3], time.Now())
		fmt.Fprintf(out, "%d new signals\n", n)
		return err
	case "score":
		a, err := app.Open(ctx, cfg, false)
		if err != nil {
			return err
		}
		defer a.Close()
		product := ""
		if len(args) > 1 {
			product = args[1]
		}
		n, err := score.RecomputeAll(ctx, a.Pool, product, time.Now())
		fmt.Fprintf(out, "scored %d\n", n)
		return err
	case "doctor":
		fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
		connect := fs.Bool("connect", false, "also test mailbox logins, API keys and DNS (sends nothing)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		checks := doctor.Run(ctx, doctor.Input{DatabaseURL: cfg.DatabaseURL, ProductsDir: cfg.ProductsDir, SecretsDir: cfg.SecretsDir,
			Mode: cfg.Mode, Password: cfg.Password, Connect: *connect})
		if doctor.Print(out, checks) {
			return errors.New("doctor found problems (✗ above)")
		}
		fmt.Fprintln(out, "\nNo blocking problems.")
		return nil
	case "init":
		return initCmd(args[1:], os.Stdin, out)
	case "mailbox":
		return mailboxCmd(ctx, cfg, args[1:], os.Stdin, out)
	case "mode":
		pool, err := store.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer pool.Close()
		if len(args) == 3 && args[1] == "set" {
			if err := sandbox.SetMode(ctx, pool, args[2]); err != nil {
				return err
			}
			fmt.Fprintf(out, "This database is now in %s mode.\n", args[2])
			return nil
		}
		m, err := sandbox.Mode(ctx, pool)
		if m == "" {
			m = "not set yet (fixed on first serve)"
		}
		fmt.Fprintln(out, "mode:", m)
		return err
	case "schema":
		dir := "schemas"
		if len(args) > 1 {
			dir = args[1]
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for name, b := range product.Schemas() {
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
				return err
			}
			fmt.Fprintln(out, "wrote", filepath.Join(dir, name))
		}
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(out, usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

// loadProducts loads valid products and logs problems with the others.
func loadProducts(ctx context.Context, a *app.App) *atomic.Pointer[[]*product.Product] {
	var p atomic.Pointer[[]*product.Product]
	list, err := a.LoadProducts(ctx)
	if err != nil {
		slog.Warn("some products were not loaded", "err", err)
	}
	p.Store(&list)
	return &p
}

func serve(ctx context.Context, cfg app.Config) error {
	var err error
	if cfg.Password, err = adminPassword(cfg); err != nil {
		return err
	}
	// Migrate on start (safe with several servers: it takes a lock).
	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	applied, err := store.Migrate(ctx, pool)
	pool.Close()
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if len(applied) > 0 {
		slog.Info("database migrated", "applied", len(applied))
	}
	a, err := app.Open(ctx, cfg, true)
	if err != nil {
		return err
	}
	defer a.Close()
	products := loadProducts(ctx, a)

	w := &web.Server{S: a.Outreach, Signals: a.Signals, Writer: a.Writer, Accounts: a.Accounts,
		Hooks:  &hooks.Handler{Signals: a.Signals, Out: a.Outreach, Secret: hooks.EnvSecret},
		Studio: a.Studio, Mode: a.Cfg.Mode, Mail: a.Mail, Password: cfg.Password,
		Products: func() []*product.Product { return *products.Load() },
		Reload: func(ctx context.Context) error {
			list, err := a.LoadProducts(ctx)
			products.Store(&list)
			return err
		}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Pool.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	mux.Handle("/", w.Handler())
	httpSrv := &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	if err := a.River.Start(ctx); err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	slog.Info("astrolabe running", "url", "http://"+cfg.Addr, "version", version, "mode", a.Cfg.Mode)

	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutdown)
	// Let an in-flight send record its result before exiting.
	if serr := a.River.Stop(shutdown); serr != nil {
		slog.Error("stopping workers", "err", serr)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func validate(cfg app.Config, dirs []string, out io.Writer) error {
	if len(dirs) == 0 {
		products, err := product.LoadAll(cfg.ProductsDir)
		for _, p := range products {
			fmt.Fprintf(out, "ok  %s (%d sequences)\n", p.ID, len(p.Sequences))
		}
		return err
	}
	var errs []error
	for _, d := range dirs {
		p, err := product.Load(d)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Fprintf(out, "ok  %s (%d sequences)\n", p.ID, len(p.Sequences))
	}
	return errors.Join(errs...)
}

func importCSV(ctx context.Context, cfg app.Config, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	productID := fs.String("product", "", "product id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *productID == "" || fs.NArg() != 1 {
		return errors.New("usage: astrolabe import -product <id> <file.csv>")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	rows, err := people.ReadCSV(f)
	if err != nil {
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
	products, _ := a.LoadProducts(ctx)
	found := false
	for _, p := range products {
		found = found || p.ID == *productID
	}
	if !found {
		return fmt.Errorf("product %q is not loaded (is it valid? run astrolabe product validate)", *productID)
	}
	res, err := people.Import(ctx, a.Pool, *productID, rows)
	fmt.Fprintf(out, "%d new, %d updated\n", res.Created, res.Updated)
	for _, s := range res.Skipped {
		fmt.Fprintln(out, "skipped", s)
	}
	return err
}

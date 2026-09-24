// Package app wires configuration, the database, the job queue and services.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/channel"
	"github.com/AstrolabeGTM/astrolabe/internal/content"
	"github.com/AstrolabeGTM/astrolabe/internal/digest"
	"github.com/AstrolabeGTM/astrolabe/internal/draft"
	"github.com/AstrolabeGTM/astrolabe/internal/github"
	"github.com/AstrolabeGTM/astrolabe/internal/gmail"
	"github.com/AstrolabeGTM/astrolabe/internal/llm"
	"github.com/AstrolabeGTM/astrolabe/internal/mail"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/play"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/sandbox"
	"github.com/AstrolabeGTM/astrolabe/internal/score"
	"github.com/AstrolabeGTM/astrolabe/internal/signal"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

type Config struct {
	DatabaseURL  string
	ProductsDir  string
	Addr         string
	Password     string
	SecretsDir   string
	GoogleID     string
	GoogleSecret string
	GitHubToken  string
	StackKey     string
	// Mode is sandbox (everything goes to a local outbox) or live.
	Mode string
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ConfigFromEnv reads ASTROLABE_* variables (DATABASE_URL is also accepted).
func ConfigFromEnv() Config {
	home, _ := os.UserHomeDir()
	return Config{
		DatabaseURL:  getenv("ASTROLABE_DATABASE_URL", getenv("DATABASE_URL", "postgres:///astrolabe?host=/tmp")),
		ProductsDir:  getenv("ASTROLABE_PRODUCTS", "products"),
		Addr:         getenv("ASTROLABE_ADDR", "127.0.0.1:8080"),
		Password:     os.Getenv("ASTROLABE_PASSWORD"),
		SecretsDir:   getenv("ASTROLABE_SECRETS_DIR", filepath.Join(home, ".config", "astrolabe")),
		GoogleID:     os.Getenv("ASTROLABE_GOOGLE_CLIENT_ID"),
		GoogleSecret: os.Getenv("ASTROLABE_GOOGLE_CLIENT_SECRET"),
		GitHubToken:  os.Getenv("ASTROLABE_GITHUB_TOKEN"),
		StackKey:     os.Getenv("ASTROLABE_STACKEXCHANGE_KEY"),
		Mode:         getenv("ASTROLABE_MODE", "live"),
	}
}

type App struct {
	Cfg      Config
	Pool     *pgxpool.Pool
	River    *river.Client[pgx.Tx]
	Outreach *outreach.Service
	Accounts *gmail.Accounts
	Signals  *signal.Service
	Sources  *signal.Sources
	GitHub   *github.Client
	LLM      *llm.Client
	Writer   *draft.Writer
	Plays    *play.Engine
	Studio   *content.Studio
	Mail     *mail.Store
}

// Open connects and builds services. With work=true the River client also
// runs workers (serve); otherwise it only enqueues (CLI, MCP).
func Open(ctx context.Context, cfg Config, work bool) (*App, error) {
	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	a := &App{Cfg: cfg, Pool: pool}
	if work {
		// serve fixes the database's mode; other commands follow it.
		if err := a.CheckMigrated(ctx); err == nil {
			if err := sandbox.EnsureMode(ctx, pool, cfg.Mode); err != nil {
				pool.Close()
				return nil, err
			}
		}
	} else if m, err := sandbox.Mode(ctx, pool); err == nil && m != "" {
		a.Cfg.Mode = m
	}
	a.Mail = &mail.Store{Dir: filepath.Join(cfg.SecretsDir, "mail")}
	a.Accounts = &gmail.Accounts{Dir: filepath.Join(cfg.SecretsDir, "gmail"), OAuth: gmail.OAuthConfig(cfg.GoogleID, cfg.GoogleSecret)}
	a.Outreach = &outreach.Service{Pool: pool, PublicURL: strings.TrimRight(os.Getenv("ASTROLABE_PUBLIC_URL"), "/")}
	a.Outreach.Senders = a.sender
	a.GitHub = &github.Client{Token: cfg.GitHubToken}
	a.Sources = &signal.Sources{GitHub: a.GitHub, StackKey: cfg.StackKey}
	a.Signals = &signal.Service{Pool: pool}
	a.LLM = llm.FromEnv(pool)
	a.Writer = &draft.Writer{Pool: pool, LLM: a.LLM, Out: a.Outreach}
	a.Plays = &play.Engine{Pool: pool, Out: a.Outreach}
	a.Studio = &content.Studio{Pool: pool, LLM: a.LLM, Out: a.Outreach, PublicURL: a.Outreach.PublicURL}
	a.Outreach.Recheck = a.Plays.Recheck
	// Enrichment can change fit: re-run plays on the person's recent signals.
	a.Signals.AfterEnrich = func(ctx context.Context, tx pgx.Tx, personID int64) error {
		rows, err := tx.Query(ctx, `SELECT id FROM signals WHERE person_id = $1 AND occurred_at > now() - interval '14 days'`, personID)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := a.River.InsertTx(ctx, tx, play.SignalArgs{SignalID: id}, nil); err != nil {
				return err
			}
		}
		return nil
	}
	a.Signals.Hooks = []signal.Hook{a.Signals.ScoreAndEnrich,
		func(ctx context.Context, tx pgx.Tx, s *signal.Signal, r signal.Result) error {
			_, err := a.River.InsertTx(ctx, tx, play.SignalArgs{SignalID: r.SignalID}, nil)
			return err
		}}

	rc := &river.Config{Logger: slog.Default()}
	if work {
		workers := river.NewWorkers()
		river.AddWorker(workers, &outreach.SendWorker{S: a.Outreach})
		river.AddWorker(workers, &outreach.TickWorker{S: a.Outreach})
		if a.Cfg.Mode != sandbox.Sandbox {
			river.AddWorker(workers, &gmail.PollWorker{Accounts: a.Accounts, S: a.Outreach})
			river.AddWorker(workers, &mail.PollWorker{Store: a.Mail, S: a.Outreach})
		}
		river.AddWorker(workers, &signal.MonitorWorker{S: a.Signals, Sources: a.Sources})
		river.AddWorker(workers, &signal.DueWorker{S: a.Signals})
		river.AddWorker(workers, &signal.EnrichWorker{S: a.Signals, GitHub: a.GitHub})
		river.AddWorker(workers, &score.Worker{Pool: pool})
		river.AddWorker(workers, &score.DailyWorker{Pool: pool})
		river.AddWorker(workers, &play.SignalWorker{E: a.Plays})
		river.AddWorker(workers, &play.ScanWorker{E: a.Plays})
		river.AddWorker(workers, &draft.ActionWorker{W: a.Writer})
		river.AddWorker(workers, &draft.ReplyWorker{W: a.Writer})
		river.AddWorker(workers, &draft.TaskWorker{W: a.Writer})
		river.AddWorker(workers, &digest.Worker{Pool: pool, Senders: a.sender,
			Products: func() []*product.Product {
				ps, err := product.All(context.Background(), pool)
				if err != nil {
					slog.Error("digest products", "err", err)
				}
				return ps
			}})
		rc.Workers = workers
		rc.Queues = map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 4},
			// One sender at a time: limits are counted and claimed serially.
			"send": {MaxWorkers: 1},
		}
		rc.PeriodicJobs = []*river.PeriodicJob{
			river.NewPeriodicJob(river.PeriodicInterval(time.Minute),
				func() (river.JobArgs, *river.InsertOpts) { return outreach.TickArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),

			river.NewPeriodicJob(river.PeriodicInterval(5*time.Minute),
				func() (river.JobArgs, *river.InsertOpts) { return signal.DueArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
			river.NewPeriodicJob(river.PeriodicInterval(24*time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return score.DailyArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
			river.NewPeriodicJob(river.PeriodicInterval(time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return play.ScanArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
			river.NewPeriodicJob(river.PeriodicInterval(time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return digest.Args{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
		}
		if a.Cfg.Mode != sandbox.Sandbox {
			rc.PeriodicJobs = append(rc.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(2*time.Minute),
				func() (river.JobArgs, *river.InsertOpts) { return gmail.PollArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
				river.NewPeriodicJob(river.PeriodicInterval(2*time.Minute),
					func() (river.JobArgs, *river.InsertOpts) { return mail.PollArgs{}, nil },
					&river.PeriodicJobOpts{RunOnStart: true}))
		}

	}
	a.River, err = river.NewClient(riverpgxv5.New(pool), rc)
	if err != nil {
		pool.Close()
		return nil, err
	}
	a.Outreach.River = a.River
	a.Signals.River = a.River
	a.Outreach.OnDraftCreated, a.Outreach.OnReply, a.Plays.OnTask = a.Writer.Hooks(a.River)
	return a, nil
}

func (a *App) Close() { a.Pool.Close() }

// sender picks the provider for a message: Gmail for cold email (and
// user email without Resend), Resend for email to users, the WhatsApp
// Cloud API, or the product's own notify endpoint for push/in-app/webhook.
func (a *App) sender(m channel.Message) (channel.Sender, error) {
	if a.Cfg.Mode == sandbox.Sandbox {
		return &sandbox.Sender{Pool: a.Pool}, nil
	}
	switch m.Channel {
	case "email":
		if key := os.Getenv("ASTROLABE_RESEND_API_KEY"); m.Kind == "users" && key != "" {
			return &channel.Resend{APIKey: key}, nil
		}
		// A mailbox set up with SMTP/IMAP (the simple setup), else Gmail OAuth.
		if acct, err := a.Mail.Get(m.From); err != nil {
			return nil, err
		} else if acct != nil {
			return &mail.Sender{Account: acct}, nil
		}
		return a.Accounts.Mailbox(m.From)
	case "whatsapp":
		token := os.Getenv("ASTROLABE_WHATSAPP_TOKEN")
		if token == "" {
			return nil, errors.New("ASTROLABE_WHATSAPP_TOKEN is not set")
		}
		return &channel.WhatsApp{Token: token, PhoneNumber: strings.TrimPrefix(m.From, "whatsapp:"),
			Version: getenv("ASTROLABE_WHATSAPP_API_VERSION", "v23.0")}, nil
	case "push", "in_app", "webhook":
		p, err := product.Get(context.Background(), a.Pool, m.Product)
		if err != nil {
			return nil, err
		}
		secret := os.Getenv("ASTROLABE_NOTIFY_SECRET_" + strings.ToUpper(strings.ReplaceAll(m.Product, "-", "_")))
		if p.Notify == nil || p.Notify.URL == "" || secret == "" {
			return nil, errors.New("set notify.url in product.yaml and ASTROLABE_NOTIFY_SECRET_<PRODUCT>")
		}
		return &channel.Notify{URL: p.Notify.URL, Secret: secret}, nil
	}
	return nil, fmt.Errorf("no sender for channel %q", m.Channel)
}

// LoadProducts validates every product folder and syncs the valid ones.
// Invalid products are reported but do not stop the others.
func (a *App) LoadProducts(ctx context.Context) ([]*product.Product, error) {
	all, loadErr := product.LoadAll(a.Cfg.ProductsDir)
	var products []*product.Product
	for _, p := range all {
		if p.Demo && a.Cfg.Mode != sandbox.Sandbox {
			loadErr = errors.Join(loadErr, fmt.Errorf("%s is demo data and only loads in sandbox mode (ASTROLABE_MODE=sandbox)", p.ID))
			continue
		}
		products = append(products, p)
	}
	if err := product.Sync(ctx, a.Pool, products); err != nil {
		return nil, err
	}
	if err := a.seedDemo(ctx, products); err != nil {
		return nil, err
	}
	if len(products) == 0 && loadErr == nil {
		loadErr = fmt.Errorf("no products found in %s", a.Cfg.ProductsDir)
	}
	return products, loadErr
}

// seedDemo imports a demo product's example people (demo-people.csv) the
// first time it loads in sandbox mode, so the demo has people to work with.
func (a *App) seedDemo(ctx context.Context, products []*product.Product) error {
	if a.Cfg.Mode != sandbox.Sandbox {
		return nil
	}
	for _, p := range products {
		if !p.Demo {
			continue
		}
		f, err := os.Open(filepath.Join(p.Dir, "demo-people.csv"))
		if err != nil {
			continue
		}
		var n int
		a.Pool.QueryRow(ctx, `SELECT count(*) FROM product_people WHERE product_id = $1`, p.ID).Scan(&n)
		if n == 0 {
			rows, err := people.ReadCSV(f)
			if err == nil {
				_, err = people.Import(ctx, a.Pool, p.ID, rows)
			}
			if err == nil {
				_, err = score.RecomputeAll(ctx, a.Pool, p.ID, time.Now())
			}
			if err != nil {
				f.Close()
				return fmt.Errorf("seed %s: %w", p.ID, err)
			}
			slog.Info("demo people imported", "product", p.ID, "count", len(rows))
		}
		f.Close()
	}
	return nil
}

// CheckMigrated fails early with a useful message on a fresh database.
func (a *App) CheckMigrated(ctx context.Context) error {
	var ok bool
	if err := a.Pool.QueryRow(ctx, `SELECT to_regclass('actions') IS NOT NULL`).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("database is not migrated; run `astrolabe migrate`")
	}
	return nil
}

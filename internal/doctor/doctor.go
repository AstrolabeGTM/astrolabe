// Package doctor checks an Astrolabe setup and says exactly what to fix.
// It never sends anything; with Connect it also tests logins and DNS.
package doctor

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/astrolabe-gtm/astrolabe/internal/gmail"
	"github.com/astrolabe-gtm/astrolabe/internal/mail"
	"github.com/astrolabe-gtm/astrolabe/internal/product"
	"github.com/astrolabe-gtm/astrolabe/internal/sandbox"
	"github.com/astrolabe-gtm/astrolabe/internal/store"
)

type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
)

type Check struct {
	Area   string `json:"area"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

type Input struct {
	DatabaseURL string
	ProductsDir string
	SecretsDir  string
	Mode        string // from ASTROLABE_MODE
	Password    string
	Getenv      func(string) string
	Connect     bool // also test logins, APIs and DNS (no sending)
	HTTP        *http.Client
}

// Run performs every check.
func Run(ctx context.Context, in Input) []Check {
	if in.Getenv == nil {
		in.Getenv = os.Getenv
	}
	if in.HTTP == nil {
		in.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	var out []Check
	add := func(area string, s Status, detail, fix string) {
		out = append(out, Check{Area: area, Status: s, Detail: detail, Fix: fix})
	}

	// Database, migrations, mode.
	pool, err := store.Open(ctx, in.DatabaseURL)
	if err != nil {
		add("database", Fail, err.Error(), "Check ASTROLABE_DATABASE_URL and that Postgres is running (docker compose up -d postgres).")
	} else {
		defer pool.Close()
		add("database", OK, "connected", "")
		out = append(out, migrations(ctx, pool)...)
		m, _ := sandbox.Mode(ctx, pool)
		switch {
		case m == "":
			add("mode", OK, fmt.Sprintf("%s (fixed on first start)", in.Mode), "")
		case m != in.Mode:
			add("mode", Fail, fmt.Sprintf("database is in %s mode but ASTROLABE_MODE=%s", m, in.Mode),
				fmt.Sprintf("Use a separate database for %s, or run `astrolabe mode set %s` on purpose.", in.Mode, in.Mode))
		default:
			add("mode", OK, m, "")
		}
	}
	if in.Mode == sandbox.Sandbox {
		add("mode", OK, "sandbox: messages go to the local Outbox; no mailbox or API accounts needed", "")
	}

	// Web password.
	if in.Password == "" {
		if _, err := os.Stat(filepath.Join(in.SecretsDir, "admin-password")); err == nil {
			add("password", OK, "generated password in "+filepath.Join(in.SecretsDir, "admin-password"), "")
		} else {
			add("password", Warn, "ASTROLABE_PASSWORD is not set", "serve generates one on first start and prints it; or set ASTROLABE_PASSWORD in .env.")
		}
	} else if len(in.Password) < 10 {
		add("password", Fail, "ASTROLABE_PASSWORD is shorter than 10 characters", "Use a longer password.")
	} else {
		add("password", OK, "set", "")
	}

	// Products.
	products, loadErr := product.LoadAll(in.ProductsDir)
	if loadErr != nil {
		for _, line := range strings.Split(loadErr.Error(), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				fix := ""
				if strings.Contains(line, "still a stub") {
					fix = "Write the file, then delete its STUB: line."
				}
				add("products", Fail, line, fix)
			}
		}
	}
	if len(products) == 0 && loadErr == nil {
		add("products", Fail, "no products in "+in.ProductsDir, "Run `astrolabe init` (try -template demo) or set ASTROLABE_PRODUCTS.")
	}
	for _, p := range products {
		if p.Demo && in.Mode != sandbox.Sandbox {
			add("products", Fail, p.ID+" is demo data but the mode is live", "Delete the demo folder, or run with ASTROLABE_MODE=sandbox.")
			continue
		}
		add("products", OK, fmt.Sprintf("%s: %d sequences, %d automations, %d monitors", p.ID, len(p.Sequences), len(p.Plays), len(p.Monitors)), "")
	}

	out = append(out, channels(ctx, in, products)...)
	out = append(out, integrations(ctx, in, products)...)
	return out
}

func migrations(ctx context.Context, pool *pgxpool.Pool) []Check {
	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		return []Check{{Area: "migrations", Status: Fail, Detail: "database is not migrated", Fix: "Run `astrolabe migrate` (serve in Docker migrates on start)."}}
	}
	if want := store.MigrationCount(); applied < want {
		return []Check{{Area: "migrations", Status: Fail, Detail: fmt.Sprintf("%d of %d migrations applied", applied, want), Fix: "Run `astrolabe migrate`."}}
	}
	return []Check{{Area: "migrations", Status: OK, Detail: "up to date"}}
}

// channels checks that every way a product sends can actually send.
func channels(ctx context.Context, in Input, products []*product.Product) []Check {
	var out []Check
	add := func(area string, s Status, detail, fix string) {
		out = append(out, Check{Area: area, Status: s, Detail: detail, Fix: fix})
	}
	if in.Mode == sandbox.Sandbox {
		return nil
	}
	store := &mail.Store{Dir: filepath.Join(in.SecretsDir, "mail")}
	oauth := &gmail.Accounts{Dir: filepath.Join(in.SecretsDir, "gmail")}
	gmailBoxes := oauth.Addresses()
	checked := map[string]bool{}
	for _, p := range products {
		uses := map[string]bool{}
		kinds := map[string]bool{}
		for _, s := range p.Sequences {
			kinds[s.Kind] = true
			for _, st := range s.Steps {
				uses[st.Channel] = true
			}
		}
		addrs := []string{}
		if uses["email"] && kinds["cold"] {
			addrs = append(addrs, p.Sender.Cold)
		}
		if uses["email"] && kinds["users"] && in.Getenv("ASTROLABE_RESEND_API_KEY") == "" {
			addrs = append(addrs, p.Sender.Users)
		}
		for _, addr := range addrs {
			addr = strings.ToLower(addr)
			if checked[addr] {
				continue
			}
			checked[addr] = true
			acct, err := store.Get(addr)
			switch {
			case err != nil:
				add("mailbox", Fail, addr+": "+err.Error(), "")
			case acct != nil:
				detail := addr + ": " + acct.Provider + " via SMTP/IMAP"
				if in.Connect {
					cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
					if err := (&mail.Sender{Account: acct}).Test(cctx); err != nil {
						add("mailbox", Fail, addr+": SMTP login failed: "+err.Error(), "Re-run `astrolabe mailbox add "+addr+"` with a new app password.")
					} else if err := mail.TestIMAP(acct); err != nil {
						add("mailbox", Fail, addr+": IMAP login failed: "+err.Error(), "Enable IMAP for the mailbox and check the app password.")
					} else {
						add("mailbox", OK, detail+" (SMTP and IMAP login ok)", "")
					}
					cancel()
				} else {
					add("mailbox", OK, detail, "")
				}
				if in.Connect {
					out = append(out, dnsCheck(addr)...)
				}
			case slices.Contains(gmailBoxes, addr):
				add("mailbox", OK, addr+": Gmail OAuth", "")
				if in.Getenv("ASTROLABE_GOOGLE_CLIENT_ID") == "" {
					add("mailbox", Fail, addr+": Gmail OAuth needs ASTROLABE_GOOGLE_CLIENT_ID/SECRET", "Set them, or switch to `astrolabe mailbox add "+addr+" -provider gmail` with an app password.")
				}
			default:
				add("mailbox", Fail, fmt.Sprintf("%s (%s sender) is not connected", addr, p.ID),
					"Run `astrolabe mailbox add "+addr+"` (app password), or connect it in Settings → Mailboxes.")
			}
		}
		if uses["whatsapp"] {
			for _, v := range []string{"ASTROLABE_WHATSAPP_TOKEN", "ASTROLABE_WHATSAPP_APP_SECRET", "ASTROLABE_WHATSAPP_VERIFY_TOKEN"} {
				if in.Getenv(v) == "" {
					add("whatsapp", Fail, p.ID+" uses WhatsApp but "+v+" is not set", "Set it from your Meta app (WhatsApp → API setup).")
				}
			}
		}
		if uses["push"] || uses["in_app"] || uses["webhook"] {
			env := "ASTROLABE_NOTIFY_SECRET_" + strings.ToUpper(strings.ReplaceAll(p.ID, "-", "_"))
			if in.Getenv(env) == "" {
				add("notify", Fail, p.ID+" sends push/in-app messages but "+env+" is not set", "Set a shared secret; your app verifies it on "+notifyURL(p)+".")
			}
		}
	}
	return out
}

func notifyURL(p *product.Product) string {
	if p.Notify != nil {
		return p.Notify.URL
	}
	return "notify.url"
}

func integrations(ctx context.Context, in Input, products []*product.Product) []Check {
	var out []Check
	add := func(area string, s Status, detail, fix string) {
		out = append(out, Check{Area: area, Status: s, Detail: detail, Fix: fix})
	}
	pub := in.Getenv("ASTROLABE_PUBLIC_URL")
	switch {
	case pub == "":
		add("public url", Warn, "ASTROLABE_PUBLIC_URL is not set: no tracked links, and webhooks can't reach you", "Set it to the https address this server is reachable at.")
	case !strings.HasPrefix(pub, "https://") && !strings.Contains(pub, "localhost") && !strings.Contains(pub, "127.0.0.1"):
		add("public url", Fail, pub+" is not https", "Put the server behind HTTPS (Caddy, a tunnel or your host's TLS).")
	default:
		add("public url", OK, pub, "")
	}
	monitors := 0
	for _, p := range products {
		monitors += len(p.Monitors)
		for _, kind := range []string{"events", "stripe"} {
			if in.Getenv("ASTROLABE_"+strings.ToUpper(kind)+"_SECRET_"+strings.ToUpper(strings.ReplaceAll(p.ID, "-", "_"))) == "" {
				what := "product events (signups, activation)"
				if kind == "stripe" {
					what = "Stripe payments"
				}
				add("webhooks", Warn, fmt.Sprintf("%s: %s webhook is off", p.ID, what),
					fmt.Sprintf("Set ASTROLABE_%s_SECRET_%s to turn it on.", strings.ToUpper(kind), strings.ToUpper(strings.ReplaceAll(p.ID, "-", "_"))))
			}
		}
	}
	token := in.Getenv("ASTROLABE_GITHUB_TOKEN")
	switch {
	case monitors > 0 && token == "":
		add("github", Warn, "no GitHub token: 60 requests/hour, so enrichment falls behind and fit stays 0", "Create a token (no scopes needed) and set ASTROLABE_GITHUB_TOKEN.")
	case token != "" && in.Connect:
		if code, err := get(ctx, in.HTTP, "https://api.github.com/rate_limit", map[string]string{"Authorization": "Bearer " + token}); err != nil || code != 200 {
			add("github", Fail, fmt.Sprintf("GitHub token rejected (%d %v)", code, err), "Create a new token.")
		} else {
			add("github", OK, "token works", "")
		}
	case token != "":
		add("github", OK, "token set", "")
	}
	key := in.Getenv("ASTROLABE_ANTHROPIC_API_KEY")
	if key == "" {
		key = in.Getenv("ANTHROPIC_API_KEY")
	}
	switch {
	case key == "":
		add("claude api", Warn, "no Claude API key: no AI research, drafts or reply sorting (you or Claude over MCP write drafts)", "Set ASTROLABE_ANTHROPIC_API_KEY to turn them on.")
	case in.Connect:
		if code, err := get(ctx, in.HTTP, "https://api.anthropic.com/v1/models", map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}); err != nil || code != 200 {
			add("claude api", Fail, fmt.Sprintf("Claude API key rejected (%d %v)", code, err), "Check the key at console.anthropic.com.")
		} else {
			add("claude api", OK, "key works", "")
		}
	default:
		add("claude api", OK, "key set", "")
	}
	if in.Getenv("ASTROLABE_DIGEST_TO") == "" {
		add("digest", Warn, "weekly digest is not emailed (still on the Digest page)", "Set ASTROLABE_DIGEST_TO and ASTROLABE_DIGEST_FROM.")
	}
	return out
}

func get(ctx context.Context, c *http.Client, url string, headers map[string]string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// dnsCheck warns when a sending domain lacks SPF or DMARC, which sends
// cold mail to spam.
func dnsCheck(addr string) []Check {
	domain := addr[strings.LastIndex(addr, "@")+1:]
	var out []Check
	has := func(name, prefix string) bool {
		txts, err := net.LookupTXT(name)
		if err != nil {
			return false
		}
		for _, t := range txts {
			if strings.HasPrefix(strings.ToLower(t), prefix) {
				return true
			}
		}
		return false
	}
	if !has(domain, "v=spf1") {
		out = append(out, Check{Area: "dns", Status: Warn, Detail: domain + " has no SPF record", Fix: "Add the SPF TXT record your mail provider gives you."})
	}
	if !has("_dmarc."+domain, "v=dmarc1") {
		out = append(out, Check{Area: "dns", Status: Warn, Detail: domain + " has no DMARC record", Fix: `Add a TXT record at _dmarc.` + domain + ` like "v=DMARC1; p=none".`})
	}
	if len(out) == 0 {
		out = append(out, Check{Area: "dns", Status: OK, Detail: domain + " has SPF and DMARC"})
	}
	return out
}

// Print writes checks for a terminal and reports whether any failed.
func Print(w io.Writer, checks []Check) bool {
	failed := false
	for _, c := range checks {
		mark := map[Status]string{OK: "✓", Warn: "!", Fail: "✗"}[c.Status]
		fmt.Fprintf(w, "%s %-11s %s\n", mark, c.Area, c.Detail)
		if c.Fix != "" && c.Status != OK {
			fmt.Fprintf(w, "  %-11s → %s\n", "", c.Fix)
		}
		failed = failed || c.Status == Fail
	}
	return failed
}

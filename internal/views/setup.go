package views

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/AstrolabeGTM/astrolabe/internal/gmail"
	"github.com/AstrolabeGTM/astrolabe/internal/hooks"
	"github.com/AstrolabeGTM/astrolabe/internal/mail"
	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/sandbox"
)

// LoadIssue is a product folder that did not load, and why, one problem
// per line. Folder is empty for problems not tied to one folder.
type LoadIssue struct {
	Folder   string   `json:"folder,omitempty"`
	Problems []string `json:"problems"`
	Fix      string   `json:"fix,omitempty"`
}

// LoadIssues turns the error from loading product folders into one issue
// per folder, as the operator should read it.
func LoadIssues(err error) []LoadIssue {
	out := []LoadIssue{}
	var walk func(error)
	walk = func(err error) {
		if err == nil {
			return
		}
		if j, ok := err.(interface{ Unwrap() []error }); ok {
			for _, e := range j.Unwrap() {
				walk(e)
			}
			return
		}
		var v *product.ValidationError
		if errors.As(err, &v) {
			out = append(out, LoadIssue{Folder: v.Dir, Problems: v.Problems, Fix: fixFor(strings.Join(v.Problems, " "))})
			return
		}
		msg := strings.TrimSpace(err.Error())
		out = append(out, LoadIssue{Problems: []string{msg}, Fix: fixFor(msg)})
	}
	walk(err)
	return out
}

func fixFor(problem string) string {
	switch {
	case strings.Contains(problem, "still a stub") || strings.Contains(problem, "STUB"):
		return "Write the file in your own words, then delete its STUB: line, and reload."
	case strings.Contains(problem, "demo data"):
		return "Delete the demo folder for live use, or run with ASTROLABE_MODE=sandbox."
	case strings.Contains(problem, "no products found"):
		return "Run astrolabe init (try -template demo), or point ASTROLABE_PRODUCTS at your products folder."
	}
	return "Fix the file, then reload products. `astrolabe product validate` shows the same problems."
}

// Mailbox is a sending address a product uses, and whether it is set up.
type Mailbox struct {
	Address string   `json:"address"`
	Uses    []string `json:"uses"` // e.g. "pipewrench cold", "pipewrench users"
	Ready   bool     `json:"ready"`
	// How it is connected: "SMTP/IMAP (gmail)", "Gmail OAuth", or "".
	How  string `json:"how,omitempty"`
	Note string `json:"note,omitempty"`
}

// Pause is one pause switch: everything, a product, or a sequence.
type Pause struct {
	Scope  string `json:"scope"`
	Label  string `json:"label"`
	Paused bool   `json:"paused"`
}

// Webhook is an endpoint a product's app or Stripe posts to.
type Webhook struct {
	Product string `json:"product"`
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Env     string `json:"secret_env"`
	Set     bool   `json:"secret_set"`
}

// Setup is the instance's configuration as the operator checks it.
type Setup struct {
	Mode      string            `json:"mode"`
	Products  []ProductSummary  `json:"products"`
	Folders   map[string]string `json:"folders"`
	Problems  []LoadIssue       `json:"load_problems"`
	Mailboxes []Mailbox         `json:"mailboxes"`
	Pauses    []Pause           `json:"pauses"`
	Webhooks  []Webhook         `json:"webhooks"`
	PublicURL string            `json:"public_url,omitempty"`
}

// GetSetup reports products (and folders that failed to load), sending
// mailboxes, pause switches and webhook endpoints.
func GetSetup(ctx context.Context, out *outreach.Service, mode string, products []*product.Product, problems []LoadIssue,
	store *mail.Store, accounts *gmail.Accounts) (*Setup, error) {
	paused, err := out.Pauses(ctx)
	if err != nil {
		return nil, err
	}
	if problems == nil {
		problems = []LoadIssue{}
	}
	s := &Setup{Mode: mode, Products: Summarize(products), Folders: map[string]string{}, Problems: problems,
		Mailboxes: []Mailbox{}, Pauses: []Pause{}, Webhooks: []Webhook{}, PublicURL: out.PublicURL}
	var authorised []string
	if accounts != nil {
		authorised = accounts.Addresses()
	}
	byAddr := map[string]*Mailbox{}
	var order []string // addresses in the order first used
	pause := func(scope, label string) {
		s.Pauses = append(s.Pauses, Pause{scope, label, slices.Contains(paused, scope)})
	}
	pause("global", "All sending")
	for _, p := range products {
		s.Folders[p.ID] = p.Dir
		for _, use := range []struct{ addr, role string }{{p.Sender.Cold, "cold"}, {p.Sender.Users, "users"}} {
			addr := strings.ToLower(strings.TrimSpace(use.addr))
			if addr == "" {
				continue
			}
			m := byAddr[addr]
			if m == nil {
				m = &Mailbox{Address: addr}
				if store != nil {
					if acct, _ := store.Get(addr); acct != nil {
						m.Ready, m.How = true, "SMTP/IMAP ("+acct.Provider+")"
					}
				}
				if !m.Ready && slices.Contains(authorised, addr) {
					m.Ready, m.How = true, "Gmail OAuth"
				}
				if mode == sandbox.Sandbox {
					m.Note = "Sandbox mode: not used; messages go to the local outbox."
				}
				byAddr[addr] = m
				order = append(order, addr)
			}
			m.Uses = append(m.Uses, p.ID+" "+use.role)
		}
		pause("product:"+p.ID, p.Name)
		seqs := make([]string, 0, len(p.Sequences))
		for id := range p.Sequences {
			seqs = append(seqs, id)
		}
		slices.Sort(seqs)
		for _, id := range seqs {
			pause("sequence:"+p.ID+"/"+id, p.Name+" / "+id)
		}
		for _, kind := range []struct{ name, env, path string }{{"product events", "events", "/hooks/events/"}, {"Stripe", "stripe", "/hooks/stripe/"}} {
			s.Webhooks = append(s.Webhooks, Webhook{Product: p.ID, Kind: kind.name, Path: kind.path + p.ID,
				Env: "ASTROLABE_" + strings.ToUpper(kind.env) + "_SECRET_" + strings.ToUpper(strings.ReplaceAll(p.ID, "-", "_")),
				Set: hooks.EnvSecret(kind.env, p.ID) != ""})
		}
	}
	for _, addr := range order {
		s.Mailboxes = append(s.Mailboxes, *byAddr[addr])
	}
	// Pauses on scopes no longer loaded stay listed so they can be lifted.
	for _, scope := range paused {
		if !slices.ContainsFunc(s.Pauses, func(p Pause) bool { return p.Scope == scope }) {
			s.Pauses = append(s.Pauses, Pause{scope, scope + " (not loaded)", true})
		}
	}
	return s, nil
}

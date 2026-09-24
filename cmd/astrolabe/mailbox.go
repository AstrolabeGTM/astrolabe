package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/app"
	"github.com/AstrolabeGTM/astrolabe/internal/mail"
)

const mailboxUsage = `usage:
  astrolabe mailbox add <address> [-provider gmail|fastmail|icloud|zoho|custom] [-username u]
                        [-smtp host:port] [-smtp-tls starttls|tls] [-imap host:port] [-sent folder] [-no-test]
      The app password is read from ASTROLABE_MAIL_PASSWORD, or from stdin.
  astrolabe mailbox list
  astrolabe mailbox test <address>
  astrolabe mailbox remove <address>`

func mailboxCmd(ctx context.Context, cfg app.Config, args []string, in io.Reader, out io.Writer) error {
	store := &mail.Store{Dir: cfg.SecretsDir + "/mail"}
	if len(args) == 0 {
		return errors.New(mailboxUsage)
	}
	switch args[0] {
	case "list":
		accts, err := store.List()
		for _, a := range accts {
			fmt.Fprintf(out, "%s  %s  smtp %s:%d  imap %s:%d\n", a.Address, a.Provider, a.SMTPHost, a.SMTPPort, a.IMAPHost, a.IMAPPort)
		}
		if len(accts) == 0 {
			fmt.Fprintln(out, "No mailboxes. Add one with: astrolabe mailbox add you@example.com -provider gmail")
		}
		return err
	case "remove":
		if len(args) != 2 {
			return errors.New(mailboxUsage)
		}
		return store.Remove(args[1])
	case "test":
		if len(args) != 2 {
			return errors.New(mailboxUsage)
		}
		a, err := store.Get(args[1])
		if err != nil || a == nil {
			return fmt.Errorf("no mailbox %s", args[1])
		}
		return testMailbox(ctx, a, out)
	case "add":
		fs := flag.NewFlagSet("mailbox add", flag.ContinueOnError)
		provider := fs.String("provider", "", "gmail, fastmail, icloud, zoho or custom")
		username := fs.String("username", "", "login name (default: the address)")
		smtpAddr := fs.String("smtp", "", "SMTP host:port (custom)")
		smtpTLS := fs.String("smtp-tls", "", "starttls (587) or tls (465)")
		imapAddr := fs.String("imap", "", "IMAP host:port (custom)")
		sent := fs.String("sent", "", "Sent folder name")
		noTest := fs.Bool("no-test", false, "save without testing the login")
		if len(args) < 2 {
			return errors.New(mailboxUsage)
		}
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		a := &mail.Account{Address: args[1], Provider: *provider, Username: *username, SMTPTLS: *smtpTLS, SentFolder: *sent}
		if a.Provider == "" {
			a.Provider = guessProvider(a.Address)
		}
		if err := splitHostPort(*smtpAddr, &a.SMTPHost, &a.SMTPPort); err != nil {
			return err
		}
		if err := splitHostPort(*imapAddr, &a.IMAPHost, &a.IMAPPort); err != nil {
			return err
		}
		a.Password = os.Getenv("ASTROLABE_MAIL_PASSWORD")
		if a.Password == "" {
			if p, ok := mail.Presets[a.Provider]; ok && p.Help != "" {
				fmt.Fprintln(out, p.Help)
			}
			fmt.Fprint(out, "App password: ")
			line, _ := bufio.NewReader(in).ReadString('\n')
			a.Password = strings.TrimSpace(line)
		}
		if err := a.Apply(); err != nil {
			return err
		}
		if !*noTest {
			if err := testMailbox(ctx, a, out); err != nil {
				return fmt.Errorf("%w\nNothing was saved. Fix the settings or use -no-test.", err)
			}
		}
		if err := store.Save(a); err != nil {
			return err
		}
		fmt.Fprintf(out, "Saved %s. Products whose sender is this address will send through it.\n", a.Address)
		return nil
	}
	return errors.New(mailboxUsage)
}

func testMailbox(ctx context.Context, a *mail.Account, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := (&mail.Sender{Account: a}).Test(ctx); err != nil {
		return fmt.Errorf("SMTP: %w", err)
	}
	fmt.Fprintln(out, "SMTP login ok")
	if err := mail.TestIMAP(a); err != nil {
		return fmt.Errorf("IMAP: %w", err)
	}
	fmt.Fprintln(out, "IMAP login ok")
	return nil
}

func guessProvider(address string) string {
	d := strings.ToLower(address[strings.LastIndex(address, "@")+1:])
	switch d {
	case "gmail.com", "googlemail.com":
		return "gmail"
	case "fastmail.com", "fastmail.fm":
		return "fastmail"
	case "icloud.com", "me.com", "mac.com":
		return "icloud"
	case "zoho.com", "zohomail.com":
		return "zoho"
	}
	return "custom"
}

func splitHostPort(v string, host *string, port *int) error {
	if v == "" {
		return nil
	}
	h, p, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("%q must be host:port", v)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return fmt.Errorf("%q: bad port", v)
	}
	*host, *port = h, n
	return nil
}

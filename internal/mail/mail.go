// Package mail is the simple email setup: any mailbox with SMTP and IMAP
// and an app password. It sends over SMTP, finds replies and bounces over
// IMAP, and looks up uncertain sends in the Sent folder.
package mail

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Account is one mailbox. It lives in the secrets directory (mode 0600),
// never in the database.
type Account struct {
	Address  string `json:"address"`
	Provider string `json:"provider"`
	Username string `json:"username"`
	Password string `json:"password"` // an app password, not your main password
	// IMAPUsername, if set, is used for IMAP instead of Username (iCloud
	// wants the name part only for IMAP but the full address for SMTP).
	IMAPUsername string `json:"imap_username,omitempty"`

	SMTPHost string `json:"smtp_host"`
	SMTPPort int    `json:"smtp_port"`
	// SMTPTLS is "tls" (implicit TLS, usually 465) or "starttls" (usually 587).
	SMTPTLS  string `json:"smtp_tls"`
	IMAPHost string `json:"imap_host"`
	IMAPPort int    `json:"imap_port"`
	// SentFolder is where sent mail is stored; SavesSent means the provider
	// files SMTP sends there itself (Gmail), so we don't append a copy.
	SentFolder string `json:"sent_folder"`
	SavesSent  bool   `json:"saves_sent"`

	// Insecure disables TLS: only for local tests.
	Insecure bool `json:"insecure,omitempty"`
}

// Preset is a provider's known settings.
type Preset struct {
	Name, Help        string
	SMTPHost, SMTPTLS string
	SMTPPort          int
	IMAPHost          string
	IMAPPort          int
	SentFolder        string
	SavesSent         bool
}

// Presets for common providers. Microsoft (Outlook.com / Microsoft 365)
// is not listed: it is retiring password-based SMTP/IMAP in favour of OAuth.
var Presets = map[string]Preset{
	"gmail": {Name: "Gmail / Google Workspace", SMTPHost: "smtp.gmail.com", SMTPPort: 587, SMTPTLS: "starttls",
		IMAPHost: "imap.gmail.com", IMAPPort: 993, SentFolder: "[Gmail]/Sent Mail", SavesSent: true,
		Help: "Turn on 2-Step Verification, then create an app password at myaccount.google.com/apppasswords. If a Workspace admin has disabled app passwords, use astrolabe gmail auth (OAuth) instead."},
	"fastmail": {Name: "Fastmail", SMTPHost: "smtp.fastmail.com", SMTPPort: 465, SMTPTLS: "tls",
		IMAPHost: "imap.fastmail.com", IMAPPort: 993, SentFolder: "Sent",
		Help: "Create an app password under Settings → Privacy & Security → Integrations."},
	"icloud": {Name: "iCloud Mail", SMTPHost: "smtp.mail.me.com", SMTPPort: 587, SMTPTLS: "starttls",
		IMAPHost: "imap.mail.me.com", IMAPPort: 993, SentFolder: "Sent Messages",
		Help: "Create an app-specific password at account.apple.com."},
	"zoho": {Name: "Zoho Mail", SMTPHost: "smtppro.zoho.com", SMTPPort: 465, SMTPTLS: "tls",
		IMAPHost: "imappro.zoho.com", IMAPPort: 993, SentFolder: "Sent",
		Help: "Enable IMAP access in Zoho Mail settings and use an app-specific password. Custom domains use smtppro/imappro.zoho.com (set automatically; @zohomail.com uses smtp/imap.zoho.com); outside the US data centre use your region's domain (e.g. zoho.eu, zoho.in) with -smtp/-imap."},
	"custom": {Name: "Other (enter SMTP/IMAP settings)", SMTPTLS: "starttls", SMTPPort: 587, IMAPPort: 993, SentFolder: "Sent"},
}

// PresetNames lists presets in display order.
func PresetNames() []string { return []string{"gmail", "fastmail", "icloud", "zoho", "custom"} }

// Apply fills empty connection fields from the provider preset.
func (a *Account) Apply() error {
	p, ok := Presets[a.Provider]
	if !ok {
		return fmt.Errorf("unknown provider %q (have %v)", a.Provider, PresetNames())
	}
	set := func(dst *string, v string) {
		if *dst == "" {
			*dst = v
		}
	}
	seti := func(dst *int, v int) {
		if *dst == 0 {
			*dst = v
		}
	}
	a.Address = strings.ToLower(strings.TrimSpace(a.Address))
	set(&a.Username, a.Address)
	set(&a.SMTPHost, p.SMTPHost)
	seti(&a.SMTPPort, p.SMTPPort)
	set(&a.SMTPTLS, p.SMTPTLS)
	set(&a.IMAPHost, p.IMAPHost)
	seti(&a.IMAPPort, p.IMAPPort)
	set(&a.SentFolder, p.SentFolder)
	domain := a.Address[strings.LastIndex(a.Address, "@")+1:]
	switch a.Provider {
	case "icloud":
		if a.IMAPUsername == "" {
			a.IMAPUsername = strings.SplitN(a.Username, "@", 2)[0] // Apple: name part only for IMAP
		}
	case "zoho":
		if domain == "zohomail.com" || domain == "zoho.com" {
			if a.SMTPHost == p.SMTPHost {
				a.SMTPHost = "smtp.zoho.com"
			}
			if a.IMAPHost == p.IMAPHost {
				a.IMAPHost = "imap.zoho.com"
			}
		}
	}
	if p.SavesSent {
		a.SavesSent = true
	}
	switch {
	case !strings.Contains(a.Address, "@"):
		return errors.New("address must be an email address")
	case a.Password == "":
		return errors.New("password (an app password) is required")
	case a.SMTPHost == "" || a.IMAPHost == "":
		return errors.New("SMTP and IMAP hosts are required for a custom provider")
	case a.SMTPTLS != "tls" && a.SMTPTLS != "starttls":
		return errors.New(`smtp_tls must be "tls" or "starttls"`)
	}
	return nil
}

func (a *Account) smtpAddr() string { return net.JoinHostPort(a.SMTPHost, strconv.Itoa(a.SMTPPort)) }
func (a *Account) imapAddr() string { return net.JoinHostPort(a.IMAPHost, strconv.Itoa(a.IMAPPort)) }

func (a *Account) tlsConfig(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// Store keeps accounts as files in a directory.
type Store struct{ Dir string }

func (s *Store) path(address string) string {
	return filepath.Join(s.Dir, strings.ToLower(address)+".json")
}

func (s *Store) Save(a *Account) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(a.Address) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(a.Address))
}

// Get returns the account for an address, or nil if none is set up.
func (s *Store) Get(address string) (*Account, error) {
	b, err := os.ReadFile(s.path(address))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	a := &Account{}
	return a, json.Unmarshal(b, a)
}

func (s *Store) List() ([]*Account, error) {
	files, _ := filepath.Glob(filepath.Join(s.Dir, "*@*.json"))
	sort.Strings(files)
	var out []*Account
	for _, f := range files {
		a, err := s.Get(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil {
			return nil, err
		}
		if a != nil {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *Store) Remove(address string) error {
	err := os.Remove(s.path(address))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

const dialTimeout = 20 * time.Second

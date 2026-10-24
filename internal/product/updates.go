package product

import (
	"bytes"
	"fmt"
	"net/mail"
	"strings"
	"text/template"
)

// Updates configures the product's updates list (sales terms: newsletter,
// mailing list, nurture list): people who asked, and confirmed, to hear
// from you now and then. Add the section to product.yaml to turn it on.
type Updates struct {
	// Name of the list, as subscribers see it. Default "<product> updates".
	Name string `yaml:"name" json:"name,omitempty"`
	// From is the sender; default sender.users. Replies to it confirm and
	// unsubscribe, so for via: smtp it must be a connected mailbox.
	From string `yaml:"from" json:"from,omitempty"`
	// Via is smtp (the From mailbox, default) or resend.
	Via string `yaml:"via" json:"via,omitempty"`
	// PerDay caps broadcast emails sent in any 24 hours (default 100).
	PerDay int `yaml:"per_day" json:"per_day,omitempty"`
	// PostalAddress goes in every message; commercial email law requires
	// one, and no broadcast is sent without it.
	PostalAddress string `yaml:"postal_address" json:"postal_address,omitempty"`
	// ConfirmWithin is how long an unconfirmed sign-up stays valid (default 7d).
	ConfirmWithin string `yaml:"confirm_within" json:"confirm_within,omitempty"`
	// Texts. Subject and body templates can use {{.List}}, {{.Product}},
	// {{.Email}} and {{.Expires}}. How to confirm and how to unsubscribe are
	// added by Astrolabe and cannot be edited out.
	ConfirmSubject string `yaml:"confirm_subject" json:"confirm_subject,omitempty"`
	ConfirmBody    string `yaml:"confirm_body" json:"confirm_body,omitempty"`
	WelcomeSubject string `yaml:"welcome_subject" json:"welcome_subject,omitempty"`
	WelcomeBody    string `yaml:"welcome_body" json:"welcome_body,omitempty"`
}

var UpdatesVias = []string{"smtp", "resend"}

const (
	DefaultUpdatesPerDay = 100
	DefaultConfirmWithin = "7d"

	defaultConfirmSubject = "Confirm: {{.List}}"
	defaultConfirmBody    = "Someone (hopefully you) asked to get {{.List}} at {{.Email}}.\n\nNothing is sent until you confirm. The request expires in {{.Expires}}."
	defaultWelcomeSubject = "You're on the list: {{.List}}"
	defaultWelcomeBody    = "You're confirmed. You'll get {{.List}} now and then: what shipped and what's next, nothing else."
)

// UpdatesSettings returns the product's updates settings with defaults
// filled in, or false when the product has no updates list.
func (p *Product) UpdatesSettings() (Updates, bool) {
	if p.Updates == nil {
		return Updates{}, false
	}
	u := *p.Updates
	if u.Name == "" {
		u.Name = p.Name + " updates"
	}
	if u.From == "" {
		u.From = p.Sender.Users
	}
	if u.Via == "" {
		u.Via = "smtp"
	}
	if u.PerDay == 0 {
		u.PerDay = DefaultUpdatesPerDay
	}
	if u.ConfirmWithin == "" {
		u.ConfirmWithin = DefaultConfirmWithin
	}
	for dst, def := range map[*string]string{&u.ConfirmSubject: defaultConfirmSubject, &u.ConfirmBody: defaultConfirmBody,
		&u.WelcomeSubject: defaultWelcomeSubject, &u.WelcomeBody: defaultWelcomeBody} {
		if *dst == "" {
			*dst = def
		}
	}
	u.PostalAddress = strings.TrimSpace(u.PostalAddress)
	return u, true
}

// UpdatesTemplateData is what the list's texts can use.
type UpdatesTemplateData struct{ List, Product, Email, Expires string }

func (p *Product) validateUpdates() []string {
	if p.Updates == nil {
		return nil
	}
	var bad []string
	add := func(format string, a ...any) { bad = append(bad, "updates."+fmt.Sprintf(format, a...)) }
	u := p.Updates
	if u.From != "" {
		if a, err := mail.ParseAddress(u.From); err != nil || a.Address != u.From {
			add("from must be a plain email address")
		}
	}
	if u.Via != "" && u.Via != "smtp" && u.Via != "resend" {
		add("via must be one of %v", UpdatesVias)
	}
	if u.PerDay < 0 {
		add("per_day must be at least 1")
	}
	if u.ConfirmWithin != "" {
		if d, err := AfterDuration(u.ConfirmWithin); err != nil {
			add("confirm_within: %v", err)
		} else if d <= 0 {
			add("confirm_within must be more than zero")
		}
	}
	for field, text := range map[string]string{"confirm_subject": u.ConfirmSubject, "confirm_body": u.ConfirmBody,
		"welcome_subject": u.WelcomeSubject, "welcome_body": u.WelcomeBody} {
		if _, err := RenderUpdates(text, UpdatesTemplateData{}); err != nil {
			add("%s template: %v", field, err)
		}
	}
	for _, s := range []string{u.ConfirmSubject, u.WelcomeSubject} {
		if strings.Contains(s, "\n") {
			add("subjects must be one line")
		}
	}
	return bad
}

// RenderUpdates fills one of the list's texts. Unknown fields are errors.
func RenderUpdates(text string, d UpdatesTemplateData) (string, error) {
	t, err := template.New("updates").Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	err = t.Execute(&b, d)
	return b.String(), err
}

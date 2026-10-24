package forms

import (
	"fmt"
	"regexp"
	"strings"
)

// defaults are the field names tried, in order, for each Astrolabe key when
// the monitor's fields map does not name one. Names are compared lower-cased
// with every run of other characters turned into "_".
var defaults = map[string][]string{
	"email":    {"email", "work_email", "e_mail", "email_address"},
	"name":     {"name", "full_name", "your_name"},
	"company":  {"company", "company_name", "organization", "organisation"},
	"domain":   {"domain", "company_domain", "website", "company_website"},
	"role":     {"role", "title", "job_title"},
	"source":   {"source", "how_did_you_hear", "how_did_you_hear_about_us", "heard_about_us", "hear_about_us", "referral_source"},
	"campaign": {"campaign", "utm_campaign"},
	"message":  {"message", "comments", "details", "notes", "use_case"},
}

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

func norm(s string) string {
	return strings.Trim(nonWord.ReplaceAllString(strings.ToLower(s), "_"), "_")
}

// Mapped is a submission read through a field mapping.
type Mapped struct {
	Email, Name, Company, Domain, Role, Source, Campaign, Message string
}

// text turns a submitted value into a trimmed string; lists join with ", ".
func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case []any:
		var parts []string
		for _, e := range x {
			if s := text(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	case float64:
		return fmt.Sprint(x)
	case bool:
		return fmt.Sprint(x)
	}
	return ""
}

// Map reads the Astrolabe keys out of a submission's fields. fields maps an
// Astrolabe key to the form's own field name; keys it does not set use the
// common names above.
func Map(raw map[string]any, fields map[string]string) Mapped {
	byNorm := map[string]any{}
	for k, v := range raw {
		if _, ok := byNorm[norm(k)]; !ok {
			byNorm[norm(k)] = v
		}
	}
	get := func(key string) string {
		names := defaults[key]
		if n := fields[key]; n != "" {
			names = []string{n}
		}
		for _, n := range names {
			if s := text(byNorm[norm(n)]); s != "" {
				return s
			}
		}
		return ""
	}
	m := Mapped{Email: get("email"), Name: get("name"), Company: get("company"), Domain: get("domain"),
		Role: get("role"), Source: get("source"), Campaign: get("campaign"), Message: get("message")}
	if m.Name == "" && fields["name"] == "" {
		m.Name = strings.TrimSpace(text(byNorm["first_name"]) + " " + text(byNorm["last_name"]))
	}
	return m
}

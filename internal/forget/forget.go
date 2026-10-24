// Package forget erases a person on request: everything Astrolabe holds about
// them goes in one transaction, a salted-hash tombstone keeps them from being
// recreated, and copies Netlify holds are deleted through its API first.
//
// Rows are deleted, not blanked. The exceptions (rows scrubbed instead) are
// the ones other numbers depend on, and carry no personal field afterwards:
//
//   - erased_sends: one anonymous row (product, which cap, when) per message to
//     them still inside a rolling 24-hour send cap, so erasing a message cannot
//     free room under limits.cold_per_day or updates.per_day. Pruned after two days.
//   - play_runs inside the 24-hour window of a play's limit_per_day: the row
//     stays with person, signal, enrollment, task and detail cleared.
//
// Free text that other records hold about them (an account's evidence, a
// company note, a weekly digest) is overwritten with a marker wherever it
// contains an address, login or name of theirs; the account and company stay.
//
// What cannot be reached is listed in Report.NotCovered: mailboxes, backups
// and the LLM provider.
package forget

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/forms"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
)

// Marker replaces scrubbed free text.
const Marker = "[removed on request]"

// Options changes how Forget runs.
type Options struct {
	// DryRun counts what would go and changes nothing (the work is done in a
	// transaction that is rolled back; Netlify is not called).
	DryRun bool
	// Netlify deletes submissions at Netlify. Nil means copies there are only
	// listed in Report.RemoteLeft.
	Netlify *forms.Netlify
	Now     func() time.Time
}

// Remote is a copy held outside the database.
type Remote struct {
	Source string `json:"source"` // netlify
	ID     string `json:"id"`     // the submission id at the source
	Form   string `json:"form,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Report says what was (or, in a dry run, would be) done.
type Report struct {
	DryRun   bool   `json:"dry_run"`
	PersonID *int64 `json:"person_id,omitempty"` // nil: no person row; leftovers were still swept
	// Deleted counts rows removed, per table.
	Deleted map[string]int `json:"deleted"`
	// Scrubbed counts rows kept with their personal fields cleared.
	Scrubbed map[string]int `json:"scrubbed,omitempty"`
	// Tombstones is how many identity hashes were stored.
	Tombstones int `json:"tombstones"`
	// RemoteDeleted are copies deleted at the source; RemoteLeft are copies
	// that still exist and must be deleted by hand; RemotePlanned are the
	// copies a real run would delete (dry run only).
	RemoteDeleted []Remote `json:"remote_deleted,omitempty"`
	RemoteLeft    []Remote `json:"remote_left,omitempty"`
	RemotePlanned []Remote `json:"remote_planned,omitempty"`
	// Review lists saved summaries that may still quote them.
	Review []string `json:"review,omitempty"`
	// NotCovered lists places Astrolabe cannot reach.
	NotCovered []string `json:"not_covered"`
}

// Total is the number of rows deleted.
func (r *Report) Total() int {
	n := 0
	for _, c := range r.Deleted {
		n += c
	}
	return n
}

var notCovered = []string{
	"Messages in your mailbox (sent and received): delete them there.",
	"Database backups taken before now: they hold the old data until they expire; the tombstone is not in them.",
	"The LLM provider's own copy of any prompt that named them (Astrolabe deleted its logged copy).",
}

// who is what the operator typed.
type who struct {
	personID int64
	email    string
}

func parse(s string) (who, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return who{}, errors.New("give an email address or a person id")
	}
	if strings.Contains(s, "@") {
		e := people.NormalizeEmail(s)
		if e == "" {
			return who{}, fmt.Errorf("%q is not an email address", s)
		}
		return who{email: e}, nil
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return who{}, fmt.Errorf("%q is neither an email address nor a person id", s)
	}
	return who{personID: id}, nil
}

// target is everything that identifies the person, read from the database.
type target struct {
	personID int64
	idents   [][2]string // identity (kind, value) pairs, including the typed address
	emails   []string
	phones   []string
	terms    []string // lower-case strings to look for in free text
}

func (t *target) addIdent(kind, v string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return
	}
	if kind == "email" || kind == "github" {
		v = strings.ToLower(v)
	}
	p := [2]string{kind, v}
	if !slices.Contains(t.idents, p) {
		t.idents = append(t.idents, p)
	}
	switch kind {
	case "email":
		if !slices.Contains(t.emails, v) {
			t.emails = append(t.emails, v)
		}
	case "phone":
		t.phones = append(t.phones, v)
	}
}

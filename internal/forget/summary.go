package forget

import (
	"fmt"
	"sort"
	"strings"
)

// Summary is the report as plain text, for the command line.
func (r *Report) Summary() string {
	var b strings.Builder
	verb := "Deleted"
	if r.DryRun {
		verb = "Would delete"
	}
	switch {
	case r.PersonID != nil:
		fmt.Fprintf(&b, "Person %d.\n", *r.PersonID)
	default:
		b.WriteString("No person row for this address; leftover copies were swept.\n")
	}
	fmt.Fprintf(&b, "%s %d rows:\n", verb, r.Total())
	for _, k := range sortedKeys(r.Deleted) {
		fmt.Fprintf(&b, "  %-22s %d\n", k, r.Deleted[k])
	}
	if len(r.Scrubbed) > 0 {
		b.WriteString("Kept with personal fields cleared:\n")
		for _, k := range sortedKeys(r.Scrubbed) {
			fmt.Fprintf(&b, "  %-22s %d\n", k, r.Scrubbed[k])
		}
	}
	if !r.DryRun {
		fmt.Fprintf(&b, "Tombstones stored: %d (they are never contacted or recreated)\n", r.Tombstones)
	}
	list := func(title string, rs []Remote) {
		if len(rs) == 0 {
			return
		}
		b.WriteString(title + "\n")
		for _, x := range rs {
			fmt.Fprintf(&b, "  %s submission %s (form %s)", x.Source, x.ID, x.Form)
			if x.Error != "" {
				fmt.Fprintf(&b, ": %s", x.Error)
			}
			b.WriteString("\n")
		}
	}
	list("Would delete at the source:", r.RemotePlanned)
	list("Deleted at the source:", r.RemoteDeleted)
	list("STILL AT THE SOURCE, delete these by hand:", r.RemoteLeft)
	if len(r.Review) > 0 {
		b.WriteString("Review:\n")
		for _, x := range r.Review {
			b.WriteString("  " + x + "\n")
		}
	}
	b.WriteString("Not covered by Astrolabe:\n")
	for _, x := range r.NotCovered {
		b.WriteString("  " + x + "\n")
	}
	return b.String()
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

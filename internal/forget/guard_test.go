package forget_test

import (
	"context"
	"slices"
	"sort"
	"testing"

	"github.com/AstrolabeGTM/astrolabe/internal/forget"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// TestEveryPersonColumnIsHandled fails when a table refers to a person
// (a foreign key to people(id), or a column named person_id) that Forget does
// not handle. Add the table to Forget (run.go) and to the registry (handled.go).
func TestEveryPersonColumnIsHandled(t *testing.T) {
	pool := store.TestDB(t)
	rows, err := pool.Query(context.Background(), `
		SELECT DISTINCT table_name || '.' || column_name FROM (
			SELECT kcu.table_name, kcu.column_name
			FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage kcu
				ON kcu.constraint_name = tc.constraint_name AND kcu.constraint_schema = tc.constraint_schema
			JOIN information_schema.constraint_column_usage ccu
				ON ccu.constraint_name = tc.constraint_name AND ccu.constraint_schema = tc.constraint_schema
			WHERE tc.constraint_type = 'FOREIGN KEY' AND tc.table_schema = 'public'
				AND ccu.table_name = 'people' AND ccu.column_name = 'id'
			UNION
			SELECT table_name, column_name FROM information_schema.columns
			WHERE table_schema = 'public' AND column_name = 'person_id'
		) x ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var c string
		rows.Scan(&c)
		found = append(found, c)
	}
	for _, c := range found {
		if _, ok := forget.Handled[c]; !ok {
			t.Errorf("%s refers to a person but forget.Forget does not handle it: erase it in internal/forget/run.go and list it in handled.go", c)
		}
	}
	var stale []string
	for c := range forget.Handled {
		if !slices.Contains(found, c) {
			stale = append(stale, c)
		}
	}
	sort.Strings(stale)
	for _, c := range stale {
		t.Errorf("forget.Handled lists %s, which is not a person column in the schema any more", c)
	}
}

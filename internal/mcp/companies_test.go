package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/AstrolabeGTM/astrolabe/internal/outreach"
	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/score"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

func TestImportCompaniesFacts(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	p.Fit = []product.FitRule{
		{Any: []string{"fact:agent_moves_money"}, Weight: 40, Label: "agent moves money"},
		{Any: []string{"fact:sells_to_enterprise"}, Weight: 25, Label: "sells to enterprise"},
		{Any: []string{"fact:consumer_app"}, Exclude: true, Label: "consumer app"},
	}
	if err := product.Sync(ctx, pool, []*product.Product{p}); err != nil {
		t.Fatal(err)
	}
	rc, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	s := &Server{S: &outreach.Service{Pool: pool, River: rc}, Products: func() []*product.Product { return []*product.Product{p} }}
	st, ct := sdk.NewInMemoryTransports()
	if _, err := s.Build("test").Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args any) map[string]any {
		t.Helper()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := res.Content[0].(*sdk.TextContent).Text
		if res.IsError {
			return map[string]any{"error": text}
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("%s: %s", name, text)
		}
		return out
	}

	csv := "Company,Domain,Facts,Notes,Funding\n" +
		"Pipewrench,https://www.Pipewrench.example/,fact:agent_moves_money;fact:sells_to_enterprise,met at a meetup,seed\n" +
		"Sprocket,sprocket.example,,,\n" +
		"Bad,not a domain,,,\n" +
		"Candy,candy.example,fact:consumer_app,,\n" +
		"Oops,oops.example,badfact,,\n"
	out := call("import_companies", map[string]any{"product": "demo", "csv": csv})
	if out["created"].(float64) != 3 || len(out["skipped"].([]any)) != 2 {
		t.Fatalf("import: %v", out)
	}
	pw := out["companies"].([]any)[0].(map[string]any)
	if pw["domain"] != "pipewrench.example" || pw["fit"].(float64) != 65 || pw["new_facts"].(float64) != 2 || pw["notes"] != "met at a meetup" {
		t.Fatalf("pipewrench: %v", pw)
	}
	if pw["facts"].(map[string]any)["fact:agent_moves_money"] != "import" {
		t.Fatalf("facts: %v", pw["facts"])
	}

	// Re-import: nothing duplicated, nothing new.
	out = call("import_companies", map[string]any{"product": "demo", "csv": csv})
	pw = out["companies"].([]any)[0].(map[string]any)
	if out["created"].(float64) != 0 || out["updated"].(float64) != 3 || pw["new_facts"].(float64) != 0 || pw["fit"].(float64) != 65 {
		t.Fatalf("re-import: %v", out)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM companies`).Scan(&n)
	if n != 3 {
		t.Fatalf("%d companies", n)
	}

	// Hand-set facts by domain; an excluded company stays out.
	set := call("set_company_facts", map[string]any{"company": "sprocket.example", "facts": []string{"Fact:Agent_Moves_Money"}, "product": "demo"})
	sp := set["company"].(map[string]any)
	if sp["fit"].(float64) != 40 || sp["facts"].(map[string]any)["fact:agent_moves_money"] != "manual" {
		t.Fatalf("set_company_facts: %v", set)
	}
	if e := call("set_company_facts", map[string]any{"company": "sprocket.example", "facts": []string{"nocolon"}}); e["error"] == nil {
		t.Fatal("bad fact accepted")
	}
	if e := call("set_company_facts", map[string]any{"company": "nope.example", "facts": []string{"a:b"}}); e["error"] == nil {
		t.Fatal("unknown company accepted")
	}

	// A person at the company gets its facts in their fit.
	call("import_people", map[string]any{"product": "demo", "csv": "email,name\nana@pipewrench.example,Ana Ruiz\n"})
	var personID int64
	pool.QueryRow(ctx, `SELECT person_id FROM identities WHERE value = 'ana@pipewrench.example'`).Scan(&personID)
	sc, err := score.Recompute(ctx, pool, "demo", personID, timeNow())
	if err != nil {
		t.Fatal(err)
	}
	if sc.Fit != 65 || !strings.Contains(sc.Summary(), "agent moves money +40") {
		t.Fatalf("person fit: %+v", sc)
	}
	// Excluded rules apply to the company's fit too.
	c, _ := people.FindCompany(ctx, pool, "candy.example")
	if f := score.CompanyFit(p, c.Facts); f.Eligible || f.Excluded != "consumer app" {
		t.Fatalf("%+v", f)
	}
}

func timeNow() time.Time { return time.Now() }

package app_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/app"
	"github.com/AstrolabeGTM/astrolabe/internal/digest"
	"github.com/AstrolabeGTM/astrolabe/internal/pipeline"
	"github.com/AstrolabeGTM/astrolabe/internal/sandbox"
	"github.com/AstrolabeGTM/astrolabe/internal/starter"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// The demo workspace in sandbox mode has a pipeline, and its rules hold:
// the gate refuses unqualified accounts, a stuck rule creates its task
// once, the digest shows counts by stage and overdue next actions, and a
// paused account's revisit task appears on its date.
func TestDemoPipelineInSandbox(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := starter.Init(starter.Options{Dir: dir, Template: "demo"}); err != nil {
		t.Fatal(err)
	}
	pool, url := store.TestDBURL(t)
	if err := sandbox.EnsureMode(ctx, pool, sandbox.Sandbox); err != nil {
		t.Fatal(err)
	}
	a, err := app.Open(ctx, app.Config{DatabaseURL: url, ProductsDir: filepath.Join(dir, "products"), SecretsDir: t.TempDir(), Mode: sandbox.Sandbox}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	products, err := a.LoadProducts(ctx)
	if err != nil || len(products) != 1 {
		t.Fatalf("demo did not load: %v", err)
	}
	if _, err := a.LoadProducts(ctx); err != nil { // loading again seeds nothing twice
		t.Fatal(err)
	}
	p := products[0]
	now := time.Now()

	b, err := pipeline.BuildBoard(ctx, a.Pool, p, now)
	if err != nil {
		t.Fatal(err)
	}
	if b.Paused != 1 || len(b.Open) != 4 || len(b.Stuck) != 1 || b.Stuck[0].Company != "Initech" ||
		len(b.Overdue) != 1 || b.Overdue[0].Company != "Globex" {
		t.Fatalf("demo board: %+v", b)
	}

	// Moving to qualified is refused until the gating checks are yes.
	initech, err := pipeline.Find(ctx, a.Pool, p, "initech.example")
	if err != nil {
		t.Fatal(err)
	}
	err = pipeline.Move(ctx, a.Pool, p, initech, pipeline.MoveOpts{Stage: "qualified", Evidence: "Call with Tom"}, now)
	if err == nil || !strings.Contains(err.Error(), "champion is unknown") || !strings.Contains(err.Error(), "timing is unknown") {
		t.Fatalf("gate: %v", err)
	}

	// The stuck rule (contacted for 14 days) creates its task once; the
	// stage rule fires for the account that just entered qualified.
	for range 2 {
		if err := a.Plays.Scan(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	tasks, err := a.Outreach.Tasks(ctx, true, 50)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tk := range tasks {
		got = append(got, tk.NextAction)
	}
	all := strings.Join(got, "\n")
	if len(got) != 2 || !strings.Contains(all, "No answer for two weeks: try another person, or pause it with a revisit date: Initech in contacted for 18 days") ||
		!strings.Contains(all, "Send a one-page trial plan with two kickoff dates: Globex entered qualified") {
		t.Fatalf("automation tasks:\n%s", all)
	}

	// The digest shows counts by stage and the overdue next action.
	d, err := digest.Build(ctx, a.Pool, products, now)
	if err != nil {
		t.Fatal(err)
	}
	text := d.Text()
	for _, want := range []string{"Pipeline (accounts): identified 1, contacted 1, qualified 1, trial 1, customer 0; paused 1, lost 0",
		"Overdue next actions (1): Globex: Send the one-page trial plan with two kickoff dates", "Stuck accounts (over 14d in one stage, 1): Initech (contacted, 18d)"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest lacks %q:\n%s", want, text)
		}
	}

	// Hooli was paused with a revisit date 21 days out: its task appears then.
	if n, err := pipeline.Revisit(ctx, a.Pool, now.AddDate(0, 0, 20)); err != nil || n != 0 {
		t.Fatalf("revisit early: %d %v", n, err)
	}
	if n, err := pipeline.Revisit(ctx, a.Pool, now.AddDate(0, 0, 21)); err != nil || n != 1 {
		t.Fatalf("revisit on the date: %d %v", n, err)
	}
}

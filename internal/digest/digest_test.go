package digest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AstrolabeGTM/astrolabe/internal/campaign"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

func TestDigestShowsRunningCampaignsWithWarnings(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	p := product.TestProduct(t)
	product.WriteTestCampaign(t, p.Dir)
	p, err := product.Load(p.Dir)
	if err != nil {
		t.Fatal(err)
	}
	product.Sync(ctx, pool, []*product.Product{p})
	now := time.Date(2026, 12, 3, 9, 0, 0, 0, time.UTC)

	// Before the campaign starts there is no campaign section.
	early := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	d, err := Build(ctx, pool, []*product.Product{p}, early)
	if err != nil || strings.Contains(d.Text(), "Campaign ") {
		t.Fatalf("%v\n%s", err, d.Text())
	}

	if _, err := campaign.RecordSpend(ctx, pool, p, p.Campaigns[0], "me", campaign.SpendInput{Date: "2026-12-02", Amount: 17000, Platform: "linkedin"}, now); err != nil {
		t.Fatal(err)
	}
	d, err = Build(ctx, pool, []*product.Product{p}, now)
	if err != nil {
		t.Fatal(err)
	}
	text := d.Text()
	for _, want := range []string{"Campaign Launch week (launch): 170.00 of 200.50 USD spent, gate 0 of 2 checked, until 2026-12-08", "WARNING: Near the cap", "before the gate"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest lacks %q:\n%s", want, text)
		}
	}
	// The product read back from the database (as the weekly job does) has campaigns too.
	all, _ := product.All(ctx, pool)
	if d, _ := Build(ctx, pool, all, now); !strings.Contains(d.Text(), "Campaign Launch week") {
		t.Error("campaigns must survive the database round trip")
	}
}

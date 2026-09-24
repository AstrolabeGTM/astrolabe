package people_test

import (
	"context"
	"strings"
	"testing"

	"github.com/AstrolabeGTM/astrolabe/internal/people"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

func TestImportMatchesDeterministically(t *testing.T) {
	ctx := context.Background()
	pool := store.TestDB(t)
	product.Sync(ctx, pool, []*product.Product{product.TestProduct(t)})

	rows, err := people.ReadCSV(strings.NewReader(
		"Email,Name,GitHub,Company\n" +
			"Ana@AcmePay.example,Ana Ruiz,,AcmePay\n" +
			"bo@gmail.com,Bo,https://github.com/bo-dev,\n" +
			"ana@acmepay.example,,anaruiz,\n" + // same person: adds GitHub
			"new@x.example,,bo-dev,\n" + // email new, GitHub belongs to Bo: new email joins Bo
			"ana@acmepay.example,,bo-dev,\n")) // Ana's email + Bo's GitHub: refuse to guess
	if err != nil {
		t.Fatal(err)
	}
	res, err := people.Import(ctx, pool, "demo", rows)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 2 || res.Updated != 2 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "different people") {
		t.Fatalf("%+v", res)
	}
	ana, _ := people.FindByEmail(ctx, pool, "ana@acmepay.example")
	if ana.GitHub != "anaruiz" || ana.Domain != "acmepay.example" || ana.FirstName != "Ana" {
		t.Fatalf("ana: %+v", ana)
	}
	bo, _ := people.FindByEmail(ctx, pool, "new@x.example")
	// gmail.com never becomes a company; the later work email fills it in.
	if bo.Name != "Bo" || bo.Domain != "x.example" {
		t.Fatalf("bo: %+v", bo)
	}
	var gmailCompanies int
	pool.QueryRow(ctx, `SELECT count(*) FROM companies WHERE domain = 'gmail.com'`).Scan(&gmailCompanies)
	if gmailCompanies != 0 {
		t.Fatal("gmail.com became a company")
	}
}

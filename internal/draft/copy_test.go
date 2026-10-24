package draft

import (
	"strings"
	"testing"

	"github.com/AstrolabeGTM/astrolabe/internal/product"
)

func TestCheckCopy(t *testing.T) {
	p := product.TestProduct(t) // claims.md: "It does one thing"; never: guaranteed
	p.ClaimsText = "- Runs in 3 seconds on a laptop"
	has := func(flags []string, want string) bool { return strings.Contains(strings.Join(flags, "|"), want) }

	if f := CheckCopy(p, "Runs in 3 seconds. See https://demo.example/c/2026-launch-7", ""); len(f) != 0 {
		t.Errorf("a claimed number and a URL are fine: %v", f)
	}
	if f := CheckCopy(p, "Saves 40% of CI time", ""); !has(f, "number without a source: 40%") {
		t.Errorf("%v", f)
	}
	if f := CheckCopy(p, "Saves 40% of CI time", "the 2026 study: 40% fewer minutes"); len(f) != 0 {
		t.Errorf("a source vouches for it: %v", f)
	}
	if f := CheckCopy(p, "Guaranteed. Hi [Name]", ""); !has(f, "never-claim") || !has(f, "placeholder") {
		t.Errorf("same checks as email: %v", f)
	}
	if f := CheckCopy(p, "Saves 40% and 40%", ""); strings.Count(strings.Join(f, "|"), "40%") != 1 {
		t.Errorf("one flag per number: %v", f)
	}
}

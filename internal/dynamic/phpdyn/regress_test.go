package phpdyn

import (
	"slices"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

// Regression tests for misses found by review; each must keep the test file at risk.

// A test file that failed in the previous run, named by its sub-unit ID (in always_run), is kept even when its record would
// remove it.
func TestPreviouslyFailedTestFile(t *testing.T) {
	base := with(nil)
	head := with(map[string]string{"src/Price.php": strings.Replace(baseFiles["src/Price.php"], "(string) $x", "(string) ($x + 1)", 1)})
	s := selection("src/Price.php")
	s.AlwaysRun = []string{SubID(unitID, "tests/CartTest.php")}
	out, why := run(t, base, head, records(base), s, change("src/Price.php", model.RoleSource))
	if why != "" {
		t.Fatal(why)
	}
	if got := kept(out); !slices.Equal(got, []string{"tests/CartTest.php", "tests/PriceTest.php"}) {
		t.Fatalf("kept %v", got)
	}
}

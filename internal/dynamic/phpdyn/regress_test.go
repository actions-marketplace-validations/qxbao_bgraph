package phpdyn

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

// Regression tests for misses found by review; each must keep the test file at risk.

// A body change can add a compile-time diagnostic (a deprecation PHP emits when it compiles the file, whether or not the
// function runs): the file must fall back to file level.
func TestCompileDiagnosticsDiffer(t *testing.T) {
	base := "\nNo syntax errors detected in /t/0-base.php\n"
	head := "Deprecated: Using ${var} in strings is deprecated, use {$var} instead in /t/0-head.php on line 19\nNo syntax errors detected in /t/0-head.php\n"
	if d := diagDiff(base, "/t/0-base.php", head, "/t/0-head.php"); !strings.Contains(d, "Deprecated") {
		t.Fatalf("diff %q", d)
	}
	// The same diagnostic on another line (lines moved) is not a difference.
	moved := strings.Replace(head, "line 19", "line 23", 1)
	both := strings.Replace(head, "0-head", "0-base", 2)
	if d := diagDiff(both, "/t/0-base.php", moved, "/t/0-head.php"); d != "" {
		t.Fatalf("moved diagnostic: %q", d)
	}
}

// With PHP on PATH: the real cross-check sends such a change back to file level.
func TestCrossCheckCompileDiagnostics(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php not on PATH")
	}
	base := "<?php\nclass U\n{\n    public static function bar(string $x): string\n    {\n        return \"a\" . $x;\n    }\n}\n"
	head := strings.Replace(base, `"a" . $x`, `"a${x}"`, 1)
	fail := CrossCheck(context.Background(), php, []CheckFile{{Path: "U.php", Base: []byte(base), Head: []byte(head)}})
	if fail["U.php"] == "" {
		t.Skip("this PHP emits no deprecation for ${var} (before 8.2)")
	}
}

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

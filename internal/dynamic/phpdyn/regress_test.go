package phpdyn

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
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

// Shared state outside the repository (docs/dynamic-layer.md §11.4): a test file that writes a cache file outside the
// repository and its own temporary directory has an incomplete record; a test file that read what another one wrote has one
// too; and a file read from a vendor directory that changed after composer install (a cache written there) makes the record
// incomplete.
func TestSharedStateWrites(t *testing.T) {
	root := t.TempDir()
	tr := tree.NewMapTree(map[string]string{"tests/ATest.php": "<?php\n", "tests/BTest.php": "<?php\n", "tests/CTest.php": "<?php\n",
		"src/config.php": "<?php\n"})
	abs := func(p string) string { return filepath.ToSlash(filepath.Join(root, p)) }
	cache := "/var/cache/app/config.php"
	outs := []Outcome{
		{Unit: ".::php#tests/ATest.php", File: "tests/ATest.php", Tests: 1, Included: []string{abs("tests/ATest.php")}, Written: []string{cache}, OutsideRead: []string{cache}},
		{Unit: ".::php#tests/BTest.php", File: "tests/BTest.php", Tests: 1, Included: []string{abs("tests/BTest.php")}, OutsideRead: []string{cache}},
		{Unit: ".::php#tests/CTest.php", File: "tests/CTest.php", Tests: 1, Included: []string{abs("tests/CTest.php")}},
	}
	var recs []dynamic.Record
	for _, o := range outs {
		recs = append(recs, RecordOf(o, FileUnit{Unit: Unit{ID: ".::php"}, File: o.File}, tr, root, "env", "c", time.Unix(0, 0), nil))
	}
	if recs[0].Complete || !recs[1].Complete || !recs[2].Complete {
		t.Fatalf("before taint: %+v", recs)
	}
	recs = SharedStateTaint(outs, recs)
	if recs[1].Complete || !strings.Contains(recs[1].Incomplete, "tests/ATest.php") || !recs[2].Complete {
		t.Fatalf("taint: %+v", recs)
	}
	// A vendor file newer than composer/installed.json.
	for _, p := range []string{"vendor/composer/installed.json", "vendor/acme/lib/Cache.php", "vendor/acme/lib/Old.php"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte("x"), 0o644)
	}
	now := time.Now()
	os.Chtimes(filepath.Join(root, "vendor/composer/installed.json"), now.Add(-time.Hour), now.Add(-time.Hour))
	os.Chtimes(filepath.Join(root, "vendor/acme/lib/Old.php"), now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	o := outs[2]
	o.OutsideRead = []string{abs("vendor/acme/lib/Old.php")}
	if r := RecordOf(o, FileUnit{Unit: Unit{ID: ".::php"}, File: o.File}, tr, root, "env", "c", time.Unix(0, 0), []string{"vendor"}); !r.Complete {
		t.Fatalf("installed vendor file: %s", r.Incomplete)
	}
	o.OutsideRead = append(o.OutsideRead, abs("vendor/acme/lib/Cache.php"))
	if r := RecordOf(o, FileUnit{Unit: Unit{ID: ".::php"}, File: o.File}, tr, root, "env", "c", time.Unix(0, 0), []string{"vendor"}); r.Complete {
		t.Fatal("vendor file written after install accepted")
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

package phpdyn

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestTestCount(t *testing.T) {
	for out, want := range map[string]int{
		"PHPUnit 11\n\nOK (31 tests, 52 assertions)\n":                            31,
		"OK (1 test, 1 assertion)\n":                                              1,
		"FAILURES!\nTests: 7, Assertions: 9, Failures: 1.\n":                      7,
		"OK, but there were issues!\nTests: 487, Assertions: 604, Warnings: 1.\n": 487,
		"No tests executed!\n":                                                    0,
		"  Tests:    1 failed, 7 passed (13 assertions)\n  Duration: 0.1s\n":      8,
		"\x1b[32m  Tests:    \x1b[39m7 warnings (37 assertions)\n":                7,
		"   INFO  No tests found.\n":                                              0,
		"Fatal error: x\n":                                                        -1,
	} {
		if got := testCount(out); got != want {
			t.Errorf("%q: got %d want %d", out, got, want)
		}
	}
}

func TestArgv(t *testing.T) {
	u := Unit{ID: "app::php", Project: "app", Runner: RunnerPest, Config: "app/phpunit.xml", BinDir: "app/vendor/bin",
		RunnerPaths: []string{"app/test"}}
	got, err := Argv(u, "app/-OddTest.php", "php", "/r", "/tmp/c")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"php", filepath.Join("/r", "app/vendor/bin", "pest"), "--configuration", "phpunit.xml", "--test-directory=test",
		"--no-coverage", "--do-not-cache-result", "--cache-directory", "/tmp/c", "--no-logging", "--colors=never", "./-OddTest.php"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v", got)
	}
	if _, err := Argv(Unit{Project: "app", Runner: RunnerPHPUnit, BinDir: "vendor/bin"}, "app/x.php", "php", "/r", "/c"); err == nil {
		t.Fatal("no configuration accepted")
	}
}

func writeOut(t *testing.T, dir string, ps ...procOut) {
	t.Helper()
	for i, p := range ps {
		if p.Collector == "" {
			p.Collector = CollectorID
		}
		b, _ := json.Marshal(p)
		if err := os.WriteFile(filepath.Join(dir, strings.Repeat("x", i+1)+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMergeProcessTree(t *testing.T) {
	root := procOut{Pid: 10, Token: "a", Included: []string{"/r/tests/ATest.php"}, Coverage: map[string]string{"/r/tests/ATest.php": "1-3|4"}}
	child := procOut{Pid: 11, Ppid: 10, Token: "b", Parent: "a"}
	cases := map[string]struct {
		procs []procOut
		bad   string
	}{
		"single process": {procs: []procOut{root}},
		"runner child":   {procs: []procOut{withLaunch(root, 1), child}},
		"launch without output": {procs: []procOut{withLaunch(root, 1)},
			bad: "the runner started 1 processes and 0 wrote output"},
		"child through a shell": {procs: []procOut{withLaunch(root, 1), {Pid: 12, Ppid: 99, Token: "b", Parent: "a"}},
			bad: "started through another program"},
		"two roots":       {procs: []procOut{root, {Pid: 13, Token: "c"}}, bad: "processes wrote output as the test process"},
		"orphan child":    {procs: []procOut{root, {Pid: 14, Ppid: 1, Token: "d", Parent: "zz"}}, bad: "whose parent wrote no output"},
		"other collector": {procs: []procOut{root, {Collector: "php-xdebug-0", Pid: 15, Token: "e", Parent: "a"}}, bad: "collector version"},
	}
	for name, c := range cases {
		dir := t.TempDir()
		writeOut(t, dir, c.procs...)
		var o Outcome
		merge(&o, dir, 10)
		got := strings.Join(o.Incomplete, "; ")
		switch {
		case c.bad == "" && got != "":
			t.Errorf("%s: incomplete %q", name, got)
		case c.bad != "" && !strings.Contains(got, c.bad):
			t.Errorf("%s: got %q, want %q", name, got, c.bad)
		}
	}
	var o Outcome
	merge(&o, t.TempDir(), 10)
	if o.Err == "" {
		t.Error("no output accepted")
	}
}

func withLaunch(p procOut, n int) procOut {
	for i := 0; i < n; i++ {
		p.Launches = append(p.Launches, launch{Fn: "proc_open", At: "/r/vendor/phpunit/phpunit/src/Util/PHP/JobRunner.php:153"})
	}
	return p
}

func TestRecordOf(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"tests/ATest.php": "<?php\n", "src/A.php": "<?php\n", "src/B.php": "<?php\n", "data/x.json": "{}",
		"data/sub/y.json": "{}"}
	for p, c := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(c), 0o644)
	}
	os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "created.txt"), []byte("x"), 0o644)
	t1 := tree.NewMapTree(files)
	abs := func(p string) string { return filepath.ToSlash(filepath.Join(root, p)) }
	f := FileUnit{Unit: Unit{ID: ".::php"}, File: "tests/ATest.php"}
	o := Outcome{Unit: f.ID(), File: f.File, Exit: 0, Tests: 3,
		Included: []string{abs("tests/ATest.php"), abs("src/A.php"), abs("vendor/x.php")},
		Lines:    map[string]LineSet{abs("src/A.php"): {2: true, 3: false}},
		Paths: map[string]Path{
			abs("data/x.json"): {Kinds: map[string]bool{"read": true}, Pre: true},
			abs("data/sub"):    {Kinds: map[string]bool{"list": true}, Pre: true},
			abs("created.txt"): {Kinds: map[string]bool{"write": true, "read": true}},
			abs("missing.txt"): {Kinds: map[string]bool{"stat": true}},
		}}
	r := RecordOf(o, f, t1, root, "env", "c", time.Unix(0, 0), []string{"vendor"})
	if !r.Complete || !r.Pass {
		t.Fatalf("incomplete: %s", r.Incomplete)
	}
	for _, d := range []string{"tests/ATest.php", "src/A.php", "data/x.json", "data/sub/y.json"} {
		if _, ok := r.Deps[d]; !ok {
			t.Errorf("missing dep %s: %v", d, r.Deps)
		}
	}
	if _, ok := r.Deps["src/B.php"]; ok || len(r.Deps) != 4 {
		t.Errorf("deps %v", r.Deps)
	}
	if strings.Join(r.Reads, ",") != "data/sub/y.json,data/x.json" || r.LineCov["src/A.php"] != "2|3" {
		t.Errorf("reads %v line_cov %v", r.Reads, r.LineCov)
	}
	// Untracked repository file read, missing canary, no tests.
	o.Paths[abs("untracked.txt")] = Path{Kinds: map[string]bool{"read": true}, Pre: true}
	o.Included = o.Included[1:]
	o.Tests = 0
	r = RecordOf(o, f, t1, root, "env", "c", time.Unix(0, 0), []string{"vendor"})
	for _, w := range []string{"reads untracked file untracked.txt", "canary", "no tests ran"} {
		if r.Complete || !strings.Contains(r.Incomplete, w) {
			t.Errorf("want %q in %q", w, r.Incomplete)
		}
	}
	if r.Pass {
		t.Error("zero tests passed")
	}
}

func TestListRecordOf(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"phpunit.xml": "<phpunit><testsuites><testsuite name=\"u\"><directory>tests</directory></testsuite></testsuites></phpunit>",
		"tests/ATest.php": "<?php\n", "tests/BTest.php": "<?php\n", "tests/Helper.php": "<?php\n", "src/A.php": "<?php\n"}
	t1 := tree.NewMapTree(files)
	u := Unit{ID: ".::php", Project: ".", Runner: RunnerPHPUnit, Config: "phpunit.xml", BinDir: "vendor/bin", Vendor: "vendor"}
	abs := func(p string) string { return filepath.ToSlash(filepath.Join(root, p)) }
	lo := ListOutcome{Files: []string{abs("tests/ATest.php"), abs("tests/BTest.php")}, Runner: "phpunit@11.5.0"}
	r := ListRecordOf(lo, u, t1, root, "env", "c", time.Unix(0, 0), []string{"vendor"}, nil)
	if !r.Complete || !r.Pass || len(r.Classes) != 2 || len(r.Deps) != 4 || r.Runner != "phpunit@11.5.0" {
		t.Fatalf("%+v", r)
	}
	for name, l := range map[string]ListOutcome{
		"untracked":    {Files: []string{abs("tests/CTest.php")}, Runner: "phpunit@11.5.0"},
		"outside":      {Files: []string{abs("src/A.php")}, Runner: "phpunit@11.5.0"},
		"error":        {Err: "listing the test files: boom"},
		"empty":        {Runner: "phpunit@11.5.0"},
		"no version":   {Files: []string{abs("tests/ATest.php")}},
		"cannot alone": {Files: []string{abs("tests/ATest.php")}, Runner: "phpunit@11.5.0"},
	} {
		var failed []string
		if name == "cannot alone" {
			failed = []string{"tests/ATest.php"}
		}
		if r := ListRecordOf(l, u, t1, root, "env", "c", time.Unix(0, 0), nil, failed); r.Complete {
			t.Errorf("%s: complete", name)
		}
	}
}

func TestLineTable(t *testing.T) {
	ls, ok := ParseLineTable("3-5,9|6-8")
	if !ok || !ls[4] || ls[7] || len(ls) != 7 {
		t.Fatalf("%v %v", ls, ok)
	}
	if ls.Format() != "3-5,9|6-8" {
		t.Fatalf("format %q", ls.Format())
	}
	for _, bad := range []string{"3-5", "a|", "5-3|", "0|"} {
		if _, ok := ParseLineTable(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	u := LineSet{1: false, 2: true}.Union(LineSet{1: true, 3: false})
	if !u[1] || !u[2] || u[3] {
		t.Fatalf("union %v", u)
	}
}

// The embedded PHP scripts must at least compile with the PHP that records (skipped without php on PATH).
func TestEmbeddedPHPCompiles(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php not on PATH")
	}
	dir := t.TempDir()
	for name, src := range map[string][]byte{"collect.php": collectorSrc, "list.php": listSrc, "spans.php": []byte(spansScript)} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, src, 0o644)
		if out, err := exec.Command(php, "-l", p).CombinedOutput(); err != nil {
			t.Errorf("%s: %v %s", name, err, out)
		}
	}
}

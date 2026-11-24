package evalkit

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/dynamic/phpdyn"
	"github.com/qxbao/bgraph/internal/model"
)

func TestPHPJUnitFilesPHPUnit(t *testing.T) {
	root := filepath.FromSlash("/w/repo")
	x := `<?xml version="1.0"?>
<testsuites>
  <testsuite name="all" tests="5" errors="1" failures="1">
    <testsuite name="A\ATest" file="/w/repo/tests/ATest.php" tests="2" errors="0" failures="1">
      <testcase name="a1" file="/w/repo/tests/ATest.php" line="3"/>
      <testcase name="a2" file="/w/repo/tests/ATest.php" line="9"><failure>boom</failure></testcase>
    </testsuite>
    <testsuite name="B\BTest" file="/w/repo/tests/BTest.php" tests="1" errors="0" failures="0">
      <testsuite name="B\BTest::testData" file="/w/repo/tests/BTest.php" tests="1">
        <testcase name="testData with data set #0" file="/w/repo/tests/BTest.php"/>
      </testsuite>
    </testsuite>
    <testsuite name="C\CTest" file="/w/repo/tests/CTest.php" tests="1" errors="1" failures="0">
    </testsuite>
    <testsuite name="D\DTest" file="/w/repo/tests/DTest.php" tests="1" errors="0" failures="0">
      <testcase name="d1" class="D\DTest"><error>x</error></testcase>
    </testsuite>
  </testsuite>
</testsuites>`
	tests, seen, red, err := PHPJUnitFiles(strings.NewReader(x), root, ".")
	if err != nil {
		t.Fatal(err)
	}
	if tests != 4 {
		t.Errorf("tests = %d, want 4", tests)
	}
	if got, want := sortedKeys(seen), []string{"tests/ATest.php", "tests/BTest.php", "tests/CTest.php", "tests/DTest.php"}; !reflect.DeepEqual(got, want) {
		t.Errorf("seen = %v, want %v", got, want)
	}
	// ATest: a failing case; CTest: errors on the class suite without a case (setUpBeforeClass); DTest: a case without a file
	// attribute inherits its suite's.
	if got, want := sortedKeys(red), []string{"tests/ATest.php", "tests/CTest.php", "tests/DTest.php"}; !reflect.DeepEqual(got, want) {
		t.Errorf("red = %v, want %v", got, want)
	}
}

func TestPHPJUnitFilesPest(t *testing.T) {
	x := `<testsuites><testsuite name="/w/p/phpunit.xml" tests="2" errors="0" failures="1">
  <testsuite name="default" tests="2" failures="1">
    <testsuite name="Tests\Arch" file="tests/Arch.php" tests="1" failures="0">
      <testcase name="base" file="tests/Arch.php::base"/>
    </testsuite>
    <testsuite name="Tests\Layer" file="tests/Layer.php" tests="1" failures="1">
      <testcase name="it works with data set &quot;('a::b')&quot;" file="tests/Layer.php::it works with data set &quot;('a::b')&quot;"><failure/></testcase>
    </testsuite>
  </testsuite></testsuite></testsuites>`
	_, seen, red, err := PHPJUnitFiles(strings.NewReader(x), "/w/p", "pkg")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedKeys(seen), []string{"pkg/tests/Arch.php", "pkg/tests/Layer.php"}; !reflect.DeepEqual(got, want) {
		t.Errorf("seen = %v, want %v", got, want)
	}
	if got, want := sortedKeys(red), []string{"pkg/tests/Layer.php"}; !reflect.DeepEqual(got, want) {
		t.Errorf("red = %v, want %v", got, want)
	}
	if _, _, _, err := PHPJUnitFiles(strings.NewReader(`<html/>`), "/", "."); err == nil {
		t.Error("a non-JUnit root must be an error")
	}
}

func phpSelFixture() model.Selection {
	return model.Selection{
		Scope:   model.TierGraph,
		Changed: []string{"src/A.php"},
		Tests: []model.SelectedTest{
			{Unit: ".::php", Project: ".", Runner: "phpunit", ClassFilter: true, Classes: []string{"tests/ATest.php"}, Dynamic: "PHP layer: 1 of 3 test files run"},
		},
		Narrowed: []model.NarrowedTest{{Unit: ".::php#tests/BTest.php"}, {Unit: ".::php#tests/CTest.php"}},
	}
}

func TestPHPSummarizeAndRuns(t *testing.T) {
	lists := map[string][]string{".::php": {"tests/ATest.php", "tests/BTest.php", "tests/CTest.php"}}
	hy := phpSelFixture()
	d := PHPSummarize(hy, lists, []string{"tests/ATest.php", "tests/CTest.php"})
	if d.Files != 1 || !d.Narrowed || d.Units != 1 {
		t.Errorf("hybrid summary = %+v", d)
	}
	if !reflect.DeepEqual(d.Removed, []string{"tests/BTest.php", "tests/CTest.php"}) {
		t.Errorf("removed = %v", d.Removed)
	}
	if !reflect.DeepEqual(d.Missed, []string{"tests/CTest.php"}) {
		t.Errorf("missed = %v, want the red removed file", d.Missed)
	}
	if d.Notes["narrowed"] != 1 {
		t.Errorf("notes = %v", d.Notes)
	}
	st := hy
	st.Tests = []model.SelectedTest{{Unit: ".::php", Project: ".", Runner: "phpunit"}}
	st.Narrowed = nil
	s := PHPSummarize(st, lists, []string{"tests/CTest.php", "tests/Unlisted.php"})
	if s.Files != 3 || s.Narrowed || len(s.Missed) != 0 {
		t.Errorf("static summary = %+v (a whole unit runs every listed file and covers unlisted ones)", s)
	}
	// A narrowed unit does not run an unlisted test file; another project's unit does not cover it either.
	if PHPRuns(hy, "tests/Unlisted.php") {
		t.Error("a narrowed unit runs only its kept files")
	}
	sub := model.Selection{Tests: []model.SelectedTest{{Unit: "lib::php", Project: "lib", Runner: "pest"}}}
	if PHPRuns(sub, "tests/ATest.php") || !PHPRuns(sub, "lib/tests/ATest.php") {
		t.Error("project enclosure")
	}
	whole := model.Selection{Tests: []model.SelectedTest{{Unit: ".::php", Project: ".", Runner: "composer", Whole: true}}}
	if PHPSummarize(whole, lists, nil).Units != 0 {
		t.Error("a Composer-script unit is not a PHP layer unit")
	}
}

func TestPHPSameTests(t *testing.T) {
	a := phpSelFixture()
	if w := PHPSameTests(a, a); w != "" {
		t.Errorf("same selection: %s", w)
	}
	b := a
	b.Tests = []model.SelectedTest{{Unit: ".::php", Project: ".", Runner: "phpunit"}}
	if PHPSameTests(a, b) == "" {
		t.Error("a filter difference must be reported")
	}
	c := a
	c.Narrowed = nil
	if PHPSameTests(a, c) == "" {
		t.Error("a narrowed-list difference must be reported")
	}
}

func TestPHPLevels(t *testing.T) {
	ch := []phpdyn.Changed{
		{Path: "src/A.php", Kind: phpdyn.KindLines, Members: []phpdyn.Span{{From: 3, To: 9}}},
		{Path: "src/B.php", Kind: phpdyn.KindFile, Note: "signature changed (file level)"},
		{Path: "tests/BTest.php", Kind: phpdyn.KindFile, Test: true, Note: "a test file (file level)"},
		{Path: "README.md", Kind: phpdyn.KindDoc},
	}
	ls := PHPLevels(ch)
	got := []string{}
	for _, l := range ls {
		got = append(got, l.Level)
	}
	if !reflect.DeepEqual(got, []string{"function", "file", "test", "doc"}) || ls[0].Funcs != 1 {
		t.Errorf("levels = %v", ls)
	}
	if PHPCommitLevel(ls) != "mixed" || PHPCommitLevel(ls[:1]) != "function" || PHPCommitLevel(ls[1:2]) != "file" || PHPCommitLevel(ls[2:]) != "none" {
		t.Error("commit level")
	}
	if !phpTestFile("tests/FooTest.php") || !phpTestFile("tests/a.phpt") || phpTestFile("src/Contest.php.dist") {
		t.Error("test role mirror")
	}
}

func TestPHPReasonClass(t *testing.T) {
	for in, want := range map[string]string{
		"src/Foo.php: status A (only modifications of existing files are observable: an addition can add a test)":         "status A",
		".github/workflows/ci.yml: not a PHP code file nor an unreferenced documentation file (data, fixtures)":           "not a PHP code file nor an unreferenced documentation file [.yml]",
		"Makefile: not a PHP code file nor an unreferenced documentation file (data)":                                     "not a PHP code file nor an unreferenced documentation file [Makefile]",
		"widening manifest_changed on composer.json (a PHP manifest, lockfile, runner configuration, or a global change)": "widening manifest_changed on <path>",
		"the selection is full (never narrowed)":                                                                          "the selection is full",
	} {
		if got := PHPReasonClass(in); got != want {
			t.Errorf("PHPReasonClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPHPDynMarkdown(t *testing.T) {
	sel := PHPSummarize(phpSelFixture(), map[string][]string{".::php": {"a", "b", "c"}}, nil)
	t2 := PHPDynT2Markdown([]PHPDynT2Result{{Repo: "r", R: "0123456789ab", Lists: map[string]int{".::php": 3}, Baseline: PHPDynBaseline{RunMs: []int64{1000}},
		Mutants: []PHPDynMutant{{ID: "m1", Oracle: PHPDynOracle{Status: "killed", Red: []string{"tests/ATest.php"}, Ms: 900}, Total: 3,
			Static: PHPDynSel{Files: 3}, Hybrid: sel, FileLevel: 2, Levels: []PHPLevel{{Path: "src/A.php", Level: "function"}}},
			{ID: "m2", Oracle: PHPDynOracle{Status: "survived"}}}}})
	for _, want := range []string{"| r | 3 |", "| 1.000 | 0.333 | 0.667 | 1 | **0** |", "not killed here: survived"} {
		if !strings.Contains(t2, want) {
			t.Errorf("T2 markdown lacks %q:\n%s", want, t2)
		}
	}
	g := PHPGuardMarkdown([]PHPGuardResult{{Repo: "r", Examined: 4, Commits: []PHPGuardCommit{
		{C: "c1", Stage: "accepted", Level: "function"}, {C: "c2", Stage: "refused", Reason: "x.yml: not a PHP code file nor an unreferenced documentation file (data)"},
		{C: "c3", Stage: "inactive", Reason: "scope graph, 0 tests selected, 0 PHP units"}}}})
	if !strings.Contains(g, "| r | 4 | 1 | 0 | 1 | 1 | 0.50 | 0.33 | 1 | 0 | 0 | 0 |") || !strings.Contains(g, "[.yml]") {
		t.Errorf("guard markdown:\n%s", g)
	}
	rp := PHPReplayMarkdown(PHPReplayResult{Repo: "r", Rev: "abc", Commits: []PHPReplayCommit{
		{C: "c1", Stage: "accepted", Level: "file", Total: 3, Static: PHPDynSel{Files: 3}, Hybrid: sel, FailAtC: []string{"tests/BTest.php"},
			Misses: []string{"tests/BTest.php"}, MissDetail: []string{"tests/BTest.php: boom"}},
		{C: "c2", Stage: "refused", Skip: "a.json: not a PHP code file nor an unreferenced documentation file (data)", AssertFail: "scope"}}})
	for _, want := range []string{"**misses 1**", "**1**", "MISS at c1"} {
		if !strings.Contains(rp, want) {
			t.Errorf("replay markdown lacks %q:\n%s", want, rp)
		}
	}
}

package phpdyn

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

const cartPHP = `<?php
namespace App;

class Cart
{
    public function total(): int
    {
        return 1;
    }

    public function count(): int
    {
        return 2;
    }
}
`

const phpunitXMLSrc = `<?xml version="1.0"?>
<phpunit bootstrap="vendor/autoload.php">
  <testsuites>
    <testsuite name="unit">
      <directory>./tests</directory>
    </testsuite>
  </testsuites>
</phpunit>
`

var baseFiles = map[string]string{
	"composer.json":          "{}\n",
	"phpunit.xml.dist":       phpunitXMLSrc,
	"src/Cart.php":           cartPHP,
	"src/Price.php":          "<?php\nnamespace App;\n\nfinal class Price\n{\n    public static function fmt(int $x): string\n    {\n        return (string) $x;\n    }\n}\n",
	"tests/CartTest.php":     "<?php\n// cart\n",
	"tests/CountTest.php":    "<?php\n// count\n",
	"tests/PriceTest.php":    "<?php\n// price\n",
	"tests/fixtures/a.json":  "{}\n",
	"README.md":              "# x\n",
	"tests/Support/Base.php": "<?php\n",
}

var testFiles = []string{"tests/CartTest.php", "tests/CountTest.php", "tests/PriceTest.php"}

func with(edits map[string]string) *tree.MapTree {
	m := map[string]string{}
	for k, v := range baseFiles {
		m[k] = v
	}
	for k, v := range edits {
		if v == "" {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return tree.NewMapTree(m)
}

func change(p string, role model.Role) dynamic.Change {
	return dynamic.Change{Path: p, Status: 'M', OldMode: "100644", NewMode: "100644", BaseRole: role, HeadRole: role}
}

const unitID = ".::php"

func selection(changed ...string) model.Selection {
	return model.Selection{Scope: model.TierGraph, Changed: changed, IndexStats: map[string]int{},
		Tests: []model.SelectedTest{{Unit: unitID, Project: ".", Runner: RunnerPHPUnit, Config: "phpunit.xml.dist",
			BinDir: "vendor/bin", Files: []string{"<php:.::php>"}}}}
}

var unitEco = map[string]string{unitID: Eco}

const env = "env1"

// records: CartTest includes Cart.php and runs total (lines 6-9); CountTest includes Cart.php and runs count (11-14) and reads
// the fixture; PriceTest includes Price.php only.
func records(t tree.Tree) []dynamic.Record {
	u := Unit{ID: unitID, Project: ".", Runner: RunnerPHPUnit, Config: "phpunit.xml.dist", BinDir: "vendor/bin", Vendor: "vendor"}
	list := dynamic.Record{Eco: Eco, Unit: unitID, Collector: CollectorID, Env: env, Complete: true, Pass: true,
		Recorded: time.Unix(1000, 0), Deps: map[string]string{}, Classes: testFiles, Runner: "phpunit@11.5.0"}
	for _, f := range append(RootFiles(t, TestRoots(t, u)), ConfigFiles(t, u)...) {
		list.Deps[f] = t.Key(f)
	}
	mk := func(file string, deps []string, cov map[string]string, reads []string) dynamic.Record {
		r := dynamic.Record{Eco: Eco, Unit: SubID(unitID, file), Collector: CollectorID, Env: env, Complete: true, Pass: true,
			Recorded: time.Unix(1000, 0), Deps: map[string]string{}, LineCov: cov, Reads: reads}
		for _, d := range append(append(deps, reads...), file, "phpunit.xml.dist") {
			r.Deps[d] = t.Key(d)
		}
		r.Reads = append(r.Reads, "phpunit.xml.dist")
		return r
	}
	return []dynamic.Record{
		list,
		mk("tests/CartTest.php", []string{"src/Cart.php"}, map[string]string{"src/Cart.php": "8-9|13-14"}, nil),
		mk("tests/CountTest.php", []string{"src/Cart.php"}, map[string]string{"src/Cart.php": "13-14|8-9"}, []string{"tests/fixtures/a.json"}),
		mk("tests/PriceTest.php", []string{"src/Price.php"}, map[string]string{"src/Price.php": "8-9|"}, nil),
	}
}

// run classifies changes, refines them (no tokenizer: the body guard result is trusted, as after a passing cross-check) and
// narrows.
func run(t *testing.T, base, head tree.Tree, recs []dynamic.Record, s model.Selection, changes ...dynamic.Change) (model.Selection, string) {
	t.Helper()
	ch, why := Guard(s, changes)
	if why != "" {
		return s, why
	}
	ch = Refine(ch, base, head)
	return Narrow(s, Params{Records: recs, Base: base, UnitEco: unitEco, Changed: ch,
		Env: func(model.SelectedTest) (string, string) { return env, "" }}), ""
}

func kept(s model.Selection) []string {
	t := s.Tests[0]
	if !t.ClassFilter {
		return nil // whole unit
	}
	return t.Classes
}

func TestNarrowFunctionLevel(t *testing.T) {
	base := with(nil)
	head := with(map[string]string{"src/Cart.php": strings.Replace(cartPHP, "return 1;", "return 3;", 1)})
	out, why := run(t, base, head, records(base), selection("src/Cart.php"), change("src/Cart.php", model.RoleSource))
	if why != "" {
		t.Fatal(why)
	}
	if got := kept(out); !slices.Equal(got, []string{"tests/CartTest.php"}) {
		t.Fatalf("kept %v (%s)", got, out.Tests[0].Dynamic)
	}
	if len(out.Narrowed) != 2 || out.Narrowed[0].Unit != ".::php#tests/CountTest.php" {
		t.Fatalf("narrowed %+v", out.Narrowed)
	}
}

func TestNarrowFileLevel(t *testing.T) {
	base := with(nil)
	// A property added at class level: file level, every test file that included Cart.php runs.
	head := with(map[string]string{"src/Cart.php": strings.Replace(cartPHP, "class Cart\n{\n", "class Cart\n{\n    public int $x = 1;\n", 1)})
	out, _ := run(t, base, head, records(base), selection("src/Cart.php"), change("src/Cart.php", model.RoleSource))
	if got := kept(out); !slices.Equal(got, []string{"tests/CartTest.php", "tests/CountTest.php"}) {
		t.Fatalf("kept %v", got)
	}
	// A changed test file runs itself; the others are removed.
	head = with(map[string]string{"tests/PriceTest.php": "<?php\n// price 2\n"})
	out, _ = run(t, base, head, records(base), selection("tests/PriceTest.php"), change("tests/PriceTest.php", model.RoleTest))
	if got := kept(out); !slices.Equal(got, []string{"tests/PriceTest.php"}) {
		t.Fatalf("kept %v", got)
	}
	// Documentation read by nobody: every test file is removed (the unit stays selected with no file to run).
	head = with(map[string]string{"README.md": "# y\n"})
	// The PHP analysis owns it (data role, an ownership edge): still a documentation file.
	doc := dynamic.Change{Path: "README.md", Status: 'M', OldMode: "100644", NewMode: "100644", BaseRole: model.RoleData,
		HeadRole: model.RoleData, Referenced: true}
	out, _ = run(t, base, head, records(base), selection("README.md"), doc)
	if !out.Tests[0].ClassFilter || len(out.Tests[0].Classes) != 0 || len(out.Narrowed) != 3 {
		t.Fatalf("doc change: %+v", out.Tests[0])
	}
	// A test file that read it is kept.
	recs := records(base)
	recs[3].Deps["README.md"] = base.Key("README.md")
	recs[3].Reads = append(recs[3].Reads, "README.md")
	out, _ = run(t, base, head, recs, selection("README.md"), doc)
	if got := kept(out); !slices.Equal(got, []string{"tests/PriceTest.php"}) {
		t.Fatalf("doc read: kept %v", got)
	}
	doc.OtherRole = true
	if _, why := Guard(selection("README.md"), []dynamic.Change{doc}); why == "" {
		t.Fatal("documentation file with another ecosystem's role accepted")
	}
}

func TestNarrowFunctionLevelKeeps(t *testing.T) {
	base := with(nil)
	body := strings.Replace(cartPHP, "return 2;", "return 4;", 1)
	cases := map[string]struct {
		cov   map[string]string
		reads []string
		head  string
	}{
		"no line table":           {cov: map[string]string{}, head: body},
		"no instrumentable line":  {cov: map[string]string{"src/Cart.php": "8-9|"}, head: body},
		"read as data":            {cov: map[string]string{"src/Cart.php": "8-9|13-14"}, reads: []string{"src/Cart.php"}, head: body},
		"line shift below change": {cov: map[string]string{"src/Cart.php": "8-9,15|13-14"}, head: strings.Replace(cartPHP, "return 2;", "$a = 1;\n        return 2;", 1)},
	}
	for name, c := range cases {
		recs := records(base)
		r := &recs[1] // CartTest: does not run count()
		r.LineCov = c.cov
		if len(r.LineCov) == 0 {
			r.LineCov = nil
		}
		r.Reads = append(r.Reads, c.reads...)
		head := with(map[string]string{"src/Cart.php": c.head})
		out, why := run(t, base, head, recs, selection("src/Cart.php"), change("src/Cart.php", model.RoleSource))
		if why != "" {
			t.Fatalf("%s: %s", name, why)
		}
		if got := kept(out); !slices.Contains(got, "tests/CartTest.php") {
			t.Errorf("%s: CartTest removed (kept %v)", name, got)
		}
	}
	// Without line shift and with a table showing total only, CartTest is removed.
	recs := records(base)
	head := with(map[string]string{"src/Cart.php": body})
	out, _ := run(t, base, head, recs, selection("src/Cart.php"), change("src/Cart.php", model.RoleSource))
	if got := kept(out); slices.Contains(got, "tests/CartTest.php") {
		t.Fatalf("CartTest kept: %v", got)
	}
}

// Fault injection (docs/dynamic-layer.md §11.2): none of these may narrow.
func TestGuardRefusals(t *testing.T) {
	m := func(p string, f func(*dynamic.Change)) dynamic.Change {
		c := change(p, model.RoleSource)
		f(&c)
		return c
	}
	cases := map[string]dynamic.Change{
		"addition":    m("src/New.php", func(c *dynamic.Change) { c.Status = 'A' }),
		"deletion":    m("src/Cart.php", func(c *dynamic.Change) { c.Status = 'D' }),
		"mode change": m("src/Cart.php", func(c *dynamic.Change) { c.NewMode = "100755" }),
		"symlink":     m("src/Cart.php", func(c *dynamic.Change) { c.OldMode, c.NewMode = "120000", "120000" }),
		"linked":      m("src/Cart.php", func(c *dynamic.Change) { c.Linked = true }),
		"composer.json": m("composer.json", func(c *dynamic.Change) {
			c.BaseRole, c.HeadRole, c.Claimed = model.RoleManifest, model.RoleManifest, true
		}),
		"composer.lock":  m("composer.lock", func(c *dynamic.Change) { c.BaseRole, c.HeadRole, c.Claimed = model.RoleLock, model.RoleLock, true }),
		"phpunit.xml":    m("phpunit.xml.dist", func(c *dynamic.Change) { c.BaseRole, c.HeadRole, c.Claimed = model.RoleConfig, model.RoleConfig, true }),
		"Pest.php":       m("tests/Pest.php", func(c *dynamic.Change) { c.Claimed = true }),
		"data file":      m("tests/fixtures/a.json", func(c *dynamic.Change) { c.BaseRole, c.HeadRole = model.RoleData, model.RoleData }),
		"[[config]]":     m("src/Cart.php", func(c *dynamic.Change) { c.Declared = true }),
		"[[rule]]":       m("src/Cart.php", func(c *dynamic.Change) { c.Rule = true }),
		"global":         m("src/Cart.php", func(c *dynamic.Change) { c.Global = true }),
		"role data/none": m("src/view.phtml", func(c *dynamic.Change) { c.BaseRole, c.HeadRole = model.RoleData, "" }),
	}
	for name, c := range cases {
		if _, why := Guard(selection(c.Path), []dynamic.Change{c}); why == "" {
			t.Errorf("%s: accepted", name)
		}
	}
	s := selection("src/Cart.php")
	s.Widening = []model.Widening{{Tier: model.TierModule, Reason: model.ReasonManifestChanged, Path: "composer.json", Eco: Eco}}
	if _, why := Guard(s, []dynamic.Change{change("src/Cart.php", model.RoleSource)}); why == "" {
		t.Error("PHP widening accepted")
	}
	s = selection("src/Cart.php")
	s.Scope = model.TierFull
	if _, why := Guard(s, []dynamic.Change{change("src/Cart.php", model.RoleSource)}); why == "" {
		t.Error("full selection accepted")
	}
}

// Fault injection on records: each case must keep every test file that includes the changed file (here: all three, since
// the change is to a file every record includes).
func TestNarrowUnusableRecords(t *testing.T) {
	base := with(nil)
	head := with(map[string]string{"src/Price.php": strings.Replace(baseFiles["src/Price.php"], "(string) $x", "(string) ($x + 1)", 1)})
	ch := change("src/Price.php", model.RoleSource)
	ok, _ := run(t, base, head, records(base), selection("src/Price.php"), ch)
	if got := kept(ok); !slices.Equal(got, []string{"tests/PriceTest.php"}) {
		t.Fatalf("baseline: kept %v", got)
	}
	whole := func(name string, recs []dynamic.Record, b tree.Tree) {
		t.Helper()
		out, why := run(t, b, head, recs, selection("src/Price.php"), ch)
		if why == "" && out.Tests[0].ClassFilter {
			t.Errorf("%s: narrowed to %v", name, out.Tests[0].Classes)
		}
	}
	cartKept := func(name string, recs []dynamic.Record) {
		t.Helper()
		out, _ := run(t, base, head, recs, selection("src/Price.php"), ch)
		if !slices.Contains(kept(out), "tests/CartTest.php") && out.Tests[0].ClassFilter {
			t.Errorf("%s: CartTest removed", name)
		}
	}
	edit := func(i int, f func(*dynamic.Record)) []dynamic.Record {
		recs := records(base)
		f(&recs[i])
		return recs
	}
	for _, reason := range []string{"starts a process (proc_open) at x:1", "forks the process (pcntl_fork) at x:1",
		"calls native code through FFI at x:1", "another coverage user: xdebug_start_code_coverage at x:1",
		"the file:// stream wrapper proxy was replaced or removed during the run", "reads untracked file a.txt",
		"canary: the test file was not included", "no tests ran", "timeout",
		"reads a file without the stream wrapper (php_strip_whitespace) at x:1", "another coverage user: xdebug_set_filter at x:1"} {
		cartKept("incomplete: "+reason, edit(1, func(r *dynamic.Record) { r.Complete, r.Incomplete = false, reason }))
	}
	cartKept("failed", edit(1, func(r *dynamic.Record) { r.Pass = false }))
	cartKept("stale dependency", edit(1, func(r *dynamic.Record) { r.Deps["src/Cart.php"] = "0000" }))
	cartKept("missing canary dependency", edit(1, func(r *dynamic.Record) { delete(r.Deps, "tests/CartTest.php") }))
	cartKept("other environment", edit(1, func(r *dynamic.Record) { r.Env = "env2" }))
	cartKept("other collector", edit(1, func(r *dynamic.Record) { r.Collector = "php-xdebug-0" }))
	cartKept("no record", records(base)[:1])
	whole("PHPUnit 9", edit(0, func(r *dynamic.Record) { r.Runner = "phpunit@9.6.0" }), base)
	whole("Pest 1", edit(0, func(r *dynamic.Record) { r.Runner = "pest@1.22.0" }), base)
	whole("unknown runner", edit(0, func(r *dynamic.Record) { r.Runner = "" }), base)
	whole("file list incomplete", edit(0, func(r *dynamic.Record) { r.Complete, r.Incomplete = false, "listing the test files: x" }), base)
	whole("file list stale", edit(0, func(r *dynamic.Record) { r.Deps["tests/CartTest.php"] = "0000" }), base)
	whole("file list other env", edit(0, func(r *dynamic.Record) { r.Env = "env2" }), base)
	// A test file added under the test directories since the file list was recorded.
	added := with(map[string]string{"tests/NewTest.php": "<?php\n"})
	whole("test file added", records(base), added)
	// A file of the test directories removed since.
	removed := with(map[string]string{"tests/Support/Base.php": ""})
	whole("test directory file removed", records(base), removed)
	// Environment fingerprint unavailable.
	out := Narrow(selection("src/Price.php"), Params{Records: records(base), Base: base, UnitEco: unitEco,
		Changed: []Changed{{Path: "src/Price.php", Kind: KindFile}}, Env: func(model.SelectedTest) (string, string) { return "", "no vendor" }})
	if out.Tests[0].ClassFilter {
		t.Error("narrowed without an environment")
	}
	// Previously failed unit.
	s := selection("src/Price.php")
	s.AlwaysRun = []string{unitID}
	out = Narrow(s, Params{Records: records(base), Base: base, UnitEco: unitEco, Changed: []Changed{{Path: "src/Price.php", Kind: KindFile}},
		Env: func(model.SelectedTest) (string, string) { return env, "" }})
	if out.Tests[0].ClassFilter {
		t.Error("narrowed a previously failed unit")
	}
	// Whole-suite units and units without a configuration are never narrowed.
	for _, f := range []func(*model.SelectedTest){func(t *model.SelectedTest) { t.Whole = true }, func(t *model.SelectedTest) { t.Config = "" }} {
		s := selection("src/Price.php")
		f(&s.Tests[0])
		out := Narrow(s, Params{Records: records(base), Base: base, UnitEco: unitEco, Changed: []Changed{{Path: "src/Price.php", Kind: KindFile}},
			Env: func(model.SelectedTest) (string, string) { return env, "" }})
		if out.Tests[0].ClassFilter {
			t.Error("narrowed a whole or unconfigured unit")
		}
	}
}

// Function-level fault injection: changes the refinement cannot judge fall back to file level (every test file that included
// the file runs), never to a removal.
func TestRefineFallsBackToFileLevel(t *testing.T) {
	base := with(nil)
	edits := map[string]string{
		"signature":       strings.Replace(cartPHP, "public function count(): int", "public function count(): ?int", 1),
		"doc comment":     strings.Replace(cartPHP, "class Cart\n", "/** @internal */\nclass Cart\n", 1),
		"attribute":       strings.Replace(cartPHP, "    public function count", "    #[\\Pure]\n    public function count", 1),
		"constant":        strings.Replace(cartPHP, "class Cart\n{\n", "class Cart\n{\n    const X = 1;\n", 1),
		"nested function": strings.Replace(cartPHP, "return 2;", "function inner() {}\n        return 2;", 1),
		"unparsable head": strings.Replace(cartPHP, "return 2;", "return \"2;", 1),
		"closure outside": cartPHP + "$f = function () {\n    return 1;\n};\n",
	}
	for name, h := range edits {
		head := with(map[string]string{"src/Cart.php": h})
		out, why := run(t, base, head, records(base), selection("src/Cart.php"), change("src/Cart.php", model.RoleSource))
		if why != "" {
			t.Fatalf("%s: refused the selection: %s", name, why)
		}
		if got := kept(out); !slices.Equal(got, []string{"tests/CartTest.php", "tests/CountTest.php"}) {
			t.Errorf("%s: kept %v", name, got)
		}
	}
	// A tokenizer disagreement (or no PHP for the cross-check) downgrades a function-level file.
	head := with(map[string]string{"src/Cart.php": strings.Replace(cartPHP, "return 2;", "return 4;", 1)})
	ch, _ := Guard(selection("src/Cart.php"), []dynamic.Change{change("src/Cart.php", model.RoleSource)})
	ch = Refine(ch, base, head)
	if ch[0].Kind != KindLines {
		t.Fatalf("refine: %+v", ch[0])
	}
	for _, d := range []struct {
		fail    map[string]string
		checked map[string]bool
	}{
		{map[string]string{"src/Cart.php": "spans differ"}, map[string]bool{"src/Cart.php": true}},
		{map[string]string{}, map[string]bool{}},
	} {
		dg := Downgrade(ch, d.fail, d.checked)
		if dg[0].Kind != KindFile {
			t.Fatalf("not downgraded: %+v", dg[0])
		}
		out := Narrow(selection("src/Cart.php"), Params{Records: records(base), Base: base, UnitEco: unitEco, Changed: dg,
			Env: func(model.SelectedTest) (string, string) { return env, "" }})
		if got := kept(out); !slices.Equal(got, []string{"tests/CartTest.php", "tests/CountTest.php"}) {
			t.Errorf("downgraded: kept %v", got)
		}
	}
}

func TestTestRoots(t *testing.T) {
	u := Unit{ID: unitID, Project: ".", Config: "phpunit.xml.dist"}
	if got := TestRoots(with(nil), u); !slices.Equal(got, []string{"tests"}) {
		t.Fatalf("roots %v", got)
	}
	for _, x := range []string{"<directory>tests/*/Unit</directory>", "<directory>/abs</directory>", "<directory>%env%</directory>",
		"<directory>../out</directory>", "<file>x</file><directory>", ""} {
		cfg := strings.Replace(phpunitXMLSrc, "<directory>./tests</directory>", x, 1)
		if got := TestRoots(with(map[string]string{"phpunit.xml.dist": cfg}), u); !slices.Equal(got, []string{"."}) {
			t.Errorf("%q: roots %v", x, got)
		}
	}
	cfg := strings.Replace(phpunitXMLSrc, "<directory>./tests</directory>", "<directory>tests/unit</directory><directory>tests</directory><file>other/XTest.php</file>", 1)
	if got := TestRoots(with(map[string]string{"phpunit.xml.dist": cfg}), u); !slices.Equal(got, []string{"other/XTest.php", "tests"}) {
		t.Fatalf("roots %v", got)
	}
}

func TestMultiOperand(t *testing.T) {
	for r, ok := range map[string]bool{"phpunit@10.0.0": true, "phpunit@11.5.58": true, "pest@v3.8.7": true, "pest@2.0.0": true,
		"phpunit@9.6.20": false, "pest@1.23.1": false, "": false, "phpunit@dev-main": false} {
		if (MultiOperand(r) == "") != ok {
			t.Errorf("%q: %q", r, MultiOperand(r))
		}
	}
}

// A comment line added above one function and a blank line removed below another keep the line count but move every line in
// between: a test file that executed one of those lines (an exception's getLine()) must be kept.
func TestLineShiftWithSameLineCount(t *testing.T) {
	base := with(nil)
	h := strings.Replace(cartPHP, "    public function total(): int\n", "    // note\n    public function total(): int\n", 1)
	h = strings.Replace(h, "    }\n\n    public function count", "    }\n    public function count", 1)
	if strings.Count(h, "\n") != strings.Count(cartPHP, "\n") {
		t.Fatal("edit changed the line count")
	}
	spans, shift, first, why := BodyGuard("src/Cart.php", []byte(cartPHP), []byte(h))
	if why != "" || len(spans) != 0 || !shift || first != 6 {
		t.Fatalf("spans %v shift %v first %d why %q", spans, shift, first, why)
	}
	head := with(map[string]string{"src/Cart.php": h})
	recs := records(base)
	recs[1].LineCov = map[string]string{"src/Cart.php": "8|"} // CartTest executed line 8 (moves to 9)
	out, why := run(t, base, head, recs, selection("src/Cart.php"), change("src/Cart.php", model.RoleSource))
	if why != "" {
		t.Fatal(why)
	}
	if got := kept(out); !slices.Contains(got, "tests/CartTest.php") {
		t.Fatalf("CartTest removed: kept %v", got)
	}
	// A comment change that keeps every line where it was moves nothing.
	if _, shift, _, why := BodyGuard("x.php", []byte(cartPHP), []byte(strings.Replace(cartPHP, "namespace App;", "namespace App; // x", 1))); why != "" || shift {
		t.Fatalf("in-place comment: shift %v why %q", shift, why)
	}
}

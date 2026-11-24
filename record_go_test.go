package bgraph

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/audit"
	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/dynamic/godyn"
	"github.com/qxbao/bgraph/internal/runner"
)

const goCart = `package shop

// Cart holds items.
type Cart struct{ Items []int }

// Total sums the items.
func (c Cart) Total() int {
	n := 0
	for _, x := range c.Items {
		n += x
	}
	return n
}

// Count counts the items.
func (c Cart) Count() int {
	return len(c.Items)
}

// Describe describes the cart.
func Describe(c Cart) string {
	return "cart of " + string(rune('0'+c.Count()))
}
`

const goPrice = `package shop

// Price doubles x.
func Price(x int) int {
	return x * 2
}
`

const goCartTest = `package shop

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("SHOP_HELPER") == "1" {
		fmt.Println(Price(1))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestTotal(t *testing.T) {
	if (Cart{Items: []int{1, 2}}).Total() != 3 {
		t.Fatal("total")
	}
}

func TestCount(t *testing.T) {
	if (Cart{Items: []int{1, 2}}).Count() != 2 {
		t.Fatal("count")
	}
}

// TestHelper re-runs the test binary as a helper process: it inherits GOCOVERDIR and writes coverage, so it is observed.
func TestHelper(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "SHOP_HELPER=1")
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "2\n" {
		t.Fatalf("%v %q", err, out)
	}
}

// TestExternal runs a program that is not instrumented: its record is incomplete.
func TestExternal(t *testing.T) {
	if out, err := exec.Command("git", "--version").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
}

func ExampleDescribe() {
	fmt.Println(Describe(Cart{Items: []int{1}}))
	// Output: cart of 1
}
`

const goReportTest = `package report

import (
	"os"
	"testing"
)

func TestRender(t *testing.T) {
	if Render() != "cart of 2" {
		t.Fatal(Render())
	}
}

// TestSource reads a source file of another package as data: the static graph has a fixture edge to it.
func TestSource(t *testing.T) {
	if _, err := os.ReadFile("../shop/price.go"); err != nil {
		t.Fatal(err)
	}
}

// TestData reads a data file of the package directory (no static edge: the path is inside the package).
func TestData(t *testing.T) {
	if b, err := os.ReadFile("testdata/a.json"); err != nil || string(b) != "{}\n" {
		t.Fatal(err, string(b))
	}
}

// TestEnv runs only when REPORT_IT=1: skipped while recording, so its record is incomplete.
func TestEnv(t *testing.T) {
	if os.Getenv("REPORT_IT") != "1" {
		t.Skip("REPORT_IT not set")
	}
	if Render() == "" {
		t.Fatal("empty")
	}
}
`

const goLint = `package lint

// Value returns one.
func Value() int {
	return 1
}
`

const goLintTest = `package lint

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestDoc reads the package's own sources: every doc comment starts with the function's name.
func TestDoc(t *testing.T) {
	pkgs, err := parser.ParseDir(token.NewFileSet(), ".", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pkgs {
		for _, f := range p.Files {
			for _, cg := range f.Comments {
				if strings.HasPrefix(cg.Text(), "Returns") {
					t.Fatal(cg.Text())
				}
			}
		}
	}
	if Value() != 1 {
		t.Fatal("value")
	}
}
`

// TestRecordGoIntegration records a small Go module with the real collector (coverage of every package, os and syscall) and
// narrows one-line changes inside function bodies end to end.
func TestRecordGoIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("builds instrumented test binaries")
	}
	goExe, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	root := testRepo(t, map[string]string{
		"go.mod":                   "module example.com/shop\n\ngo 1.21\n",
		"shop/cart.go":             goCart,
		"shop/price.go":            goPrice,
		"shop/cart_test.go":        goCartTest,
		"report/report.go":         "package report\n\nimport \"example.com/shop/shop\"\n\n// Render renders a cart.\nfunc Render() string {\n\treturn shop.Describe(shop.Cart{Items: []int{1, 2}})\n}\n",
		"report/report_test.go":    goReportTest,
		"report/testdata/a.json":   "{}\n",
		"README.md":                "# shop\n",
		"lint/lint.go":             goLint,
		"lint/lint_test.go":        goLintTest,
		"shop/doc_unreferenced.md": "notes\n",
	})
	state := t.TempDir()
	ctx := context.Background()
	ix, err := Open(ctx, root, Options{MemoryOnly: true, StateDir: state, Go: goExe})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	sum, err := ix.Record(ctx, RecordOptions{Eco: "go", Jobs: 2, Timeout: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	recs, warn := dynamic.Load(dynamic.StorePath(state, "go"))
	by := map[string]dynamic.Record{}
	for _, r := range recs {
		by[r.Unit] = r
	}
	if sum.Units != 10 || len(recs) != 13 || len(warn) > 0 {
		t.Fatalf("summary %+v, %d records, %v", sum, len(recs), warn)
	}
	if l := by["go:shop"]; !l.Complete || !slices.Equal(l.Classes, []string{"ExampleDescribe", "TestCount", "TestExternal", "TestHelper", "TestTotal"}) {
		t.Fatalf("list record %+v", l)
	}
	for _, u := range []string{"go:shop#TestTotal", "go:shop#TestCount", "go:shop#TestHelper", "go:shop#ExampleDescribe", "go:report#TestRender", "go:report#TestSource",
		"go:report#TestData", "go:lint#TestDoc"} {
		if r := by[u]; !r.Complete || !r.Pass {
			t.Errorf("%s: complete=%v pass=%v (%s)", u, r.Complete, r.Pass, r.Incomplete)
		}
	}
	if r := by["go:shop#TestExternal"]; r.Complete || !r.Pass || !strings.Contains(r.Incomplete, "starts a process the collector does not observe") {
		t.Errorf("TestExternal: complete=%v pass=%v (%s)", r.Complete, r.Pass, r.Incomplete)
	}
	if r := by["go:report#TestEnv"]; r.Complete || !r.Pass || !strings.Contains(r.Incomplete, "test skipped") {
		t.Errorf("TestEnv: complete=%v pass=%v (%s)", r.Complete, r.Pass, r.Incomplete)
	}
	if r := by["go:report#TestData"]; r.Deps["report/testdata/a.json"] == "" || !slices.Contains(r.Dirs, "shop") || !slices.Contains(r.Dirs, "report") {
		t.Errorf("TestData deps %v dirs %v", r.Deps, r.Dirs)
	}
	if r := by["go:report#TestRender"]; r.Deps["shop/cart.go"] == "" || r.Deps["report/report_test.go"] == "" || r.Deps["go.mod"] == "" {
		t.Errorf("TestRender deps %v", r.Deps)
	}
	if ls, ok := godyn.ParseLineTable(by["go:shop#TestCount"].LineCov["shop/cart.go"]); !ok || !ls[17] || ls[9] {
		t.Errorf("TestCount table %q", by["go:shop#TestCount"].LineCov["shop/cart.go"])
	}
	if out, err := exec.Command("git", "-C", root, "status", "--porcelain").CombinedOutput(); err != nil || len(out) > 0 {
		t.Errorf("recording dirtied the tree: %v %s", err, out)
	}

	write := func(rel, c string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sel := func() Selection {
		t.Helper()
		s, err := ix.Select(ctx, Change{Base: "HEAD", Worktree: true, Dynamic: DynamicOn})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	type unitRun struct {
		filter bool
		run    []string
	}
	runs := func(s Selection) map[string]unitRun {
		m := map[string]unitRun{}
		for _, x := range s.Tests {
			m[x.Unit] = unitRun{x.ClassFilter, x.Classes}
		}
		return m
	}
	check := func(name string, want map[string]unitRun) Selection {
		t.Helper()
		s := sel()
		got := runs(s)
		if len(got) != len(want) {
			t.Fatalf("%s: got %v want %v (warnings %+v)", name, got, want, s.Warnings)
		}
		for u, w := range want {
			g, ok := got[u]
			if !ok || g.filter != w.filter || !slices.Equal(g.run, w.run) {
				t.Fatalf("%s: %s got %+v want %+v (notes: %v)", name, u, g, w, notes(s))
			}
		}
		return s
	}
	whole := unitRun{}

	// Cart.Total: only TestTotal (and the incomplete TestExternal) run in shop; report compiles only.
	write("shop/cart.go", strings.Replace(goCart, "n += x", "n += x * 1", 1))
	s := check("Total", map[string]unitRun{"go:shop": {true, []string{"TestExternal", "TestTotal"}}, "go:report": {true, []string{"TestEnv"}}})
	plan, err := runner.Commands(s, runner.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, c := range plan.Commands {
		lines = append(lines, strings.Join(c.Argv, " "))
	}
	if !slices.Contains(lines, "go test -run ^(TestEnv)$ ./report") || !slices.Contains(lines, "go test -run ^(TestExternal|TestTotal)$ ./shop") {
		t.Errorf("commands %v", lines)
	}
	// Audit: a failure of a removed function of a selected package is a miss; one of a kept function is not.
	audit1 := func(test string) AuditReport {
		t.Helper()
		rep, err := ix.Audit(ctx, Change{Base: "HEAD", Worktree: true, Dynamic: DynamicOn}, []AuditResult{{Format: audit.FormatGoJSON,
			Failures: []audit.Failure{{Test: test, Package: "example.com/shop/shop"}}}})
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	if rep := audit1("TestCount/sub"); rep.Clean || len(rep.Missed) != 1 || !strings.Contains(rep.Missed[0].Reason, "removed by the dynamic layer") {
		t.Errorf("audit of a removed function: %+v", rep)
	}
	if rep := audit1("TestTotal"); !rep.Clean {
		t.Errorf("audit of a kept function: %+v", rep)
	}
	// A package-level failure (build, vet, TestMain) is not a removed function.
	if rep := audit1("(package failed)"); !rep.Clean {
		t.Errorf("audit of a package failure: %+v", rep)
	}
	// Cart.Count: the example (through Describe) and report's TestRender run it.
	write("shop/cart.go", strings.Replace(goCart, "return len(c.Items)", "return len(c.Items[:])", 1))
	check("Count", map[string]unitRun{"go:shop": {true, []string{"ExampleDescribe", "TestCount", "TestExternal"}},
		"go:report": {true, []string{"TestEnv", "TestRender"}}})
	// A line added inside Total moves Count and Describe: every function that executed a line below it is kept.
	write("shop/cart.go", strings.Replace(goCart, "n := 0", "n := 0\n\tn += 0", 1))
	check("shift", map[string]unitRun{"go:shop": {true, []string{"ExampleDescribe", "TestCount", "TestExternal", "TestTotal"}},
		"go:report": {true, []string{"TestEnv", "TestRender"}}})
	// A signature change: the guard refuses, nothing is narrowed.
	write("shop/cart.go", strings.Replace(goCart, "func Describe(c Cart) string", "func Describe(c Cart, _ ...int) string", 1))
	s = check("signature", map[string]unitRun{"go:shop": whole, "go:report": whole})
	if len(s.Narrowed) != 0 || !strings.Contains(notes(s), "outside function bodies") {
		t.Errorf("signature: narrowed %v notes %s", s.Narrowed, notes(s))
	}
	write("shop/cart.go", goCart)
	// Price: report reads price.go as data (a fixture edge): report runs whole; in shop only the helper test ran Price.
	write("shop/price.go", strings.Replace(goPrice, "x * 2", "x + x", 1))
	s = check("Price", map[string]unitRun{"go:shop": {true, []string{"TestExternal", "TestHelper"}}, "go:report": whole})
	if !strings.Contains(notes(s), "fixture, embed, cgo or directory-pattern edge") {
		t.Errorf("Price notes %s", notes(s))
	}
	write("shop/price.go", goPrice)
	// A comment-only change in a package whose tests parse their own sources: kept whole.
	write("lint/lint.go", strings.Replace(goLint, "// Value returns one.", "// Returns one.", 1))
	s = check("lint comment", map[string]unitRun{"go:lint": whole})
	if !strings.Contains(notes(s), "Go sources as data") {
		t.Errorf("lint notes %s", notes(s))
	}
	write("lint/lint.go", goLint)
	// A changed test file keeps its package whole.
	write("shop/cart_test.go", strings.Replace(goCartTest, "t.Fatal(\"count\")", "t.Fatal(\"count!\")", 1))
	check("test file", map[string]unitRun{"go:shop": whole})
	write("shop/cart_test.go", goCartTest)
	// A data file refuses.
	write("report/testdata/a.json", "[]\n")
	if s := sel(); len(s.Narrowed) != 0 {
		t.Errorf("testdata change narrowed %v", s.Narrowed)
	}
	write("report/testdata/a.json", "{}\n")
	// A new test file in shop since the recording: the function list is not usable.
	write("shop/cart.go", strings.Replace(goCart, "n += x", "n += x * 1", 1))
	write("shop/more_test.go", "package shop\n\nimport \"testing\"\n\nfunc TestMore(t *testing.T) {}\n")
	git := func(args ...string) {
		t.Helper()
		run := exec.Command("git", append([]string{"-c", "core.autocrlf=false"}, args...)...)
		run.Dir = root
		if out, err := run.CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	git("add", "shop/more_test.go")
	git("commit", "-q", "-m", "more")
	s = check("added test file", map[string]unitRun{"go:shop": whole, "go:report": {true, []string{"TestEnv"}}})
	if !strings.Contains(notes(s), "was not there when recorded") {
		t.Errorf("added test file notes %s", notes(s))
	}
	// A source file added since the recording to a package report's binary compiles (shop): report's function records are not
	// usable, every function runs.
	write("shop/hook.go", "package shop\n\nfunc init() {}\n")
	git("add", "shop/hook.go")
	git("commit", "-q", "-m", "hook")
	check("added source file", map[string]unitRun{"go:shop": whole, "go:report": whole})
	git("rm", "-q", "shop/hook.go")
	git("commit", "-q", "-m", "no hook")
	check("hook removed", map[string]unitRun{"go:shop": whole, "go:report": {true, []string{"TestEnv"}}})
	// A data file of report changed between the recording and the base: report's records are stale.
	write("report/testdata/a.json", "[]\n")
	git("add", "report/testdata/a.json")
	git("commit", "-q", "-m", "data")
	s = check("stale data", map[string]unitRun{"go:shop": whole, "go:report": whole})
	if !strings.Contains(notes(s), "stale") {
		t.Errorf("stale data notes %s", notes(s))
	}
}

func notes(s Selection) string {
	var out []string
	for _, x := range s.Tests {
		out = append(out, x.Unit+": "+x.Dynamic)
	}
	for _, w := range s.Warnings {
		out = append(out, w.Detail)
	}
	return strings.Join(out, " | ")
}

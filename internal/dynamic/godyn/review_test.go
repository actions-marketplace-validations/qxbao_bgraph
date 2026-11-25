package godyn

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// A data file of the package directory that changed between the recording and the base makes the records stale.
func TestNarrowStaleDataFile(t *testing.T) {
	recBase := with(nil)
	recs := records(recBase)
	for i := range recs {
		recs[i].Deps["shop/testdata/a.json"] = recBase.Key("shop/testdata/a.json")
	}
	base := with(map[string]string{"shop/testdata/a.json": "[]\n"})
	head := with(map[string]string{"shop/testdata/a.json": "[]\n", "shop/cart.go": strings.Replace(cartGo, "func Empty() {}", "func Empty() { _ = 0 }", 1)})
	s, why := run(t, sel("shop/cart.go"), []dynamic.Change{change("shop/cart.go")}, base, head, recs, nil)
	if why != "" || len(s.Narrowed) != 0 || s.Tests[0].ClassFilter || !strings.Contains(s.Tests[0].Dynamic, "stale") {
		t.Fatalf("why %q narrowed %v note %q", why, narrowed(s), s.Tests[0].Dynamic)
	}
}

// A .go file added since the recording to a package directory of a function's build closure makes that record unusable.
func TestNarrowAddedGoFile(t *testing.T) {
	recBase := with(nil)
	recs := records(recBase)
	for i := range recs {
		if strings.Contains(recs[i].Unit, "#") {
			recs[i].Dirs = []string{"shop"}
		}
	}
	base := with(map[string]string{"shop/hook.go": "package shop\n\nfunc init() { Adjust = 1 }\n"})
	head := with(map[string]string{"shop/hook.go": "package shop\n\nfunc init() { Adjust = 1 }\n",
		"shop/cart.go": strings.Replace(cartGo, "func Empty() {}", "func Empty() { _ = 0 }", 1)})
	s, _ := run(t, sel("shop/cart.go"), []dynamic.Change{change("shop/cart.go")}, base, head, recs, nil)
	if len(s.Narrowed) != 0 {
		t.Fatalf("narrowed %v (%s)", narrowed(s), s.Tests[0].Dynamic)
	}
	// Without the added file the same records narrow.
	s, _ = run(t, sel("shop/cart.go"), []dynamic.Change{change("shop/cart.go")}, recBase,
		with(map[string]string{"shop/cart.go": strings.Replace(cartGo, "func Empty() {}", "func Empty() { _ = 0 }", 1)}), recs, nil)
	if len(s.Narrowed) != 3 {
		t.Fatalf("control: narrowed %v (%s)", narrowed(s), s.Tests[0].Dynamic)
	}
}

// A changed .go file that the binary does not compile (listed in Reads: another build configuration) does not keep a function.
func TestKeepReasonNotCompiled(t *testing.T) {
	base := with(nil)
	rec := records(base)[1]
	rec.Reads = []string{"shop/cart.go"}
	delete(rec.LineCov, "shop/cart.go")
	if why := keepReason(rec, []Changed{{Path: "shop/cart.go", Kind: KindLines, Spans: []Span{{15, 21, "Cart.Total"}}}}); why != "" {
		t.Errorf("kept: %s", why)
	}
}

// Tests that read their own package's Go sources as data keep the package whole, even for a comment-only change.
func TestReadsOwnSources(t *testing.T) {
	cases := map[string]string{
		"parser":      "package shop\n\nimport \"go/parser\"\n\nvar _ = parser.ParseDir\n",
		"ast":         "package shop\n\nimport _ \"go/ast\"\n",
		"doc":         "package shop\n\nimport _ \"go/doc\"\n",
		"build":       "package shop\n\nimport _ \"go/build\"\n",
		"format":      "package shop\n\nimport _ \"go/format\"\n",
		"types":       "package shop\n\nimport _ \"go/types\"\n",
		"packages":    "package shop\n\nimport _ \"golang.org/x/tools/go/packages\"\n",
		"analysis":    "package shop\n\nimport _ \"golang.org/x/tools/go/analysis/analysistest\"\n",
		"printer":     "package shop\n\nimport (\n\t_ \"go/printer\"\n\t_ \"go/token\"\n)\n",
		"dot":         "package shop\n\nvar d = \".\"\n",
		"glob":        "package shop\n\nvar d = \"*.go\"\n",
		"own file":    "package shop\n\nvar d = \"cart.go\"\n",
		"own file ./": "package shop\n\nvar d = \"./price.go\"\n",
		"unparsable":  "package shop\n\nvar d = \n",
	}
	for name, src := range cases {
		tr := with(map[string]string{"shop/cart_test.go": src})
		if ReadsOwnSources(tr, "shop") == "" {
			t.Errorf("%s: not detected", name)
		}
	}
	for name, src := range map[string]string{
		"token only":   "package shop\n\nimport _ \"go/token\"\n",
		"other file":   "package shop\n\nvar d = \"testdata/a.json\"\n",
		"other dir go": "package shop\n\nvar d = \"../other/cart.go\"\n",
	} {
		if why := ReadsOwnSources(with(map[string]string{"shop/cart_test.go": src}), "shop"); why != "" {
			t.Errorf("%s: %s", name, why)
		}
	}
	// End to end through Narrow: a comment-only change of cart.go.
	base := with(map[string]string{"shop/cart_test.go": cases["parser"]})
	recs := records(base)
	head := with(map[string]string{"shop/cart_test.go": cases["parser"], "shop/cart.go": strings.Replace(cartGo, "// Total sums the items.", "// Returns the sum.", 1)})
	s, why := run(t, sel("shop/cart.go"), []dynamic.Change{change("shop/cart.go")}, base, head, recs, nil)
	if why != "" || len(s.Narrowed) != 0 || !strings.Contains(s.Tests[0].Dynamic, "Go sources as data") {
		t.Fatalf("why %q narrowed %v note %q", why, narrowed(s), s.Tests[0].Dynamic)
	}
}

// The directives before the package clause must not change (a header with "+build" or a block comment is compared byte for
// byte); documentation edits there pass. An approximate diff counts as a line shift.
func TestBodyGuardPrefixAndApproximateDiff(t *testing.T) {
	for name, edit := range map[string][2]string{
		"+build added":      {"// Package shop", "// +build linux\n\n// Package shop"},
		"go:build added":    {"// Package shop", "//go:build linux\n\n// Package shop"},
		"go:generate added": {"// Package shop", "//go:generate stringer -type=X\n\n// Package shop"},
		"block comment":     {"// Package shop is a test fixture.", "/* Package shop is a test fixture. */"},
	} {
		head := strings.Replace(cartGo, edit[0], edit[1], 1)
		if head == cartGo {
			t.Fatalf("%s: no change", name)
		}
		if _, _, _, why := BodyGuard("shop.go", []byte(cartGo), []byte(head)); !strings.Contains(why, "before the package clause") {
			t.Errorf("%s: %q", name, why)
		}
	}
	for name, edit := range map[string][2]string{
		"package doc":      {"// Package shop is a test fixture.", "// Package shop is a fixture."},
		"blank line added": {"// Package shop is a test fixture.\npackage shop", "// Package shop is a test fixture.\n\npackage shop"},
	} {
		head := strings.Replace(cartGo, edit[0], edit[1], 1)
		if head == cartGo {
			t.Fatalf("%s: no change", name)
		}
		spans, shift, _, why := BodyGuard("shop.go", []byte(cartGo), []byte(head))
		if why != "" || len(spans) != 0 {
			t.Errorf("%s: spans %v why %q", name, spans, why)
		}
		if want := name == "blank line added"; shift != want {
			t.Errorf("%s: shift %v", name, shift)
		}
	}
	var b, h strings.Builder
	b.WriteString("package shop\n\nfunc Big() {\n\tx := 0\n")
	h.WriteString("package shop\n\nfunc Big() {\n\tx := 0\n")
	for i := 0; i < 1200; i++ {
		b.WriteString("\tx = 1\n")
		h.WriteString("\tx = 2\n")
	}
	b.WriteString("\t_ = x\n}\n")
	h.WriteString("\t_ = x\n}\n")
	if _, exact := diffHunks(strings.Split(b.String(), "\n"), strings.Split(h.String(), "\n")); exact {
		t.Fatal("expected the approximate diff")
	}
	spans, shift, first, why := BodyGuard("big.go", []byte(b.String()), []byte(h.String()))
	if why != "" || len(spans) != 1 || !shift || first != 5 {
		t.Errorf("spans %v shift %v first %d why %q", spans, shift, first, why)
	}
}

// A skipped test's record is incomplete; data files and package directories enter the record.
func TestRecordOfSkippedAndData(t *testing.T) {
	repo := t.TempDir()
	files := map[string]string{"go.mod": "module example.com/shop\n", "shop/cart.go": cartGo, "shop/cart_test.go": "package shop\n",
		"shop/cart_other.go": "//go:build never\n\npackage shop\n", "shop/testdata/a.json": "{}\n"}
	for f, c := range files {
		p := filepath.Join(repo, filepath.FromSlash(f))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(c), 0o644)
	}
	wt := tree.NewMapTree(files)
	abs := func(f string) string { return filepath.ToSlash(filepath.Join(repo, filepath.FromSlash(f))) }
	pkg := &Package{Unit: Unit{ID: unit, Dir: "shop", Module: "."}, Files: []string{abs("go.mod"), abs("shop/cart.go"), abs("shop/cart_test.go")},
		TestFiles: []string{abs("shop/cart_test.go")}, Pkgs: map[string]string{"example.com/shop": abs("shop")}, Dirs: []string{abs("shop")}}
	o := Outcome{Unit: SubID(unit, "TestTotal"), Name: "TestTotal", Pkg: pkg, Ran: true, Exit: 0, Counters: 1, MainWrote: true, Started: 0}
	data := DataFiles(wt, "shop", []model.Edge{{From: "<go:shop>", To: "go.mod", Kind: model.EdgeFixtureData},
		{From: "<go:shop>", To: "shop/testdata/*.json", Kind: model.EdgePattern}, {From: "<go:shop>", To: "<golib:x>", Kind: model.EdgeImport}})
	if !slices.Equal(data, []string{"go.mod", "shop/cart.go", "shop/cart_other.go", "shop/cart_test.go", "shop/testdata/a.json"}) {
		t.Fatalf("data %v", data)
	}
	r := RecordOf(o, wt, repo, env, "c1", time.Now(), data)
	if !r.Complete || !slices.Equal(r.Dirs, []string{"shop"}) || r.Deps["shop/testdata/a.json"] == "" || r.Deps["shop/cart_other.go"] == "" ||
		!slices.Equal(r.Reads, []string{"shop/cart_other.go", "shop/testdata/a.json"}) {
		t.Fatalf("%+v", r)
	}
	l := ListRecordOf(pkg, wt, env, "c1", time.Now(), data)
	if l.Deps["shop/testdata/a.json"] == "" {
		t.Errorf("list record deps %v", l.Deps)
	}
	o.Skipped = true
	if r := RecordOf(o, wt, repo, env, "c1", time.Now(), data); r.Complete || !strings.Contains(r.Incomplete, "test skipped") {
		t.Errorf("skipped: complete=%v %q", r.Complete, r.Incomplete)
	}
}

func TestWithPathFirst(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	env := withPathFirst([]string{"A=1", "PATH=/usr/bin"}, dir)
	if env[len(env)-1] != "PATH="+dir+string(os.PathListSeparator)+"/usr/bin" || len(env) != 2 {
		t.Errorf("%v", env)
	}
	if runtime.GOOS == "windows" {
		env = withPathFirst([]string{"Path=C:\\x"}, dir)
		if env[0] != "Path="+dir+";C:\\x" {
			t.Errorf("%v", env)
		}
	}
	if env := withPathFirst([]string{"A=1"}, ""); len(env) != 1 {
		t.Errorf("%v", env)
	}
}

// The recording's extra environment reaches the probe and the fingerprint.
func TestProbeAndFingerprintExtraEnv(t *testing.T) {
	p := Probe{Vars: map[string]string{"GOVERSION": "go1.25.1"}}
	if Fingerprint(p, nil, nil) == Fingerprint(p, nil, []string{"GOEXPERIMENT=zz"}) {
		t.Error("extra environment does not change the fingerprint")
	}
	if GOFLAGS([]string{"GOFLAGS=-tags=x"}) != "-tags=x" {
		t.Error("GOFLAGS of the extra environment")
	}
	goExe, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go")
	}
	pr, err := RunProbe(t.Context(), goExe, t.TempDir(), []string{"GOFLAGS=-tags=bgraphprobe"})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Get("GOFLAGS") != "-tags=bgraphprobe" {
		t.Errorf("probe GOFLAGS %q", pr.Get("GOFLAGS"))
	}
}

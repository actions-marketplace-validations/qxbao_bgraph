package godyn

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

var baseFiles = map[string]string{
	"go.mod":               "module example.com/shop\n\ngo 1.25\n",
	"go.sum":               "",
	"shop/cart.go":         cartGo,
	"shop/price.go":        "package shop\n\nfunc Price(x int) int {\n\treturn x * 2\n}\n",
	"shop/cart_test.go":    "package shop\n",
	"shop/price_test.go":   "package shop\n",
	"shop/testdata/a.json": "{}\n",
	"README.md":            "# shop\n",
}

const unit = "go:shop"

func with(edits map[string]string) *tree.MapTree {
	m := map[string]string{}
	for k, v := range baseFiles {
		m[k] = v
	}
	for k, v := range edits {
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return tree.NewMapTree(m)
}

const env = "env-1"

// records returns a usable function-list record and function records recorded on base: TestTotal executed Cart.Total (lines
// 15-21 of cart.go), TestCount executed Cart.Count (line 23), TestPrice executed Price (price.go 3-5).
func records(base tree.Tree) []dynamic.Record {
	at := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	deps := func(files ...string) map[string]string {
		m := map[string]string{}
		for _, f := range files {
			m[f] = base.Key(f)
		}
		return m
	}
	all := []string{"go.mod", "go.sum", "shop/cart.go", "shop/price.go", "shop/cart_test.go", "shop/price_test.go"}
	fn := func(name, cart, price string) dynamic.Record {
		return dynamic.Record{Eco: Eco, Unit: SubID(unit, name), Collector: CollectorID, Env: env, Complete: true, Pass: true,
			Recorded: at, Deps: deps(all...), LineCov: map[string]string{"shop/cart.go": cart, "shop/price.go": price}}
	}
	return []dynamic.Record{
		{Eco: Eco, Unit: unit, Collector: CollectorID, Env: env, Complete: true, Pass: true, Recorded: at,
			Classes: []string{"TestCount", "TestPrice", "TestTotal"}, Deps: deps("shop/cart_test.go", "shop/price_test.go")},
		fn("TestTotal", "15-20|23,27,31-34,37", "|4"),
		fn("TestCount", "23|15-20,27,31-34,37", "|4"),
		fn("TestPrice", "|15-20,23,27,31-34,37", "4|"),
	}
}

func goTest(id string) model.SelectedTest {
	return model.SelectedTest{Unit: id, Project: ".", Runner: "go", Files: []string{"<" + id + ">"}}
}

func sel(changed ...string) model.Selection {
	return model.Selection{Scope: model.TierGraph, Changed: changed, Tests: []model.SelectedTest{goTest(unit)}}
}

func change(p string) dynamic.Change {
	c := dynamic.Change{Path: p, Status: 'M', OldMode: "100644", NewMode: "100644"}
	switch {
	case strings.HasSuffix(p, "_test.go"):
		c.BaseRole, c.HeadRole = model.RoleTest, model.RoleTest
	case strings.HasSuffix(p, ".go") && !strings.Contains(p, "testdata/"):
		c.BaseRole, c.HeadRole = model.RoleSource, model.RoleSource
	case strings.Contains(p, "testdata/"):
		c.BaseRole, c.HeadRole = model.RoleData, model.RoleData
	}
	return c
}

// run classifies, refines and narrows; it returns the selection and the guard's refusal ("" when the guard passed).
func run(t *testing.T, s model.Selection, changes []dynamic.Change, base, head tree.Tree, recs []dynamic.Record, tainted map[string]bool) (model.Selection, string) {
	t.Helper()
	ch, why := Guard(s, changes)
	if why != "" {
		return s, why
	}
	ch, why = Refine(ch, base, head)
	if why != "" {
		return s, why
	}
	return Narrow(s, Params{Env: env, Records: recs, Base: base, UnitEco: map[string]string{unit: Eco}, Changed: ch, Tainted: tainted}), ""
}

func narrowed(s model.Selection) []string {
	var out []string
	for _, n := range s.Narrowed {
		out = append(out, n.Unit)
	}
	return out
}

func TestNarrowBodyChange(t *testing.T) {
	base := with(nil)
	recs := records(base)
	head := with(map[string]string{"shop/cart.go": strings.Replace(cartGo, "n += x", "n += x * 1", 1)})
	s, why := run(t, sel("shop/cart.go"), []dynamic.Change{change("shop/cart.go")}, base, head, recs, nil)
	if why != "" {
		t.Fatal(why)
	}
	got := s.Tests[0]
	if !got.ClassFilter || !slices.Equal(got.Classes, []string{"TestTotal"}) ||
		!slices.Equal(narrowed(s), []string{"go:shop#TestCount", "go:shop#TestPrice"}) {
		t.Fatalf("classes %v narrowed %v (%s)", got.Classes, narrowed(s), got.Dynamic)
	}
	// A function nobody ran: every function is removed, the package still compiles.
	head = with(map[string]string{"shop/cart.go": strings.Replace(cartGo, "func Empty() {}", "func Empty() { _ = 0 }", 1)})
	s, _ = run(t, sel("shop/cart.go"), []dynamic.Change{change("shop/cart.go")}, base, head, recs, nil)
	if got := s.Tests[0]; !got.ClassFilter || len(got.Classes) != 0 || len(s.Narrowed) != 3 || !strings.Contains(got.Dynamic, "-run '^$'") {
		t.Fatalf("Empty: %+v narrowed %v", got, narrowed(s))
	}
	// A documentation file nothing embeds.
	head = with(map[string]string{"README.md": "# shop!\n"})
	s, why = run(t, sel("README.md"), []dynamic.Change{change("README.md")}, base, head, recs, nil)
	if why != "" || len(s.Narrowed) != 3 {
		t.Fatalf("README: %s %v", why, narrowed(s))
	}
}

// Fault injection: change kinds the guard refuses. None may narrow.
func TestGuardRefuses(t *testing.T) {
	type tc struct {
		name string
		c    dynamic.Change
		want string
	}
	mod := func(f func(*dynamic.Change)) dynamic.Change {
		c := change("shop/cart.go")
		f(&c)
		return c
	}
	cases := []tc{
		{"addition", mod(func(c *dynamic.Change) { c.Status = 'A' }), "status A"},
		{"deletion", mod(func(c *dynamic.Change) { c.Status = 'D' }), "status D"},
		{"mode change", mod(func(c *dynamic.Change) { c.NewMode = "100755" }), "mode changed"},
		{"symlink", mod(func(c *dynamic.Change) { c.OldMode, c.NewMode = "120000", "120000" }), "not a regular file"},
		{"read through a link", mod(func(c *dynamic.Change) { c.Linked = true }), "symbolic link"},
		{"go.mod", dynamic.Change{Path: "go.mod", Status: 'M', OldMode: "100644", NewMode: "100644", Claimed: true}, "claimed"},
		{"go.sum", dynamic.Change{Path: "go.sum", Status: 'M', OldMode: "100644", NewMode: "100644", Claimed: true}, "claimed"},
		{"go.work", dynamic.Change{Path: "go.work", Status: 'M', OldMode: "100644", NewMode: "100644", Global: true}, "global"},
		{"vendor file", dynamic.Change{Path: "vendor/modules.txt", Status: 'M', OldMode: "100644", NewMode: "100644", Claimed: true}, "claimed"},
		{"vendored .go", change("vendor/example.com/x/x.go"), "vendor"},
		{"assembly", dynamic.Change{Path: "shop/sum_amd64.s", Status: 'M', OldMode: "100644", NewMode: "100644", BaseRole: model.RoleSource, HeadRole: model.RoleSource}, "not a Go source file"},
		{"testdata file", change("shop/testdata/a.json"), "not a Go source file"},
		{".go under testdata", change("shop/testdata/prog/main.go"), "testdata"},
		{".go under _tools", change("_tools/gen.go"), "testdata, vendor"},
		{"[[config]] file", mod(func(c *dynamic.Change) { c.Declared = true }), "[[config]]"},
		{"[[rule]] path", mod(func(c *dynamic.Change) { c.Rule = true }), "[[rule]]"},
		{"global input", mod(func(c *dynamic.Change) { c.Global = true }), "global"},
		{"no Go role", mod(func(c *dynamic.Change) { c.BaseRole = "" }), "not source"},
		{"referenced documentation", dynamic.Change{Path: "README.md", Status: 'M', OldMode: "100644", NewMode: "100644", Referenced: true}, "not a Go source file"},
	}
	for _, c := range cases {
		_, why := Guard(sel(c.c.Path), []dynamic.Change{c.c})
		if !strings.Contains(why, c.want) {
			t.Errorf("%s: why %q, want %q", c.name, why, c.want)
		}
	}
	// A rename arrives as a deletion and an addition.
	if _, why := Guard(sel("shop/cart.go", "shop/basket.go"), []dynamic.Change{mod(func(c *dynamic.Change) { c.Status = 'D' }),
		{Path: "shop/basket.go", Status: 'A', NewMode: "100644"}}); why == "" {
		t.Error("rename accepted")
	}
	s := sel("shop/cart.go")
	s.Widening = []model.Widening{{Tier: model.TierModule, Reason: model.ReasonManifestChanged, Path: "go.mod", Eco: "go"}}
	if _, why := Guard(s, []dynamic.Change{change("shop/cart.go")}); !strings.Contains(why, "widening") {
		t.Errorf("widening: %q", why)
	}
	s = sel("shop/cart.go")
	s.Scope = model.TierFull
	if _, why := Guard(s, []dynamic.Change{change("shop/cart.go")}); !strings.Contains(why, "full") {
		t.Errorf("full: %q", why)
	}
	// The body guard refuses declaration-level changes (TestBodyGuardRefuses lists them): Refine refuses the selection.
	base := with(nil)
	head := with(map[string]string{"shop/cart.go": strings.Replace(cartGo, "const Limit = 10", "const Limit = 11", 1)})
	if _, why := run(t, sel("shop/cart.go"), []dynamic.Change{change("shop/cart.go")}, base, head, records(base), nil); !strings.Contains(why, "outside function bodies") {
		t.Errorf("const: %q", why)
	}
	head = with(map[string]string{"shop/cart.go": strings.Replace(cartGo, "n += x", "n +=", 1)})
	if _, why := run(t, sel("shop/cart.go"), []dynamic.Change{change("shop/cart.go")}, base, head, records(base), nil); !strings.Contains(why, "does not parse") {
		t.Errorf("unparsable head: %q", why)
	}
}

// Fault injection: records and situations that keep the whole package or a function. None may narrow the kept part.
func TestNarrowKeeps(t *testing.T) {
	base := with(nil)
	bodyHead := with(map[string]string{"shop/cart.go": strings.Replace(cartGo, "n += x", "n += x * 1", 1)})
	ch := []dynamic.Change{change("shop/cart.go")}
	mutate := func(f func([]dynamic.Record) []dynamic.Record) []dynamic.Record { return f(records(base)) }
	byUnit := func(recs []dynamic.Record, u string, f func(*dynamic.Record)) []dynamic.Record {
		for i := range recs {
			if recs[i].Unit == u {
				f(&recs[i])
			}
		}
		return recs
	}
	whole := func(name string, s model.Selection, want string) {
		t.Helper()
		if len(s.Narrowed) != 0 || s.Tests[0].ClassFilter || !strings.Contains(s.Tests[0].Dynamic, want) {
			t.Errorf("%s: narrowed %v filter %v note %q (want %q)", name, narrowed(s), s.Tests[0].ClassFilter, s.Tests[0].Dynamic, want)
		}
	}
	keptCount := func(name string, s model.Selection) {
		t.Helper()
		if !slices.Contains(s.Tests[0].Classes, "TestCount") || slices.Contains(narrowed(s), "go:shop#TestCount") {
			t.Errorf("%s: TestCount removed (classes %v)", name, s.Tests[0].Classes)
		}
	}

	// A stale list record (a test file differs at the base).
	recs := mutate(func(r []dynamic.Record) []dynamic.Record {
		return byUnit(r, unit, func(x *dynamic.Record) { x.Deps["shop/cart_test.go"] = "0000" })
	})
	s, _ := run(t, sel("shop/cart.go"), ch, base, bodyHead, recs, nil)
	whole("stale list", s, "stale")
	// Environment mismatch.
	recs = mutate(func(r []dynamic.Record) []dynamic.Record {
		for i := range r {
			r[i].Env = "other"
		}
		return r
	})
	s, _ = run(t, sel("shop/cart.go"), ch, base, bodyHead, recs, nil)
	whole("env", s, "no function-list record for the current environment")
	// Another collector version.
	recs = mutate(func(r []dynamic.Record) []dynamic.Record {
		return byUnit(r, unit, func(x *dynamic.Record) { x.Collector = "go-cover-0" })
	})
	s, _ = run(t, sel("shop/cart.go"), ch, base, bodyHead, recs, nil)
	whole("collector", s, "collector")
	// A test file added to the package since the list record.
	added := with(map[string]string{"shop/new_test.go": "package shop\n"})
	s, _ = run(t, sel("shop/cart.go"), ch, added, bodyHead, records(base), nil)
	whole("added test file", s, "was not there when recorded")
	// An empty function list.
	recs = mutate(func(r []dynamic.Record) []dynamic.Record {
		return byUnit(r, unit, func(x *dynamic.Record) { x.Classes = nil })
	})
	s, _ = run(t, sel("shop/cart.go"), ch, base, bodyHead, recs, nil)
	whole("empty list", s, "no test function listed")
	// A changed test file keeps the whole package.
	testHead := with(map[string]string{"shop/cart_test.go": "package shop\n\n// x\n"})
	s, _ = run(t, sel("shop/cart_test.go"), []dynamic.Change{change("shop/cart_test.go")}, base, testHead, records(base), nil)
	whole("changed test file", s, "test file shop/cart_test.go changed")
	// The static graph reaches the unit through a fixture or program-building edge.
	s, _ = run(t, sel("shop/cart.go"), ch, base, bodyHead, records(base), map[string]bool{unit: true})
	whole("tainted", s, "fixture, embed, cgo or program-building edge")
	// Previously failed.
	f := sel("shop/cart.go")
	f.AlwaysRun = []string{unit}
	s, _ = run(t, f, ch, base, bodyHead, records(base), nil)
	whole("always run", s, "always run")
	f.AlwaysRun = []string{"go:shop#TestCount"}
	s, _ = run(t, f, ch, base, bodyHead, records(base), nil)
	keptCount("always run sub-unit", s)

	// Per function: Count's record is unusable or keeps it.
	cases := map[string]func(*dynamic.Record){
		"incomplete (unobserved process)": func(x *dynamic.Record) {
			x.Complete, x.Incomplete = false, "starts a process the collector does not observe (1 started, 0 instrumented processes besides the test wrote coverage)"
		},
		"failed alone":         func(x *dynamic.Record) { x.Pass = false },
		"stale dependency":     func(x *dynamic.Record) { x.Deps["shop/price.go"] = "0000" },
		"dependency gone":      func(x *dynamic.Record) { x.Deps["shop/gone.go"] = "0000" },
		"no coverage of file":  func(x *dynamic.Record) { delete(x.LineCov, "shop/cart.go") },
		"no block in function": func(x *dynamic.Record) { x.LineCov["shop/cart.go"] = "23|27" },
		"malformed table":      func(x *dynamic.Record) { x.LineCov["shop/cart.go"] = "x" },
		"other environment":    func(x *dynamic.Record) { x.Env = "other" },
	}
	for name, f := range cases {
		recs := mutate(func(r []dynamic.Record) []dynamic.Record { return byUnit(r, "go:shop#TestCount", f) })
		s, _ := run(t, sel("shop/cart.go"), ch, base, bodyHead, recs, nil)
		keptCount(name, s)
	}
	// No record for TestCount at all.
	recs = slices.DeleteFunc(records(base), func(r dynamic.Record) bool { return r.Unit == "go:shop#TestCount" })
	s, _ = run(t, sel("shop/cart.go"), ch, base, bodyHead, recs, nil)
	keptCount("no record", s)

	// Line shift: a line added inside Total moves Count and Describe; TestCount executed line 23 (below the change).
	shiftHead := with(map[string]string{"shop/cart.go": strings.Replace(cartGo, "n := 0", "n := 0\n\tn += 0", 1)})
	s, _ = run(t, sel("shop/cart.go"), ch, base, shiftHead, records(base), nil)
	keptCount("line shift", s)
	if !slices.Contains(narrowed(s), "go:shop#TestPrice") {
		t.Errorf("line shift: TestPrice (no line of cart.go executed) kept: %v", s.Tests[0].Classes)
	}
	// A comment-only change that shifts lines keeps what executed below it, and nothing above.
	commentHead := with(map[string]string{"shop/cart.go": strings.Replace(cartGo, "//go:noinline", "// Describe describes.\n//go:noinline", 1)})
	s, _ = run(t, sel("shop/cart.go"), ch, base, commentHead, records(base), nil)
	if !slices.Equal(narrowed(s), []string{"go:shop#TestCount", "go:shop#TestPrice", "go:shop#TestTotal"}) {
		t.Errorf("comment below every executed line: kept %v", s.Tests[0].Classes)
	}
	commentHead = with(map[string]string{"shop/cart.go": strings.Replace(cartGo, "// Total sums", "// x\n// Total sums", 1)})
	s, _ = run(t, sel("shop/cart.go"), ch, base, commentHead, records(base), nil)
	if !slices.Equal(s.Tests[0].Classes, []string{"TestCount", "TestTotal"}) {
		t.Errorf("comment above Total and Count: kept %v", s.Tests[0].Classes)
	}
	// A documentation file a test binary embeds.
	recs = mutate(func(r []dynamic.Record) []dynamic.Record {
		return byUnit(r, "go:shop#TestCount", func(x *dynamic.Record) { x.Deps["README.md"] = base.Key("README.md") })
	})
	s, _ = run(t, sel("README.md"), []dynamic.Change{change("README.md")}, base, with(map[string]string{"README.md": "# x\n"}), recs, nil)
	keptCount("embedded documentation", s)
}

func TestPlainEdge(t *testing.T) {
	for _, c := range []struct {
		e    model.Edge
		want bool
	}{
		{model.Edge{Kind: model.EdgeImport, Spec: "example.com/x"}, true},
		{model.Edge{Kind: model.EdgeImplicit, Spec: "package file"}, true},
		{model.Edge{Kind: model.EdgeImplicit, Spec: "go test compiles the package"}, true},
		{model.Edge{Kind: model.EdgeImplicit, Spec: "module ."}, true},
		{model.Edge{Kind: model.EdgeImplicit, Spec: "module-level file"}, true},
		{model.Edge{Kind: model.EdgeImplicit, Spec: "test may build package ../cmd/x"}, false},
		{model.Edge{Kind: model.EdgeImplicit, Spec: "test builds packages ../..."}, false},
		{model.Edge{Kind: model.EdgeImplicit, Spec: "may run the program built from cmd/x (../x)"}, false},
		{model.Edge{Kind: model.EdgeFixtureData, Spec: "../x/a.go"}, false},
		{model.Edge{Kind: model.EdgePattern, Spec: "//go:embed *.go"}, false},
		{model.Edge{Kind: model.EdgePattern, Spec: "cgo"}, false},
		{model.Edge{Kind: model.EdgeSymlink}, false},
	} {
		if PlainEdge(c.e) != c.want {
			t.Errorf("%+v: %v", c.e, !c.want)
		}
	}
}

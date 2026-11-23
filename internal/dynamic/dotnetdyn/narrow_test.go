package dotnetdyn

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

func treeOf(files map[string]string) *tree.MapTree { return tree.NewMapTree(files) }

var baseFiles = map[string]string{
	"Lib.sln":                       "",
	"src/Lib/Lib.csproj":            "<Project Sdk=\"Microsoft.NET.Sdk\"/>",
	"src/Lib/Foo.cs":                "namespace Lib;\npublic class Foo\n{\n    public int F()\n    {\n        return 1;\n    }\n}\n",
	"src/Lib/Bar.cs":                "namespace Lib;\npublic class Bar\n{\n    public int G()\n    {\n        return 2;\n    }\n}\n",
	"src/Lib/data.json":             "{}",
	"test/T/T.csproj":               "<Project Sdk=\"Microsoft.NET.Sdk\"><ItemGroup><PackageReference Include=\"xunit\" Version=\"2.0.0\" /></ItemGroup></Project>",
	"test/T/FooTests.cs":            "namespace T;\npublic class FooTests { }\n",
	"test/T/BarTests.cs":            "namespace T;\npublic class BarTests { }\n",
	"docs/guide.md":                 "# guide\n",
	"src/Lib/PublicAPI.Shipped.txt": "",
}

const unitT = "dotnet:test/T/T.csproj"

func with(files map[string]string, edits map[string]string) *tree.MapTree {
	m := map[string]string{}
	for k, v := range files {
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

func change(p string, role model.Role) dynamic.Change {
	return dynamic.Change{Path: p, Status: 'M', OldMode: "100644", NewMode: "100644", BaseRole: role, HeadRole: role}
}

func sel(changed ...string) model.Selection {
	return model.Selection{Scope: model.TierModule, Changed: changed, IndexStats: map[string]int{},
		Tests: []model.SelectedTest{{Unit: unitT, Project: "test/T", Runner: "dotnet", Package: "test/T/T.csproj"}}}
}

// recordsAt builds the records a recording at tree t would make: FooTests executes Foo.cs, BarTests executes Bar.cs.
func recordsAt(t tree.Tree, env string) []dynamic.Record {
	m := &Mapper{Root: "/repo", Tree: t}
	cov := func(lib string, test string) *Coverage {
		return &Coverage{
			Hit:        map[string]bool{"/repo/src/Lib/" + lib: true, "/repo/test/T/" + test: true},
			Lines:      map[string][]int{"/repo/src/Lib/Foo.cs": {5, 6}, "/repo/src/Lib/Bar.cs": {5, 6}, "/repo/test/T/" + test: {2}},
			Classes:    map[string]bool{"T." + strings.TrimSuffix(test, ".cs"): true},
			ClassFiles: map[string][]string{"T." + strings.TrimSuffix(test, ".cs"): {"/repo/test/T/" + test}},
		}
	}
	o := Outcome{Unit: Unit{ID: unitT, Project: "test/T/T.csproj", Dir: "test/T"}, Frameworks: []string{"net8.0"},
		Unrecorded: map[string]string{}, Classes: map[string]TestOutcome{"T.FooTests": {1, 0}, "T.BarTests": {1, 0}},
		Runs: []ClassRun{
			{Class: "T.FooTests", Framework: "net8.0", Coverage: cov("Foo.cs", "FooTests.cs"), Outcome: TestOutcome{1, 0}, Reported: true},
			{Class: "T.BarTests", Framework: "net8.0", Coverage: cov("Bar.cs", "BarTests.cs"), Outcome: TestOutcome{1, 0}, Reported: true},
		}}
	return Records(m, o, env, "c0", time.Unix(1000, 0))
}

func TestRecords(t *testing.T) {
	base := treeOf(baseFiles)
	recs := recordsAt(base, "env")
	if len(recs) != 3 {
		t.Fatalf("%d records", len(recs))
	}
	by := map[string]dynamic.Record{}
	for _, r := range recs {
		by[r.Unit] = r
		if !r.Complete || !r.Pass {
			t.Errorf("%s: complete=%v pass=%v %s", r.Unit, r.Complete, r.Pass, r.Incomplete)
		}
	}
	foo := by[unitT+"#T.FooTests"]
	if _, ok := foo.Deps["src/Lib/Foo.cs"]; !ok || len(foo.Deps) != 2 {
		t.Errorf("FooTests deps %v", foo.Deps)
	}
	cl := by[unitT]
	if !slices.Equal(cl.Classes, []string{"T.BarTests", "T.FooTests"}) || len(cl.Deps) != 3 {
		t.Errorf("class list %v deps %v", cl.Classes, cl.Deps)
	}
	if v := cl.Lines["src/Lib/Foo.cs"]; v != base.Key("src/Lib/Foo.cs")+"|5-6" {
		t.Errorf("lines %q", v)
	}
}

func TestRecordsIncomplete(t *testing.T) {
	base := treeOf(baseFiles)
	m := &Mapper{Root: "/repo", Tree: base}
	o := Outcome{Unit: Unit{ID: unitT, Project: "test/T/T.csproj", Dir: "test/T"}, Frameworks: []string{"net8.0", "net481"},
		Unrecorded: map[string]string{}, Classes: map[string]TestOutcome{"T.FooTests": {1, 0}, "T.BarTests": {1, 0}, "T.GenTests": {1, 0}},
		Runs: []ClassRun{
			// No line of the class itself: the canary fails.
			{Class: "T.FooTests", Framework: "net8.0", Coverage: &Coverage{Hit: map[string]bool{"/repo/src/Lib/Foo.cs": true}, Classes: map[string]bool{}}, Outcome: TestOutcome{1, 0}, Reported: true},
			{Class: "T.FooTests", Framework: "net481", Why: "no TRX report"},
			// An untracked repository file and a generated one.
			{Class: "T.BarTests", Framework: "net8.0", Coverage: &Coverage{Hit: map[string]bool{"/repo/test/T/BarTests.cs": true, "/repo/scratch.cs": true}, Classes: map[string]bool{"T.BarTests": true}, ClassFiles: map[string][]string{"T.BarTests": {"/repo/test/T/BarTests.cs"}}}, Outcome: TestOutcome{1, 0}, Reported: true},
			{Class: "T.BarTests", Framework: "net481", Coverage: &Coverage{Hit: map[string]bool{"/repo/test/T/BarTests.cs": true}, Classes: map[string]bool{"T.BarTests": true}, ClassFiles: map[string][]string{"T.BarTests": {"/repo/test/T/BarTests.cs"}}}, Outcome: TestOutcome{1, 1}, Reported: true},
			{Class: "T.GenTests", Framework: "net8.0", Coverage: &Coverage{Hit: map[string]bool{"/_/src/Lib/obj/Debug/net8.0/Gen.g.cs": true, "/_/test/T/FooTests.cs": true}, Classes: map[string]bool{"T.GenTests": true}, ClassFiles: map[string][]string{"T.GenTests": {"/_/test/T/FooTests.cs"}}}, Outcome: TestOutcome{1, 0}, Reported: true},
			{Class: "T.GenTests", Framework: "net481", Coverage: &Coverage{Hit: map[string]bool{"/repo/test/T/FooTests.cs": true}, Classes: map[string]bool{"T.GenTests": true}, ClassFiles: map[string][]string{"T.GenTests": {"/repo/test/T/FooTests.cs"}}}, Outcome: TestOutcome{1, 0}, Reported: true},
		}}
	by := map[string]dynamic.Record{}
	for _, r := range Records(m, o, "env", "c0", time.Unix(1000, 0)) {
		by[r.Unit] = r
	}
	foo := by[unitT+"#T.FooTests"]
	if foo.Complete || foo.Pass || !strings.Contains(foo.Incomplete, "no executed line of the class itself in a repository file") || !strings.Contains(foo.Incomplete, "no TRX") {
		t.Errorf("FooTests: %+v", foo)
	}
	bar := by[unitT+"#T.BarTests"]
	if bar.Complete || bar.Pass || !strings.Contains(bar.Incomplete, "untracked repository file scratch.cs") {
		t.Errorf("BarTests: %+v", bar)
	}
	gen := by[unitT+"#T.GenTests"]
	if !gen.Complete || !gen.Pass {
		t.Errorf("GenTests: %+v", gen)
	}
	for _, f := range []string{"src/Lib/Foo.cs", "src/Lib/Bar.cs", "src/Lib/Lib.csproj", "test/T/FooTests.cs"} {
		if _, ok := gen.Deps[f]; !ok {
			t.Errorf("GenTests misses %s (generated source → every file of its project): %v", f, gen.Deps)
		}
	}
	// A framework that could not run, an unsafe reference: the class list is incomplete.
	o.Unrecorded["net481"] = "no runtime"
	files := maps(baseFiles)
	files["test/T/T.csproj"] = `<Project><ItemGroup><PackageReference Include="Microsoft.CodeAnalysis.CSharp.Workspaces" Version="4.0.0" /></ItemGroup></Project>`
	m.Tree = treeOf(files)
	for _, r := range Records(m, o, "env", "c0", time.Unix(1000, 0)) {
		if r.Unit == unitT && (r.Complete || !strings.Contains(r.Incomplete, "net481 not recorded") || !strings.Contains(r.Incomplete, "Microsoft.CodeAnalysis")) {
			t.Errorf("class list: %+v", r)
		}
	}
}

func maps(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestGuardNonCode(t *testing.T) {
	base := treeOf(baseFiles)
	cases := []struct {
		name  string
		edits map[string]string
		ch    dynamic.Change
		want  string // substring of the refusal; "" = allowed
	}{
		{"data file", map[string]string{"src/Lib/data.json": "{\"a\":1}"}, change("src/Lib/data.json", model.RoleData), "file reads are not observed"},
		{"doc outside projects", map[string]string{"docs/guide.md": "# guide 2\n"}, change("docs/guide.md", ""), ""},
		{"public API baseline", map[string]string{"src/Lib/PublicAPI.Shipped.txt": "X\n"}, func() dynamic.Change {
			c := change("src/Lib/PublicAPI.Shipped.txt", model.RoleData)
			c.Referenced, c.Claimed = true, true
			return c
		}(), ""},
		{"added C# file", map[string]string{"src/Lib/New.cs": "class N {}"}, dynamic.Change{Path: "src/Lib/New.cs", Status: 'A', NewMode: "100644", HeadRole: model.RoleSource}, "status A"},
		{"claimed", nil, func() dynamic.Change { c := change("test/T/T.csproj", model.RoleManifest); c.Claimed = true; return c }(), "claimed"},
	}
	for _, c := range cases {
		head := with(baseFiles, c.edits)
		_, why := Guard(sel(c.ch.Path), []dynamic.Change{c.ch}, base, head)
		if c.want == "" && why != "" || c.want != "" && !strings.Contains(why, c.want) {
			t.Errorf("%s: why = %q, want %q", c.name, why, c.want)
		}
	}
	// A doc file a C# file names, or inside a project directory, is refused.
	files := maps(baseFiles)
	files["test/T/ReadmeTests.cs"] = `class R { string p = "guide.md"; }`
	b2 := treeOf(files)
	if _, why := Guard(sel("docs/guide.md"), []dynamic.Change{change("docs/guide.md", "")}, b2, b2); !strings.Contains(why, "named by test/T/ReadmeTests.cs") {
		t.Errorf("named doc: %q", why)
	}
	files = maps(baseFiles)
	files["src/Lib/notes.md"] = "x"
	b3 := treeOf(files)
	if _, why := Guard(sel("src/Lib/notes.md"), []dynamic.Change{change("src/Lib/notes.md", "")}, b3, b3); !strings.Contains(why, "inside the project directory src/Lib") {
		t.Errorf("doc in project: %q", why)
	}
	// Floating versions refuse everything.
	files = maps(baseFiles)
	files["Directory.Packages.props"] = `<Project><ItemGroup><PackageVersion Include="X" Version="1.*" /></ItemGroup></Project>`
	b4 := treeOf(files)
	if _, why := Guard(sel("docs/guide.md"), []dynamic.Change{change("docs/guide.md", "")}, b4, b4); !strings.Contains(why, "floating") {
		t.Errorf("floating: %q", why)
	}
	// A build definition widening refuses.
	s := sel("docs/guide.md")
	s.Widening = []model.Widening{{Eco: Eco, Reason: "manifest", Path: "test/T/T.csproj", Tier: model.TierModule}}
	if _, why := Guard(s, []dynamic.Change{change("docs/guide.md", "")}, base, base); !strings.Contains(why, "build definition") {
		t.Errorf("widening: %q", why)
	}
}

func TestInstrumented(t *testing.T) {
	base := treeOf(baseFiles)
	maps := NewLineMaps(recordsAt(base, "env"))
	ok := Guarded{"src/Lib/Foo.cs": {{From: 4, To: 7, What: "method F"}}, "src/Lib/Bar.cs": nil}
	if why := Instrumented(ok, maps, base); why != "" {
		t.Errorf("instrumented member refused: %s", why)
	}
	excluded := Guarded{"src/Lib/Foo.cs": {{From: 1, To: 3, What: "method X"}}}
	if why := Instrumented(excluded, maps, base); !strings.Contains(why, "method X has no instrumented line") {
		t.Errorf("uninstrumented member: %q", why)
	}
	invisible := Guarded{"src/Lib/Other.cs": {{From: 1, To: 3, What: "method Y"}}}
	if why := Instrumented(invisible, maps, base); !strings.Contains(why, "no record lists") {
		t.Errorf("invisible file: %q", why)
	}
	// The map is tied to the base content of the file.
	head := with(baseFiles, map[string]string{"src/Lib/Foo.cs": baseFiles["src/Lib/Foo.cs"] + "\n"})
	if why := Instrumented(ok, maps, head); !strings.Contains(why, "no record lists") {
		t.Errorf("stale map: %q", why)
	}
}

func TestNarrow(t *testing.T) {
	base := treeOf(baseFiles)
	recs := recordsAt(base, "env")
	head := with(baseFiles, map[string]string{"src/Lib/Foo.cs": strings.Replace(baseFiles["src/Lib/Foo.cs"], "return 1", "return 3", 1)})
	p := Params{Env: "env", Records: recs, Base: base, Head: head, UnitEco: map[string]string{unitT: Eco}}
	out := Narrow(sel("src/Lib/Foo.cs"), p)
	tt := out.Tests[0]
	if !tt.ClassFilter || !slices.Equal(tt.Classes, []string{"T.FooTests"}) {
		t.Fatalf("test %+v", tt)
	}
	if len(out.Narrowed) != 1 || out.Narrowed[0].Unit != unitT+"#T.BarTests" || out.IndexStats["narrowed_classes"] != 1 {
		t.Errorf("narrowed %+v %v", out.Narrowed, out.IndexStats)
	}
	// Another environment: nothing narrowed.
	p.Env = "other"
	if out := Narrow(sel("src/Lib/Foo.cs"), p); out.Tests[0].ClassFilter || !strings.Contains(out.Tests[0].Dynamic, "no class-list record") {
		t.Errorf("other env: %+v", out.Tests[0])
	}
	// A file added to the test project since the recording: the class list is not usable.
	p.Env = "env"
	files := maps(baseFiles)
	files["test/T/NewTests.cs"] = "namespace T; public class NewTests {}"
	p.Base = treeOf(files)
	if out := Narrow(sel("src/Lib/Foo.cs"), p); out.Tests[0].ClassFilter || !strings.Contains(out.Tests[0].Dynamic, "class list not usable") {
		t.Errorf("drift: %+v", out.Tests[0])
	}
	// A failed class record keeps the class.
	p.Base = base
	for i := range p.Records {
		if p.Records[i].Unit == unitT+"#T.BarTests" {
			p.Records[i].Pass = false
		}
	}
	if out := Narrow(sel("src/Lib/Foo.cs"), p); out.Tests[0].ClassFilter {
		t.Errorf("failed record narrowed: %+v", out.Tests[0])
	}
	// Microsoft.Testing.Platform projects narrow like VSTest ones; whole units are not handled.
	s := sel("src/Lib/Foo.cs")
	s.Tests[0].Config = "global.json"
	if out := Narrow(s, Params{Env: "env", Records: recordsAt(base, "env"), Base: base, Head: head, UnitEco: p.UnitEco}); !out.Tests[0].ClassFilter {
		t.Errorf("MTP unit not narrowed: %+v", out.Tests[0])
	}
	s = sel("src/Lib/Foo.cs")
	s.Tests[0].Whole = true
	if out := Narrow(s, Params{Env: "env", Records: recordsAt(base, "env"), Base: base, Head: head, UnitEco: p.UnitEco}); out.Tests[0].Dynamic != "" {
		t.Errorf("whole unit touched: %+v", out.Tests[0])
	}
}

func TestGuardCode(t *testing.T) {
	base := treeOf(baseFiles)
	maps := NewLineMaps(recordsAt(base, "env"))
	body := with(baseFiles, map[string]string{"src/Lib/Foo.cs": strings.Replace(baseFiles["src/Lib/Foo.cs"], "return 1", "return 3", 1)})
	g, why := Guard(sel("src/Lib/Foo.cs"), []dynamic.Change{change("src/Lib/Foo.cs", model.RoleSource)}, base, body)
	if why != "" || len(g["src/Lib/Foo.cs"]) != 1 {
		t.Fatalf("body change: %q %v", why, g)
	}
	if why := Instrumented(g, maps, base); why != "" {
		t.Errorf("body change not instrumented: %s", why)
	}
	sig := with(baseFiles, map[string]string{"src/Lib/Foo.cs": strings.Replace(baseFiles["src/Lib/Foo.cs"], "public int F()", "public long F()", 1)})
	if _, why := Guard(sel("src/Lib/Foo.cs"), []dynamic.Change{change("src/Lib/Foo.cs", model.RoleSource)}, base, sig); why == "" {
		t.Error("signature change allowed")
	}
	if _, why := Guard(sel("src/Lib/Foo.cs"), []dynamic.Change{change("src/Lib/Foo.cs", model.RoleData)}, base, body); !strings.Contains(why, "not source or test") {
		t.Errorf("role: %q", why)
	}
}

// TestMapperPathMap: "/_/" names are the repository only when the tree has the file or it is generated under one of its
// projects (deterministic builds of NuGet packages use the same path map).
func TestMapperPathMap(t *testing.T) {
	m := &Mapper{Root: "/repo", Tree: treeOf(baseFiles)}
	for name, want := range map[string]place{
		"/_/src/Lib/Foo.cs":                  placeTracked,
		"/_/src/Lib/obj/Debug/net8.0/G.g.cs": placeGenerated,
		"/_/src/Events.cs":                   placeOutside,
		"/_/src/Other/obj/G.g.cs":            placeOutside,
		"/repo/src/Lib/obj/G.g.cs":           placeGenerated,
		"/repo/scratch.cs":                   placeUntracked,
		"/elsewhere/x.cs":                    placeOutside,
	} {
		if _, got := m.rel(name); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

// TestRecordsPerFramework: a class that exists on some frameworks only is recorded from those (its absence elsewhere is
// not a failure).
func TestRecordsPerFramework(t *testing.T) {
	m := &Mapper{Root: "/repo", Tree: treeOf(baseFiles)}
	cov := &Coverage{Hit: map[string]bool{"/repo/test/T/FooTests.cs": true}, Classes: map[string]bool{"T.FooTests": true},
		ClassFiles: map[string][]string{"T.FooTests": {"/repo/test/T/FooTests.cs"}}}
	o := Outcome{Unit: Unit{ID: unitT, Project: "test/T/T.csproj", Dir: "test/T"}, Frameworks: []string{"net48", "net8.0"},
		Unrecorded: map[string]string{}, Classes: map[string]TestOutcome{"T.FooTests": {1, 0}},
		On:   map[string][]string{"T.FooTests": {"net8.0"}},
		Runs: []ClassRun{{Class: "T.FooTests", Framework: "net8.0", Coverage: cov, Outcome: TestOutcome{1, 0}, Reported: true}}}
	for _, r := range Records(m, o, "env", "c0", time.Unix(1000, 0)) {
		if !r.Complete || !r.Pass {
			t.Errorf("%s: %+v", r.Unit, r)
		}
	}
}

// TestGeneratedProjects: generated sources map to their project in the default and the artifacts layouts; otherwise the record
// is incomplete.
func TestGeneratedProjects(t *testing.T) {
	m := &Mapper{Root: "/repo", Tree: treeOf(baseFiles)}
	for r, want := range map[string]string{
		"src/Lib/obj/Debug/net8.0/G.g.cs":     "src/Lib",
		"artifacts/obj/Lib/debug/G.g.cs":      "src/Lib",
		"artifacts/obj/T/debug_net8.0/G.g.cs": "test/T",
		"artifacts/obj/Unknown/debug/G.g.cs":  "",
		"src/Other/obj/G.g.cs":                "",
	} {
		dirs, ok := m.generatedProjects(r)
		if want == "" && ok || want != "" && (!ok || len(dirs) != 1 || dirs[0] != want) {
			t.Errorf("%s: %v %v, want %q", r, dirs, ok, want)
		}
	}
	deps, why := m.deps(&Coverage{Hit: map[string]bool{"/repo/artifacts/obj/Unknown/debug/G.g.cs": true}})
	if len(deps) != 0 || len(why) != 1 || !strings.Contains(why[0], "no known project") {
		t.Errorf("unknown generated: %v %v", deps, why)
	}
}

func TestBuildTimeProject(t *testing.T) {
	files := maps(baseFiles)
	files["gen/Gen.csproj"] = `<Project><PropertyGroup><IsRoslynComponent>true</IsRoslynComponent></PropertyGroup></Project>`
	files["gen/G.cs"] = "class G { void F() { } }"
	files["ana/Ana.csproj"] = `<Project><ItemGroup><PackageReference Include="Microsoft.CodeAnalysis.CSharp" Version="4.0.0" /></ItemGroup></Project>`
	files["ana/A.cs"] = "class A { }"
	files["task/Task.csproj"] = `<Project />`
	files["task/T.cs"] = "class T { }"
	files["src/Lib/Lib.csproj"] = `<Project><ItemGroup><ProjectReference Include="..\..\task\Task.csproj" OutputItemType="Analyzer" ReferenceOutputAssembly="false" /></ItemGroup></Project>`
	tr := treeOf(files)
	for p, want := range map[string]string{"gen/G.cs": "runs at build time", "ana/A.cs": "source generator or analyzer",
		"task/T.cs": "referenced as an analyzer by src/Lib/Lib.csproj", "src/Lib/Foo.cs": ""} {
		got := buildTimeProject(tr, p)
		if want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Errorf("%s: %q, want %q", p, got, want)
		}
	}
}

func TestNarrowSafety(t *testing.T) {
	base := treeOf(baseFiles)
	head := with(baseFiles, map[string]string{"src/Lib/Foo.cs": strings.Replace(baseFiles["src/Lib/Foo.cs"], "return 1", "return 3", 1)})
	mk := func() Params {
		return Params{Env: "env", Records: recordsAt(base, "env"), Base: base, Head: head, UnitEco: map[string]string{unitT: Eco},
			Guarded: Guarded{"src/Lib/Foo.cs": {{From: 4, To: 7, What: "method F"}}}}
	}
	if out := Narrow(sel("src/Lib/Foo.cs"), mk()); !out.Tests[0].ClassFilter {
		t.Fatalf("baseline not narrowed: %+v", out.Tests[0])
	}
	// The unit's own maps do not instrument the member.
	p := mk()
	p.Guarded = Guarded{"src/Lib/Foo.cs": {{From: 1, To: 2, What: "method X"}}}
	if out := Narrow(sel("src/Lib/Foo.cs"), p); out.Tests[0].ClassFilter || !strings.Contains(out.Tests[0].Dynamic, "no instrumented line") {
		t.Errorf("uninstrumented member: %+v", out.Tests[0])
	}
	// A class whose own run did not report it: no proof the filter runs it.
	p = mk()
	for i := range p.Records {
		if p.Records[i].Unit == unitT+"#T.BarTests" {
			p.Records[i].Complete, p.Records[i].Incomplete = false, "net8.0: the class was not reported in its own run"
		}
	}
	if out := Narrow(sel("src/Lib/Foo.cs"), p); out.Tests[0].ClassFilter || !strings.Contains(out.Tests[0].Dynamic, "no proof") {
		t.Errorf("unproven class: %+v", out.Tests[0])
	}
	// A type declared by a changed file of the project is kept even when the class list does not have it.
	files := maps(baseFiles)
	files["test/T/FooTests.cs"] = "namespace T;\npublic class FooTests { }\npublic class NewTests { }\n"
	p = mk()
	p.Head = treeOf(files)
	s := sel("src/Lib/Foo.cs", "test/T/FooTests.cs")
	out := Narrow(s, p)
	if !slices.Equal(out.Tests[0].Classes, []string{"T.FooTests", "T.NewTests"}) {
		t.Errorf("declared type dropped: %+v", out.Tests[0])
	}
}

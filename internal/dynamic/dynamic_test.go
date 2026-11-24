package dynamic

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
)

const (
	coll = "python-audit-1"
	env  = "env-A"
)

var t0 = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

// base is the blob of every file at the base.
var base = map[string]string{
	"pkg/a.py":           "oa",
	"pkg/b.py":           "ob",
	"pkg/loader.py":      "ol",
	"tests/conftest.py":  "oc",
	"tests/test_a.py":    "ota",
	"tests/test_b.py":    "otb",
	"tests/test_load.py": "otl",
}

func rec(unit string, deps ...string) Record {
	m := map[string]string{unit: base[unit]}
	for _, d := range deps {
		m[d] = base[d]
	}
	return Record{Eco: "python", Unit: unit, Collector: coll, Commit: "c0", Deps: m, Env: env, Complete: true, Pass: true, Recorded: t0}
}

func mod(p string) Change {
	return Change{Path: p, Status: 'M', OldMode: "100644", NewMode: "100644", BaseRole: model.RoleSource, HeadRole: model.RoleSource}
}

// fixture: pkg/a.py changed; test_a is precise; test_b and test_load are widened (dynamic import in pkg/loader.py).
func fixture(changes ...Change) (model.Selection, []Change) {
	if len(changes) == 0 {
		changes = []Change{mod("pkg/a.py")}
	}
	sel := model.Selection{Scope: model.TierGraph,
		Widening: []model.Widening{{Tier: model.TierModule, Reason: model.ReasonDynamicImport, Path: "pkg/loader.py", Scope: "."}},
		Tests: []model.SelectedTest{
			{Unit: "tests/test_a.py", Files: []string{"tests/test_a.py"}, Runner: "pytest"},
			{Unit: "tests/test_b.py", Files: []string{"tests/test_b.py"}, Runner: "pytest", Widened: true},
			{Unit: "tests/test_load.py", Files: []string{"tests/test_load.py"}, Runner: "pytest", Widened: true},
		},
		IndexStats: map[string]int{"units": 3},
	}
	for _, c := range changes {
		sel.Changed = append(sel.Changed, c.Path)
	}
	return sel, changes
}

func params(recs ...Record) Params {
	return Params{Eco: "python", Collector: coll, Env: env, BaseOID: func(p string) string { return base[p] }, Records: recs,
		UnitEco: map[string]string{"tests/test_a.py": "python", "tests/test_b.py": "python", "tests/test_load.py": "python"}}
}

func units(sel model.Selection) []string {
	var out []string
	for _, t := range sel.Tests {
		out = append(out, t.Unit)
	}
	return out
}

func narrowed(sel model.Selection) []string {
	var out []string
	for _, t := range sel.Narrowed {
		out = append(out, t.Unit)
	}
	return out
}

// goodRecords: test_b never loads pkg/a.py, test_load does.
func goodRecords() []Record {
	return []Record{
		rec("tests/test_a.py", "pkg/a.py"),
		rec("tests/test_b.py", "pkg/b.py", "tests/conftest.py"),
		rec("tests/test_load.py", "pkg/loader.py", "pkg/a.py", "tests/conftest.py"),
	}
}

func TestNarrowPositive(t *testing.T) {
	sel, ch := fixture()
	out := Narrow(sel, ch, params(goodRecords()...))
	if got := units(out); !slices.Equal(got, []string{"tests/test_a.py", "tests/test_load.py"}) {
		t.Fatalf("tests = %v", got)
	}
	if got := narrowed(out); !slices.Equal(got, []string{"tests/test_b.py"}) {
		t.Fatalf("narrowed = %v", got)
	}
	if n := out.Narrowed[0]; n.Commit != "c0" || !n.Recorded.Equal(t0) || n.Reason == "" {
		t.Errorf("narrowed entry: %+v", n)
	}
	if !strings.Contains(out.Tests[1].Dynamic, "pkg/a.py") {
		t.Errorf("kept unit note: %q", out.Tests[1].Dynamic)
	}
	if out.Tests[0].Dynamic != "" {
		t.Errorf("precise unit got a note: %q", out.Tests[0].Dynamic)
	}
	if out.IndexStats["narrowed"] != 1 || sel.IndexStats["narrowed"] != 0 {
		t.Errorf("index stats: %v / input %v", out.IndexStats, sel.IndexStats)
	}
	if len(sel.Narrowed) != 0 || len(sel.Tests) != 3 {
		t.Error("input selection was modified")
	}
}

// A precise unit is never removed, even with a record that says it does not load the changed file.
func TestNarrowNeverRemovesPrecise(t *testing.T) {
	sel, ch := fixture()
	recs := goodRecords()
	recs[0] = rec("tests/test_a.py") // claims test_a does not load pkg/a.py
	out := Narrow(sel, ch, params(recs...))
	if !slices.Contains(units(out), "tests/test_a.py") {
		t.Fatal("precise unit removed")
	}
}

// Every widened unit removed and nothing precise: scope none (an empty non-none scope would mean "run everything").
func TestNarrowAllRemovedScopeNone(t *testing.T) {
	sel, ch := fixture()
	sel.Tests = sel.Tests[1:2]
	out := Narrow(sel, ch, params(goodRecords()...))
	if len(out.Tests) != 0 || out.Scope != model.TierNone {
		t.Fatalf("tests %v scope %s", units(out), out.Scope)
	}
}

// Units of other ecosystems and whole-project units are never removed.
func TestNarrowOtherEcoAndWhole(t *testing.T) {
	sel, ch := fixture()
	p := params(goodRecords()...)
	p.UnitEco["tests/test_b.py"] = "node"
	sel.Tests = append(sel.Tests, model.SelectedTest{Unit: "<py-all-tests:.>", Whole: true, Widened: true})
	p.UnitEco["<py-all-tests:.>"] = "python"
	out := Narrow(sel, ch, p)
	for _, u := range []string{"tests/test_b.py", "<py-all-tests:.>"} {
		if !slices.Contains(units(out), u) {
			t.Errorf("%s removed", u)
		}
	}
}

// A changed conftest.py (source) in a widened unit's recorded dependencies keeps the unit.
func TestNarrowConftestInDeps(t *testing.T) {
	sel, ch := fixture(mod("tests/conftest.py"))
	out := Narrow(sel, ch, params(goodRecords()...))
	if len(out.Narrowed) != 0 {
		t.Fatalf("narrowed %v", narrowed(out))
	}
}

// Guards of docs/dynamic-layer.md §2: each forbidden change kind skips narrowing for the whole selection.
func TestGuards(t *testing.T) {
	stub := mod("pkg/a.pyi")
	config := Change{Path: "pytest.ini", Status: 'M', OldMode: "100644", NewMode: "100644", BaseRole: model.RoleConfig, HeadRole: model.RoleConfig, Claimed: true}
	lock := Change{Path: "uv.lock", Status: 'M', OldMode: "100644", NewMode: "100644", BaseRole: model.RoleLock, HeadRole: model.RoleLock, Claimed: true}
	setup := mod("setup.py")
	setup.BaseRole, setup.HeadRole, setup.Claimed = model.RoleManifest, model.RoleManifest, true
	noxfile := mod("noxfile.py")
	noxfile.Claimed = true
	declared := mod("pkg/b.py")
	declared.Declared = true
	global := mod("pkg/b.py")
	global.Global = true
	added := mod("pkg/new.py")
	added.Status, added.OldMode = 'A', ""
	deleted := mod("pkg/b.py")
	deleted.Status, deleted.NewMode = 'D', ""
	renamedFrom := deleted
	renamedTo := mod("pkg/b2.py")
	renamedTo.Status, renamedTo.OldMode = 'A', ""
	typeChange := mod("pkg/b.py")
	typeChange.Status, typeChange.NewMode = 'T', "120000"
	modeOnly := mod("pkg/b.py")
	modeOnly.NewMode = "100755"
	symlink := mod("pkg/b.py")
	symlink.OldMode, symlink.NewMode = "120000", "120000"
	submodule := mod("vendor/x")
	submodule.OldMode, submodule.NewMode = "160000", "160000"
	data := mod("tests/data.json")
	data.Path = "tests/data.json"
	pyx := mod("pkg/fast.pyx")
	unclaimed := mod("pkg/b.py")
	unclaimed.BaseRole = ""
	cases := map[string][]Change{
		"add":         {mod("pkg/a.py"), added},
		"delete":      {mod("pkg/a.py"), deleted},
		"rename":      {renamedFrom, renamedTo},
		"pyi":         {stub},
		"config":      {mod("pkg/a.py"), config},
		"lockfile":    {lock},
		"manifest":    {setup},
		"claimed .py": {noxfile},
		"declared":    {declared},
		"global file": {global},
		"type change": {typeChange},
		"mode only":   {modeOnly},
		"symlink":     {symlink},
		"submodule":   {submodule},
		"data file":   {data},
		"pyx":         {pyx},
		"no role":     {unclaimed},
	}
	for name, chs := range cases {
		t.Run(name, func(t *testing.T) {
			sel, ch := fixture(chs...)
			out := Narrow(sel, ch, params(goodRecords()...))
			assertSkipped(t, out)
		})
	}
	t.Run("global widening", func(t *testing.T) {
		sel, ch := fixture()
		sel.Widening = append(sel.Widening, model.Widening{Tier: model.TierModule, Reason: model.ReasonGlobalInput, Path: ".env"})
		assertSkipped(t, Narrow(sel, ch, params(goodRecords()...)))
	})
	t.Run("full widening", func(t *testing.T) {
		sel, ch := fixture()
		sel.Widening = append(sel.Widening, model.Widening{Tier: model.TierFull, Reason: model.ReasonUserRule, Path: "x"})
		assertSkipped(t, Narrow(sel, ch, params(goodRecords()...)))
	})
	t.Run("full scope", func(t *testing.T) {
		sel, ch := fixture()
		sel.Scope = model.TierFull
		out := Narrow(sel, ch, params(goodRecords()...))
		assertSkipped(t, out)
		if out.Scope != model.TierFull {
			t.Error("scope changed")
		}
	})
	t.Run("unclassified changes", func(t *testing.T) {
		sel, _ := fixture()
		assertSkipped(t, Narrow(sel, nil, params(goodRecords()...)))
	})
	t.Run("unsupported eco", func(t *testing.T) {
		sel, ch := fixture()
		p := params(goodRecords()...)
		p.Eco = "node"
		for u := range p.UnitEco {
			p.UnitEco[u] = "node"
		}
		assertSkipped(t, Narrow(sel, ch, p))
	})
	t.Run("allowed", func(t *testing.T) {
		test := mod("tests/test_load.py")
		test.BaseRole, test.HeadRole = model.RoleTest, model.RoleTest
		exe := mod("pkg/b.py")
		exe.OldMode, exe.NewMode = "100755", "100755"
		sel, ch := fixture(mod("pkg/a.py"), test, exe, mod("tests/conftest.py"))
		if why := Guard(sel, ch, "python"); why != "" {
			t.Fatal(why)
		}
	})
}

func assertSkipped(t *testing.T, out model.Selection) {
	t.Helper()
	if len(out.Narrowed) != 0 || len(out.Tests) != 3 {
		t.Errorf("narrowed %v, tests %v", narrowed(out), units(out))
	}
	if !slices.ContainsFunc(out.Warnings, func(w model.Widening) bool { return w.Reason == model.ReasonDynamicSkipped && w.Detail != "" }) {
		t.Errorf("no dynamic_skipped warning: %+v", out.Warnings)
	}
}

// Record usability (docs/dynamic-layer.md §3): each defect keeps test_b.
func TestUnusableRecords(t *testing.T) {
	mk := func(f func(r *Record)) []Record {
		recs := goodRecords()
		f(&recs[1])
		return recs
	}
	cases := map[string]struct {
		recs []Record
		p    func(p *Params)
		note string
	}{
		"stale blob":        {recs: mk(func(r *Record) { r.Deps["pkg/b.py"] = "old" }), note: "differs at the base"},
		"missing dep":       {recs: mk(func(r *Record) { r.Deps["pkg/gone.py"] = "og" }), note: "does not exist at the base"},
		"env mismatch":      {recs: mk(func(r *Record) { r.Env = "env-B" }), note: "other environments"},
		"incomplete":        {recs: mk(func(r *Record) { r.Complete, r.Incomplete = false, "subprocess" }), note: "subprocess"},
		"failed":            {recs: mk(func(r *Record) { r.Pass = false }), note: "did not pass"},
		"collector version": {recs: mk(func(r *Record) { r.Collector = "python-audit-0" }), note: "collector"},
		"own file missing":  {recs: mk(func(r *Record) { delete(r.Deps, "tests/test_b.py") }), note: "own file"},
		"own file stale":    {recs: mk(func(r *Record) { r.Deps["tests/test_b.py"] = "x" }), note: "differs"},
		"no record":         {recs: goodRecords()[:1], note: "no record"},
		"current env changed": {recs: goodRecords(), p: func(p *Params) { p.Env = "env-B" },
			note: "other environments"},
		"trust setting changed": {recs: goodRecords(), p: func(p *Params) { p.Collector = coll + "+trust-native" }, note: "collector"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			sel, ch := fixture()
			p := params(c.recs...)
			if c.p != nil {
				c.p(&p)
			}
			out := Narrow(sel, ch, p)
			if len(out.Narrowed) != 0 {
				t.Fatalf("narrowed %v", narrowed(out))
			}
			for _, tt := range out.Tests {
				if tt.Unit == "tests/test_b.py" && !strings.Contains(tt.Dynamic, c.note) {
					t.Errorf("note %q does not mention %q", tt.Dynamic, c.note)
				}
			}
		})
	}
}

// The latest record per (unit, env) wins, even when an older one would have allowed narrowing.
func TestLatestRecordWins(t *testing.T) {
	old := rec("tests/test_b.py", "pkg/b.py")
	latest := rec("tests/test_b.py", "pkg/b.py", "pkg/a.py")
	latest.Recorded = t0.Add(time.Hour)
	other := rec("tests/test_b.py")
	other.Env = "env-B"
	other.Recorded = t0.Add(2 * time.Hour)
	for _, order := range [][]Record{{old, latest, other}, {other, latest, old}} {
		got, ok, others := Pick(order, "python", "tests/test_b.py", env)
		if !ok || !got.Recorded.Equal(latest.Recorded) || others != 1 {
			t.Fatalf("pick: %+v %v %d", got, ok, others)
		}
		sel, ch := fixture()
		if out := Narrow(sel, ch, params(order...)); slices.Contains(narrowed(out), "tests/test_b.py") {
			t.Fatal("older record used")
		}
	}
	m := Merge([]Record{old, other}, []Record{latest})
	if len(m) != 2 {
		t.Fatalf("merge: %d records", len(m))
	}
	// An older record imported later does not replace a newer one.
	m = Merge([]Record{latest}, []Record{old})
	if len(m) != 1 || !m[0].Recorded.Equal(latest.Recorded) {
		t.Fatalf("merge kept %+v", m)
	}
}

func TestDeclared(t *testing.T) {
	rf, err := rules.Parse("bgraph.toml", []byte(`
[[dynamic]]
file = "pkg/loader.py"
none = true
reason = "x"
[[config]]
file = "vitest.config.ts"
replaces = ["alias"]
sources = ["tools/alias.js"]
reason = "y"
`))
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{"pkg/loader.py": true, "vitest.config.ts": true, "tools/alias.js": true, "pkg/a.py": false} {
		if Declared(rf, p) != want {
			t.Errorf("Declared(%s) != %v", p, want)
		}
	}
	if Declared(nil, "pkg/loader.py") {
		t.Error("nil rules")
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	file := StorePath(dir, "python")
	if recs, warn := Load(file); recs != nil || warn != nil {
		t.Fatalf("missing store: %v %v", recs, warn)
	}
	r1 := rec("tests/test_a.py", "pkg/a.py")
	r2 := rec("tests/test_b.py", "pkg/b.py")
	if _, err := Add(file, []Record{r1}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(file, []Record{r2}); err != nil {
		t.Fatal(err)
	}
	recs, warn := Load(file)
	if len(recs) != 2 || len(warn) != 0 {
		t.Fatalf("load: %d records, %v", len(recs), warn)
	}
	if recs[0].Deps["pkg/a.py"] != "oa" || !recs[0].Recorded.Equal(t0) || !recs[0].Complete || !recs[0].Pass {
		t.Errorf("round trip: %+v", recs[0])
	}
	// Corrupt and invalid lines are skipped with a warning; the good ones survive.
	b, _ := os.ReadFile(file)
	b = append(b, []byte("{not json\n{\"eco\":\"python\",\"unit\":\"x\"}\n{\"eco\":\"python\",\"unit\":\"y\",\"collector\":\"c\",\"env\":\"e\",\"deps\":{\"../x\":\"o\"}}\n")...)
	os.WriteFile(file, b, 0o644)
	recs, warn = Load(file)
	if len(recs) != 2 || len(warn) != 3 {
		t.Fatalf("corrupt: %d records, warnings %v", len(recs), warn)
	}
	// Add rewrites without the corrupt lines and leaves no temporary file behind.
	if _, err := Add(file, nil); err != nil {
		t.Fatal(err)
	}
	if recs, warn = Load(file); len(recs) != 2 || len(warn) != 0 {
		t.Fatalf("after rewrite: %d %v", len(recs), warn)
	}
	ents, _ := os.ReadDir(filepath.Dir(file))
	if len(ents) != 1 {
		t.Errorf("store dir: %v", ents)
	}
	// An unreadable store (a directory in its place) is a warning, not an error.
	bad := filepath.Join(dir, "dir.jsonl")
	os.Mkdir(bad, 0o755)
	if recs, warn := Load(bad); recs != nil || len(warn) != 1 {
		t.Errorf("unreadable: %v %v", recs, warn)
	}
	if _, err := Add(file, []Record{{Eco: "python"}}); err == nil {
		t.Error("invalid record accepted")
	}
}

func TestFingerprint(t *testing.T) {
	a := Fingerprint(coll, []string{"os=Windows", "dist=pytest==9.2.0"}, map[string]string{"pyproject.toml": "o1"})
	b := Fingerprint(coll, []string{"dist=pytest==9.2.0", "os=Windows"}, map[string]string{"pyproject.toml": "o1"})
	if a != b {
		t.Error("order-sensitive")
	}
	for _, other := range []string{
		Fingerprint(coll+"+trust-native", []string{"os=Windows", "dist=pytest==9.2.0"}, map[string]string{"pyproject.toml": "o1"}),
		Fingerprint(coll, []string{"os=Windows", "dist=pytest==9.2.1"}, map[string]string{"pyproject.toml": "o1"}),
		Fingerprint(coll, []string{"os=Windows", "dist=pytest==9.2.0"}, map[string]string{"pyproject.toml": "o2"}),
		Fingerprint(coll, []string{"os=Windows", "dist=pytest==9.2.0"}, map[string]string{"pyproject.toml": "o1", "uv.lock": "o3"}),
	} {
		if other == a {
			t.Error("fingerprint does not change")
		}
	}
}

func docChange(p string) Change {
	return Change{Path: p, Status: 'M', OldMode: "100644", NewMode: "100644"}
}

// docs/dynamic-layer.md §2: a modified, unreferenced documentation file next to a modified .py file allows narrowing, and still counts
// as a changed file for the per-unit check.
func TestGuardDocFile(t *testing.T) {
	sel, ch := fixture(mod("pkg/a.py"), docChange("CHANGES.rst"), docChange("docs/usage.md"))
	out := Narrow(sel, ch, params(goodRecords()...))
	if got := narrowed(out); !slices.Equal(got, []string{"tests/test_b.py"}) {
		t.Fatalf("narrowed %v, warnings %+v", got, out.Warnings)
	}
	// A unit whose record lists the doc file is kept.
	recs := goodRecords()
	recs[1].Deps["CHANGES.rst"] = "ocl"
	base["CHANGES.rst"] = "ocl"
	defer delete(base, "CHANGES.rst")
	out = Narrow(sel, ch, params(recs...))
	if len(out.Narrowed) != 0 {
		t.Fatalf("doc dependency ignored: narrowed %v", narrowed(out))
	}
	for _, tt := range out.Tests {
		if tt.Unit == "tests/test_b.py" && !strings.Contains(tt.Dynamic, "CHANGES.rst") {
			t.Errorf("note %q", tt.Dynamic)
		}
	}
	// Doc files that are referenced, named by bgraph.toml, claimed, given a role, global or not modified still block.
	for name, f := range map[string]func(c *Change){
		"referenced":   func(c *Change) { c.Referenced = true },
		"rule":         func(c *Change) { c.Rule = true },
		"declared":     func(c *Change) { c.Declared = true },
		"claimed":      func(c *Change) { c.Claimed = true },
		"other role":   func(c *Change) { c.OtherRole = true },
		"eco role":     func(c *Change) { c.BaseRole, c.HeadRole = model.RoleData, model.RoleData },
		"global":       func(c *Change) { c.Global = true },
		"added":        func(c *Change) { c.Status, c.OldMode = 'A', "" },
		"deleted":      func(c *Change) { c.Status, c.NewMode = 'D', "" },
		"mode changed": func(c *Change) { c.NewMode = "100755" },
	} {
		t.Run(name, func(t *testing.T) {
			d := docChange("README.md")
			f(&d)
			sel, ch := fixture(mod("pkg/a.py"), d)
			assertSkipped(t, Narrow(sel, ch, params(goodRecords()...)))
		})
	}
	// Not documentation: a JSON data file or a YAML config is still refused.
	for _, p := range []string{"tests/data/x.json", ".pre-commit-config.yaml", "uv.lock"} {
		sel, ch := fixture(mod("pkg/a.py"), docChange(p))
		assertSkipped(t, Narrow(sel, ch, params(goodRecords()...)))
	}
}

// TestGuardNodeSources: the source kinds the Node collector observes (docs/dynamic-layer.md §8.4); every other kind is refused.
func TestGuardNodeSources(t *testing.T) {
	sel := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{{Unit: "test/a.test.ts", Widened: true}}}
	for p, ok := range map[string]bool{
		"src/a.js": true, "src/a.mjs": true, "src/a.cjs": true, "src/a.jsx": true, "src/a.ts": true, "src/a.tsx": true,
		"src/a.mts": true, "src/a.cts": true,
		"src/a.d.ts": false, "src/a.d.mts": false, "src/a.d.cts": false, "src/A.vue": false, "src/A.svelte": false,
		"src/A.astro": false, "src/a.mdx": false, "src/data.json": false, "src/a.py": false,
	} {
		sel.Changed = []string{p}
		why := Guard(sel, []Change{mod(p)}, "node")
		if (why == "") != ok {
			t.Errorf("%s: guard %q, want pass=%v", p, why, ok)
		}
	}
	// A config, manifest or lockfile role is refused even with a JS extension.
	for _, c := range []Change{
		{Path: "vitest.config.ts", Status: 'M', OldMode: "100644", NewMode: "100644", BaseRole: model.RoleConfig, HeadRole: model.RoleConfig},
		{Path: "babel.config.js", Status: 'M', OldMode: "100644", NewMode: "100644", BaseRole: model.RoleConfig, HeadRole: model.RoleConfig},
		{Path: "package.json", Status: 'M', OldMode: "100644", NewMode: "100644", BaseRole: model.RoleManifest, HeadRole: model.RoleManifest},
		{Path: "package-lock.json", Status: 'M', OldMode: "100644", NewMode: "100644", BaseRole: model.RoleLock, HeadRole: model.RoleLock},
		{Path: "tsconfig.json", Status: 'M', OldMode: "100644", NewMode: "100644", BaseRole: model.RoleConfig, HeadRole: model.RoleConfig},
		{Path: "src/new.ts", Status: 'A', NewMode: "100644", HeadRole: model.RoleSource},
		{Path: "src/old.ts", Status: 'D', OldMode: "100644", BaseRole: model.RoleSource},
	} {
		sel.Changed = []string{c.Path}
		if why := Guard(sel, []Change{c}, "node"); why == "" {
			t.Errorf("%s (%c): guard passed", c.Path, c.Status)
		}
	}
}

// TestRecordFormatCompat: store lines written before the Ruby fields existed still decode, and the Ruby fields round-trip.
func TestRecordFormatCompat(t *testing.T) {
	old := `{"eco":"python","unit":"tests/test_a.py","collector":"python-audit-1","deps":{"tests/test_a.py":"abc"},"env":"e","complete":true,"pass":true,"recorded":"2026-11-01T00:00:00Z"}` + "\n"
	recs, warn := Parse("old", []byte(old))
	if len(warn) > 0 || len(recs) != 1 || recs[0].Methods != nil || recs[0].Reads != nil || recs[0].Deps["tests/test_a.py"] != "abc" {
		t.Fatalf("old line: %+v %v", recs, warn)
	}
	r := Record{Eco: "ruby", Unit: "spec/a_spec.rb", Collector: "ruby-cov-2", Env: "e", Complete: true, Pass: true, Recorded: t0,
		Deps:    map[string]string{"spec/a_spec.rb": "1", "lib/a.rb": "2", "data/x.yml": "3"},
		Methods: map[string]string{"lib/a.rb": "2-4:1,6-8:0", "spec/a_spec.rb": ""}, Reads: []string{"data/x.yml"}}
	b, err := Encode([]Record{r})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(old, "methods") || !strings.Contains(string(b), `"methods"`) {
		t.Fatalf("encoding: %s", b)
	}
	back, warn := Parse("new", b)
	if len(warn) > 0 || len(back) != 1 || back[0].Methods["lib/a.rb"] != "2-4:1,6-8:0" || !slices.Equal(back[0].Reads, r.Reads) {
		t.Fatalf("round trip: %+v %v", back, warn)
	}
	if _, ok := back[0].Methods["spec/a_spec.rb"]; !ok {
		t.Fatal("an empty method table was dropped")
	}
	bad := strings.Replace(string(b), `"data/x.yml"]`, `"../x"]`, 1)
	if recs, warn := Parse("bad", []byte(bad)); len(recs) != 0 || len(warn) == 0 {
		t.Fatalf("unclean read path accepted: %v", recs)
	}
	// PHP fields: the runner of a file-list record and line tables of a file record.
	p := Record{Eco: "php", Unit: ".::php#tests/ATest.php", Collector: "php-xdebug-1", Env: "e", Complete: true, Pass: true, Recorded: t0,
		Deps: map[string]string{"tests/ATest.php": "1", "src/A.php": "2"}, LineCov: map[string]string{"src/A.php": "3-5,9|6-8", "tests/ATest.php": "|"},
		Runner: "phpunit@11.5.0"}
	b, err = Encode([]Record{p})
	if err != nil {
		t.Fatal(err)
	}
	back, warn = Parse("php", b)
	if len(warn) > 0 || len(back) != 1 || back[0].LineCov["src/A.php"] != "3-5,9|6-8" || back[0].Runner != "phpunit@11.5.0" {
		t.Fatalf("php round trip: %+v %v", back, warn)
	}
	if strings.Contains(old, "line_cov") || strings.Contains(old, "runner") {
		t.Fatal("old line has php fields")
	}
	for _, bad := range []string{strings.Replace(string(b), `3-5,9|6-8`, `3-5,x`, 1), strings.Replace(string(b), `"src/A.php":"3`, `"../A.php":"3`, 1),
		strings.Replace(string(b), `phpunit@11.5.0`, `phpunit 11`, 1)} {
		if recs, warn := Parse("bad", []byte(bad)); len(recs) != 0 || len(warn) == 0 {
			t.Errorf("malformed php record accepted: %s", bad)
		}
	}
}

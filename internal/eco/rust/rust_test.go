package rust

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/tree"
)

func TestUnitIDRoundTrip(t *testing.T) {
	cases := []struct{ dir, kind, name, id string }{
		{".", KindLib, "", ".::lib"},
		{"crates/foo", KindDoc, "", "crates/foo::doc"},
		{"crates/foo", KindBin, "cli", "crates/foo::bin:cli"},
		{"crates/foo", KindTest, "it", "crates/foo::test:it"},
		{"a", KindBench, "b-1", "a::bench:b-1"},
		{"a", KindExample, "demo", "a::example:demo"},
	}
	for _, c := range cases {
		if got := UnitID(c.dir, c.kind, c.name); got != c.id {
			t.Errorf("UnitID(%q,%q,%q) = %q, want %q", c.dir, c.kind, c.name, got, c.id)
		}
		d, k, n, ok := ParseUnitID(c.id)
		if !ok || d != c.dir || k != c.kind || n != c.name {
			t.Errorf("ParseUnitID(%q) = %q %q %q %v", c.id, d, k, n, ok)
		}
	}
	for _, bad := range []string{"", "src/a.test.ts", "x::", "x::bin", "x::bin:", "x::lib:y", "::lib", "x::weird:y"} {
		if _, _, _, ok := ParseUnitID(bad); ok {
			t.Errorf("ParseUnitID(%q) should fail", bad)
		}
	}
}

type failScanner struct{ calls int }

func (f *failScanner) Scan(tree.Tree, string) (model.ScanResult, error) {
	f.calls++
	return model.ScanResult{}, errors.New("must not be called")
}

// A repo without Cargo.toml costs nothing: an empty analysis and no scanning.
func TestNoCargoTomlIsFree(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{"src/a.rs": "mod b;", "package.json": "{}", "a.ts": ""})
	sc := &failScanner{}
	a, err := New(nil).Analyze(context.Background(), tr, sc)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Projects)+len(a.Edges)+len(a.Tests)+len(a.Unresolved)+len(a.Roles) != 0 || sc.calls != 0 {
		t.Fatalf("expected an empty analysis, got %+v (scan calls %d)", a, sc.calls)
	}
}

func analyze(t *testing.T, files map[string]string) *eco.Analysis {
	t.Helper()
	a, err := New(nil).Analyze(context.Background(), tree.NewMapTree(files), scancache.New())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

var sample = map[string]string{
	"Cargo.toml":                 "[workspace]\nmembers = [\"crates/*\"]\n",
	"crates/a/Cargo.toml":        "[package]\nname = \"a\"\n",
	"crates/a/src/lib.rs":        "mod m;\n",
	"crates/a/src/m.rs":          "",
	"crates/a/src/main.rs":       "fn main() {}\n",
	"crates/a/tests/t.rs":        "",
	"crates/b/Cargo.toml":        "[package]\nname = \"b\"\n[dependencies]\na = { path = \"../a\" }\n",
	"crates/b/src/lib.rs":        "",
	"crates/b/examples/e.rs":     "",
	"crates/b/benches/x/main.rs": "",
}

func TestAnalyzeUnitsAndDeterminism(t *testing.T) {
	a := analyze(t, sample)
	var ids []string
	for _, u := range a.Tests {
		if u.Eco != Name || u.Runner != Runner || len(u.Files) != 1 || u.Files[0] != Node(u.ID) {
			t.Errorf("bad unit %+v", u)
		}
		if u.Package == "" {
			t.Errorf("unit without package: %+v", u)
		}
		ids = append(ids, u.ID)
	}
	want := []string{"crates/a::lib", "crates/a::doc", "crates/a::bin:a", "crates/a::test:t",
		"crates/b::lib", "crates/b::doc", "crates/b::bench:x", "crates/b::example:e"}
	if len(ids) != len(want) {
		t.Fatalf("units %v, want %v", ids, want)
	}
	have := map[string]bool{}
	for _, id := range ids {
		have[id] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing unit %s (have %v)", w, ids)
		}
	}
	// Deterministic edges, and gob-serializable (the analysis snapshot cache stores it with gob).
	b := analyze(t, sample)
	if !reflect.DeepEqual(a.Edges, b.Edges) {
		t.Fatal("edge order is not deterministic")
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(a); err != nil {
		t.Fatal(err)
	}
	var back eco.Analysis
	if err := gob.NewDecoder(&buf).Decode(&back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Tests, a.Tests) || len(back.Edges) != len(a.Edges) {
		t.Fatal("gob round trip changed the analysis")
	}
}

func TestKeyLevels(t *testing.T) {
	cases := map[string]keyLevel{
		"package.description": levelNone, "package.metadata": levelNone, "package.license": levelNone, "package.readme": levelNone,
		"package.version": levelPackage, "package.edition": levelPackage, "package.build": levelPackage, "package.autotests": levelPackage,
		"package.resolver": levelWorkspace,
		"lib":              levelPackage, "bin": levelPackage, "test": levelPackage, "lints": levelPackage,
		"dependencies": levelWorkspace, "dev-dependencies": levelWorkspace, "build-dependencies": levelWorkspace,
		"target": levelWorkspace, "features": levelWorkspace, "patch": levelWorkspace, "profile": levelWorkspace,
		"workspace.members": levelWorkspace, "workspace.dependencies": levelWorkspace, "workspace.metadata": levelNone,
		"workspace.package.description": levelNone, "workspace.package.version": levelWorkspace,
		"badges": levelNone, "some-future-key": levelWorkspace,
	}
	for k, want := range cases {
		if got := keyLevelOf(k); got != want {
			t.Errorf("keyLevelOf(%q) = %d, want %d", k, got, want)
		}
	}
}

func TestMalformedManifestIsReported(t *testing.T) {
	a := analyze(t, map[string]string{"Cargo.toml": "[package\n", "src/lib.rs": ""})
	if len(a.Unresolved) != 1 || a.Unresolved[0].ScopeTier != model.TierFull || a.Unresolved[0].ScopeDir != "." {
		t.Fatalf("unresolved: %+v", a.Unresolved)
	}
	if a.Roles["Cargo.toml"] != model.RoleManifest {
		t.Fatalf("roles: %+v", a.Roles)
	}
}

func TestTargetSkipsAndTargetDirIgnored(t *testing.T) {
	a := analyze(t, map[string]string{
		"Cargo.toml":                        "[package]\nname = \"p\"\n",
		"src/lib.rs":                        "",
		"target/package/p-0.1.0/Cargo.toml": "[package]\nname = \"p\"\n",
		"target/package/p-0.1.0/src/lib.rs": "",
		"node_modules/x/Cargo.toml":         "[package]\nname = \"nm\"\n",
	})
	if len(a.Projects) != 1 || a.Projects[0].Dir != "." {
		t.Fatalf("projects: %+v", a.Projects)
	}
}

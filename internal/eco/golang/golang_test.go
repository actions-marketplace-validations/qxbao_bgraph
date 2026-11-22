package golang

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/tree"
)

func TestUnitIDRoundTrip(t *testing.T) {
	for _, dir := range []string{".", "a", "internal/foo", "v2/x-y_z"} {
		id := UnitID(dir)
		got, ok := ParseUnitID(id)
		if !ok || got != dir {
			t.Errorf("ParseUnitID(%q) = %q %v", id, got, ok)
		}
	}
	if UnitID("internal/foo") != "go:internal/foo" || UnitID(".") != "go:." {
		t.Fatal("unit ID format changed")
	}
	for _, bad := range []string{"", "go:", "a/b", "go:/a", "go:../a", "go:..", "go:a//b", "go:a/", "go:./a", "rust::lib"} {
		if _, ok := ParseUnitID(bad); ok {
			t.Errorf("ParseUnitID(%q) should fail", bad)
		}
	}
}

type failScanner struct{ calls int }

func (f *failScanner) Scan(tree.Tree, string) (model.ScanResult, error) {
	f.calls++
	return model.ScanResult{}, errors.New("must not be called")
}

// A repo without go.mod costs nothing: an empty analysis and no scanning.
func TestNoGoModIsFree(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{"main.go": "package main", "package.json": "{}", "a.ts": "", "testdata/go.mod": "module x"})
	sc := &failScanner{}
	a, err := New(nil).Analyze(context.Background(), tr, sc)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Projects)+len(a.Edges)+len(a.Tests)+len(a.Unresolved)+len(a.Roles) != 0 || sc.calls != 0 {
		t.Fatalf("expected an empty analysis, got %+v (scan calls %d)", a, sc.calls)
	}
}

func TestAnalyzeShape(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{
		"go.mod":                    "module example.com/m\n\ngo 1.22\n",
		"go.sum":                    "",
		"a/a.go":                    "package a\nimport \"example.com/m/b\"\n",
		"a/a_test.go":               "package a_test\nimport (\n\"testing\"\n\"example.com/m/a\"\n\"example.com/m/c\"\n)\n",
		"a/testdata/x.json":         "{}",
		"b/b.go":                    "package b\n",
		"c/c.go":                    "package c\nimport \"example.com/m/missing\"\n",
		"vendor/ext.io/d/d.go":      "package d\n",
		"vendor/ext.io/d/d_test.go": "package d\n",
		"docs/x.md":                 "doc",
		"scripts/run.sh":            "echo",
	})
	a, err := New(nil).Analyze(context.Background(), tr, scancache.New())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Tests) != 1 || a.Tests[0].ID != "go:a" || a.Tests[0].Project != "." || a.Tests[0].Package != "example.com/m/a" ||
		!reflect.DeepEqual(a.Tests[0].Files, []string{"<go:a>"}) || a.Tests[0].Runner != "go" {
		t.Fatalf("tests: %+v", a.Tests)
	}
	has := func(from, to string, kind model.EdgeKind) bool {
		return slices.ContainsFunc(a.Edges, func(e model.Edge) bool { return e.From == from && e.To == to && e.Kind == kind })
	}
	for _, e := range []struct {
		from, to string
		kind     model.EdgeKind
	}{
		{"<go:a>", "<golib:a>", model.EdgeImplicit},
		{"<go:a>", "a/a_test.go", model.EdgeImplicit},
		{"<golib:a>", "a/a.go", model.EdgeImplicit},
		{"<golib:a>", "a/testdata/x.json", model.EdgeImplicit},
		{"<golib:a>", "<golib:b>", model.EdgeImport},
		{"<go:a>", "<golib:c>", model.EdgeImport},
		{"<golib:c>", "<goimp:example.com/m/missing>", model.EdgeImport},
		{"<goimp:example.com/m/missing>", "missing/*.go", model.EdgePattern},
		{"<golib:a>", "<gomod:.>", model.EdgeImplicit},
		{"<gomod:.>", "scripts/run.sh", model.EdgeImplicit},
	} {
		if !has(e.from, e.to, e.kind) {
			t.Errorf("missing edge %s -> %s (%s)", e.from, e.to, e.kind)
		}
	}
	for _, e := range a.Edges {
		if e.To == "docs/x.md" || e.To == "go.mod" || e.To == "go.sum" || e.From == "<golib:a>" && e.To == "<golib:c>" {
			t.Errorf("unexpected edge %+v", e)
		}
	}
	if a.Roles["go.mod"] != model.RoleManifest || a.Roles["go.sum"] != model.RoleLock || a.Roles["a/a_test.go"] != model.RoleTest ||
		a.Roles["a/a.go"] != model.RoleSource || a.Roles["a/testdata/x.json"] != model.RoleData {
		t.Errorf("roles: %v", a.Roles)
	}
	if len(a.Projects) != 1 || a.Projects[0].Dir != "." || a.Projects[0].Name != "example.com/m" {
		t.Errorf("projects: %+v", a.Projects)
	}
}

func TestParseModFile(t *testing.T) {
	src := "// comment\nmodule \"example.com/m\" // trailing\n\ngo 1.22\ntoolchain go1.23.1\n\nrequire (\n\tgolang.org/x/text v0.3.0 // indirect\n\texample.com/y v1.0.0\n)\n\n" +
		"replace example.com/y => ../y\nreplace (\n\texample.com/z v1.0.0 => example.com/z2 v1.1.0\n\texample.com/w => ./w\n)\nretract [v1.0.0, v1.0.5]\n"
	m, err := parseModFile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if m.Module() != "example.com/m" {
		t.Fatalf("module %q", m.Module())
	}
	if got, want := m.LocalReplaces(), []replacement{{"example.com/y", "../y"}, {"example.com/w", "./w"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("replaces %+v", got)
	}
	if got := m.entries["require"]; !reflect.DeepEqual(got, []string{"golang.org/x/text v0.3.0", "example.com/y v1.0.0"}) {
		t.Fatalf("require %q", got)
	}
	// Reordering, comments and layout do not count; a version does.
	m2, _ := parseModFile([]byte("module example.com/m\ngo 1.22\ntoolchain go1.23.1\nrequire example.com/y v1.0.0\nrequire golang.org/x/text v0.3.0\n" +
		"replace (\nexample.com/y => ../y\nexample.com/w => ./w\nexample.com/z v1.0.0 => example.com/z2 v1.1.0\n)\nretract v1.0.0\n"))
	if got := changedVerbs(m, m2); !reflect.DeepEqual(got, []string{"retract"}) {
		t.Fatalf("changed verbs %v", got)
	}
	m3, _ := parseModFile([]byte("module example.com/m\ngo 1.23\n"))
	if got := changedVerbs(m, m3); !slices.Contains(got, "go") || !slices.Contains(got, "require") {
		t.Fatalf("changed verbs %v", got)
	}
	for _, bad := range []string{"require (\n", "module a\nmodule b\n", ")\n", "require x (\n", "module \"x\n", "go\n", "require (\na (b)\n)\n"} {
		if _, err := parseModFile([]byte(bad)); err == nil {
			t.Errorf("%q: want a syntax error", bad)
		}
	}
	w, err := parseModFile([]byte("go 1.22\n\nuse (\n\t./a\n\t.\\b\n)\nuse ./c\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Uses(); !reflect.DeepEqual(got, []string{"./a", "./b", "./c"}) {
		t.Fatalf("uses %v", got)
	}
}

func TestForeignRoots(t *testing.T) {
	got := foreignRoots([]string{"go.mod", "main.go", "package.json", "web/package.json", "web/src/a.ts", "py/pyproject.toml",
		"svc/App.csproj", "mixed/package.json", "mixed/tools/gen.go", "node_modules/x/package.json"})
	want := map[string]bool{"web": true, "py": true, "svc": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("foreign roots %v, want %v", got, want)
	}
	if !underForeign("web/src", got) || underForeign("mixed/tools", got) || underForeign(".", got) {
		t.Fatal("underForeign")
	}
}

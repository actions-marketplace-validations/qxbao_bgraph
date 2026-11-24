package dart

import (
	"context"
	"slices"
	"testing"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/tree"
)

func analyze(t *testing.T, files map[string]string) *eco.Analysis {
	t.Helper()
	a, err := New(nil).Analyze(context.Background(), tree.NewMapTree(files), scancache.New())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func hasEdge(a *eco.Analysis, from, to string) bool {
	return slices.ContainsFunc(a.Edges, func(e model.Edge) bool { return e.From == from && e.To == to })
}

func TestWholeUnitID(t *testing.T) {
	for _, d := range []string{".", "packages/a", "a b"} {
		if id := WholeUnitID(d); !IsWholeUnitID(id) || id != "<dart-all-tests:"+d+">" {
			t.Fatalf("%q -> %q", d, id)
		}
	}
	for _, id := range []string{"test/a_test.dart", "<dart-all-tests:>", "<all-tests:.>", "<dart-all-tests:x"} {
		if IsWholeUnitID(id) {
			t.Fatalf("%q is not a whole unit ID", id)
		}
	}
}

func TestNoDart(t *testing.T) {
	a := analyze(t, map[string]string{"package.json": "{}", "src/a.ts": "", "tool/x.dart": "import 'y.dart';"})
	if len(a.Roles) != 0 || len(a.Projects) != 0 || len(a.Edges) != 0 {
		t.Fatalf("a repository without pubspec.yaml must produce nothing: %+v", a)
	}
}

func TestUnitsAndRunner(t *testing.T) {
	a := analyze(t, map[string]string{
		"pubspec.yaml":                            "name: app\ndependencies:\n  flutter:\n    sdk: flutter\n",
		"test/a_test.dart":                        "",
		"test/helper.dart":                        "",
		"test/sub/b_test.dart":                    "",
		"integration_test/app_test.dart":          "",
		"lib/x_test.dart":                         "",
		"pkgs/p/pubspec.yaml":                     "name: p\n",
		"pkgs/p/test/p_test.dart":                 "",
		"pkgs/p/integration_test/i_test.dart":     "",
		".dart_tool/x/pubspec.yaml":               "name: cached\n",
		"pkgs/p/test/fixtures/q/pubspec.yaml":     "name: q\n",
		"pkgs/p/test/fixtures/q/test/q_test.dart": "",
	})
	got := map[string]string{}
	for _, u := range a.Tests {
		got[u.ID] = u.Project + "|" + u.Runner
	}
	want := map[string]string{
		"test/a_test.dart":                        ".|flutter",
		"test/sub/b_test.dart":                    ".|flutter",
		"integration_test/app_test.dart":          ".|flutter",
		"pkgs/p/test/p_test.dart":                 "pkgs/p|dart",
		"pkgs/p/test/fixtures/q/test/q_test.dart": "pkgs/p/test/fixtures/q|dart",
	}
	if len(got) != len(want) {
		t.Fatalf("units %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("unit %s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if len(a.Projects) != 3 {
		t.Fatalf("hidden directories are not packages: %+v", a.Projects)
	}
	if a.Roles["test/helper.dart"] != model.RoleSource || a.Roles["pubspec.yaml"] != model.RoleManifest {
		t.Fatalf("roles: %v", a.Roles)
	}
}

func TestTestConfig(t *testing.T) {
	for _, c := range []struct {
		yaml   string
		opaque bool
		tests  []string
	}{
		{yaml: "", tests: []string{"test/a_test.dart"}},
		{yaml: "# only a comment\n", tests: []string{"test/a_test.dart"}},
		{yaml: "paths: [spec, test/a_test.dart]\nfilename: '*_spec.dart'\n", tests: []string{"spec/b_spec.dart", "test/a_test.dart"}},
		{yaml: "paths: spec\n", tests: []string{"test/a_test.dart"}},
		{yaml: "filename: '*_spec.dart'\n", tests: []string{"test/a_test.dart", "test/c_spec.dart"}},
		{yaml: "tags:\n  slow:\n    timeout: 2x\non_platform:\n  windows: {skip: true}\n", tests: []string{"test/a_test.dart"}},
		{yaml: "presets:\n  ci:\n    filename: '*_x.dart'\n", opaque: true},
		{yaml: "on_os:\n  linux:\n    paths: [test/linux]\n", opaque: true},
		{yaml: "include: package:x/dart_test.yaml\n", opaque: true},
		{yaml: "paths: [\n", opaque: true},
		{yaml: "- a\n", opaque: true},
		{yaml: "filename: [a]\n", opaque: true},
	} {
		files := map[string]string{"pubspec.yaml": "name: app\n", "test/a_test.dart": "", "spec/b_spec.dart": "", "test/c_spec.dart": "", "lib/d_test.dart": ""}
		if c.yaml != "" {
			files["dart_test.yaml"] = c.yaml
		}
		a := analyze(t, files)
		var ids []string
		whole := false
		for _, u := range a.Tests {
			if u.Whole {
				whole = true
				continue
			}
			ids = append(ids, u.ID)
		}
		slices.Sort(ids)
		if whole != c.opaque || (!c.opaque && !slices.Equal(ids, c.tests)) || (c.opaque && len(ids) != 0) {
			t.Errorf("%q: whole=%v units=%v, want whole=%v units=%v", c.yaml, whole, ids, c.opaque, c.tests)
		}
	}
}

// Every load-like site yields an edge or an unresolved import.
func TestLoadSitesAreNeverDropped(t *testing.T) {
	srcs := []string{
		"void f() => Isolate.spawnUri(Uri.parse('w.dart'), [], null);",
		"void f() => Isolate.spawnUri(Uri.parse('missing.dart'), [], null);",
		"void f() => Isolate.spawnUri(Uri.file('bin/w.dart'), [], null);",
		"void f() => Isolate.spawnUri(Uri.parse('package:app/w.dart'), [], null);",
		"void f() => Isolate.spawnUri(Uri.parse('package:app/nope.dart'), [], null);",
		"void f() => Isolate.spawnUri(u, [], null);",
		"void f() => Isolate.spawnUri(Uri.parse('dir/$x.dart'), [], null);",
		"final g = Isolate.spawnUri;",
		"void f() => currentMirrorSystem().isolate.loadUri(Uri.parse('w.dart'));",
		"void f() => currentMirrorSystem().isolate.loadUri(x);",
		"import 'missing.dart';",
		"import 'package:app/missing.g.dart';",
		"import 'package:flutter_gen/gen_l10n/app_localizations.dart';",
		"import 'http://example.com/a.dart';",
		"import '/abs/a.dart';",
		"import '../../../../outside.dart';",
		"import 'package:app/../../x.dart';",
	}
	for _, src := range srcs {
		a := analyze(t, map[string]string{
			"pubspec.yaml": "name: app\n", "lib/src.dart": src, "lib/w.dart": "", "bin/w.dart": "",
		})
		sc := scancache.New()
		r, _ := sc.Scan(tree.NewMapTree(map[string]string{"x.dart": src}), "x.dart")
		for _, im := range r.Imports {
			if im.Kind != model.ImportDartLoad && im.Kind != model.ImportDartImport {
				continue
			}
			if im.Kind == model.ImportDartImport && (im.Spec == "dart:io") {
				continue
			}
			edge := slices.ContainsFunc(a.Edges, func(e model.Edge) bool { return e.From == "lib/src.dart" && e.Kind != model.EdgeFixtureData })
			unres := slices.ContainsFunc(a.Unresolved, func(u model.Unresolved) bool {
				return u.File == "lib/src.dart" && u.ScopeTier != model.TierNone
			})
			if !edge && !unres {
				t.Errorf("%q: load site %+v produced neither an edge nor an unresolved import\nedges %+v\nunresolved %+v", src, im, a.Edges, a.Unresolved)
			}
			// A target that is not in the tree must never be a confident edge alone.
			if im.Spec == "missing.dart" || im.Spec == "package:app/nope.dart" {
				if !unres {
					t.Errorf("%q: missing target without an unresolved import", src)
				}
			}
		}
	}
}

func TestLiterals(t *testing.T) {
	a := analyze(t, map[string]string{
		"pkg/pubspec.yaml": "name: p\n",
		"pkg/test/a_test.dart": `void main() {
  File('test/data/x.json');
  File('data/y.json');
  File('pubspec.yaml');
  File('./tool');
  File('fixtures');
  File('../');
  File('x/../..');
  File('$root/deep/z.json');
  Directory('test/data');
  Uri.parse('package:p/w.dart');
}`,
		"pkg/test/data/x.json": "", "pkg/test/data/y.json": "", "pkg/tool": "", "pkg/fixtures/k": "",
		"pkg/lib/w.dart": "", "pkg/deep/z.json": "",
	})
	for _, to := range []string{"pkg/test/data/x.json", "pkg/test/data/y.json", "pkg/pubspec.yaml", "pkg/test/data/**", "**/deep/z.json", "pkg/lib/w.dart"} {
		if !hasEdge(a, "pkg/test/a_test.dart", to) {
			t.Errorf("missing edge to %s: %+v", to, a.Edges)
		}
	}
	for _, to := range []string{"pkg/tool", "pkg/fixtures/**", "pkg/**", "pkg/test/**"} {
		if hasEdge(a, "pkg/test/a_test.dart", to) {
			t.Errorf("unexpected edge to %s", to)
		}
	}
}

func TestClassifyHarmlessAndWorkspace(t *testing.T) {
	base := tree.NewMapTree(map[string]string{
		"pubspec.yaml":   "name: ws\nworkspace: [a, b]\n",
		"a/pubspec.yaml": "name: a\nresolution: workspace\ndescription: x\n",
		"b/pubspec.yaml": "name: b\nresolution: workspace\n",
		"c/pubspec.yaml": "name: c\ndependencies:\n  a: any\n",
		"d/pubspec.yaml": "name: d\n",
		"e/pubspec.yaml": "{{{",
	})
	head := tree.NewMapTree(map[string]string{
		"pubspec.yaml":   "name: ws\nworkspace: [a, b]\n",
		"a/pubspec.yaml": "name: a\nresolution: workspace\ndescription: y\nversion: 2.0.0\n",
		"b/pubspec.yaml": "name: b\nresolution: workspace\n",
		"c/pubspec.yaml": "name: c\ndependencies:\n  a: any\n",
		"d/pubspec.yaml": "name: d\n",
		"e/pubspec.yaml": "{{{",
	})
	e := New(nil)
	if ws, ok := e.ClassifyChange("a/pubspec.yaml", base, head, nil); !ok || len(ws) != 0 {
		t.Fatalf("harmless change: %v %+v", ok, ws)
	}
	head2 := tree.NewMapTree(map[string]string{
		"pubspec.yaml":   "name: ws\nworkspace: [a, b]\n",
		"a/pubspec.yaml": "name: a\nresolution: workspace\ndependencies:\n  x: any\n",
		"b/pubspec.yaml": "name: b\nresolution: workspace\n",
		"c/pubspec.yaml": "name: c\ndependencies:\n  a: any\n",
		"d/pubspec.yaml": "name: d\n",
		"e/pubspec.yaml": "{{{",
	})
	ws, ok := e.ClassifyChange("a/pubspec.yaml", base, head2, nil)
	var scopes []string
	for _, w := range ws {
		if w.Eco != Name || w.Tier != model.TierModule || w.Reason != model.ReasonManifestChanged {
			t.Fatalf("bad widening %+v", w)
		}
		scopes = append(scopes, w.Scope)
	}
	slices.Sort(scopes)
	// The workspace root and members share the resolution; c depends on a; e cannot be read (it may depend on a); d does not.
	if !ok || !slices.Equal(scopes, []string{".", "a", "b", "c", "e"}) {
		t.Fatalf("scopes %v", scopes)
	}
	if ws, ok := e.ClassifyChange("lib/analysis_options.yaml", base, head, nil); !ok || len(ws) != 0 {
		t.Fatalf("analysis_options: %v %+v", ok, ws)
	}
	if _, ok := e.ClassifyChange("x/dart_test.yaml", base, head, nil); ok {
		t.Fatal("dart_test.yaml outside a package root is not claimed")
	}
	empty := tree.NewMapTree(map[string]string{"package.json": "{}"})
	if _, ok := e.ClassifyChange("analysis_options.yaml", empty, empty, nil); ok {
		t.Fatal("a repository without Dart claims nothing")
	}
}

func TestIsBuildYAML(t *testing.T) {
	for n, want := range map[string]bool{"build.yaml": true, "build.debug.yaml": true, "build.a.b.yaml": false, "builds.yaml": false} {
		if isBuildYAML(n) != want {
			t.Errorf("%s: %v", n, !want)
		}
	}
}

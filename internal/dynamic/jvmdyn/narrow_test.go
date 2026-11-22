package jvmdyn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

var baseFiles = map[string]string{
	"pom.xml":                              "<project/>",
	"core/pom.xml":                         "<project/>",
	"core/src/main/java/a/Foo.java":        "package a;\npublic class Foo {\n  static final int LIMIT = 1;\n  int f() { return 1; }\n}\n",
	"core/src/main/java/a/Bar.java":        "package a;\npublic class Bar { int g() { return 2; } }\n",
	"core/src/main/java/a/Consts.java":     "package a;\npublic interface Consts {\n  int X = 1;\n}\n",
	"core/src/main/java/a/Inst.java":       "package a;\npublic class Inst {\n  final int x = 5;\n}\n",
	"core/src/main/java/a/Ann.java":        "package a;\npublic @interface Ann {\n  int v() default 1;\n}\n",
	"core/src/main/kotlin/a/K.kt":          "package a\n\nconst val K = 1\n\nfun k() = 2\n",
	"core/src/test/java/a/FooTest.java":    "package a;\npublic class FooTest {}\n",
	"core/src/test/java/a/BarTest.java":    "package a;\npublic class BarTest {}\n",
	"core/src/test/resources/data.json":    "{}\n",
	"core/src/main/resources/x.properties": "a=1\n",
	"README.md":                            "# readme\n",
}

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
	return model.Selection{Scope: model.TierModule, Changed: changed,
		Tests: []model.SelectedTest{{Unit: "maven:core", Project: "core", Runner: "maven", Files: []string{"<jvm:maven:core>"}}}}
}

// TestGuard: one case per guard (docs/dynamic.md §9.2 and the lead decisions of Phase 7).
func TestGuard(t *testing.T) {
	base := tree.NewMapTree(baseFiles)
	cases := []struct {
		name  string
		edits map[string]string
		ch    func() dynamic.Change
		sel   func(model.Selection) model.Selection
		want  string // "" passes
	}{
		{"method body", map[string]string{"core/src/main/java/a/Foo.java": strings.Replace(baseFiles["core/src/main/java/a/Foo.java"], "return 1", "return 3", 1)},
			func() dynamic.Change { return change("core/src/main/java/a/Foo.java", model.RoleSource) }, nil, ""},
		{"static final constant", map[string]string{"core/src/main/java/a/Foo.java": strings.Replace(baseFiles["core/src/main/java/a/Foo.java"], "LIMIT = 1", "LIMIT = 2", 1)},
			func() dynamic.Change { return change("core/src/main/java/a/Foo.java", model.RoleSource) }, nil, "LIMIT"},
		{"interface constant", map[string]string{"core/src/main/java/a/Consts.java": strings.Replace(baseFiles["core/src/main/java/a/Consts.java"], "X = 1", "X = 2", 1)},
			func() dynamic.Change { return change("core/src/main/java/a/Consts.java", model.RoleSource) }, nil, "X"},
		{"instance final constant", map[string]string{"core/src/main/java/a/Inst.java": strings.Replace(baseFiles["core/src/main/java/a/Inst.java"], "x = 5", "x = 6", 1)},
			func() dynamic.Change { return change("core/src/main/java/a/Inst.java", model.RoleSource) }, nil, "x"},
		{"annotation default", map[string]string{"core/src/main/java/a/Ann.java": strings.Replace(baseFiles["core/src/main/java/a/Ann.java"], "default 1", "default 2", 1)},
			func() dynamic.Change { return change("core/src/main/java/a/Ann.java", model.RoleSource) }, nil, "annotation"},
		{"Kotlin const val", map[string]string{"core/src/main/kotlin/a/K.kt": strings.Replace(baseFiles["core/src/main/kotlin/a/K.kt"], "K = 1", "K = 2", 1)},
			func() dynamic.Change { return change("core/src/main/kotlin/a/K.kt", model.RoleSource) }, nil, "const"},
		{"Kotlin function", map[string]string{"core/src/main/kotlin/a/K.kt": strings.Replace(baseFiles["core/src/main/kotlin/a/K.kt"], "k() = 2", "k() = 3", 1)},
			func() dynamic.Change { return change("core/src/main/kotlin/a/K.kt", model.RoleSource) }, nil, ""},
		{"Lombok module", map[string]string{"core/pom.xml": "<project><dependency><artifactId>lombok</artifactId></dependency></project>",
			"core/src/main/java/a/Bar.java": "package a;\npublic class Bar { int g() { return 3; } }\n"},
			func() dynamic.Change { return change("core/src/main/java/a/Bar.java", model.RoleSource) }, nil, "Lombok"},
		{"lombok.config", map[string]string{"lombok.config": "x", "core/src/main/java/a/Bar.java": "package a;\npublic class Bar { int g() { return 3; } }\n"},
			func() dynamic.Change { return change("core/src/main/java/a/Bar.java", model.RoleSource) }, nil, "Lombok"},
		{"resource", map[string]string{"core/src/main/resources/x.properties": "a=2\n"},
			func() dynamic.Change { return change("core/src/main/resources/x.properties", model.RoleData) }, nil, "non-code change"},
		{"build file", map[string]string{"core/pom.xml": "<project><x/></project>"},
			func() dynamic.Change { c := change("core/pom.xml", model.RoleManifest); c.Claimed = true; return c }, nil, "claimed"},
		{"documentation file", map[string]string{"README.md": "# changed\n"},
			func() dynamic.Change { c := change("README.md", model.RoleData); c.Referenced = true; return c }, nil, ""},
		{"documentation file under src/test/resources", map[string]string{"core/src/test/resources/notes.md": "# changed\n"},
			func() dynamic.Change { return change("core/src/test/resources/notes.md", model.RoleData) }, nil, "src/ directory"},
		{"documentation file with a non-conventional Maven resource root", map[string]string{"README.md": "# changed\n",
			"core/pom.xml": "<project><build><resources><resource><directory>../docs</directory></resource></resources></build></project>"},
			func() dynamic.Change { return change("README.md", model.RoleData) }, nil, "non-conventional resource root"},
		{"documentation file with a Gradle resources srcDir", map[string]string{"README.md": "# changed\n",
			"core/build.gradle": "sourceSets { main { resources { srcDir 'docs' } } }\n"},
			func() dynamic.Change { return change("README.md", model.RoleData) }, nil, "non-conventional resource root"},
		{"documentation file with conventional and build-output resource roots", map[string]string{"README.md": "# changed\n",
			"core/pom.xml": "<project><build><resources><resource><directory>src/main/res</directory></resource><resource><directory>${project.build.directory}/gen</directory></resource></resources></build></project>"},
			func() dynamic.Change { return change("README.md", model.RoleData) }, nil, ""},
		{"documentation file named by a rule", map[string]string{"README.md": "# changed\n"},
			func() dynamic.Change { c := change("README.md", model.RoleData); c.Rule = true; return c }, nil, "non-code"},
		{"added file", map[string]string{"core/src/main/java/a/New.java": "package a;\nclass New {}\n"},
			func() dynamic.Change {
				c := change("core/src/main/java/a/New.java", model.RoleSource)
				c.Status = 'A'
				return c
			}, nil, "status A"},
		{"template root", map[string]string{"core/src/main/java-templates/a/V.java": "package a;\nclass V {}\n"},
			func() dynamic.Change { return change("core/src/main/java-templates/a/V.java", model.RoleSource) }, nil, "non-code"},
		{"module-info", map[string]string{"core/src/main/java/module-info.java": "module a {}\n"},
			func() dynamic.Change { return change("core/src/main/java/module-info.java", model.RoleSource) }, nil, "non-code"},
		{"JVM widening", nil, func() dynamic.Change { return change("core/src/main/java/a/Bar.java", model.RoleSource) },
			func(s model.Selection) model.Selection {
				s.Widening = append(s.Widening, model.Widening{Tier: model.TierModule, Reason: "build_definition", Eco: Eco, Path: "pom.xml"})
				return s
			}, "widening"},
		{"full selection", nil, func() dynamic.Change { return change("core/src/main/java/a/Bar.java", model.RoleSource) },
			func(s model.Selection) model.Selection { s.Scope = model.TierFull; return s }, "full"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch := c.ch()
			head := with(baseFiles, c.edits)
			b := base
			if ch.Status == 'A' {
				b = base
			} else if _, ok := baseFiles[ch.Path]; !ok {
				b = with(baseFiles, map[string]string{ch.Path: c.edits[ch.Path] + "\n"})
			}
			s := sel(ch.Path)
			if c.sel != nil {
				s = c.sel(s)
			}
			got := Guard(s, []dynamic.Change{ch}, b, head)
			if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
				t.Errorf("Guard = %q, want %q", got, c.want)
			}
		})
	}
}

// testJar is an external archive the class records list (created once per test binary).
var testJar = func() string {
	d, _ := os.MkdirTemp("", "jvmdyn-art")
	p := filepath.Join(d, "lib.jar")
	os.WriteFile(p, []byte("PK lib 1"), 0o644)
	return p
}()

func classRecords(base tree.Tree, env string, now time.Time, deps map[string][]string) []dynamic.Record {
	digest, _ := ArtifactDigest(testJar)
	cl := dynamic.Record{Eco: Eco, Unit: "maven:core", Collector: "jvm-agent-2", Env: env, Complete: true, Pass: true, Recorded: now,
		Deps: map[string]string{}}
	for _, f := range TestRootFiles(base, "core") {
		cl.Deps[f] = base.Key(f)
	}
	recs := []dynamic.Record{cl}
	for c, ds := range deps {
		cl.Classes = append(cl.Classes, c)
		r := dynamic.Record{Eco: Eco, Unit: "maven:core#" + c, Collector: "jvm-agent-2", Env: env, Complete: true, Pass: true, Recorded: now,
			Deps: map[string]string{}, Artifacts: map[string]string{testJar: digest}}
		for _, d := range ds {
			r.Deps[d] = base.Key(d)
		}
		recs = append(recs, r)
	}
	recs[0].Classes = cl.Classes
	return recs
}

func TestNarrow(t *testing.T) {
	base := tree.NewMapTree(baseFiles)
	now := time.Now()
	deps := map[string][]string{
		"a.FooTest": {"core/src/test/java/a/FooTest.java", "core/src/main/java/a/Foo.java"},
		"a.BarTest": {"core/src/test/java/a/BarTest.java", "core/src/main/java/a/Bar.java"},
	}
	recs := classRecords(base, "env", now, deps)
	p := Params{Collector: "jvm-agent-2", Env: "env", Records: recs, Base: base, Head: base, UnitEco: map[string]string{"maven:core": Eco}}
	run := func(p Params, changed ...string) model.SelectedTest { return Narrow(sel(changed...), p).Tests[0] }

	got := run(p, "core/src/main/java/a/Foo.java")
	if !got.ClassFilter || strings.Join(got.Classes, ",") != "a.FooTest" {
		t.Errorf("Foo.java changed: %+v", got)
	}
	if s := Narrow(sel("core/src/main/java/a/Foo.java"), p); len(s.Narrowed) != 1 || s.Narrowed[0].Unit != "maven:core#a.BarTest" {
		t.Errorf("narrowed: %+v", s.Narrowed)
	}
	// A file no class loaded: the module is still built, no class runs.
	if got := run(p, "core/src/main/java/a/Consts.java"); !got.ClassFilter || len(got.Classes) != 0 {
		t.Errorf("unloaded file: %+v", got)
	}
	// A changed test file: its top-level types always run (a class declared since the recording).
	head := with(baseFiles, map[string]string{"core/src/test/java/a/BarTest.java": "package a;\npublic class BarTest {}\nclass BarTest2 {}\n"})
	p2 := p
	p2.Head = head
	if got := run(p2, "core/src/test/java/a/BarTest.java"); strings.Join(got.Classes, ",") != "a.BarTest,a.BarTest2" {
		t.Errorf("changed test file: %+v", got)
	}
	// Unusable records keep classes: failed run, incomplete record, stale dependency, other environment.
	for name, mod := range map[string]func(r *dynamic.Record){
		"failed":     func(r *dynamic.Record) { r.Pass = false },
		"incomplete": func(r *dynamic.Record) { r.Complete, r.Incomplete = false, "maps repository file x" },
		"stale":      func(r *dynamic.Record) { r.Deps["core/src/main/java/a/Bar.java"] = "0000" },
		"collector":  func(r *dynamic.Record) { r.Collector = "jvm-agent-0" },
	} {
		rs := classRecords(base, "env", now, deps)
		for i := range rs {
			if rs[i].Unit == "maven:core#a.BarTest" {
				mod(&rs[i])
			}
		}
		q := p
		q.Records = rs
		if got := run(q, "core/src/main/java/a/Foo.java"); strings.Join(got.Classes, ",") != "a.BarTest,a.FooTest" || got.ClassFilter {
			if got.ClassFilter {
				t.Errorf("%s: %+v", name, got)
			}
		}
	}
	// External artifacts: a record whose archive changed (a SNAPSHOT re-resolved) or disappeared, or that lists none, is not
	// usable.
	for name, mod := range map[string]func(r *dynamic.Record){
		"changed": func(r *dynamic.Record) { r.Artifacts = map[string]string{testJar: "8:0000"} },
		"missing": func(r *dynamic.Record) { r.Artifacts = map[string]string{testJar + ".gone": "8:0000"} },
		"none":    func(r *dynamic.Record) { r.Artifacts = nil },
	} {
		rs := classRecords(base, "env", now, deps)
		for i := range rs {
			if rs[i].Unit == "maven:core#a.BarTest" {
				mod(&rs[i])
			}
		}
		q := p
		q.Records = rs
		if got := run(q, "core/src/main/java/a/Foo.java"); got.ClassFilter {
			t.Errorf("artifact %s: BarTest removed: %+v", name, got)
		}
	}
	// Rewriting the archive itself (same path, new content) is seen at selection time.
	os.WriteFile(testJar, []byte("PK lib 2"), 0o644)
	if got := run(p, "core/src/main/java/a/Foo.java"); got.ClassFilter {
		t.Errorf("rewritten artifact: %+v", got)
	}
	os.WriteFile(testJar, []byte("PK lib 1"), 0o644)
	// The class list is not trusted when a test-root file was added since the recording, or for another environment.
	added := with(baseFiles, map[string]string{"core/src/test/java/a/NewTest.java": "package a;\nclass NewTest {}\n"})
	q := p
	q.Base, q.Head = added, added
	if got := run(q, "core/src/main/java/a/Foo.java"); got.ClassFilter || !strings.Contains(got.Dynamic, "class list not usable") {
		t.Errorf("test root drift: %+v", got)
	}
	q = p
	q.Env = "other"
	if got := run(q, "core/src/main/java/a/Foo.java"); got.ClassFilter || !strings.Contains(got.Dynamic, "no class-list record") {
		t.Errorf("env mismatch: %+v", got)
	}
}

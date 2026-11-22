package bgraph

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/dynamic/jvmdyn"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/runner"
)

var jvmDynFiles = map[string]string{
	"pom.xml":                      `<project><modelVersion>4.0.0</modelVersion><groupId>g</groupId><artifactId>core</artifactId><version>1</version></project>`,
	"src/main/java/a/Foo.java":     "package a;\npublic class Foo {\n  static final int LIMIT = 1;\n  int f() { return 1; }\n}\n",
	"src/main/java/a/Bar.java":     "package a;\npublic class Bar { int g() { return 2; } }\n",
	"src/test/java/a/FooTest.java": "package a;\npublic class FooTest {}\n",
	"src/test/java/a/BarTest.java": "package a;\npublic class BarTest {}\n",
	"src/main/resources/x.txt":     "x\n",
}

// TestJVMClassWiring: with records of the current environment, Select narrows the selected Maven module to the test classes
// whose records reach the changed file, only with the layer on; a constant change and a resource change refuse; the runner
// passes the classes through MAVEN_ARGS. No JDK is needed: the fingerprint reads JAVA_HOME's release file.
func TestJVMClassWiring(t *testing.T) {
	jdk := t.TempDir()
	os.WriteFile(filepath.Join(jdk, "release"), []byte("JAVA_VERSION=\"17\"\n"), 0o644)
	t.Setenv("JAVA_HOME", jdk)
	root := testRepo(t, jvmDynFiles)
	state := t.TempDir()
	ctx := context.Background()
	ix, err := Open(ctx, root, Options{MemoryOnly: true, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	head, _ := ix.repo.RevParse(ctx, "HEAD")
	ht, err := ix.repo.CommitTree(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	env, coll, _, err := ix.jvmEnv(nil, ht, nil)
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel, c string) {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sel := func(mode DynamicMode) Selection {
		t.Helper()
		s, err := ix.Select(ctx, Change{Base: "HEAD", Worktree: true, Dynamic: mode})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	skip := func(s Selection) string {
		for _, w := range s.Warnings {
			if w.Reason == model.ReasonDynamicSkipped {
				return w.Detail
			}
		}
		return ""
	}
	write("src/main/java/a/Foo.java", strings.Replace(jvmDynFiles["src/main/java/a/Foo.java"], "return 1", "return 3", 1))
	if s := sel(DynamicOn); !strings.Contains(skip(s), "no records") {
		t.Fatalf("no records: %+v", s.Warnings)
	}
	now := time.Now().UTC()
	jar := filepath.Join(t.TempDir(), "junit.jar")
	os.WriteFile(jar, []byte("PK junit"), 0o644)
	digest, err := jvmdyn.ArtifactDigest(jar)
	if err != nil {
		t.Fatal(err)
	}
	rec := func(unit string, deps ...string) dynamic.Record {
		m := map[string]string{}
		for _, d := range deps {
			m[d] = ht.Key(d)
		}
		r := dynamic.Record{Eco: jvmdyn.Eco, Unit: unit, Collector: coll, Env: env, Deps: m, Complete: true, Pass: true, Recorded: now}
		if strings.Contains(unit, "#") {
			r.Artifacts = map[string]string{jar: digest}
		}
		return r
	}
	cl := rec("maven:.", "src/test/java/a/BarTest.java", "src/test/java/a/FooTest.java")
	cl.Classes = []string{"a.BarTest", "a.FooTest"}
	if _, err := dynamic.Add(dynamic.StorePath(state, jvmdyn.Eco), []dynamic.Record{cl,
		rec("maven:.#a.FooTest", "src/test/java/a/FooTest.java", "src/main/java/a/Foo.java"),
		rec("maven:.#a.BarTest", "src/test/java/a/BarTest.java", "src/main/java/a/Bar.java"),
	}); err != nil {
		t.Fatal(err)
	}
	if s := sel(DynamicOff); len(s.Narrowed) != 0 || s.Tests[0].ClassFilter {
		t.Fatal("narrowed while off")
	}
	on := sel(DynamicOn)
	if len(on.Tests) != 1 || !on.Tests[0].ClassFilter || !slices.Equal(on.Tests[0].Classes, []string{"a.FooTest"}) ||
		len(on.Narrowed) != 1 || on.Narrowed[0].Unit != "maven:.#a.BarTest" {
		t.Fatalf("class layer: %+v narrowed %+v warnings %+v", on.Tests, on.Narrowed, on.Warnings)
	}
	plan, err := runner.Commands(on, runner.Options{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range plan.Commands {
		if slices.Contains(c.Env, "MAVEN_ARGS=-Dtest=a.FooTest,a.FooTest$*") {
			found = true
		}
	}
	if !found {
		t.Errorf("no MAVEN_ARGS class filter in %+v", plan.Commands)
	}
	// Guards: a constant change, a non-code change.
	write("src/main/java/a/Foo.java", strings.Replace(jvmDynFiles["src/main/java/a/Foo.java"], "LIMIT = 1", "LIMIT = 2", 1))
	if s := sel(DynamicOn); !strings.Contains(skip(s), "LIMIT") || s.Tests[0].ClassFilter {
		t.Errorf("constant guard: %q", skip(s))
	}
	write("src/main/java/a/Foo.java", jvmDynFiles["src/main/java/a/Foo.java"])
	write("src/main/resources/x.txt", "y\n")
	if s := sel(DynamicOn); !strings.Contains(skip(s), "x.txt") || s.Tests[0].ClassFilter {
		t.Errorf("resource guard: %q", skip(s))
	}
	// Status: records usable at HEAD.
	write("src/main/resources/x.txt", jvmDynFiles["src/main/resources/x.txt"])
	st, err := ix.DynamicStatusOf(ctx, jvmdyn.Eco)
	if err != nil || st.Usable != 3 {
		t.Errorf("status: %+v %v", st, err)
	}
}

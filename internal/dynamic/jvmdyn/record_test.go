package jvmdyn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/tree"
)

// repoFixture: a Maven module "core" with two main classes, a test class, a test resource and a doc file; the files also exist
// on disk under root (the mapper checks untracked files and resource copies there).
func repoFixture(t *testing.T) (string, *tree.MapTree) {
	t.Helper()
	files := map[string]string{
		"pom.xml":                           "<project/>",
		"core/pom.xml":                      "<project/>",
		"core/src/main/java/a/Foo.java":     "package a;\npublic class Foo { int f() { return 1; } }\n",
		"core/src/main/java/a/Bar.java":     "package a;\npublic class Bar {}\n",
		"core/src/main/kotlin/a/-Ext.kt":    "@file:JvmName(\"ExtKt\")\npackage a\ninline fun ext() = 1\n",
		"core/src/test/java/a/FooTest.java": "package a;\npublic class FooTest {}\n",
		"core/src/test/java/a/BarTest.java": "package a;\npublic class BarTest {}\n",
		"core/src/test/resources/data.json": "{}\n",
		"README.md":                         "# readme\n",
	}
	root := t.TempDir()
	for p, c := range files {
		f := filepath.Join(root, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte(c), 0o644)
	}
	return root, tree.NewMapTree(files)
}

func fileURL(root, rel string) string {
	return "file:/" + strings.TrimPrefix(filepath.ToSlash(filepath.Join(root, filepath.FromSlash(rel))), "/") + "/"
}

func abs(root, rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }

func newMapper(root string, mt *tree.MapTree) *Mapper {
	return &Mapper{Root: root, Tree: mt, Index: NewSourceIndex(mt), SystemDirs: []string{filepath.Join(root, "..", "os")}}
}

// fooFork is a complete fork that ran a.FooTest and loaded a.Foo.
func fooFork(root string) *Fork {
	jar := filepath.Join(filepath.Dir(root), "m2", "junit.jar")
	os.MkdirAll(filepath.Dir(jar), 0o755)
	if _, err := os.Stat(jar); err != nil {
		os.WriteFile(jar, []byte("PK junit"), 0o644)
	}
	return &Fork{Complete: true, JavaHome: filepath.Join(root, "..", "jdk"), Classes: []ClassLoad{
		{Name: "a/FooTest", Location: fileURL(root, "core/target/test-classes"), SourceFile: "FooTest.java"},
		{Name: "a/Foo", Location: fileURL(root, "core/target/classes"), SourceFile: "Foo.java"},
		{Name: "a/Foo$1", Location: fileURL(root, "core/target/classes"), SourceFile: "Foo.java"},
		{Name: "org/junit/Assert", Location: "file:/" + strings.TrimPrefix(filepath.ToSlash(jar), "/"), SourceFile: "Assert.java"},
	}}
}

func TestMapCompleteFork(t *testing.T) {
	root, mt := repoFixture(t)
	m := newMapper(root, mt)
	f := fooFork(root)
	f.Classes = append(f.Classes, ClassLoad{Name: "a/FooTest$1", Location: fileURL(root, "core/target/test-classes"), SourceFile: "FooTest.java",
		SMAP: []SMAPFile{{Name: "-Ext.kt", Path: "a/ExtKt"}, {Name: "_Collections.kt", Path: "kotlin/collections/CollectionsKt"}}})
	f.Reads = []string{abs(root, "core/src/test/resources/data.json"), abs(root, "README.md"), filepath.Join(root, "..", "outside.txt"),
		abs(root, "core/missing.txt")}
	deps, why := m.Map(f)
	if len(why) != 0 {
		t.Fatalf("incomplete: %v", why)
	}
	for _, d := range []string{"core/src/test/java/a/FooTest.java", "core/src/main/java/a/Foo.java", "core/src/main/kotlin/a/-Ext.kt",
		"core/src/test/resources/data.json", "README.md"} {
		if !deps[d] {
			t.Errorf("missing dep %s in %v", d, deps)
		}
	}
	if deps["core/src/main/java/a/Bar.java"] {
		t.Errorf("Bar.java was never loaded")
	}
	dir, class, ok := m.TestClassOf(f)
	if !ok || dir != "core" || class != "a.FooTest" {
		t.Errorf("TestClassOf = %q %q %v", dir, class, ok)
	}
}

// TestMapIncomplete: one case per incomplete reason of a fork (docs/dynamic-layer.md §9.5).
func TestMapIncomplete(t *testing.T) {
	root, mt := repoFixture(t)
	os.MkdirAll(abs(root, "core/target/classes"), 0o755)
	os.WriteFile(abs(root, "core/target/classes/data.json"), []byte("{\"other\":1}\n"), 0o644)
	os.WriteFile(abs(root, "core/target/test-classes-copy.json"), []byte("x"), 0o644)
	os.MkdirAll(abs(root, "core/target/test-classes"), 0o755)
	os.WriteFile(abs(root, "core/target/test-classes/data.json"), []byte("{}\n"), 0o644)
	os.WriteFile(abs(root, "core/target/core.jar"), []byte("PK"), 0o644)
	os.MkdirAll(abs(root, "core/lib"), 0o755)
	os.WriteFile(abs(root, "core/lib/x.jar"), []byte("PK"), 0o644)
	os.WriteFile(abs(root, "notes.txt"), []byte("untracked"), 0o644)
	os.WriteFile(abs(root, "core/native.dll"), []byte("MZ"), 0o644)
	cases := []struct {
		name string
		mod  func(f *Fork)
		want string
	}{
		{"truncated log", func(f *Fork) { f.Complete = false }, "collector log truncated"},
		{"jfr failed", func(f *Fork) { f.JFR = "JFR not started: x" }, "JFR not started"},
		{"mmap hook failed", func(f *Fork) { f.Hook = "mmap hook not installed: y" }, "mmap hook not installed"},
		{"mmap of a repository file", func(f *Fork) { f.MMaps = []string{abs(root, "core/src/test/resources/data.json")} }, "maps repository file core/src/test/resources/data.json"},
		{"mmap without a path", func(f *Fork) { f.MMaps = []string{""} }, "maps a file channel without a path"},
		{"child process", func(f *Fork) { f.Processes = []string{"cmd /c echo"} }, "starts a process"},
		{"native library in the repository", func(f *Fork) { f.Natives = []string{abs(root, "core/native.dll")} }, "native library in the repository"},
		{"native library elsewhere", func(f *Fork) { f.Natives = []string{filepath.Join(root, "..", "lib", "x.dll")} }, "native library outside the JDK and the OS"},
		{"unmapped repository class", func(f *Fork) {
			f.Classes = append(f.Classes, ClassLoad{Name: "a/Gen", Location: fileURL(root, "core/target/classes"), SourceFile: "Gen.java"})
		}, "unmapped repository class a.Gen"},
		{"class without SourceFile from the repository", func(f *Fork) {
			f.Classes = append(f.Classes, ClassLoad{Name: "a/Obf", Location: fileURL(root, "core/target/classes")})
		}, "unmapped repository class a.Obf"},
		{"repository class loaded before the agent", func(f *Fork) { f.Unseen = []string{"a/Early\t" + fileURL(root, "core/target/classes")} }, "loaded before the agent"},
		{"untracked file", func(f *Fork) { f.Reads = []string{abs(root, "notes.txt")} }, "reads untracked file notes.txt"},
		{"build output that is not a copy", func(f *Fork) { f.Reads = []string{abs(root, "core/target/classes/data.json")} }, "reads build output"},
		{"archive built in the repository", func(f *Fork) { f.Reads = []string{abs(root, "core/lib/x.jar")} }, "reads an archive"},
		{"class file read that is gone", func(f *Fork) { f.Reads = []string{abs(root, "core/target/classes/a/Gone.class")} }, "reads class file that no longer exists"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMapper(root, mt)
			f := fooFork(root)
			c.mod(f)
			_, why := m.Map(f)
			if !strings.Contains(strings.Join(why, "; "), c.want) {
				t.Errorf("why = %v, want %q", why, c.want)
			}
		})
	}
	// Not incomplete: a byte-identical resource copy maps to the tracked resource; JDK and OS natives; trust mode.
	m := newMapper(root, mt)
	f := fooFork(root)
	f.Reads = []string{abs(root, "core/target/test-classes/data.json")}
	f.Natives = []string{filepath.Join(f.JavaHome, "bin", "java.dll"), filepath.Join(root, "..", "os", "System32", "kernel32.dll")}
	deps, why := m.Map(f)
	if len(why) != 0 || !deps["core/src/test/resources/data.json"] {
		t.Errorf("resource copy / natives: deps %v why %v", deps, why)
	}
	// A module's own archive: every main source and resource of the module (not its tests).
	f.Reads = []string{abs(root, "core/target/core.jar")}
	deps, why = m.Map(f)
	if len(why) != 0 || !deps["core/src/main/java/a/Bar.java"] || !deps["core/src/main/kotlin/a/-Ext.kt"] || deps["core/src/test/java/a/BarTest.java"] {
		t.Errorf("module archive: deps %v why %v", deps, why)
	}
	// Kotlin inline regeneration: a/Foo$f$$inlined$map$1 with the library's SourceFile belongs to a/Foo's file.
	f = fooFork(root)
	f.Classes = append(f.Classes, ClassLoad{Name: "a/Foo$f$$inlined$map$1", Location: fileURL(root, "core/target/classes"), SourceFile: "_Collections.kt"})
	if _, why := m.Map(f); len(why) != 0 {
		t.Errorf("inlined regenerated class: %v", why)
	}
	m.Trust = true
	f.Natives = []string{filepath.Join(root, "..", "lib", "x.dll")}
	if _, why := m.Map(f); len(why) != 0 {
		t.Errorf("trust_native_env: %v", why)
	}
}

func TestRecordsCrossCheck(t *testing.T) {
	root, mt := repoFixture(t)
	m := newMapper(root, mt)
	u := Unit{ID: "maven:core", Dir: "core", Runner: "maven"}
	foo := fooFork(root)
	bar := &Fork{Complete: true, Classes: []ClassLoad{{Name: "a/BarTest", Location: fileURL(root, "core/target/test-classes"), SourceFile: "BarTest.java"}}}
	now := time.Now()
	get := func(recs []dynamic.Record, unit string) dynamic.Record {
		for _, r := range recs {
			if r.Unit == unit {
				return r
			}
		}
		t.Fatalf("no record %s", unit)
		return dynamic.Record{}
	}
	// Normal: two classes, one fork each.
	recs := Records(m, []UnitResult{{Unit: u, Reports: Reports{"a.FooTest": true, "a.BarTest": false}, BuildOK: true}}, []*Fork{foo, bar}, "env", "c", "h", now)
	if r := get(recs, "maven:core#a.FooTest"); !r.Complete || !r.Pass || r.Deps["core/src/main/java/a/Foo.java"] == "" || len(r.Artifacts) != 1 {
		t.Errorf("FooTest: %+v", r)
	}
	if r := get(recs, "maven:core#a.BarTest"); !r.Complete || r.Pass {
		t.Errorf("BarTest failed in isolation: must not pass: %+v", r)
	}
	cl := get(recs, "maven:core")
	if !cl.Complete || len(cl.Classes) != 2 || len(cl.Deps) != 3 {
		t.Errorf("class list: %+v", cl)
	}
	// A class without its own fork, a class with two forks, a fork of an unreported class.
	foo2 := fooFork(root)
	extra := &Fork{Complete: true, Classes: []ClassLoad{{Name: "a/Hidden", Location: fileURL(root, "core/target/test-classes"), SourceFile: "FooTest.java"}}}
	recs = Records(m, []UnitResult{{Unit: u, Reports: Reports{"a.FooTest": true, "a.BarTest": true}, BuildOK: true}}, []*Fork{foo, foo2, extra}, "env", "c", "h", now)
	if r := get(recs, "maven:core#a.BarTest"); r.Complete || !strings.Contains(r.Incomplete, "no collector log") {
		t.Errorf("missing log: %+v", r)
	}
	if r := get(recs, "maven:core#a.FooTest"); r.Complete || !strings.Contains(r.Incomplete, "2 collector logs") {
		t.Errorf("two logs: %+v", r)
	}
	if r := get(recs, "maven:core"); r.Complete || !strings.Contains(r.Incomplete, "did not report") {
		t.Errorf("unreported fork class: %+v", r)
	}
	// Build failure and missing reports make the class list incomplete.
	recs = Records(m, []UnitResult{{Unit: u, BuildOK: false, BuildWhy: "exit code 1"}}, nil, "env", "c", "h", now)
	if r := get(recs, "maven:core"); r.Complete || !strings.Contains(r.Incomplete, "build: exit code 1") || !strings.Contains(r.Incomplete, "no test class reported") {
		t.Errorf("build failed: %+v", r)
	}
}

func TestTestOutputDir(t *testing.T) {
	for in, want := range map[string]string{
		"core/target/test-classes":             "core",
		"target/test-classes":                  ".",
		"moshi/build/classes/kotlin/test":      "moshi",
		"a/b/build/classes/java/test":          "a/b",
		"m/build/classes/kotlin/jvm/test":      "m",
		"m/build/classes/java/main":            "",
		"m/build/classes/java/integrationTest": "m",
	} {
		got, ok := testOutputDir(in)
		if want == "" && ok || want != "" && got != want {
			t.Errorf("testOutputDir(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
}

// TestDocResourceRead: a documentation file under a resource root is read by tests through its build-output copy; the read maps
// back to the tracked file (Mapper.resourceCopy), and a copy that differs makes the record incomplete.
func TestDocResourceRead(t *testing.T) {
	root, mt := repoFixture(t)
	files := map[string]string{"core/src/test/resources/notes.md": "# notes\n"}
	for _, p := range mt.Files() {
		b, _ := mt.Read(p)
		files[p] = string(b)
	}
	mt = tree.NewMapTree(files)
	os.MkdirAll(abs(root, "core/src/test/resources"), 0o755)
	os.MkdirAll(abs(root, "core/target/test-classes"), 0o755)
	os.WriteFile(abs(root, "core/src/test/resources/notes.md"), []byte("# notes\n"), 0o644)
	os.WriteFile(abs(root, "core/target/test-classes/notes.md"), []byte("# notes\n"), 0o644)
	m := newMapper(root, mt)
	f := fooFork(root)
	f.Reads = []string{abs(root, "core/target/test-classes/notes.md")}
	deps, why := m.Map(f)
	if len(why) != 0 || !deps["core/src/test/resources/notes.md"] {
		t.Errorf("copy: deps %v why %v", deps, why)
	}
	os.WriteFile(abs(root, "core/target/test-classes/notes.md"), []byte("# filtered\n"), 0o644)
	if _, why := m.Map(f); !strings.Contains(strings.Join(why, ";"), "reads build output") {
		t.Errorf("differing copy: %v", why)
	}
}

// TestExternal: the archives outside the repository and the JDK that a fork loaded classes from or read are its external
// artifacts; a class directory outside the repository cannot be checked and makes the record incomplete.
func TestExternal(t *testing.T) {
	root, mt := repoFixture(t)
	m := newMapper(root, mt)
	f := fooFork(root)
	jdkJar := filepath.Join(f.JavaHome, "lib", "jrt-fs.jar")
	extDir := filepath.Join(filepath.Dir(root), "classes")
	os.MkdirAll(extDir, 0o755)
	f.Classes = append(f.Classes, ClassLoad{Name: "x/Y", Location: "file:/" + strings.TrimPrefix(filepath.ToSlash(jdkJar), "/")})
	zip := filepath.Join(filepath.Dir(root), "m2", "data.zip")
	os.WriteFile(zip, []byte("PK"), 0o644)
	f.Reads = append(f.Reads, zip, abs(root, "core/target/core.jar"))
	paths, why := m.External(f)
	if len(paths) != 2 || len(why) != 0 {
		t.Errorf("external: %v %v", paths, why)
	}
	f.Classes = append(f.Classes, ClassLoad{Name: "x/Z", Location: "file:/" + strings.TrimPrefix(filepath.ToSlash(extDir), "/") + "/"})
	if _, why := m.External(f); len(why) != 1 || !strings.Contains(why[0], "directory outside the repository") {
		t.Errorf("class directory: %v", why)
	}
}

// TestExternalTemporaryManifestJar: a read-only archive that is gone after the run (surefire's booter jar) is no artifact; a
// gone archive that classes were loaded from still is (and fails to hash at record time).
func TestExternalTemporaryManifestJar(t *testing.T) {
	root, mt := repoFixture(t)
	m := newMapper(root, mt)
	f := fooFork(root)
	gone := filepath.Join(filepath.Dir(root), "tmp", "surefirebooter-1.jar")
	f.Reads = append(f.Reads, gone)
	if paths, _ := m.External(f); len(paths) != 1 {
		t.Errorf("booter jar listed: %v", paths)
	}
	f.Classes = append(f.Classes, ClassLoad{Name: "x/Y", Location: "file:/" + strings.TrimPrefix(filepath.ToSlash(gone), "/")})
	if paths, _ := m.External(f); len(paths) != 2 {
		t.Errorf("class source missing: %v", paths)
	}
}

package jvmdyn

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/eco/jvm"
	"github.com/qxbao/bgraph/internal/tree"
)

// Unit is one JVM test unit to record (model.TestUnit fields).
type Unit struct {
	ID      string
	Dir     string // module / project directory (repo-relative, "." for the root)
	Runner  string // maven or gradle
	Config  string // jvm.LaunchConfig
	Package string // Gradle project path / Maven coordinates
}

// SourceIndex maps (package, source file name) to the tracked Java/Kotlin files that declare that package with that name: the
// SourceFile attribute of a class (and the file entries of its SMAP) name the file, the package of the class names the
// package declaration (Kotlin files need not sit in their package directory).
type SourceIndex map[[2]string][]string

// NewSourceIndex indexes every tracked .java/.kt file of t.
func NewSourceIndex(t tree.Tree) SourceIndex {
	ix := SourceIndex{}
	for _, p := range t.Files() {
		if ext := path.Ext(p); ext != ".java" && ext != ".kt" {
			continue
		}
		b, err := t.Read(p)
		if err != nil {
			continue
		}
		pkg, ok := PackageName(p, b)
		if !ok {
			continue // never indexed: a class compiled from it is unmapped (incomplete), never silently dropped
		}
		k := [2]string{pkg, path.Base(p)}
		ix[k] = append(ix[k], p)
	}
	return ix
}

// Lookup returns the files of a class in package pkg (dotted) whose SourceFile is name.
func (ix SourceIndex) Lookup(pkg, name string) []string { return ix[[2]string{pkg, name}] }

// Mapper turns the forks of one recording into dependencies (docs/dynamic.md §9.5).
type Mapper struct {
	Root  string // repository root on disk (absolute, cleaned)
	Tree  tree.Tree
	Index SourceIndex
	Trust bool // trust_native_env: native libraries outside the repository are accepted
	// SystemDirs: directories of the operating system's own native libraries (accepted like the JDK's).
	SystemDirs []string
}

// DefaultSystemDirs are the operating system library directories.
func DefaultSystemDirs() []string {
	if runtime.GOOS == "windows" {
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		return []string{root}
	}
	return []string{"/lib", "/lib64", "/usr/lib", "/usr/lib64", "/System/Library", "/usr/lib/system"}
}

// rel returns the repository path of an absolute path, "" when it is outside the repository.
func (m *Mapper) rel(abs string) string {
	if abs == "" {
		return ""
	}
	abs = filepath.Clean(abs)
	r, err := filepath.Rel(m.Root, abs)
	if err != nil {
		return ""
	}
	r = filepath.ToSlash(r)
	if r == "." || r == ".." || strings.HasPrefix(r, "../") || filepath.IsAbs(r) {
		return ""
	}
	if runtime.GOOS == "windows" {
		// Rel is case-insensitive on Windows only for the volume; compare the root prefix ignoring case.
		if !strings.EqualFold(filepath.Clean(abs)[:min(len(abs), len(m.Root))], m.Root) {
			return ""
		}
	}
	return r
}

func under(p, dir string) bool {
	if dir == "" {
		return false
	}
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	if runtime.GOOS == "windows" {
		p, dir = strings.ToLower(p), strings.ToLower(dir)
	}
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// TestClassOf returns the unit directory and the top-level test class of a fork: the first class defined from a test output
// directory (Maven target/test-classes, Gradle build/classes/<lang>/<set with "test">/), ok=false when there is none.
func (m *Mapper) TestClassOf(f *Fork) (dir, class string, ok bool) {
	for _, c := range f.Classes {
		r := m.rel(locationPath(c.Location))
		if r == "" {
			continue
		}
		if d, ok := testOutputDir(r); ok {
			return d, TopLevel(strings.ReplaceAll(c.Name, "/", ".")), true
		}
	}
	return "", "", false
}

// testOutputDir: r (a repository path of a class-path directory) is a test output directory; d is the unit directory.
func testOutputDir(r string) (d string, ok bool) {
	r = strings.TrimSuffix(r, "/")
	q := "/" + r
	if strings.HasSuffix(q, "/target/test-classes") {
		return dirOrDot(strings.TrimSuffix(strings.TrimSuffix(q, "/target/test-classes"), "/")), true
	}
	if i := strings.LastIndex(q, "/build/classes/"); i >= 0 {
		rest := strings.Split(q[i+len("/build/classes/"):], "/")
		set := strings.ToLower(rest[len(rest)-1])
		if len(rest) >= 2 && strings.Contains(set, "test") {
			return dirOrDot(q[:i]), true
		}
	}
	return "", false
}

func dirOrDot(q string) string {
	q = strings.TrimPrefix(q, "/")
	if q == "" {
		return "."
	}
	return q
}

// TopLevel cuts a binary class name at its first '$' (nested classes run with their outer class).
func TopLevel(name string) string {
	if i := strings.IndexByte(name, '$'); i > 0 {
		return name[:i]
	}
	return name
}

func pkgOf(internal string) string {
	i := strings.LastIndexByte(internal, '/')
	if i < 0 {
		return ""
	}
	return strings.ReplaceAll(internal[:i], "/", ".")
}

// Map returns the repository files fork f depends on and the reasons its record is incomplete.
func (m *Mapper) Map(f *Fork) (deps map[string]bool, why []string) {
	deps = map[string]bool{}
	add := func(r string) { deps[r] = true }
	if !f.Complete {
		why = append(why, "collector log truncated (the JVM did not exit normally)")
	}
	if f.JFR != "" {
		why = append(why, f.JFR)
	}
	if f.Hook != "" {
		why = append(why, f.Hook)
	}
	// Source files of the loaded classes by name: Kotlin regenerates the anonymous classes of an inline function inlined from a
	// library into the caller's class set (a/B$f$$inlined$g$1) with the library's SourceFile; they belong to the caller's file.
	srcOf := map[string][]string{}
	for _, c := range f.Classes {
		if c.SourceFile != "" {
			srcOf[c.Name] = m.Index.Lookup(pkgOf(c.Name), c.SourceFile)
		}
	}
	mapClass := func(c ClassLoad, loc string) {
		pkg := pkgOf(c.Name)
		var hits []string
		if c.SourceFile != "" {
			hits = m.Index.Lookup(pkg, c.SourceFile)
		}
		if i := strings.Index(c.Name, "$$inlined$"); len(hits) == 0 && i > 0 {
			for host := c.Name[:i]; host != "" && len(hits) == 0; {
				hits = srcOf[host]
				j := strings.LastIndexByte(host, '$')
				if j < 0 {
					break
				}
				host = host[:j]
			}
		}
		for _, h := range hits {
			add(h)
		}
		if len(hits) == 0 && m.rel(loc) != "" {
			why = append(why, "unmapped repository class "+strings.ReplaceAll(c.Name, "/", "."))
		}
		for _, s := range c.SMAP {
			p := pkg
			if s.Path != "" {
				p = pkgOf(s.Path)
			}
			for _, h := range m.Index.Lookup(p, s.Name) {
				add(h)
			}
		}
	}
	for _, c := range f.Classes {
		mapClass(c, locationPath(c.Location))
	}
	classFiles := map[string]bool{}
	for _, c := range f.ClassFile {
		classFiles[filepath.Clean(c.Location)] = true
		mapClass(c, c.Location)
	}
	for _, u := range f.Unseen {
		name, loc, _ := strings.Cut(u, "\t")
		if m.rel(locationPath(loc)) != "" {
			why = append(why, "repository class loaded before the agent: "+strings.ReplaceAll(name, "/", "."))
		}
	}
	for _, p := range f.MMaps {
		switch r := m.rel(p); {
		case p == "":
			why = append(why, "maps a file channel without a path")
		case r != "":
			why = append(why, "maps repository file "+r)
		}
	}
	if len(f.Processes) > 0 {
		why = append(why, "starts a process: "+firstWord(f.Processes[0]))
	}
	for _, n := range f.Natives {
		if m.nativeOK(n, f.JavaHome) {
			continue
		}
		if r := m.rel(n); r != "" {
			why = append(why, "native library in the repository: "+r)
		} else {
			why = append(why, "native library outside the JDK and the OS: "+n)
		}
	}
	for _, p := range f.Reads {
		r := m.rel(p)
		if r == "" {
			continue // outside the repository: the environment fingerprint covers it
		}
		switch {
		case strings.HasSuffix(r, ".class"):
			if !classFiles[filepath.Clean(p)] {
				why = append(why, "reads class file that no longer exists: "+r)
			}
		case m.Tree.Has(r):
			add(r)
		default:
			if m.Tree.IsDir(r) {
				continue
			}
			if src, ok := m.resourceCopy(r); ok {
				add(src)
				continue
			}
			st, err := os.Stat(filepath.Join(m.Root, filepath.FromSlash(r)))
			switch {
			case err != nil || st.IsDir():
				// absent or a directory: additions are never narrowed
			case strings.HasSuffix(r, ".jar") || strings.HasSuffix(r, ".zip"):
				// The archive of a module (Gradle's runtime class path of a project dependency, a Maven reactor jar):
				// its classes map through SourceFile, its other entries are unattributable, so the record depends on every
				// main source and resource of that module. Any other archive is not understood.
				if srcs, ok := m.moduleArchive(r); ok {
					for _, s := range srcs {
						add(s)
					}
				} else {
					why = append(why, "reads an archive built in the repository: "+r)
				}
			case buildOutput(r):
				why = append(why, "reads build output that is not a copy of a tracked file: "+r)
			default:
				why = append(why, "reads untracked file "+r)
			}
		}
	}
	return deps, dedupe(why)
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (m *Mapper) nativeOK(p, javaHome string) bool {
	if javaHome != "" && under(p, javaHome) {
		return true
	}
	for _, d := range m.SystemDirs {
		if under(p, d) {
			return true
		}
	}
	return m.Trust && m.rel(p) == ""
}

// moduleArchive maps an archive built by a module (<dir>/build/libs/*.jar, <dir>/target/*.jar) to the tracked files of that
// module's non-test source sets (<dir>/src/<set>/** outside the test roots).
func (m *Mapper) moduleArchive(r string) ([]string, bool) {
	q := "/" + r
	var mod string
	switch {
	case strings.Contains(q, "/build/libs/") && path.Dir(q) == q[:strings.LastIndex(q, "/build/libs/")]+"/build/libs":
		mod = q[:strings.LastIndex(q, "/build/libs/")]
	case path.Base(path.Dir(q)) == "target":
		mod = path.Dir(path.Dir(q))
	default:
		return nil, false
	}
	mod = strings.TrimPrefix(mod, "/")
	pre := "src/"
	if mod != "" {
		pre = mod + "/src/"
	}
	var out []string
	files := m.Tree.Files()
	for i := sort.SearchStrings(files, pre); i < len(files) && strings.HasPrefix(files[i], pre); i++ {
		if !jvm.TestRoot(strings.TrimPrefix(files[i], strings.TrimSuffix(pre, "src/"))) {
			out = append(out, files[i])
		}
	}
	return out, len(out) > 0
}

// buildOutput: r lies in a Maven/Gradle build output directory.
func buildOutput(r string) bool {
	q := "/" + r + "/"
	return strings.Contains(q, "/target/") || strings.Contains(q, "/build/") || strings.Contains(q, "/.gradle/")
}

// resourceCopy maps a build-output file that is a byte-identical copy of a tracked resource (Maven target/classes/R,
// target/test-classes/R; Gradle build/resources/<set>/R) back to that resource.
func (m *Mapper) resourceCopy(r string) (string, bool) {
	q := "/" + r
	var mod, rest string
	for _, mark := range []string{"/target/classes/", "/target/test-classes/"} {
		if i := strings.Index(q, mark); i >= 0 {
			mod, rest = q[:i], q[i+len(mark):]
			break
		}
	}
	if rest == "" {
		if i := strings.Index(q, "/build/resources/"); i >= 0 {
			_, after, ok := strings.Cut(q[i+len("/build/resources/"):], "/")
			if !ok {
				return "", false
			}
			mod, rest = q[:i], after
		}
	}
	if rest == "" {
		return "", false
	}
	mod = strings.TrimPrefix(mod, "/")
	pre := "src/"
	if mod != "" {
		pre = mod + "/src/"
	}
	got, err := os.ReadFile(filepath.Join(m.Root, filepath.FromSlash(r)))
	if err != nil {
		return "", false
	}
	files := m.Tree.Files()
	for i := sort.SearchStrings(files, pre); i < len(files) && strings.HasPrefix(files[i], pre); i++ {
		f := files[i]
		if !strings.HasSuffix(f, "/resources/"+rest) || strings.Count(strings.TrimPrefix(f, pre), "/") != strings.Count(rest, "/")+2 {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.Root, filepath.FromSlash(f)))
		if err == nil && bytes.Equal(b, got) {
			return f, true
		}
	}
	return "", false
}

// Reports are the runner's per-class verdicts of one unit: binary name of the top-level class → passed.
type Reports map[string]bool

// UnitResult is everything known about one unit after a recording.
type UnitResult struct {
	Unit     Unit
	Reports  Reports // nil: no report read
	BuildOK  bool    // the build invocation ran the unit's tests (exit 0 with failures ignored)
	BuildWhy string  // why not
}

// Records builds the class records and the class-list record of every unit (docs/dynamic.md §9.1, §9.3) from the forks of
// one recording session.
func Records(m *Mapper, units []UnitResult, forks []*Fork, env, collector, commit string, now time.Time) []dynamic.Record {
	type key struct{ dir, class string }
	byClass := map[key][]*Fork{}
	for _, f := range forks {
		if d, c, ok := m.TestClassOf(f); ok {
			byClass[key{d, c}] = append(byClass[key{d, c}], f)
		}
	}
	var out []dynamic.Record
	for _, ur := range units {
		u := ur.Unit
		classes := map[string]bool{}
		for c := range ur.Reports {
			classes[c] = true
		}
		var unreported []string
		for k := range byClass {
			if k.dir == u.Dir && !classes[k.class] {
				classes[k.class] = true
				unreported = append(unreported, k.class)
			}
		}
		names := make([]string, 0, len(classes))
		for c := range classes {
			names = append(names, c)
		}
		sort.Strings(names)
		for _, c := range names {
			r := dynamic.Record{Eco: Eco, Unit: u.ID + "#" + c, Collector: collector, Commit: commit, Deps: map[string]string{},
				Env: env, Complete: true, Recorded: now.UTC()}
			passed, reported := ur.Reports[c]
			r.Pass = reported && passed
			var why []string
			fs := byClass[key{u.Dir, c}]
			switch {
			case len(fs) == 0:
				why = append(why, "no collector log for the class (not run in a fork of its own)")
			case len(fs) > 1:
				why = append(why, fmt.Sprintf("%d collector logs for one class (several executions or forks)", len(fs)))
			}
			if !reported {
				why = append(why, "not reported by the runner")
			}
			for _, f := range fs {
				deps, w := m.Map(f)
				why = append(why, w...)
				ext, w := m.External(f)
				why = append(why, w...)
				for _, p := range ext {
					d, err := ArtifactDigest(p)
					if err != nil {
						why = append(why, "cannot hash external artifact "+p)
						continue
					}
					if r.Artifacts == nil {
						r.Artifacts = map[string]string{}
					}
					r.Artifacts[p] = d
				}
				for d := range deps {
					if k := m.Tree.Key(d); k != "" {
						r.Deps[d] = k
					} else {
						why = append(why, "cannot hash "+d)
					}
				}
			}
			if len(why) > 0 {
				r.Complete, r.Incomplete = false, strings.Join(dedupe(why), "; ")
			}
			out = append(out, r)
		}
		// The class list.
		cl := dynamic.Record{Eco: Eco, Unit: u.ID, Collector: collector, Commit: commit, Deps: map[string]string{}, Env: env,
			Complete: true, Pass: ur.BuildOK, Recorded: now.UTC()}
		for c := range ur.Reports {
			cl.Classes = append(cl.Classes, c)
		}
		sort.Strings(cl.Classes)
		var why []string
		if !ur.BuildOK {
			why = append(why, "build: "+ur.BuildWhy)
		}
		if len(unreported) > 0 {
			sort.Strings(unreported)
			why = append(why, "forks ran classes the runner did not report: "+strings.Join(unreported[:min(3, len(unreported))], ", "))
		}
		code := 0
		for _, f := range TestRootFiles(m.Tree, u.Dir) {
			cl.Deps[f] = m.Tree.Key(f)
			if ext := path.Ext(f); ext == ".java" || ext == ".kt" || ext == ".groovy" || ext == ".scala" {
				code++
			}
		}
		if code > 0 && len(ur.Reports) == 0 {
			why = append(why, "test sources but no test class reported (tests skipped or reports not found)")
		}
		if len(why) > 0 {
			cl.Complete, cl.Incomplete = false, strings.Join(why, "; ")
		}
		out = append(out, cl)
	}
	return out
}

// TestRootFiles lists the files of t under the test source sets of the unit in directory dir (jvm.TestRoot).
func TestRootFiles(t tree.Tree, dir string) []string {
	pre := ""
	if dir != "." && dir != "" {
		pre = dir + "/"
	}
	var out []string
	files := t.Files()
	for i := sort.SearchStrings(files, pre+"src/"); i < len(files) && strings.HasPrefix(files[i], pre+"src/"); i++ {
		if jvm.TestRoot(strings.TrimPrefix(files[i], pre)) {
			out = append(out, files[i])
		}
	}
	return slices.Clip(out)
}

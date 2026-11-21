package evalkit

import (
	"context"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/tree"
)

// JVMTestLoc is where one JVM (Java/Kotlin/Groovy/Scala) FAIL_TO_PASS test name is defined, as far as a source scan can tell.
//
// Phase 1 (this file) only locates test source files. TODO(jvm-phase-2): map Files to build-module units with internal/eco/jvm
// (file -> owning Maven/Gradle module, T1 oracle); ambiguous names should require the units of ALL candidate files, Task names
// (Gradle tasks such as "jib-cli:test") require the units in Dirs (or every unit for a bare task like "compileTestJava"), and
// NotFound names must be reported as unmapped, never silently dropped. For f2p_kind == "n2p" tasks the name list is the whole
// suite: keep only the names with InPatch set (their file is touched by the test patch).
type JVMTestLoc struct {
	Name string
	// Form: "junit4" (method(pkg.Class), also lombok's file(pkg.Class)), "hash" (pkg.Class#method), "gradle" (pkg.Class > method),
	// "dotted" (pkg.Class.method / pkg.Class / Kotlin "pkg.Class.some name with spaces()"), "task" (a Gradle task such as
	// "jib-cli:test" or "compileTestJava": no test file, the whole module/build is the target), "other".
	Form      string
	Class     string   // resolved top-level class as pkg.Class ("" when not resolved)
	Files     []string // candidate test source files (sorted)
	Ambiguous bool     // more than one candidate file
	NotFound  bool     // no candidate file (and not a task)
	Task      bool     // Gradle task name
	Dirs      []string // Task with a project path: candidate module directories
	Scanned   bool     // resolved only by the content scan (non-standard layout: class declaration + package statement)
	InPatch   bool     // some candidate file is touched by the test patch (set by JVMLocateTask)
}

var (
	jvmExts       = map[string]bool{".java": true, ".kt": true, ".groovy": true, ".scala": true}
	jvmTaskRe     = regexp.MustCompile(`^[A-Za-z0-9_.\-]*:[A-Za-z0-9_:\-]+$`)
	jvmBareTaskRe = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)
	jvmJUnit4Re   = regexp.MustCompile(`^(.*?)\(([\p{L}\p{N}_.$]+)\)$`)
	jvmTokenRe    = regexp.MustCompile(`^[\p{L}\p{N}_.$\-]+`)
	jvmPackageRe  = regexp.MustCompile(`(?m)^[ \t]*package[ \t]+([\w.]+)`)
	jvmDeclRe     = regexp.MustCompile(`(?m)^[ \t]*(?:(?:public|private|protected|internal|abstract|final|open|sealed|data|static|inline|value|annotation|enum|fun|case)[ \t]+)*(?:class|interface|object|enum|record|trait)[ \t]+([A-Za-z_]\w*)`)
	jvmTestDirSeg = map[string]bool{"test": true, "tests": true, "it": true, "integrationtest": true, "integration-test": true,
		"functionaltest": true, "inttest": true, "testfixtures": true, "testsrc": true, "e2e": true}
)

func jvmIsSrc(p string) bool { return jvmExts[path.Ext(p)] }

// jvmTestish: the path lies under a test-like directory (src/test/java, src/it, src/integrationTest, test/transform/src, ...) or is named
// like a test class. Used to prefer test files among same-named candidates and to bound the content scan.
func jvmTestish(p string) bool {
	segs := strings.Split(p, "/")
	for _, s := range segs[:len(segs)-1] {
		l := strings.ToLower(s)
		if jvmTestDirSeg[l] || strings.HasPrefix(l, "test") || strings.HasSuffix(l, "test") || strings.HasSuffix(l, "tests") {
			return true
		}
	}
	return false
}

// jvmCand is one reading of a test name: class cls in package pkg (dotted; "" = default package).
type jvmCand struct {
	pkg, cls string
	junit4   bool // read as the class argument of method(pkg.Class)
}

// parseJVMName returns the form and the candidate (package, class) readings of name, most likely first.
func parseJVMName(name string) (form string, cands []jvmCand, task bool) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "other", nil, false
	}
	if jvmTaskRe.MatchString(n) || (jvmBareTaskRe.MatchString(n) && !strings.ContainsAny(n, ".")) {
		return "task", nil, true
	}
	fqcn := func(s string) (jvmCand, bool) {
		if i := strings.IndexByte(s, '$'); i >= 0 { // Outer$Inner: the file is the top-level class's
			s = s[:i]
		}
		segs := nonEmpty(strings.Split(s, "."))
		if len(segs) == 0 {
			return jvmCand{}, false
		}
		return jvmCand{pkg: strings.Join(segs[:len(segs)-1], "."), cls: segs[len(segs)-1]}, true
	}
	form = "dotted"
	if m := jvmJUnit4Re.FindStringSubmatch(n); m != nil && m[1] != "" {
		// "testX(com.foo.BarTest)", "javac-Foo.java(lombok.transform.TestWithDelombok)"; a Java method with parameter types, e.g.
		// "com.foo.Bar.testX(java.lang.String)", is also matched: the class guess for it simply fails to resolve and the dotted reading follows.
		if c, ok := fqcn(m[2]); ok {
			c.junit4 = true
			cands = append(cands, c)
		}
	}
	left := n
	switch {
	case strings.Contains(n, "#"):
		left, form = n[:strings.Index(n, "#")], "hash"
	case strings.Contains(n, " > "):
		left, form = n[:strings.Index(n, " > ")], "gradle"
	}
	tok := jvmTokenRe.FindString(strings.TrimSpace(left))
	if i := strings.IndexByte(tok, '$'); i >= 0 {
		tok = tok[:i]
	}
	segs := nonEmpty(strings.Split(tok, "."))
	if len(segs) > 0 {
		// pkg.Class#method and pkg.Class > method name the class last. Free-form names (Kotlin "pkg.Class.some name", test names that
		// themselves contain '#' or " > ") are read as pkg.Class.method[ words]: the class is the longest prefix that is one, whole token first.
		if form == "hash" || form == "gradle" {
			cands = append(cands, jvmCand{pkg: strings.Join(segs[:len(segs)-1], "."), cls: segs[len(segs)-1]})
		}
		for k := len(segs); k >= 1; k-- {
			c := jvmCand{pkg: strings.Join(segs[:k-1], "."), cls: segs[k-1]}
			if len(cands) == 0 || cands[len(cands)-1] != c {
				cands = append(cands, c)
			}
		}
	}
	if len(cands) == 0 {
		form = "other"
	}
	return form, cands, false
}

func nonEmpty(s []string) []string {
	out := s[:0:0]
	for _, x := range s {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// jvmPrefetcher is implemented by trees backed by a partial clone: it fetches the wanted blobs in one batch before they are Read.
type jvmPrefetcher interface {
	Prefetch(want func(string) bool) error
}

// LocateJVMTests locates the defining test source file(s) of each FAIL_TO_PASS name in tree t (the P tree: base + test patch).
// Pass 1 uses file paths only (package directory + class name, any source root: src/test/java, src/test/kotlin, src/it, src/integrationTest,
// groovy, test/transform/src, ...). Names it cannot place go to pass 2, which reads the test-like source files and matches
// `package` statement + class declaration (non-standard layouts, several classes per Kotlin file).
func LocateJVMTests(t tree.Tree, names []string) []JVMTestLoc {
	stems := map[string][]string{} // class stem -> source files
	for _, p := range t.Files() {
		if jvmIsSrc(p) {
			stem := strings.TrimSuffix(path.Base(p), path.Ext(p))
			stems[stem] = append(stems[stem], p)
		}
	}
	out := make([]JVMTestLoc, len(names))
	cands := make([][]jvmCand, len(names))
	for i, n := range names {
		l := JVMTestLoc{Name: n}
		var task bool
		l.Form, cands[i], task = parseJVMName(n)
		out[i] = l
		if task {
			out[i].Task = true
			out[i].Dirs = gradleTaskDirs(t, n)
			continue
		}
		jvmResolvePaths(&out[i], cands[i], stems)
	}
	var scan *jvmScan
	for i := range out {
		l := &out[i]
		if l.Task || len(l.Files) > 0 {
			continue
		}
		if scan == nil {
			scan = scanJVMDecls(t)
		}
		for _, c := range cands[i] {
			if files := scan.find(c); len(files) > 0 {
				l.Files, l.Class, l.Scanned = files, jvmFQCN(c), true
				if c.junit4 {
					l.Form = "junit4"
				}
				break
			}
		}
	}
	for i := range out {
		l := &out[i]
		sort.Strings(l.Files)
		l.NotFound = !l.Task && len(l.Files) == 0
		l.Ambiguous = len(l.Files) > 1
	}
	return out
}

func jvmFQCN(c jvmCand) string {
	if c.pkg == "" {
		return c.cls
	}
	return c.pkg + "." + c.cls
}

// jvmResolvePaths fills l from the first candidate reading whose package directory + class name matches a source file.
func jvmResolvePaths(l *JVMTestLoc, cands []jvmCand, stems map[string][]string) {
	for _, c := range cands {
		pkgPath := strings.ReplaceAll(c.pkg, ".", "/")
		var hits []string
		for _, p := range stems[c.cls] {
			d := path.Dir(p)
			if pkgPath == "" || d == pkgPath || strings.HasSuffix(d, "/"+pkgPath) {
				hits = append(hits, p)
			}
		}
		// Several same-named files (a main class and its test, or the same test in several modules): prefer test-like locations.
		var tests []string
		for _, p := range hits {
			if jvmTestish(p) {
				tests = append(tests, p)
			}
		}
		if len(tests) > 0 {
			hits = tests
		}
		if len(hits) > 0 {
			l.Files, l.Class = hits, jvmFQCN(c)
			if c.junit4 {
				l.Form = "junit4"
			}
			return
		}
	}
}

// gradleTaskDirs maps a Gradle task "a:b:test" to the directories of project :a:b (a directory with a build.gradle[.kts] whose path ends in a/b).
func gradleTaskDirs(t tree.Tree, task string) []string {
	i := strings.LastIndexByte(task, ':')
	if i <= 0 {
		return nil
	}
	proj := strings.Trim(task[:i], ":")
	if proj == "" {
		return nil
	}
	suf := strings.ReplaceAll(proj, ":", "/")
	var dirs []string
	for _, p := range t.Files() {
		b := path.Base(p)
		if b != "build.gradle" && b != "build.gradle.kts" {
			continue
		}
		d := path.Dir(p)
		if d == suf || strings.HasSuffix(d, "/"+suf) {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

type jvmDecl struct{ file, pkg string }

type jvmScan struct{ byClass map[string][]jvmDecl }

// scanJVMDecls indexes the class declarations (and the package of their file) of every test-like source file of t.
func scanJVMDecls(t tree.Tree) *jvmScan {
	if pf, ok := t.(jvmPrefetcher); ok {
		pf.Prefetch(func(p string) bool { return jvmIsSrc(p) && jvmTestish(p) })
	}
	s := &jvmScan{byClass: map[string][]jvmDecl{}}
	for _, p := range t.Files() {
		if !jvmIsSrc(p) || !jvmTestish(p) {
			continue
		}
		b, err := t.Read(p)
		if err != nil {
			continue
		}
		src := string(b)
		pkg := ""
		if m := jvmPackageRe.FindStringSubmatch(src); m != nil {
			pkg = m[1]
		}
		seen := map[string]bool{}
		for _, m := range jvmDeclRe.FindAllStringSubmatch(src, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				s.byClass[m[1]] = append(s.byClass[m[1]], jvmDecl{p, pkg})
			}
		}
	}
	return s
}

func (s *jvmScan) find(c jvmCand) []string {
	var files []string
	for _, d := range s.byClass[c.cls] {
		if c.pkg == "" || d.pkg == c.pkg {
			files = append(files, d.file)
		}
	}
	return files
}

// patchFiles lists the files a unified diff touches (the b/ side of every "diff --git" header).
func patchFiles(patch string) map[string]bool {
	out := map[string]bool{}
	for _, ln := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(ln, "diff --git ") {
			continue
		}
		if i := strings.LastIndex(ln, " b/"); i >= 0 {
			out[strings.TrimSpace(ln[i+3:])] = true
		}
	}
	return out
}

// JVMLocateRow is the locator outcome for one task.
type JVMLocateRow struct {
	ID     string       `json:"instance_id"`
	Repo   string       `json:"repo"`
	Source string       `json:"source"`
	Kind   string       `json:"f2p_kind,omitempty"`
	F2P    int          `json:"f2p_count"`
	Err    string       `json:"error,omitempty"`
	Locs   []JVMTestLoc `json:"-"`
	Unique int          `json:"unique"`
	Ambig  int          `json:"ambiguous"`
	NotFnd int          `json:"not_found"`
	Tasks  int          `json:"gradle_tasks"`
	// InPatch counts located names whose file the test patch touches (for n2p tasks these are the names the oracle keeps).
	InPatch int `json:"in_patch"`
}

// jvmGitTree adds batched blob prefetch (partial clones) to gitTree.
type jvmGitTree struct {
	*gitTree
	ctx     context.Context
	repoDir string
}

func (g jvmGitTree) Prefetch(want func(string) bool) error {
	return prefetchBlobs(g.ctx, g.repoDir, g.gitTree, want)
}

// JVMLocateTask builds P (base + test patch, git plumbing only, no checkout) for one task in the clone repoDir and runs LocateJVMTests
// over the P tree; blobs are fetched only if pass 2 (content scan) is needed.
func JVMLocateTask(ctx context.Context, repoDir, idxFile string, t Task) JVMLocateRow {
	row := JVMLocateRow{ID: t.InstanceID, Repo: t.Repo, Source: t.Source, Kind: t.F2PKind}
	ids, err := ParseF2P(t.F2P)
	if err != nil {
		row.Err = err.Error()
		return row
	}
	row.F2P = len(ids)
	ib := newIndexBuilder(repoDir, idxFile, t.BaseCommit)
	defer ib.Close()
	p, err := ib.Apply(ctx, t.TestPatch, "eval: test patch")
	if err != nil {
		row.Err = "test_patch: " + err.Error()
		return row
	}
	gt, err := newGitTreeWant(ctx, repoDir, p, func(string) bool { return false })
	if err != nil {
		row.Err = err.Error()
		return row
	}
	defer gt.Close()
	row.Locs = LocateJVMTests(jvmGitTree{gt, ctx, repoDir}, ids)
	tp := patchFiles(t.TestPatch)
	for i := range row.Locs {
		l := &row.Locs[i]
		for _, f := range l.Files {
			if tp[f] {
				l.InPatch = true
			}
		}
		if l.InPatch {
			row.InPatch++
		}
		switch {
		case l.Task:
			row.Tasks++
		case l.NotFound:
			row.NotFnd++
		case l.Ambiguous:
			row.Ambig++
		default:
			row.Unique++
		}
	}
	return row
}

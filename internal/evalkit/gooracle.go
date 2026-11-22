package evalkit

import (
	"context"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/qxbao/bgraph/internal/tree"
)

// GoTestLoc is where one Go FAIL_TO_PASS test name is defined, as far as a source scan can tell.
//
// The safe unit of the Go ecosystem is the package directory (unit ID "go:<dir>", "go:." for the repo root), so the locator ends in
// Units rather than in files. Dataset name forms handled:
//
//	TestX, TestX/sub/case, BenchmarkX, ExampleX_suffix, FuzzX   "top"      (Multi-SWE-bench, SWE-bench_Multilingual)
//	<import path>/TestX/sub                                     "pkg"      (SWE-bench-Live_MultiLang; the import path names the package)
//	<import path>.TestX, <import path>::TestX                   "pkg"
//	Suite.TestMethod, or a bare TestMethod that is a method     "method"   (testify/gocheck suite method: the package that defines the
//	                                                                        method runs it; the runner func is a top-level func there)
//	<import path> alone (a package-level result)               "package"  (every _test.go file of that package; ginkgo suites report this way)
//	free-text names ("Config Suite :: Config :: ...")           "ginkgo" | "gocheck" | "other"  (not-found: no Go func carries the name)
//
// TODO(go-phase-2): required units for T1 are the Units of every located name (ambiguous names require all of them); for
// f2p_kind == "n2p" keep only names with InPatch set (a candidate file is touched by the test patch); NotFound names must be reported
// as unmapped, never silently dropped.
type GoTestLoc struct {
	Name       string
	Form       string
	Func       string   // top-level test function (or method) the name resolved to
	Pkg        string   // import path / type qualifier given in the name ("" for bare names)
	Files      []string // candidate _test.go files (sorted)
	Units      []string // go:<dir> of the candidates (sorted, unique)
	PatchUnits []string // Units of the candidates the test patch touches
	Ambiguous  bool     // more than one unit
	NotFound   bool
	InPatch    bool // some candidate file is touched by the test patch
	PkgHit     bool // the import path in the name picked the candidates (otherwise it was ignored or absent)
	NoModule   bool // some candidate lies under no go.mod (GOPATH layout)
}

// GoUnitID is the unit ID of a package directory ("." = repo root).
func GoUnitID(dir string) string { return "go:" + dir }

// GoUnitDir is the package directory of a repo-relative file ("." for the root).
func GoUnitDir(file string) string { return path.Dir(file) }

// goIgnoredDir: go ignores directories named testdata/vendor and those starting with "." or "_" (no package, no test run there).
func goIgnoredDir(dir string) bool {
	if dir == "." {
		return false
	}
	for _, s := range strings.Split(dir, "/") {
		if s == "testdata" || s == "vendor" || s == "node_modules" || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "_") {
			return true
		}
	}
	return false
}

func goIsTestFile(p string) bool {
	return strings.HasSuffix(p, "_test.go") && !goIgnoredDir(path.Dir(p))
}

var (
	goTopRe     = regexp.MustCompile(`(?m)^func\s+((?:Test|Benchmark|Example|Fuzz)[\p{L}\p{N}_]*)\s*\(`)
	goMethodRe  = regexp.MustCompile(`(?m)^func\s*\(\s*(?:[\p{L}\p{N}_]+\s+)?\*?\s*([\p{L}\p{N}_]+)(?:\[[^\]]*\])?\s*\)\s*((?:Test|Benchmark|Example|Fuzz)[\p{L}\p{N}_]*)\s*\(`)
	goModuleRe  = regexp.MustCompile(`(?m)^\s*module\s+"?([^\s"]+)`)
	goGinkgoRe  = regexp.MustCompile(`"github.com/onsi/(ginkgo|gomega)`)
	goGocheckRe = regexp.MustCompile(`"(gopkg.in/check\.v1|github.com/go-check/check)"`)
)

// goFileScan is what the locator needs from one _test.go blob.
type goFileScan struct {
	funcs   []string
	methods [][2]string // receiver type, method
	ginkgo  bool
	gocheck bool
}

func scanGoTest(src []byte) *goFileScan {
	s := string(src)
	fs := &goFileScan{ginkgo: goGinkgoRe.MatchString(s), gocheck: goGocheckRe.MatchString(s)}
	for _, m := range goTopRe.FindAllStringSubmatch(s, -1) {
		fs.funcs = append(fs.funcs, m[1])
	}
	for _, m := range goMethodRe.FindAllStringSubmatch(s, -1) {
		fs.methods = append(fs.methods, [2]string{m[1], m[2]})
	}
	return fs
}

// goScanCache memoizes per-blob scans across tasks (consecutive tasks of a repo share almost all blobs):
// oid -> *goFileScan for _test.go files, oid -> string (module path) for go.mod files.
var goScanCache sync.Map

// goTestName is a parsed dataset name.
type goTestName struct {
	form, top, pkg string
}

func goIdentByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= 0x80
}

// goBoundaryTop finds the first test-func-like identifier (Test/Benchmark/Example/Fuzz followed by a non-lowercase rune) that starts
// the name or follows '/', '.' or ':'; pkg is what precedes it.
func goBoundaryTop(n string) (top, pkg string, ok bool) {
	for i := 0; i < len(n); i++ {
		if !(i == 0 || n[i-1] == '/' || n[i-1] == '.' || n[i-1] == ':') {
			continue
		}
		for _, pre := range [...]string{"Test", "Benchmark", "Example", "Fuzz"} {
			if !strings.HasPrefix(n[i:], pre) {
				continue
			}
			if rest := n[i+len(pre):]; rest != "" && rest[0] >= 'a' && rest[0] <= 'z' { // "Testing", "Examples": not a test func
				continue
			}
			j := i
			for j < len(n) && goIdentByte(n[j]) {
				j++
			}
			// The top-level name ends the string or is followed by "/sub" (go test prints no spaces before the first '/').
			if j < len(n) && n[j] != '/' {
				continue
			}
			return n[i:j], strings.TrimRight(n[:i], "/.:"), true
		}
	}
	return "", "", false
}

func parseGoName(name string) goTestName {
	n := strings.TrimSpace(name)
	top, pkg, ok := goBoundaryTop(n)
	if !ok || strings.HasPrefix(n, "/") || strings.ContainsAny(pkg, " 	") { // "/tmp/TestX123/a.md", "Removed ... from: /tmp/TestX/..": logged text
		if !strings.ContainsAny(n, " 	") && strings.Contains(n, "/") && !strings.HasPrefix(n, "/") {
			return goTestName{form: "package", pkg: n} // a bare import path: the whole package ran (ginkgo suites report this way)
		}
		return goTestName{form: "other"}
	}
	if pkg == "" {
		return goTestName{form: "top", top: top}
	}
	// A prefix that is not an import path (no '/', no '.') is a type qualifier: Suite.TestMethod.
	if !strings.ContainsAny(pkg, "/.") && !strings.Contains(n, "::") {
		return goTestName{form: "method", top: top, pkg: pkg}
	}
	return goTestName{form: "pkg", top: top, pkg: pkg}
}

// goIndex is the per-P-tree index of test functions.
type goIndex struct {
	funcs   map[string][]string    // top-level func -> files
	methods map[string][]string    // method -> files
	recv    map[[2]string][]string // (receiver type, method) -> files
	mods    map[string]string      // go.mod dir -> module path
	ginkgo  bool
	gocheck bool
	tests   []string // every _test.go file
}

func buildGoIndex(t tree.Tree) *goIndex {
	ix := &goIndex{funcs: map[string][]string{}, methods: map[string][]string{}, recv: map[[2]string][]string{}, mods: map[string]string{}}
	for _, f := range t.Files() {
		switch {
		case path.Base(f) == "go.mod" && !goIgnoredDir(path.Dir(f)):
			key := t.Key(f)
			if v, ok := goScanCache.Load(key); ok {
				ix.mods[path.Dir(f)] = v.(string)
				continue
			}
			b, err := t.Read(f)
			if err != nil {
				continue
			}
			mod := ""
			if m := goModuleRe.FindSubmatch(b); m != nil {
				mod = string(m[1])
			}
			goScanCache.Store(key, mod)
			ix.mods[path.Dir(f)] = mod
		case goIsTestFile(f):
			ix.tests = append(ix.tests, f)
			key := t.Key(f)
			var fs *goFileScan
			if v, ok := goScanCache.Load(key); ok {
				fs = v.(*goFileScan)
			} else {
				b, err := t.Read(f)
				if err != nil {
					continue
				}
				fs = scanGoTest(b)
				goScanCache.Store(key, fs)
			}
			for _, fn := range fs.funcs {
				ix.funcs[fn] = append(ix.funcs[fn], f)
			}
			for _, m := range fs.methods {
				ix.methods[m[1]] = append(ix.methods[m[1]], f)
				ix.recv[m] = append(ix.recv[m], f)
			}
			ix.ginkgo = ix.ginkgo || fs.ginkgo
			ix.gocheck = ix.gocheck || fs.gocheck
		}
	}
	return ix
}

// moduleOf returns the dir of the innermost go.mod containing dir.
func (ix *goIndex) moduleOf(dir string) (string, bool) {
	for d := dir; ; d = path.Dir(d) {
		if _, ok := ix.mods[d]; ok {
			return d, true
		}
		if d == "." || d == "/" || d == "" {
			return "", false
		}
	}
}

// importPath is the import path of dir ("" when it lies under no go.mod).
func (ix *goIndex) importPath(dir string) string {
	md, ok := ix.moduleOf(dir)
	if !ok {
		return ""
	}
	mp := ix.mods[md]
	switch {
	case dir == md:
		return mp
	case md == ".":
		return mp + "/" + dir
	}
	return mp + "/" + strings.TrimPrefix(dir, md+"/")
}

func uniqSorted(s []string) []string {
	sort.Strings(s)
	out := s[:0]
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// LocateGoTests resolves every name to candidate _test.go files and package units in the tree t. patch is the set of files the test patch
// touches (may be nil).
func LocateGoTests(t tree.Tree, names []string, patch map[string]bool) []GoTestLoc {
	return locateGo(buildGoIndex(t), names, patch)
}

func locateGo(ix *goIndex, names []string, patch map[string]bool) []GoTestLoc {
	out := make([]GoTestLoc, 0, len(names))
	for _, name := range names {
		pn := parseGoName(name)
		l := GoTestLoc{Name: name, Form: pn.form, Func: pn.top, Pkg: pn.pkg}
		var files []string
		switch pn.form {
		case "top", "pkg":
			files = ix.funcs[pn.top]
			if len(files) == 0 { // a suite method named directly
				if files = ix.methods[pn.top]; len(files) > 0 {
					l.Form = "method"
				}
			}
		case "package":
			for _, f := range ix.tests {
				if ix.importPath(path.Dir(f)) == pn.pkg {
					files = append(files, f)
				}
			}
			l.PkgHit = len(files) > 0
		case "method":
			if files = ix.recv[[2]string{pn.pkg, pn.top}]; len(files) == 0 {
				if files = ix.funcs[pn.top]; len(files) == 0 {
					files = ix.methods[pn.top]
				}
			}
		}
		files = uniqSorted(append([]string(nil), files...))
		if pn.form == "pkg" && len(files) > 0 {
			var hit []string
			for _, f := range files {
				if ix.importPath(path.Dir(f)) == pn.pkg {
					hit = append(hit, f)
				}
			}
			if len(hit) > 0 {
				files, l.PkgHit = hit, true
			}
		}
		if len(files) == 0 {
			l.NotFound = true
			if pn.form == "other" {
				switch {
				case ix.ginkgo:
					l.Form = "ginkgo"
				case ix.gocheck:
					l.Form = "gocheck"
				}
			}
			out = append(out, l)
			continue
		}
		l.Files = files
		var units, pu []string
		for _, f := range files {
			d := path.Dir(f)
			units = append(units, GoUnitID(d))
			if _, ok := ix.moduleOf(d); !ok {
				l.NoModule = true
			}
			if patch[f] {
				l.InPatch = true
				pu = append(pu, GoUnitID(d))
			}
		}
		l.Units, l.PatchUnits = uniqSorted(units), uniqSorted(pu)
		l.Ambiguous = len(l.Units) > 1
		out = append(out, l)
	}
	return out
}

// GoLocateRow is the locator outcome for one task.
type GoLocateRow struct {
	ID      string      `json:"instance_id"`
	Repo    string      `json:"repo"`
	Source  string      `json:"source"`
	Kind    string      `json:"f2p_kind,omitempty"`
	F2P     int         `json:"f2p_count"`
	Err     string      `json:"error,omitempty"`
	Locs    []GoTestLoc `json:"-"`
	Unique  int         `json:"unique"`
	Ambig   int         `json:"ambiguous"`
	NotFnd  int         `json:"not_found"`
	InPatch int         `json:"in_patch"`
}

// GoLocateTask builds P (base + test patch, git plumbing only, no checkout) for one task in the clone repoDir, prefetches the _test.go
// and go.mod blobs of the tree that are not scanned yet in one batch (partial clone) and runs LocateGoTests.
func GoLocateTask(ctx context.Context, repoDir, idxFile string, t Task) GoLocateRow {
	row := GoLocateRow{ID: t.InstanceID, Repo: t.Repo, Source: t.Source, Kind: t.F2PKind}
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
	if err := prefetchBlobs(ctx, repoDir, gt, func(f string) bool {
		if !(goIsTestFile(f) || path.Base(f) == "go.mod") {
			return false
		}
		_, cached := goScanCache.Load(gt.oid[f])
		return !cached
	}); err != nil {
		row.Err = "prefetch: " + err.Error()
		return row
	}
	row.Locs = LocateGoTests(gt, ids, patchFiles(t.TestPatch))
	for _, l := range row.Locs {
		switch {
		case l.NotFound:
			row.NotFnd++
		case l.Ambiguous:
			row.Ambig++
		default:
			row.Unique++
		}
		if l.InPatch {
			row.InPatch++
		}
	}
	return row
}

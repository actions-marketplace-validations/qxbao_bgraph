package evalkit

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/tree"
)

// PyTestLoc is where one Python FAIL_TO_PASS test name is defined, as far as a source scan can tell.
//
// Phase 1 (this file) only locates test files. TODO(python-phase-2): once internal/eco/python lands, map Files to python units (one unit per
// pytest test file) and run T1 against them; ambiguous names should then require the union of the units of all candidate files, and
// NotFound names should be reported as unmapped (never silently dropped).
type PyTestLoc struct {
	Name      string
	Files     []string // candidate test files (sorted)
	Ambiguous bool     // more than one candidate file
	NotFound  bool     // no candidate file
	// Form is how the name was interpreted: "nodeid" (pytest node id), "django" (test_x (mod.Class)), "docstring" (django shows the first
	// line of a test's docstring instead of its name), "bare" (sympy-style function name) or "other".
	Form string
}

var (
	// Django's unittest runner prints "test_name (module.Class)" or, on newer versions, "test_name (module.Class.test_name)".
	pyDjangoName = regexp.MustCompile(`^(\w+) \(([\w.]+)\)$`)
	pyBareName   = regexp.MustCompile(`^\w+$`)
	pyTestDef    = regexp.MustCompile(`(?m)^[ \t]*(?:async[ \t]+)?def[ \t]+(test\w*)`)
	// def f(...): followed by a docstring; group 1 is the function name, groups 2-5 the docstring body (triple- or single-quoted).
	pyDocDef = regexp.MustCompile(`(?s)\bdef[ \t]+(\w+)[ \t]*\([^)]*\)[ \t]*(?:->[^:\n]*)?:[ \t]*(?:#[^\n]*)?\n[ \t]*[rRuU]?(?:"""(.*?)"""|'''(.*?)'''|"((?:[^"\\\n]|\\.)*)"|'((?:[^'\\\n]|\\.)*)')`)
)

// pyIsTestFile: a .py file under a tests/test/testing directory or named like a test module.
func pyIsTestFile(p string) bool {
	if !strings.HasSuffix(p, ".py") {
		return false
	}
	segs := strings.Split(p, "/")
	for _, s := range segs[:len(segs)-1] {
		if s == "tests" || s == "test" || s == "testing" {
			return true
		}
	}
	b := segs[len(segs)-1]
	return strings.HasPrefix(b, "test_") || strings.HasSuffix(b, "_test.py") || b == "tests.py"
}

// pyNeedsScan reports whether any name needs file contents (bare function names, django docstring names); node ids and django
// "name (module.Class)" names are resolved from the file list alone.
func pyNeedsScan(names []string) bool {
	for _, n := range names {
		if pyForm(n) != "nodeid" && pyForm(n) != "django" {
			return true
		}
	}
	return false
}

func pyForm(n string) string {
	switch {
	case strings.Contains(n, "::") || (strings.HasSuffix(n, ".py") && !strings.ContainsAny(n, " ()")):
		return "nodeid"
	case pyDjangoName.MatchString(n):
		return "django"
	case pyBareName.MatchString(n):
		return "bare"
	default:
		return "docstring"
	}
}

// LocatePythonTests locates the defining test file(s) of each FAIL_TO_PASS name in tree t (the P tree: base + test patch).
// repo selects repo-specific roots (django resolves dotted modules under tests/).
func LocatePythonTests(t tree.Tree, repo string, names []string) []PyTestLoc {
	out := make([]PyTestLoc, len(names))
	var defs map[string]map[string]bool // test function name -> files defining it
	var docs map[string]map[string]bool // first docstring line -> files
	needDocs := false
	for _, n := range names {
		needDocs = needDocs || pyForm(n) == "docstring"
	}
	if pyNeedsScan(names) {
		defs, docs = scanPyTests(t, needDocs)
	}
	for i, n := range names {
		l := PyTestLoc{Name: n, Form: pyForm(n)}
		var files []string
		switch l.Form {
		case "nodeid":
			files = locateNodeID(t, n)
		case "django":
			files = locateDjango(t, repo, n)
		case "bare":
			files = sortedKeys(defs[n])
		case "docstring":
			files = sortedKeys(docs[strings.TrimSpace(n)])
			if len(files) == 0 {
				l.Form = "other"
			}
		}
		l.Files = files
		l.NotFound = len(files) == 0
		l.Ambiguous = len(files) > 1
		out[i] = l
	}
	return out
}

// locateNodeID: "path/to/test_x.py::TestC::test_y[param with :: or spaces]": the file is everything before the first "::".
func locateNodeID(t tree.Tree, n string) []string {
	f := n
	if i := strings.Index(n, "::"); i >= 0 {
		f = n[:i]
	}
	f = strings.TrimSpace(strings.ReplaceAll(f, "\\", "/"))
	f = strings.TrimPrefix(path.Clean(f), "./")
	if t.Has(f) {
		return []string{f}
	}
	return nil
}

// locateDjango: "test_x (pkg.mod.Class)" or "test_x (pkg.mod.Class.test_x)". The longest dotted prefix that is a module wins:
// tests/pkg/mod.py, tests/pkg/mod/__init__.py or tests/pkg/mod/tests.py (a test package's tests live in tests.py by convention).
func locateDjango(t tree.Tree, repo, n string) []string {
	m := pyDjangoName.FindStringSubmatch(n)
	parts := strings.Split(m[2], ".")
	roots := []string{"tests/"}
	if !strings.EqualFold(repo, "django/django") {
		roots = []string{"", "tests/"}
	}
	for k := len(parts) - 1; k >= 1; k-- {
		base := strings.Join(parts[:k], "/")
		var files []string
		for _, r := range roots {
			for _, c := range []string{r + base + ".py", r + base + "/__init__.py", r + base + "/tests.py"} {
				if t.Has(c) {
					files = append(files, c)
				}
			}
		}
		if len(files) > 0 {
			sort.Strings(files)
			return files
		}
	}
	return nil
}

// scanPyTests indexes the test files: test function names (defs) and, when needDocs, the first docstring line of every function (docs; django reports a test by it).
func scanPyTests(t tree.Tree, needDocs bool) (defs, docs map[string]map[string]bool) {
	defs, docs = map[string]map[string]bool{}, map[string]map[string]bool{}
	add := func(m map[string]map[string]bool, k, f string) {
		if m[k] == nil {
			m[k] = map[string]bool{}
		}
		m[k][f] = true
	}
	for _, p := range t.Files() {
		if !pyIsTestFile(p) {
			continue
		}
		b, err := t.Read(p)
		if err != nil {
			continue
		}
		src := string(b)
		for _, m := range pyTestDef.FindAllStringSubmatch(src, -1) {
			add(defs, m[1], p)
		}
		if !needDocs {
			continue
		}
		for _, m := range pyDocDef.FindAllStringSubmatch(src, -1) {
			// unittest's shortDescription: the first line of the stripped docstring.
			d := strings.TrimSpace(m[2] + m[3] + m[4] + m[5])
			if i := strings.IndexByte(d, '\n'); i >= 0 {
				d = strings.TrimSpace(d[:i])
			}
			if d != "" {
				add(docs, d, p)
			}
		}
	}
	return defs, docs
}

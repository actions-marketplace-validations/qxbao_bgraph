package evalkit

import (
	"path"
	"regexp"
	"strings"

	"github.com/qxbao/bgraph/internal/tree"
)

// DartTestLoc is where one Dart FAIL_TO_PASS name is defined, as far as a source scan can tell.
//
// The safe unit of the Dart ecosystem is the test file and its unit ID is the repo-relative path (file-granular, like Ruby and
// Python), so Files are unit IDs directly unless the package has a whole-project unit. Dataset name forms (Longtail-SWE and
// SWE-rebench-V2 harnesses, inspected on 2026-11-24):
//
//	/repo/pkg/test/a_test.dart: group test       "path-name"   (absolute container path, repo-relative or package-relative path)
//	/repo/pkg/test/a_test.dart                   "path"        (the suite itself: a load or compile result)
//	loading test/a_test.dart                     "loading"     (package:test's load pseudo-test)
//	(setUpAll), group (tearDownAll)              "hook"        (setUpAll/tearDownAll pseudo-tests; no file in the name)
//	Group FooTest | test_bar                     "reflective"  (test_reflective_loader: class | method)
//	group description test description           "name"        (package:test joins group and test descriptions with a space;
//	                                                            one harness joins them with " > ")
//
// A "name" must decompose into descriptions defined by one test file or by the helpers it imports (transitively, without
// entering other test files). Descriptions are, in widening stages, tried until one matches:
//
//	desc      string literals that are the first argument of a test-like call (group, test, testWidgets, blocTest, custom
//	          wrappers whose name contains test/group/...), and an upper-case identifier there (a Type description)
//	template  the same with interpolations as wildcards (at least 3 static characters)
//	weak      also first-argument literals of any other call and name:/description: named arguments (parameterized helpers)
//	dynamic   additionally one span of the name may come from a computed description (test(x.name)), when the file or its
//	          helpers have one, if the static descriptions cover at least half of the name
//
// When a name has several candidates and the task's test command restricts the run to some package directories or test files,
// candidates outside that scope are removed (they could not have produced the result): Via gets "+cmd".
type DartTestLoc struct {
	Name       string   `json:"name"`
	Form       string   `json:"form"`
	Path       string   `json:"path,omitempty"`  // the path written in the name
	Class      string   `json:"class,omitempty"` // reflective form
	Files      []string `json:"files,omitempty"`
	PatchFiles []string `json:"patch_files,omitempty"`
	Helpers    []string `json:"helpers,omitempty"`
	Via        string   `json:"via,omitempty"`
	Ambiguous  bool     `json:"ambiguous,omitempty"`
	NotFound   bool     `json:"not_found,omitempty"`
	NotUnit    bool     `json:"not_unit,omitempty"` // located only in files that are not Dart test files (contract §1)
	Hook       bool     `json:"hook,omitempty"`
	InPatch    bool     `json:"in_patch,omitempty"`
}

// dartSkipDir: generated, cache and symlink directories that never hold package sources (contract §2).
func dartSkipDir(dir string) bool {
	for _, s := range strings.Split(dir, "/") {
		switch s {
		case ".dart_tool", "build", ".pub-cache", ".symlinks", ".plugin_symlinks", "node_modules", ".git":
			return true
		}
	}
	return false
}

// DartPackageDir is the nearest directory at or above dir holding a pubspec.yaml ("" for the repo root), and whether one exists.
func DartPackageDir(t tree.Tree, p string) (string, bool) {
	for d := path.Dir(p); ; d = path.Dir(d) {
		if d == "." {
			d = ""
		}
		if t.Has(path.Join(d, "pubspec.yaml")) {
			return d, true
		}
		if d == "" {
			return "", false
		}
	}
}

// DartIsTestFile is the contract's default test-file rule: <pkg>/test/**/*_test.dart or <pkg>/integration_test/**/*_test.dart where
// <pkg> is the nearest directory with a pubspec.yaml. dart_test.yaml overrides (paths, filename) are not considered here.
func DartIsTestFile(t tree.Tree, p string) bool {
	if !strings.HasSuffix(p, "_test.dart") || dartSkipDir(path.Dir(p)) {
		return false
	}
	pkg, ok := DartPackageDir(t, p)
	if !ok {
		return false
	}
	rel := p
	if pkg != "" {
		rel = strings.TrimPrefix(p, pkg+"/")
	}
	return strings.HasPrefix(rel, "test/") || strings.HasPrefix(rel, "integration_test/")
}

// dartTestSide: test files and .dart files under a test-side directory (helpers define shared groups and test wrappers).
func dartTestSide(p string) bool {
	if !strings.HasSuffix(p, ".dart") || dartSkipDir(path.Dir(p)) {
		return false
	}
	if strings.HasSuffix(p, "_test.dart") {
		return true
	}
	for _, s := range strings.Split(path.Dir(p), "/") {
		switch s {
		case "test", "integration_test", "test_driver", "testing", "test_utils", "test_helpers":
			return true
		}
	}
	return false
}

// DartIndexFiles is the set of files the locator reads: the test side, plus every .dart file under a lib/ directory that holds a
// test-side file (test-helper packages such as dio_test/lib/src/test/ are reached through their public library).
func DartIndexFiles(files []string) map[string]bool {
	out := map[string]bool{}
	helperLibs := map[string]bool{}
	for _, f := range files {
		if dartTestSide(f) {
			out[f] = true
			if i := strings.LastIndex(f, "/lib/"); i >= 0 {
				helperLibs[f[:i+len("/lib/")]] = true
			} else if strings.HasPrefix(f, "lib/") {
				helperLibs["lib/"] = true
			}
		}
	}
	if len(helperLibs) == 0 {
		return out
	}
	for _, f := range files {
		if !strings.HasSuffix(f, ".dart") || out[f] || dartSkipDir(path.Dir(f)) {
			continue
		}
		if i := strings.LastIndex(f, "/lib/"); i >= 0 && helperLibs[f[:i+len("/lib/")]] || strings.HasPrefix(f, "lib/") && helperLibs["lib/"] {
			out[f] = true
		}
	}
	return out
}

// ---- name parsing ----

type dartName struct {
	form   string // path | path-name | loading | hook | reflective | name
	path   string
	test   string
	class  string
	method string
}

var (
	dartPathRe       = regexp.MustCompile(`^(?:loading )?(/?[\w.@+-]+(?:/[\w.@+-]+)*\.dart)(?::\s*(.*))?$`)
	dartReflectiveRe = regexp.MustCompile(`^(?:.* )?([A-Za-z_$][\w$]*) \| ((?:solo_)?test_\w+)$`)
)

func parseDartName(name string) dartName {
	n := strings.TrimRight(name, " \t\r\n")
	if m := dartPathRe.FindStringSubmatch(n); m != nil {
		if strings.HasPrefix(n, "loading ") {
			return dartName{form: "loading", path: m[1]}
		}
		if strings.Contains(n, ".dart:") {
			return dartName{form: "path-name", path: m[1], test: m[2]}
		}
		return dartName{form: "path", path: m[1]}
	}
	if dartIsHook(n) {
		return dartName{form: "hook", test: n}
	}
	if m := dartReflectiveRe.FindStringSubmatch(n); m != nil {
		return dartName{form: "reflective", test: name, class: m[1], method: m[2]}
	}
	return dartName{form: "name", test: name}
}

func dartIsHook(n string) bool {
	return strings.HasSuffix(n, "(setUpAll)") || strings.HasSuffix(n, "(tearDownAll)")
}

// ---- file scan ----

// dartLit is a string literal; an interpolated literal keeps its static pieces (len > 1).
type dartLit []string

// dartToken is one lexical token: an identifier, a string literal (lit != nil) or a punctuation byte.
type dartToken struct {
	ident    string
	lit      dartLit
	typeName string // the literal is exactly '$Type' / '${Type}': it reads as the type's name
	punct    byte
}

var dartTypeLit = regexp.MustCompile(`^['"]\$\{?([A-Z][\w$]*)\}?['"]$`)

// lexDart tokenizes Dart source: comments are skipped, numbers and operators become punctuation, string literals are decoded
// (adjacent literals are concatenated by the caller).
func lexDart(s string) []dartToken {
	var out []dartToken
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			depth := 0
			for i < len(s) {
				if s[i] == '/' && i+1 < len(s) && s[i+1] == '*' {
					depth++
					i += 2
				} else if s[i] == '*' && i+1 < len(s) && s[i+1] == '/' {
					depth--
					i += 2
					if depth == 0 {
						break
					}
				} else {
					i++
				}
			}
		case c == '\'' || c == '"' || (c == 'r' && i+1 < len(s) && (s[i+1] == '\'' || s[i+1] == '"')):
			lit, n := readDartLit(s[i:])
			if n <= 0 {
				return out
			}
			tk := dartToken{lit: lit}
			if m := dartTypeLit.FindStringSubmatch(s[i : i+n]); m != nil {
				tk.typeName = m[1]
			}
			out = append(out, tk)
			i += n
		case c == '_' || c == '$' || isASCIILetter(c):
			j := i + 1
			for j < len(s) && (s[j] == '_' || s[j] == '$' || isASCIILetter(s[j]) || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			out = append(out, dartToken{ident: s[i:j]})
			i = j
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		default:
			out = append(out, dartToken{punct: c})
			i++
		}
	}
	return out
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// readDartLit decodes the string literal at the start of s ('…', "…", ”'…”', """…""", raw r'…') and returns it with the number
// of bytes consumed (0 when unterminated). Interpolations ($x, ${…}) split the literal into pieces.
func readDartLit(s string) (dartLit, int) {
	i := 0
	raw := false
	if s[0] == 'r' {
		raw, i = true, 1
	}
	q := s[i]
	triple := strings.HasPrefix(s[i:], strings.Repeat(string(q), 3))
	if triple {
		i += 3
	} else {
		i++
	}
	var out dartLit
	var cur strings.Builder
	for i < len(s) {
		c := s[i]
		switch {
		case c == q && (!triple || strings.HasPrefix(s[i:], strings.Repeat(string(q), 3))):
			if triple {
				i += 3
			} else {
				i++
			}
			return append(out, cur.String()), i
		case c == '\n' && !triple:
			return nil, 0
		case c == '\\' && !raw && i+1 < len(s):
			i++
			switch e := s[i]; e {
			case 'n':
				cur.WriteByte('\n')
			case 't':
				cur.WriteByte('\t')
			case 'r':
				cur.WriteByte('\r')
			case 'b':
				cur.WriteByte('\b')
			case 'f':
				cur.WriteByte('\f')
			case 'v':
				cur.WriteByte('\v')
			case 'x':
				if i+2 < len(s) {
					if v, ok := hexVal(s[i+1 : i+3]); ok {
						cur.WriteRune(rune(v))
						i += 2
						break
					}
				}
				cur.WriteByte(e)
			case 'u':
				if i+1 < len(s) && s[i+1] == '{' {
					if end := strings.IndexByte(s[i:], '}'); end > 0 {
						if v, ok := hexVal(s[i+2 : i+end]); ok {
							cur.WriteRune(rune(v))
							i += end
							break
						}
					}
				} else if i+4 < len(s) {
					if v, ok := hexVal(s[i+1 : i+5]); ok {
						cur.WriteRune(rune(v))
						i += 4
						break
					}
				}
				cur.WriteByte(e)
			case '\n':
			default:
				cur.WriteByte(e)
			}
			i++
		case c == '$' && !raw && i+1 < len(s) && s[i+1] == '{':
			n := skipDartInterp(s[i+2:])
			if n < 0 {
				return nil, 0
			}
			out = append(out, cur.String())
			cur.Reset()
			i += 2 + n
		case c == '$' && !raw && i+1 < len(s) && (s[i+1] == '_' || isASCIILetter(s[i+1])):
			j := i + 2
			for j < len(s) && (s[j] == '_' || isASCIILetter(s[j]) || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			out = append(out, cur.String())
			cur.Reset()
			i = j
		default:
			cur.WriteByte(c)
			i++
		}
	}
	return nil, 0
}

// skipDartInterp returns the length of an interpolation body up to and including its closing brace (nested braces and strings
// skipped), or -1.
func skipDartInterp(s string) int {
	depth := 0
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\'' || c == '"':
			_, n := readDartLit(s[i:])
			if n <= 0 {
				return -1
			}
			i += n
			continue
		case c == '{':
			depth++
		case c == '}':
			if depth == 0 {
				return i + 1
			}
			depth--
		}
		i++
	}
	return -1
}

func hexVal(s string) (int, bool) {
	if s == "" || len(s) > 6 {
		return 0, false
	}
	v := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			v = v*16 + int(c-'0')
		case c >= 'a' && c <= 'f':
			v = v*16 + int(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v*16 + int(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

// ---- test command scope ----

// DartCmdScope is what a dataset test command runs: package directories run whole, and explicit test files.
type DartCmdScope struct {
	Dirs  []string `json:"dirs,omitempty"`
	Files []string `json:"files,omitempty"`
}

func (s DartCmdScope) empty() bool { return len(s.Dirs) == 0 && len(s.Files) == 0 }

func (s DartCmdScope) has(f string) bool {
	for _, x := range s.Files {
		if x == f {
			return true
		}
	}
	for _, d := range s.Dirs {
		if d == "" || strings.HasPrefix(f, d+"/") {
			return true
		}
	}
	return false
}

var dartCmdSplit = regexp.MustCompile(`&&|\|\||[;()|\n]`)

// resolveContainer maps a path written inside a harness container (/repo/a/b or a/b) to a tree path by dropping leading
// components until the rest exists (dir: a directory, else a file). An absolute directory that matches nothing is the repo root.
func resolveContainer(t tree.Tree, p string, dir bool) (string, bool) {
	abs := strings.HasPrefix(p, "/")
	comps := strings.Split(strings.Trim(path.Clean(p), "/"), "/")
	for i := 0; i < len(comps); i++ {
		c := strings.Join(comps[i:], "/")
		if dir && t.IsDir(c) || !dir && t.Has(c) {
			return c, true
		}
		if !abs {
			break
		}
	}
	if dir && abs {
		return "", true
	}
	return "", false
}

// ParseDartCmd extracts the scope of a harness test command: every `dart test` / `flutter test` / `pub run test` invocation
// contributes its explicit .dart files, or its working directory when it names none.
func ParseDartCmd(t tree.Tree, cmd string) DartCmdScope {
	var sc DartCmdScope
	cur := ""
	for _, seg := range dartCmdSplit.Split(cmd, -1) {
		w := strings.Fields(seg)
		for len(w) > 0 && strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "-") {
			w = w[1:] // VAR=value prefixes
		}
		if len(w) == 0 || strings.HasPrefix(w[0], "#") {
			continue
		}
		if w[0] == "cd" && len(w) > 1 {
			arg := strings.Trim(w[1], `"'`)
			if strings.HasPrefix(arg, "/") {
				if d, ok := resolveContainer(t, arg, true); ok {
					cur = d
				}
			} else {
				cur = strings.TrimPrefix(path.Clean(path.Join(cur, arg)), ".")
			}
			continue
		}
		isTest := false
		for k := 0; k+1 < len(w); k++ {
			b := path.Base(w[k])
			if (b == "dart" || b == "flutter") && (w[k+1] == "test" || w[k+1] == "run" && k+2 < len(w) && w[k+2] == "test" ||
				w[k+1] == "pub" && k+3 < len(w) && w[k+2] == "run" && w[k+3] == "test") {
				isTest = true
			}
		}
		if !isTest {
			continue
		}
		named := false
		for _, a := range w {
			a = strings.Trim(a, `"'`)
			if !strings.HasSuffix(a, ".dart") || strings.HasPrefix(a, "-") {
				continue
			}
			var f string
			if strings.HasPrefix(a, "/") {
				f, _ = resolveContainer(t, a, false)
			} else if c := path.Join(cur, a); t.Has(c) {
				f = c
			}
			if f != "" {
				sc.Files = append(sc.Files, f)
				named = true
			}
		}
		if !named {
			sc.Dirs = append(sc.Dirs, cur)
		}
	}
	sc.Files, sc.Dirs = uniqSorted(sc.Files), uniqSorted(sc.Dirs)
	return sc
}

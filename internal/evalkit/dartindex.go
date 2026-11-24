package evalkit

import (
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/qxbao/bgraph/internal/tree"
)

// dartFileScan is what the locator keeps of one Dart file.
type dartFileScan struct {
	strong, weak         []string  // descriptions (see DartTestLoc)
	strongTpls, weakTpls []dartLit // interpolated descriptions
	dynamic              bool      // a test-like call with a computed description
	classes, methods     []string  // class names and test_* methods (test_reflective_loader)
	imports              []string  // import/export/part URIs (all branches of conditional imports)
}

var (
	dartTestCallee = regexp.MustCompile(`(?i)(group|test|describe|context|scenario|spec|^it$|^should$|^given$|^when$|^then$)`)
	dartNamedDesc  = map[string]bool{"name": true, "description": true, "desc": true, "title": true, "testName": true, "testDescription": true}
	dartKeyword    = map[string]bool{"if": true, "for": true, "while": true, "switch": true, "catch": true, "return": true, "assert": true, "print": true}
)

func scanDartTest(src []byte) *dartFileScan {
	toks := lexDart(string(src))
	// Concatenate adjacent string literals ('a' 'b' is one literal in Dart).
	var ts []dartToken
	for _, t := range toks {
		if t.lit != nil && len(ts) > 0 && ts[len(ts)-1].lit != nil {
			prev := ts[len(ts)-1].lit
			merged := append(dartLit(nil), prev...)
			merged[len(merged)-1] += t.lit[0]
			merged = append(merged, t.lit[1:]...)
			ts[len(ts)-1].lit = merged
			continue
		}
		ts = append(ts, t)
	}
	fs := &dartFileScan{}
	addLit := func(l dartLit, strong bool) {
		switch {
		case len(l) == 1 && strong:
			fs.strong = append(fs.strong, l[0])
		case len(l) == 1:
			fs.weak = append(fs.weak, l[0])
		case strong:
			fs.strongTpls = append(fs.strongTpls, l)
		default:
			fs.weakTpls = append(fs.weakTpls, l)
		}
	}
	for i, t := range ts {
		switch t.ident {
		case "":
			continue
		case "import", "export", "part":
			j := i + 1
			if j < len(ts) && ts[j].ident == "of" {
				continue
			}
			for k := j; k < len(ts) && ts[k].punct != ';'; k++ {
				if len(ts[k].lit) == 1 {
					fs.imports = append(fs.imports, ts[k].lit[0])
				}
			}
			continue
		case "class":
			if i+1 < len(ts) && ts[i+1].ident != "" {
				fs.classes = append(fs.classes, ts[i+1].ident)
			}
			continue
		}
		if i+2 < len(ts) && dartNamedDesc[t.ident] && ts[i+1].punct == ':' && ts[i+2].lit != nil {
			addLit(ts[i+2].lit, false)
			continue
		}
		if (strings.HasPrefix(t.ident, "test_") || strings.HasPrefix(t.ident, "solo_test_")) && i+1 < len(ts) && ts[i+1].punct == '(' {
			fs.methods = append(fs.methods, t.ident)
		}
		if dartKeyword[t.ident] {
			continue
		}
		// callee [<…>] ( first-argument
		j := i + 1
		if j < len(ts) && ts[j].punct == '<' {
			depth := 0
			for ; j < len(ts); j++ {
				if ts[j].punct == '<' {
					depth++
				} else if ts[j].punct == '>' {
					depth--
					if depth == 0 {
						j++
						break
					}
				} else if ts[j].punct == ';' || ts[j].punct == '{' || ts[j].punct == '(' {
					break
				}
			}
		}
		if j+1 >= len(ts) || ts[j].punct != '(' {
			continue
		}
		strong := dartTestCallee.MatchString(t.ident)
		a := ts[j+1]
		switch {
		case a.typeName != "":
			addLit(dartLit{a.typeName}, strong)
		case a.lit != nil:
			addLit(a.lit, strong)
		case !strong:
		case a.ident != "" && j+2 < len(ts) && (ts[j+2].punct == ',' || ts[j+2].punct == ')') && unicode.IsUpper(rune(a.ident[0])):
			fs.strong = append(fs.strong, a.ident)
		case a.ident != "" || a.punct == '(':
			fs.dynamic = true
		}
	}
	return fs
}

// ---- index ----

type dartTpl struct {
	re     *regexp.Regexp // whole description, interpolations matching anything
	head   *regexp.Regexp // with a leading "(?:(.*) )?" capture for the group prefix
	file   string
	weak   bool
	static int // static characters (non-space)
}

// dartClosure is what one test file can contribute to its test names: its own descriptions and those of the helpers it imports.
type dartClosure struct {
	files        []string
	strong, weak map[string]bool
	tpls         []*dartTpl
	dynamic      bool // a computed description, or a template with fewer than 3 static characters
}

type dartIndex struct {
	t       tree.Tree
	scans   map[string]*dartFileScan
	units   map[string]bool
	strong  map[string][]string // description -> files
	weak    map[string][]string
	tpls    []*dartTpl
	dynamic []string            // files with a computed description or a short template
	classes map[string][]string // class -> files
	methods map[string][]string // test method -> files
	imports map[string][]string // file -> indexed files it imports (resolved)
	libs    map[string][]string // path below lib/ -> indexed files there (package: imports of test helpers)
	clos    map[string]*dartClosure
	owners  map[string][]string // file -> units whose closure contains it (built on first use)
}

// dartScanCache memoizes per-blob scans across tasks (blob oid -> *dartFileScan).
var dartScanCache sync.Map

func dartTplRe(l dartLit, head bool) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	if head {
		b.WriteString("(?:(.*) )?")
	}
	for i, p := range l {
		if i > 0 {
			b.WriteString(".*?")
		}
		b.WriteString(regexp.QuoteMeta(p))
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

func dartStatic(l dartLit) int {
	n := 0
	for _, p := range l {
		n += len(strings.ReplaceAll(p, " ", ""))
	}
	return n
}

func buildDartIndex(t tree.Tree) *dartIndex {
	ix := &dartIndex{t: t, scans: map[string]*dartFileScan{}, units: map[string]bool{}, strong: map[string][]string{}, weak: map[string][]string{},
		classes: map[string][]string{}, methods: map[string][]string{}, imports: map[string][]string{}, libs: map[string][]string{},
		clos: map[string]*dartClosure{}}
	want := DartIndexFiles(t.Files())
	files := make([]string, 0, len(want))
	for f := range want {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		if below, ok := dartBelowLib(f); ok {
			ix.libs[below] = append(ix.libs[below], f)
		}
		if DartIsTestFile(t, f) {
			ix.units[f] = true
		}
		key := t.Key(f)
		var fs *dartFileScan
		if v, ok := dartScanCache.Load(key); ok && key != "" {
			fs = v.(*dartFileScan)
		} else {
			b, err := t.Read(f)
			if err != nil {
				continue
			}
			fs = scanDartTest(b)
			if key != "" {
				dartScanCache.Store(key, fs)
			}
		}
		ix.scans[f] = fs
		for _, d := range uniqSorted(append([]string(nil), fs.strong...)) {
			ix.strong[d] = append(ix.strong[d], f)
		}
		for _, d := range uniqSorted(append([]string(nil), fs.weak...)) {
			ix.weak[d] = append(ix.weak[d], f)
		}
		dyn := fs.dynamic
		for k, ls := range [][]dartLit{fs.strongTpls, fs.weakTpls} {
			for _, l := range ls {
				re, head := dartTplRe(l, false), dartTplRe(l, true)
				if re == nil || head == nil {
					continue
				}
				tp := &dartTpl{re: re, head: head, file: f, weak: k == 1, static: dartStatic(l)}
				if tp.static < 3 {
					dyn = true
					continue
				}
				ix.tpls = append(ix.tpls, tp)
			}
		}
		if dyn {
			ix.dynamic = append(ix.dynamic, f)
		}
		for _, c := range uniqSorted(append([]string(nil), fs.classes...)) {
			ix.classes[c] = append(ix.classes[c], f)
		}
		for _, m := range uniqSorted(append([]string(nil), fs.methods...)) {
			ix.methods[m] = append(ix.methods[m], f)
		}
	}
	for _, f := range files {
		if fs := ix.scans[f]; fs != nil {
			var to []string
			for _, u := range fs.imports {
				to = append(to, ix.resolveImport(f, u)...)
			}
			ix.imports[f] = uniqSorted(to)
		}
	}
	return ix
}

// dartBelowLib is the path of f below its last lib/ directory.
func dartBelowLib(f string) (string, bool) {
	if i := strings.LastIndex(f, "/lib/"); i >= 0 {
		return f[i+len("/lib/"):], true
	}
	return strings.CutPrefix(f, "lib/")
}

// resolveImport resolves a relative URI, or a package: URI that names an indexed file under some lib/ directory, to indexed files.
func (ix *dartIndex) resolveImport(from, uri string) []string {
	if rest, ok := strings.CutPrefix(uri, "package:"); ok {
		_, p, ok := strings.Cut(rest, "/")
		if !ok {
			return nil
		}
		return ix.libs[p]
	}
	if strings.Contains(uri, ":") {
		return nil
	}
	r := path.Clean(path.Join(path.Dir(from), uri))
	if ix.scans[r] != nil {
		return []string{r}
	}
	return nil
}

// closure of file u: u and the indexed non-test files it imports, transitively (other test files are not entered: their main()
// does not run).
func (ix *dartIndex) closure(u string) *dartClosure {
	if c, ok := ix.clos[u]; ok {
		return c
	}
	c := &dartClosure{strong: map[string]bool{}, weak: map[string]bool{}}
	seen := map[string]bool{u: true}
	q := []string{u}
	for len(q) > 0 {
		f := q[0]
		q = q[1:]
		c.files = append(c.files, f)
		for _, n := range ix.imports[f] {
			if !seen[n] && !ix.units[n] {
				seen[n] = true
				q = append(q, n)
			}
		}
	}
	sort.Strings(c.files)
	in := map[string]bool{}
	for _, f := range c.files {
		in[f] = true
		fs := ix.scans[f]
		if fs == nil {
			continue
		}
		for _, d := range fs.strong {
			c.strong[d] = true
		}
		for _, d := range fs.weak {
			c.weak[d] = true
		}
	}
	for _, tp := range ix.tpls {
		if in[tp.file] {
			c.tpls = append(c.tpls, tp)
		}
	}
	for _, f := range ix.dynamic {
		if in[f] {
			c.dynamic = true
		}
	}
	ix.clos[u] = c
	return c
}

// ownersOf returns the test files whose closure contains f (f itself when it is a test file).
func (ix *dartIndex) ownersOf(f string) []string {
	if ix.owners == nil {
		ix.owners = map[string][]string{}
		var us []string
		for u := range ix.units {
			us = append(us, u)
		}
		sort.Strings(us)
		for _, u := range us {
			for _, g := range ix.closure(u).files {
				ix.owners[g] = append(ix.owners[g], u)
			}
		}
	}
	return ix.owners[f]
}

const (
	dartStageDesc = iota + 1
	dartStageTemplate
	dartStageWeak
	dartStageDynamic
)

var dartStageVia = map[int]string{dartStageDesc: "desc", dartStageTemplate: "template", dartStageWeak: "weak", dartStageDynamic: "dynamic"}

func (c *dartClosure) isDesc(s string, stage int) bool {
	if c.strong[s] || stage >= dartStageWeak && c.weak[s] {
		return true
	}
	if stage < dartStageTemplate {
		return false
	}
	for _, tp := range c.tpls {
		if (!tp.weak || stage >= dartStageWeak) && tp.re.MatchString(s) {
			return true
		}
	}
	return false
}

// consume reports whether s is a sequence of descriptions of the closure joined by single spaces; with wild, one non-empty span
// may be a computed description. static counts the characters covered by descriptions.
func (c *dartClosure) consume(s string, stage int, wild bool, depth int) (ok bool, static int) {
	if s == "" {
		return true, 0
	}
	if depth > 24 {
		return false, 0
	}
	for j := 0; j <= len(s); j++ {
		if j < len(s) && s[j] != ' ' {
			continue
		}
		head, rest := s[:j], ""
		if j < len(s) {
			rest = s[j+1:]
		}
		if c.isDesc(head, stage) {
			if ok, n := c.consume(rest, stage, wild, depth+1); ok {
				return true, n + len(head)
			}
		}
		if wild && head != "" {
			if ok, n := c.consume(rest, stage, false, depth+1); ok {
				return true, n
			}
		}
	}
	return false, 0
}

// matches reports whether the closure of a file produces the full name n at the given stage.
func (c *dartClosure) matches(n string, stage int) bool {
	wild := stage == dartStageDynamic && c.dynamic
	ok, static := c.consume(n, stage, wild, 0)
	if !ok {
		return false
	}
	if wild {
		// The static descriptions must cover at least half of the name.
		return 2*static >= len(strings.ReplaceAll(n, " ", ""))
	}
	return true
}

// defining returns the files that may define the last description of n at the given stage.
func (ix *dartIndex) defining(n string, stage int) map[string]bool {
	set := map[string]bool{}
	for i := 0; i < len(n); i++ {
		if i > 0 && n[i-1] != ' ' {
			continue
		}
		for _, f := range ix.strong[n[i:]] {
			set[f] = true
		}
		if stage >= dartStageWeak {
			for _, f := range ix.weak[n[i:]] {
				set[f] = true
			}
		}
	}
	if stage >= dartStageTemplate {
		for _, tp := range ix.tpls {
			if !set[tp.file] && (!tp.weak || stage >= dartStageWeak) && tp.head.MatchString(n) {
				set[tp.file] = true
			}
		}
	}
	if stage >= dartStageDynamic {
		for _, f := range ix.dynamic {
			set[f] = true
		}
	}
	return set
}

// find returns the test files producing n at a stage with the helper files (non-test) among the definers in their closures, else
// (no test file) the helper files that produce n on their own.
func (ix *dartIndex) find(n string, stage int) (units, helpers []string) {
	def := ix.defining(n, stage)
	if len(def) == 0 {
		return nil, nil
	}
	cand := map[string]bool{}
	for f := range def {
		if ix.units[f] {
			cand[f] = true
		}
		for _, u := range ix.ownersOf(f) {
			cand[u] = true
		}
	}
	via := map[string]bool{}
	for _, u := range sortedKeys(cand) {
		if c := ix.closure(u); c.matches(n, stage) {
			units = append(units, u)
			for _, f := range c.files {
				if def[f] && !ix.units[f] {
					via[f] = true
				}
			}
		}
	}
	if len(units) > 0 {
		return units, sortedKeys(via)
	}
	for _, f := range sortedKeys(def) {
		if !ix.units[f] && ix.closure(f).matches(n, stage) {
			helpers = append(helpers, f)
		}
	}
	return nil, helpers
}

var dartVariantRe = regexp.MustCompile(`\s*\(variant: [^()]*(?:\([^()]*\)[^()]*)*\)$`)

// dartNameVariants: the name as given, then the harness and runner spellings of it.
func dartNameVariants(n string) []struct{ s, tag string } {
	out := []struct{ s, tag string }{{n, ""}}
	for _, v := range []struct{ s, tag string }{
		{strings.TrimSpace(n), "+trim"},
		{strings.ReplaceAll(n, " > ", " "), "+gt"},
		{dartVariantRe.ReplaceAllString(n, ""), "+variant"},
	} {
		if v.s != n && v.s != "" {
			out = append(out, v)
		}
	}
	return out
}

// lookupName tries every stage over every variant of the name.
func (ix *dartIndex) lookupName(n string) (units, helpers []string, via string) {
	var firstHelpers []string
	firstVia := ""
	for stage := dartStageDesc; stage <= dartStageDynamic; stage++ {
		for _, v := range dartNameVariants(n) {
			u, h := ix.find(v.s, stage)
			if len(u) > 0 {
				return u, h, dartStageVia[stage] + v.tag
			}
			if len(h) > 0 && firstHelpers == nil {
				firstHelpers, firstVia = h, dartStageVia[stage]+v.tag
			}
		}
	}
	return nil, firstHelpers, firstVia
}

// unitMatches reports whether test file u defines the full name n at a stage up to weak (path-form disambiguation).
func (ix *dartIndex) unitMatches(u, n string) bool {
	if ix.scans[u] == nil {
		return false
	}
	c := ix.closure(u)
	for _, v := range dartNameVariants(n) {
		if c.matches(v.s, dartStageWeak) {
			return true
		}
	}
	return false
}

// reflective locates a test_reflective_loader name: files declaring the class and the method (the class alone when the method is
// inherited), mapped to the test files including them.
func (ix *dartIndex) reflective(class, method string) (units []string, via string) {
	cf := ix.classes[class]
	both := map[string]bool{}
	for _, f := range cf {
		for _, g := range ix.methods[method] {
			if f == g {
				both[f] = true
			}
		}
	}
	via = "reflective"
	if len(both) == 0 {
		for _, f := range cf {
			both[f] = true
		}
		via = "reflective-class"
	}
	set := map[string]bool{}
	for f := range both {
		if ix.units[f] {
			set[f] = true
		}
		for _, u := range ix.ownersOf(f) {
			set[u] = true
		}
	}
	return sortedKeys(set), via
}

// ---- locate ----

// resolveNamePath maps the path written in a name to tree files: exact, inside a harness container, relative to a directory the
// command ran tests in, then by suffix (several candidates possible).
func resolveNamePath(t tree.Tree, p string, sc DartCmdScope) ([]string, string) {
	if strings.HasPrefix(p, "/") {
		if f, ok := resolveContainer(t, p, false); ok {
			return []string{f}, "path-container"
		}
		return nil, ""
	}
	p = path.Clean(p)
	if t.Has(p) {
		return []string{p}, "path"
	}
	var out []string
	for _, f := range sc.Files {
		if strings.HasSuffix(f, "/"+p) {
			out = append(out, f)
		}
	}
	if len(out) > 0 {
		return uniqSorted(out), "path-cmd-file"
	}
	for _, d := range sc.Dirs {
		if c := path.Join(d, p); t.Has(c) {
			out = append(out, c)
		}
	}
	if len(out) > 0 {
		return uniqSorted(out), "path-cmd-dir"
	}
	for _, f := range t.Files() {
		if strings.HasSuffix(f, "/"+p) {
			out = append(out, f)
		}
	}
	if len(out) > 0 {
		return out, "path-suffix"
	}
	return nil, ""
}

// LocateDartTests resolves every name to candidate test files of the tree t. patch is the set of files the test patch touches; cmd
// is the dataset's test command (may be empty).
func LocateDartTests(t tree.Tree, names []string, patch map[string]bool, cmd string) []DartTestLoc {
	return locateDart(t, buildDartIndex(t), names, patch, ParseDartCmd(t, cmd))
}

func locateDart(t tree.Tree, ix *dartIndex, names []string, patch map[string]bool, sc DartCmdScope) []DartTestLoc {
	out := make([]DartTestLoc, 0, len(names))
	for _, name := range names {
		pn := parseDartName(name)
		l := DartTestLoc{Name: name, Form: pn.form, Path: pn.path, Class: pn.class}
		var files []string
		isPath := false
		switch pn.form {
		case "path", "path-name", "loading":
			isPath = true
			files, l.Via = resolveNamePath(t, pn.path, sc)
			if len(files) > 1 && pn.test != "" {
				// Several packages share the relative path: keep those that define the name.
				var def []string
				for _, f := range files {
					if ix.unitMatches(f, pn.test) {
						def = append(def, f)
					}
				}
				if len(def) > 0 && len(def) < len(files) {
					files, l.Via = def, l.Via+"+name"
				}
			}
		case "hook":
			l.Hook = true
		case "reflective":
			files, l.Via = ix.reflective(pn.class, pn.method)
			if len(files) == 0 {
				files, l.Helpers, l.Via = ix.lookupName(pn.test)
			}
		case "name":
			files, l.Helpers, l.Via = ix.lookupName(pn.test)
		}
		if len(files) == 0 && len(l.Helpers) > 0 {
			files, l.NotUnit = l.Helpers, true
		}
		files = uniqSorted(files)
		if len(files) > 1 && !sc.empty() {
			var in []string
			for _, f := range files {
				if sc.has(f) {
					in = append(in, f)
				}
			}
			if len(in) > 0 && len(in) < len(files) {
				files, l.Via = in, l.Via+"+cmd"
			}
		}
		if isPath && len(files) > 0 && !DartIsTestFile(t, files[0]) {
			l.NotUnit = true
		}
		l.Files = files
		for _, f := range l.Files {
			if patch[f] {
				l.PatchFiles = append(l.PatchFiles, f)
				l.InPatch = true
			}
		}
		l.NotFound = len(l.Files) == 0
		l.Ambiguous = len(l.Files) > 1
		out = append(out, l)
	}
	return out
}

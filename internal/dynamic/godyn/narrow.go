package godyn

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/qxbao/bgraph/internal/classify"
	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/eco/golang"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// Kind is how the Go layer treats a changed file.
type Kind byte

const (
	// KindLines: a non-test Go source file whose every change lies inside function bodies (body guard passed): a test function
	// whose binary compiles it is kept only when it executed a changed function, cannot be judged, or is exposed to the line shift.
	KindLines Kind = 'L'
	// KindTest: a _test.go file: its package is kept whole (coverage does not instrument test files, and other packages cannot
	// compile them).
	KindTest Kind = 'T'
	// KindDoc: an unreferenced documentation file: a test function is kept when its binary embeds it.
	KindDoc Kind = 'D'
)

// Changed is one changed file as the Go layer treats it.
type Changed struct {
	Path  string
	Kind  Kind
	Spans []Span // KindLines: the changed functions (base spans)
	Shift bool   // KindLines: the change alters the file's line count
	First int    // KindLines: the first base line whose number moves
}

// docFile: a modified documentation or harmless file (classify.Doc / classify.Harmless) that nothing in the graph references, that
// no ecosystem gives a role and that no bgraph.toml entry names (the generic rule of internal/dynamic). A Go test can only see it
// by reading it at run time through a path the static graph does not resolve, or by embedding it (then it is a dependency).
func docFile(c dynamic.Change) bool {
	return (classify.Doc(c.Path) || classify.Harmless(c.Path)) && !c.Referenced && !c.OtherRole && !c.Rule && !c.Declared &&
		!c.Global && !c.Claimed && c.BaseRole == "" && c.HeadRole == ""
}

func roleName(r model.Role) string {
	if r == "" {
		return "none"
	}
	return string(r)
}

// hiddenDir reports whether repo-relative file p lies under a directory the go command does not build as part of the module's
// packages: testdata, vendor, or a directory whose name starts with "." or "_".
func hiddenDir(p string) bool {
	parts := strings.Split(p, "/")
	for _, s := range parts[:len(parts)-1] {
		if s == "testdata" || s == "vendor" || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "_") {
			return true
		}
	}
	return false
}

// Guard returns "" when the Go layer may narrow sel at all, otherwise the reason, and how each changed file is treated. changes
// are classified as for dynamic.Guard, with roles of the go ecosystem and OtherRole meaning a role in another ecosystem. Any
// widening refuses: with only modified Go sources, test files and documentation, a widening comes from an unresolved or uncertain
// import, a manifest, a lockfile or a global input, and the units it selects are not reached through edges the records judge.
func Guard(sel model.Selection, changes []dynamic.Change) ([]Changed, string) {
	if sel.Scope == model.TierFull {
		return nil, "the selection is full (never narrowed)"
	}
	for _, w := range sel.Widening {
		return nil, fmt.Sprintf("widening %s on %s (a manifest, lockfile, unresolved import or global change)", w.Reason, w.Path)
	}
	if len(changes) == 0 || len(changes) != len(sel.Changed) {
		return nil, "the change set is not classified"
	}
	var out []Changed
	for _, c := range changes {
		var why string
		switch {
		case c.Status != 'M':
			why = fmt.Sprintf("status %c (only modifications of existing files: an addition can add a test, a file to a package or a package, a deletion or rename changes what is compiled)", c.Status)
		case c.OldMode != c.NewMode:
			why = fmt.Sprintf("mode changed %s -> %s", c.OldMode, c.NewMode)
		case c.NewMode != "100644" && c.NewMode != "100755":
			why = "mode " + c.NewMode + " is not a regular file"
		case c.Linked:
			why = "read through a symbolic link"
		case c.Global:
			why = "global input"
		case c.Declared:
			why = "named by a bgraph.toml [[config]] or [[dynamic]] declaration"
		case c.Rule:
			why = "named by a bgraph.toml [[rule]] (its targets are not narrowed)"
		case c.Claimed:
			why = "claimed as a manifest, lockfile or other build input"
		case path.Ext(c.Path) == ".go":
			if hiddenDir(c.Path) {
				why = "a .go file under testdata, vendor or a directory starting with . or _ (a program a test may build, or a vendored package)"
				break
			}
			if strings.HasSuffix(c.Path, "_test.go") {
				if c.BaseRole != model.RoleTest || c.HeadRole != model.RoleTest {
					why = fmt.Sprintf("role %s/%s at base/head is not test", roleName(c.BaseRole), roleName(c.HeadRole))
					break
				}
				out = append(out, Changed{Path: c.Path, Kind: KindTest})
				break
			}
			if c.BaseRole != model.RoleSource || c.HeadRole != model.RoleSource {
				why = fmt.Sprintf("role %s/%s at base/head is not source", roleName(c.BaseRole), roleName(c.HeadRole))
				break
			}
			out = append(out, Changed{Path: c.Path, Kind: KindLines})
		case docFile(c):
			out = append(out, Changed{Path: c.Path, Kind: KindDoc})
		default:
			why = "not a Go source file nor an unreferenced documentation file (assembly, C, data, testdata, templates and CI files refuse)"
		}
		if why != "" {
			return nil, c.Path + ": " + why
		}
	}
	return out, ""
}

// Refine applies the body guard to the Go source files of changed (both versions read from base and head). Any file the body
// guard rejects refuses the selection.
func Refine(changed []Changed, base, head tree.Tree) ([]Changed, string) {
	out := slices.Clone(changed)
	for i, c := range out {
		if c.Kind != KindLines {
			continue
		}
		b, err1 := base.Read(c.Path)
		h, err2 := head.Read(c.Path)
		if err1 != nil || err2 != nil {
			return nil, c.Path + ": cannot read both versions"
		}
		spans, shift, first, why := BodyGuard(c.Path, b, h)
		if why != "" {
			return nil, c.Path + ": " + why
		}
		out[i].Spans, out[i].Shift, out[i].First = spans, shift, first
	}
	return out, ""
}

// PlainEdge reports whether a graph edge is one the records judge: an import, or a structural edge of the Go analysis (a file of
// a package, a package's test binary compiling the package, a module node), or a package a test may build or a program it may
// run (process starts make the record incomplete). Every other edge (fixture data, //go:embed, cgo, directory patterns, any edge
// of another ecosystem) means a test may read a changed file as data, which coverage does not see.
func PlainEdge(e model.Edge) bool {
	switch e.Kind {
	case model.EdgeImport:
		return true
	case model.EdgeImplicit:
		switch e.Spec {
		case "package file", "go test compiles the package", "module-level file":
			return true
		}
		// A test that really builds or runs a program starts a process, which the collector counts: such a record is
		// incomplete and keeps the test, so these edges (usually string literals that look like import paths) are judged
		// by the records too.
		return strings.HasPrefix(e.Spec, "module ") || strings.HasPrefix(e.Spec, "test may build package ") ||
			strings.HasPrefix(e.Spec, "test builds packages ") || strings.HasPrefix(e.Spec, "may run the program built from ")
	}
	return false
}

// Narrowable reports whether the Go layer handles selected unit t: a Go package unit (not a whole-suite unit).
func Narrowable(t model.SelectedTest, eco string) bool {
	if eco != Eco || t.Whole {
		return false
	}
	_, ok := golang.ParseUnitID(t.Unit)
	return ok
}

// SubID returns the sub-unit ID of test function name of unit unit: "go:<dir>#<Name>".
func SubID(unit, name string) string { return unit + "#" + name }

// Params is what Narrow needs besides the selection.
type Params struct {
	Env     string // the environment fingerprint at the base
	Records []dynamic.Record
	Base    tree.Tree
	UnitEco map[string]string
	Changed []Changed // Guard's classification, refined
	// Tainted: units the static graph reaches from a changed file through an edge PlainEdge rejects (kept whole).
	Tainted map[string]bool
	// Head: the head tree (with Base, where ReadsOwnSources looks for tests reading their package's sources).
	Head tree.Tree
}

// latest returns the latest Go record per unit for environment env.
func latest(recs []dynamic.Record, env string) map[string]dynamic.Record {
	byUnit := map[string]dynamic.Record{}
	for _, r := range recs {
		if r.Eco != Eco || r.Env != env {
			continue
		}
		if cur, ok := byUnit[r.Unit]; !ok || !r.Recorded.Before(cur.Recorded) {
			byUnit[r.Unit] = r
		}
	}
	return byUnit
}

// keepReason returns why the usable record rec of a test function keeps it for the changed files, "" when none of the changed
// functions it compiled ran and no line it executed moves.
func keepReason(rec dynamic.Record, changed []Changed) string {
	for _, c := range changed {
		if _, ok := rec.Deps[c.Path]; !ok {
			continue // neither compiled into the test binary, embedded, nor a data file of the unit
		}
		switch c.Kind {
		case KindDoc:
			return "its test binary embeds the changed file " + c.Path + ", or it lies among the unit's data files"
		case KindTest:
			return "its record depends on the changed test file " + c.Path
		}
		if slices.Contains(rec.Reads, c.Path) {
			continue // a .go file the binary does not compile (another build configuration, a data directory)
		}
		tbl, ok := rec.LineCov[c.Path]
		if !ok {
			return "its test binary compiles " + c.Path + " without coverage (a build constraint or a file outside -coverpkg): the change cannot be judged"
		}
		ls, ok := ParseLineTable(tbl)
		if !ok {
			return "malformed line table for " + c.Path
		}
		for _, s := range c.Spans {
			inst, ran := false, false
			for l := s.From; l <= s.To; l++ {
				if e, ok := ls[l]; ok {
					inst = true
					ran = ran || e
				}
			}
			if ran {
				return fmt.Sprintf("executed the changed function %s (%s:%d)", s.What, c.Path, s.From)
			}
			if !inst {
				return fmt.Sprintf("%s:%d: the changed function %s has no coverage block in the record: the change cannot be judged", c.Path, s.From, s.What)
			}
		}
		if c.Shift {
			for l, e := range ls {
				if e && l >= c.First {
					return fmt.Sprintf("executed %s:%d, at or below the first moved line %d: the change shifts its line numbers", c.Path, l, c.First)
				}
			}
		}
	}
	return ""
}

// TestFiles returns the tracked _test.go files directly in package directory dir of t (any build constraint).
func TestFiles(t tree.Tree, dir string) []string {
	var out []string
	prefix := dir + "/"
	if dir == "." {
		prefix = ""
	}
	for _, f := range t.Files() {
		if !strings.HasSuffix(f, "_test.go") {
			continue
		}
		if rest, ok := strings.CutPrefix(f, prefix); ok && !strings.Contains(rest, "/") {
			out = append(out, f)
		}
	}
	return out
}

// sameTestFiles: the function-list record lists exactly the _test.go files of the package directory at the base (a test file
// added since the recording can add a function).
func sameTestFiles(cl dynamic.Record, base tree.Tree, dir string) string {
	files := TestFiles(base, dir)
	for _, f := range files {
		if _, ok := cl.Deps[f]; !ok {
			return "test file " + f + " was not there when recorded"
		}
	}
	n := 0
	for d := range cl.Deps {
		if path.Dir(d) == dir && strings.HasSuffix(d, "_test.go") {
			n++
		}
	}
	if n != len(files) {
		return fmt.Sprintf("%d test files when recorded, %d at the base", n, len(files))
	}
	return ""
}

// sameGoFiles: every package directory of the record's build closure holds, at the base, only .go files the record lists (a
// file added to a compiled package can add an init function or a method).
func sameGoFiles(r dynamic.Record, goByDir map[string][]string) string {
	for _, d := range r.Dirs {
		for _, f := range goByDir[d] {
			if _, ok := r.Deps[f]; !ok {
				return "Go file " + f + " was not there when recorded"
			}
		}
	}
	return ""
}

// selfReadImports are packages a test uses to read Go source files as data.
var selfReadImports = []string{"go/parser", "go/ast", "go/doc", "go/build", "go/format", "go/types", "golang.org/x/tools/go/packages"}

// ReadsOwnSources returns why the test files of package directory dir in t may read the package's own Go sources as data
// ("" when nothing says so): a test file imports a Go source parser or loader (go/parser, go/ast, go/doc, go/build, go/format,
// go/types, go/token with go/printer, golang.org/x/tools/go/packages or go/analysis...), or has a string literal ".", a *.go
// pattern, or the name of a .go file of dir. The static graph has no edge for paths inside the package's own directory, and
// coverage does not see reads. A test file that does not parse counts as reading.
func ReadsOwnSources(t tree.Tree, dir string) string {
	if t == nil {
		return ""
	}
	for _, f := range TestFiles(t, dir) {
		src, err := t.Read(f)
		if err != nil {
			return "cannot read " + f
		}
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, f, src, parser.SkipObjectResolution)
		if err != nil {
			return f + " does not parse"
		}
		imps := map[string]bool{}
		for _, im := range af.Imports {
			if p, err := strconv.Unquote(im.Path.Value); err == nil {
				imps[p] = true
			}
		}
		for p := range imps {
			if slices.Contains(selfReadImports, p) || p == "golang.org/x/tools/go/analysis" || strings.HasPrefix(p, "golang.org/x/tools/go/analysis/") {
				return f + " imports " + p
			}
		}
		if imps["go/token"] && imps["go/printer"] {
			return f + " imports go/token and go/printer"
		}
		why := ""
		ast.Inspect(af, func(n ast.Node) bool {
			if why != "" {
				return false
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			v = strings.ReplaceAll(v, "\\", "/")
			s := strings.TrimPrefix(v, "./")
			switch {
			case v == "." || v == "./" || s == "*.go" || strings.HasSuffix(s, ".go") && !strings.Contains(s, "/") && strings.ContainsAny(s, "*?["):
				why = fmt.Sprintf("%s:%d: string literal %q", f, fset.Position(lit.Pos()).Line, v)
			case strings.HasSuffix(s, ".go") && s != "" && t.Has(path.Join(dir, s)) && path.Dir(path.Join(dir, s)) == dir:
				why = fmt.Sprintf("%s:%d: string literal %q names a Go file of the package", f, fset.Position(lit.Pos()).Line, v)
			}
			return true
		})
		if why != "" {
			return why
		}
	}
	return ""
}

// Narrow applies the Go layer to sel: every selected Go package unit with a usable function-list record runs only the test
// functions without a usable record and those whose record keeps them for the changed files. The unit stays selected (it is still
// compiled and vetted); removed functions are listed in Narrowed as "go:<dir>#<Name>". Call it only after Guard and Refine passed.
func Narrow(sel model.Selection, p Params) model.Selection {
	always := dynamic.AlwaysRun(sel)
	testDirs := map[string]string{}
	var fns []string
	for _, c := range p.Changed {
		if c.Kind == KindTest {
			testDirs[path.Dir(c.Path)] = c.Path
		}
		for _, s := range c.Spans {
			fns = append(fns, fmt.Sprintf("%s (%s:%d)", s.What, c.Path, s.From))
		}
	}
	byUnit := latest(p.Records, p.Env)
	goByDir := map[string][]string{}
	for _, f := range p.Base.Files() {
		if strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") {
			goByDir[path.Dir(f)] = append(goByDir[path.Dir(f)], f)
		}
	}
	bp := dynamic.Params{Eco: Eco, Collector: CollectorID, Env: p.Env, BaseOID: func(f string) string {
		if !p.Base.Has(f) {
			return ""
		}
		return p.Base.Key(f)
	}}
	out := sel
	out.Tests = slices.Clone(sel.Tests)
	out.Narrowed = slices.Clone(sel.Narrowed)
	removedTotal := 0
	for i, t := range out.Tests {
		if !Narrowable(t, p.UnitEco[t.Unit]) {
			continue
		}
		dir, _ := golang.ParseUnitID(t.Unit)
		keep := func(why string) { out.Tests[i].Dynamic = "Go layer kept the whole package: " + why }
		if always[t.Unit] {
			keep("always run (previously failed)")
			continue
		}
		if f, ok := testDirs[dir]; ok {
			keep("its test file " + f + " changed (test files are not instrumented)")
			continue
		}
		if p.Tainted[t.Unit] {
			keep("the static graph reaches it from a changed file through a fixture, embed, cgo or directory-pattern edge (a test may read the file as data)")
			continue
		}
		if why := ReadsOwnSources(p.Base, dir); why != "" {
			keep("its tests may read the package's Go sources as data (" + why + ")")
			continue
		}
		if why := ReadsOwnSources(p.Head, dir); why != "" {
			keep("its tests may read the package's Go sources as data (" + why + ")")
			continue
		}
		cl, ok := byUnit[t.Unit]
		if !ok {
			keep("no function-list record for the current environment")
			continue
		}
		if why := dynamic.Usable(cl, nil, bp); why != "" {
			keep("function list not usable: " + why)
			continue
		}
		if why := sameTestFiles(cl, p.Base, dir); why != "" {
			keep("function list not usable: " + why)
			continue
		}
		if len(cl.Classes) == 0 {
			keep("no test function listed")
			continue
		}
		var run []string
		var removed []model.NarrowedTest
		for _, name := range cl.Classes {
			sub := SubID(t.Unit, name)
			if always[sub] {
				run = append(run, name)
				continue
			}
			r, ok := byUnit[sub]
			if !ok || dynamic.Usable(r, nil, bp) != "" || sameGoFiles(r, goByDir) != "" || keepReason(r, p.Changed) != "" {
				run = append(run, name)
				continue
			}
			what := "none of the changed files"
			if len(fns) > 0 {
				what = fmt.Sprintf("none of the %d changed functions (%s), and no line that moves", len(fns), strings.Join(firstNs(fns, 5), ", "))
			}
			removed = append(removed, model.NarrowedTest{Unit: sub, Recorded: r.Recorded, Commit: r.Commit,
				Reason: fmt.Sprintf("record by %s: executed %s", r.Collector, what)})
		}
		if len(removed) == 0 {
			out.Tests[i].Dynamic = fmt.Sprintf("Go layer: all %d test functions run (each executed a changed function or has no usable record)", len(cl.Classes))
			continue
		}
		sort.Strings(run)
		out.Tests[i].ClassFilter = true
		out.Tests[i].Classes = run
		if len(run) == 0 {
			out.Tests[i].Dynamic = fmt.Sprintf("Go layer: none of %d test functions runs; the package is still compiled and vetted (go test -run '^$')", len(cl.Classes))
		} else {
			out.Tests[i].Dynamic = fmt.Sprintf("Go layer: %d of %d test functions run", len(run), len(cl.Classes))
		}
		out.Narrowed = append(out.Narrowed, removed...)
		removedTotal += len(removed)
	}
	if removedTotal > 0 && out.IndexStats != nil {
		st := make(map[string]int, len(out.IndexStats)+1)
		for k, v := range out.IndexStats {
			st[k] = v
		}
		st["narrowed_test_functions"] = removedTotal
		out.IndexStats = st
	}
	return out
}

func firstNs(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(slices.Clone(s[:n]), fmt.Sprintf("+%d more", len(s)-n))
}

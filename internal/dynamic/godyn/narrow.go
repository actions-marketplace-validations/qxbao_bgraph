package godyn

import (
	"fmt"
	"path"
	"slices"
	"sort"
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
// a package, a package's test binary compiling the package, a module node). Every other edge (fixture data, //go:embed, cgo,
// directory patterns, packages a test may build or programs it may run, any edge of another ecosystem) means a test may read a
// changed file as data or build and run a program from it, which coverage does not see.
func PlainEdge(e model.Edge) bool {
	switch e.Kind {
	case model.EdgeImport:
		return true
	case model.EdgeImplicit:
		switch e.Spec {
		case "package file", "go test compiles the package", "module-level file":
			return true
		}
		return strings.HasPrefix(e.Spec, "module ")
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
			continue // not compiled into the test binary (nor embedded)
		}
		switch c.Kind {
		case KindDoc:
			return "its test binary embeds the changed file " + c.Path
		case KindTest:
			return "its test binary compiles the changed test file " + c.Path
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
	if len(cl.Deps) != len(files) {
		return fmt.Sprintf("%d test files when recorded, %d at the base", len(cl.Deps), len(files))
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
			keep("the static graph reaches it from a changed file through a fixture, embed, cgo or program-building edge (a test may read the file or run a program built from it)")
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
			if !ok || dynamic.Usable(r, nil, bp) != "" || keepReason(r, p.Changed) != "" {
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

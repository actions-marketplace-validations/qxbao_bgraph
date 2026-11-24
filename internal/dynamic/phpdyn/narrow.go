package phpdyn

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/classify"
	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/eco/php"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// Kind is how the PHP layer treats a changed file (docs/dynamic-layer.md §11.2).
type Kind byte

const (
	// KindFile: PHP code judged at file level: every test file that included, read, stat'ed or listed it is kept.
	KindFile Kind = 'F'
	// KindLines: PHP code whose changes all lie inside function bodies (body guard and tokenizer cross-check passed): a test file
	// that included it is kept only when it executed a changed function, cannot be judged, or is exposed to the line shift.
	KindLines Kind = 'L'
	// KindDoc: an unreferenced documentation file: every test file that read it is kept.
	KindDoc Kind = 'D'
)

// Changed is one changed file as the PHP layer treats it.
type Changed struct {
	Path    string
	Kind    Kind
	Members []Span // KindLines: the changed functions (base spans)
	Shift   bool   // KindLines: the change alters the file's line count
	First   int    // KindLines: the first changed base line
	Note    string // KindFile: why the file is judged at file level
	Test    bool   // the file has the test role (no function-level refinement)
}

// docFile: a modified documentation or harmless file that no bgraph.toml entry names, that no other ecosystem gives a role and
// that the PHP analysis sees only as a project file (data) or not at all. The PHP analysis owns every file of a project (an
// ownership edge and the data role), so the generic rule (no role, no graph edge) would never accept one; the collector
// observes every read, include, stat and listing through the file:// proxy, so the file stays a changed file for the per-test
// check instead, as in the JVM variant of the rule (docs/dynamic-layer.md §9.9, decision 3).
func docFile(c dynamic.Change) bool {
	return (classify.Doc(c.Path) || classify.Harmless(c.Path)) && !c.OtherRole && !c.Rule && !c.Declared && !c.Global && !c.Claimed &&
		(c.BaseRole == "" || c.BaseRole == model.RoleData) && (c.HeadRole == "" || c.HeadRole == model.RoleData)
}

func codeRole(r model.Role) bool { return r == model.RoleSource || r == model.RoleTest }

func roleName(r model.Role) string {
	if r == "" {
		return "none"
	}
	return string(r)
}

// Guard returns "" when the PHP layer may narrow sel at all (docs/dynamic-layer.md §11.2), otherwise the reason, and how each
// changed file is treated (PHP code at file level, documentation). changes are classified as for dynamic.Guard, with roles of
// the php ecosystem and OtherRole meaning a role in another ecosystem.
func Guard(sel model.Selection, changes []dynamic.Change) ([]Changed, string) {
	if sel.Scope == model.TierFull {
		return nil, "the selection is full (never narrowed)"
	}
	for _, w := range sel.Widening {
		if w.Tier == model.TierFull || w.Reason == model.ReasonGlobalInput || w.Eco == Eco {
			return nil, fmt.Sprintf("widening %s on %s (a PHP manifest, lockfile, runner configuration, or a global change)", w.Reason, w.Path)
		}
	}
	if len(changes) == 0 || len(changes) != len(sel.Changed) {
		return nil, "the change set is not classified"
	}
	var out []Changed
	for _, c := range changes {
		var why string
		switch {
		case c.Status != 'M':
			why = fmt.Sprintf("status %c (only modifications of existing files are observable: an addition can add a test or shadow a class, a deletion or rename changes loading)", c.Status)
		case c.OldMode != c.NewMode:
			why = fmt.Sprintf("mode changed %s -> %s", c.OldMode, c.NewMode)
		case c.NewMode != "100644" && c.NewMode != "100755":
			why = "mode " + c.NewMode + " is not a regular file"
		case c.Linked:
			why = "read through a symbolic link (the recorded path may be the link's)"
		case c.Global:
			why = "global input"
		case c.Declared:
			why = "named by a bgraph.toml [[config]] or [[dynamic]] declaration"
		case c.Rule:
			why = "named by a bgraph.toml [[rule]] (its targets are not narrowed)"
		case c.Claimed:
			why = "claimed as a manifest, configuration, lockfile or build input"
		case php.DynamicSource(c.Path):
			if !codeRole(c.BaseRole) || !codeRole(c.HeadRole) {
				why = fmt.Sprintf("role %s/%s at base/head is not source or test", roleName(c.BaseRole), roleName(c.HeadRole))
				break
			}
			out = append(out, Changed{Path: c.Path, Kind: KindFile, Note: "file level",
				Test: c.BaseRole == model.RoleTest || c.HeadRole == model.RoleTest})
		case docFile(c):
			out = append(out, Changed{Path: c.Path, Kind: KindDoc})
		default:
			why = "not a PHP code file nor an unreferenced documentation file (data, fixtures, templates without a PHP role, CI files refuse)"
		}
		if why != "" {
			if !strings.HasPrefix(why, c.Path) {
				why = c.Path + ": " + why
			}
			return nil, why
		}
	}
	return out, ""
}

// Refine applies the body guard (docs/dynamic-layer.md §11.2) to the PHP code files of changed: a .php file that is not a test
// file and whose every change lies inside function bodies becomes KindLines (pending the tokenizer cross-check); any other
// stays KindFile with a note. It never refuses the selection.
func Refine(changed []Changed, base, head tree.Tree) []Changed {
	out := slices.Clone(changed)
	for i, c := range out {
		if c.Kind != KindFile {
			continue
		}
		switch {
		case c.Test:
			out[i].Note = "a test file (file level)"
			continue
		case strings.ToLower(path.Ext(c.Path)) != ".php":
			out[i].Note = "not a .php file (file level)"
			continue
		}
		b, err1 := base.Read(c.Path)
		h, err2 := head.Read(c.Path)
		if err1 != nil || err2 != nil {
			out[i].Note = "cannot read both versions (file level)"
			continue
		}
		spans, shift, first, why := BodyGuard(c.Path, b, h)
		if why != "" {
			out[i].Note = why + " (file level)"
			continue
		}
		out[i].Kind, out[i].Members, out[i].Shift, out[i].First = KindLines, spans, shift, first
	}
	return out
}

// Params is what Narrow needs besides the selection.
type Params struct {
	// Env returns the environment fingerprint of a unit (selected test) at the base, or why there is none.
	Env     func(t model.SelectedTest) (string, string)
	Records []dynamic.Record
	Base    tree.Tree
	UnitEco map[string]string
	Changed []Changed // Guard's classification, refined
}

// Narrowable reports whether the PHP layer handles selected unit t: a Composer / PHPUnit project unit run by PHPUnit or Pest,
// not a whole-suite unit.
func Narrowable(t model.SelectedTest, eco string) bool {
	return eco == Eco && !t.Whole && (t.Runner == RunnerPHPUnit || t.Runner == RunnerPest)
}

// UnitOf returns the recording unit of a selected test.
func UnitOf(t model.SelectedTest) Unit {
	p := t.Project
	if p == "" {
		p = "."
	}
	return Unit{ID: t.Unit, Project: p, Runner: t.Runner, Config: t.Config, BinDir: t.BinDir, RunnerPaths: t.RunnerPaths,
		Vendor: VendorOf(t.BinDir)}
}

// latest returns the latest PHP record per unit for environment env.
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

// keepReason returns why rec (usable) keeps its test file for the changed files, "" when nothing it included, read, stat'ed or
// listed changed (or the changed functions it included never ran).
func keepReason(rec dynamic.Record, changed []Changed) string {
	for _, c := range changed {
		if _, ok := rec.Deps[c.Path]; !ok {
			continue // neither included nor read, stat'ed or listed
		}
		switch c.Kind {
		case KindDoc:
			return "read the changed file " + c.Path
		case KindFile:
			return "included or read the changed file " + c.Path + " (" + c.Note + ")"
		}
		if slices.Contains(rec.Reads, c.Path) {
			return "read, stat'ed or listed the changed file " + c.Path + " as data"
		}
		tbl, ok := rec.LineCov[c.Path]
		if !ok {
			return "included " + c.Path + " without a line table: the change cannot be judged"
		}
		ls, ok := ParseLineTable(tbl)
		if !ok {
			return "malformed line table for " + c.Path
		}
		for _, s := range c.Members {
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
				return fmt.Sprintf("%s:%d: the changed function %s has no instrumented line in the record (defined by eval, or a coverage gap): the change cannot be judged", c.Path, s.From, s.What)
			}
		}
		if c.Shift {
			for l, e := range ls {
				if e && l >= c.First {
					return fmt.Sprintf("executed %s:%d, below the first changed line %d: the change shifts its line numbers", c.Path, l, c.First)
				}
			}
		}
	}
	return ""
}

// sameTestRoots: the file-list record lists exactly the tracked files of the unit's test locations at the base (a file added
// or removed since the recording could add or remove a test file).
func sameTestRoots(cl dynamic.Record, base tree.Tree, u Unit) string {
	files := RootFiles(base, TestRoots(base, u))
	n := 0
	for _, f := range files {
		if _, ok := cl.Deps[f]; !ok {
			return "test location file " + f + " was not there when recorded"
		}
		n++
	}
	cfg := ConfigFiles(base, u)
	extra := 0
	for d := range cl.Deps {
		if !slices.Contains(files, d) && !slices.Contains(cfg, d) && !slices.Contains(cl.Classes, d) {
			extra++
		}
	}
	if extra > 0 {
		return fmt.Sprintf("%d files of the test locations when recorded are no longer there", extra)
	}
	return ""
}

// Narrow applies the PHP layer to sel (docs/dynamic-layer.md §11.1): every selected PHP unit with a usable file-list record runs
// only the test files without a usable record, the test files whose record keeps them for the changed files, and the changed
// test files. The unit stays selected; removed test files are listed in Narrowed as "<unit>#<file>". Call it only after Guard
// passed.
func Narrow(sel model.Selection, p Params) model.Selection {
	changed := map[string]bool{}
	for _, c := range sel.Changed {
		changed[c] = true
	}
	always := map[string]bool{}
	for _, a := range sel.AlwaysRun {
		always[a] = true
	}
	var fns []string
	for _, c := range p.Changed {
		for _, s := range c.Members {
			fns = append(fns, fmt.Sprintf("%s:%d", c.Path, s.From))
		}
	}
	out := sel
	out.Tests = slices.Clone(sel.Tests)
	out.Narrowed = slices.Clone(sel.Narrowed)
	removedTotal := 0
	for i, t := range out.Tests {
		if !Narrowable(t, p.UnitEco[t.Unit]) {
			continue
		}
		keep := func(why string) { out.Tests[i].Dynamic = "PHP layer kept the whole unit: " + why }
		u := UnitOf(t)
		if u.Config == "" {
			keep("no PHPUnit XML configuration (the test files cannot be listed)")
			continue
		}
		if always[t.Unit] {
			keep("always run (previously failed)")
			continue
		}
		env, why := p.Env(t)
		if env == "" {
			keep("environment fingerprint: " + why)
			continue
		}
		byUnit := latest(p.Records, env)
		bp := dynamic.Params{Eco: Eco, Collector: CollectorID, Env: env, BaseOID: func(f string) string {
			if !p.Base.Has(f) {
				return ""
			}
			return p.Base.Key(f)
		}}
		cl, ok := byUnit[t.Unit]
		if !ok {
			keep("no file-list record for the current environment")
			continue
		}
		if why := dynamic.Usable(cl, nil, bp); why != "" {
			keep("file list not usable: " + why)
			continue
		}
		if why := MultiOperand(cl.Runner); why != "" {
			keep(why)
			continue
		}
		if why := sameTestRoots(cl, p.Base, u); why != "" {
			keep("file list not usable: " + why)
			continue
		}
		var run []string
		var removed []model.NarrowedTest
		for _, f := range cl.Classes {
			sub := SubID(t.Unit, f)
			if changed[f] || always[f] || always[sub] {
				run = append(run, f)
				continue
			}
			r, ok := byUnit[sub]
			if !ok || dynamic.Usable(r, []string{f}, bp) != "" || keepReason(r, p.Changed) != "" {
				run = append(run, f)
				continue
			}
			what := "none of the changed files"
			if len(fns) > 0 {
				what = fmt.Sprintf("none of the changed files, or none of the %d changed functions (%s) of the files it included",
					len(fns), strings.Join(firstNs(fns, 5), ", "))
			}
			removed = append(removed, model.NarrowedTest{Unit: sub, Recorded: r.Recorded, Commit: r.Commit,
				Reason: fmt.Sprintf("record by %s: saw %s", r.Collector, what)})
		}
		if len(removed) == 0 {
			out.Tests[i].Dynamic = fmt.Sprintf("PHP layer: all %d test files run (each saw a changed file or has no usable record)", len(cl.Classes))
			continue
		}
		sort.Strings(run)
		out.Tests[i].ClassFilter = true
		out.Tests[i].Classes = run
		out.Tests[i].Dynamic = fmt.Sprintf("PHP layer: %d of %d test files run", len(run), len(cl.Classes))
		out.Narrowed = append(out.Narrowed, removed...)
		removedTotal += len(removed)
	}
	if removedTotal > 0 && out.IndexStats != nil {
		st := make(map[string]int, len(out.IndexStats)+1)
		for k, v := range out.IndexStats {
			st[k] = v
		}
		st["narrowed_test_files"] = removedTotal
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

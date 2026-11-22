package dynamic

import (
	"fmt"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/classify"
	"github.com/qxbao/bgraph/internal/eco/jvm"
	"github.com/qxbao/bgraph/internal/eco/node"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
)

// Change is one changed file as the guards of docs/dynamic-layer.md §2 see it.
type Change struct {
	Path             string
	Status           byte // 'A','M','D','T' (renames and copies arrive as D+A)
	OldMode, NewMode string
	// BaseRole, HeadRole: the file's role in the narrowed ecosystem's analysis of the base and of the head tree.
	BaseRole, HeadRole model.Role
	Global             bool // matched the built-in global widening table
	Claimed            bool // some ecosystem's ClassifyChange claimed it (manifest, config, lockfile, native source...)
	Declared           bool // named by a bgraph.toml [[config]] or [[dynamic]] declaration (see Declared)
	Rule               bool // a bgraph.toml [[rule]] matches it
	Referenced         bool // a graph edge (direct or glob) points to it
	OtherRole          bool // some ecosystem (any) gives it a role in the base or head analysis
}

// Params is everything Narrow needs besides the selection and the changes.
type Params struct {
	Eco       string // the narrowed ecosystem
	Collector string // the current collector ID: a record made by another collector (or version) is not usable
	Env       string // the current environment fingerprint, computed at the base (docs/dynamic-layer.md §5)
	// BaseOID returns the blob OID of a repo-relative file at the base, "" when the base has no such file.
	BaseOID func(p string) string
	Records []Record          // records of any ecosystem/environment: Narrow picks the ones for (Eco, unit, Env)
	UnitEco map[string]string // selected unit ID → ecosystem (selector.Result.UnitEco)
}

// sourceFile reports whether p is a source file kind the ecosystem's collector observes when it is loaded (docs/dynamic-layer.md §2).
// Python: .py only (not .pyi stubs, .pyx/.pxd sources compiled to extensions, .so/.pyd, data files). Node: JS/TS modules, not
// declaration files or embedded-script formats (docs/dynamic-layer.md §8.4).
var sourceFile = map[string]func(p string) bool{
	"python": func(p string) bool { return strings.HasSuffix(p, ".py") },
	"node":   node.DynamicSource,
	// jvm: the per-test-class layer (docs/dynamic-layer.md §9) has its own guards and narrowing (package jvmdyn); this entry only
	// makes the ecosystem known to the record store, import and status.
	"jvm": jvm.DynamicSource,
}

// Supported reports whether eco has a dynamic layer.
func Supported(eco string) bool { _, ok := sourceFile[eco]; return ok }

// Declared reports whether a bgraph.toml [[config]] (file, sources, ignore_paths) or [[dynamic]] (file) declaration names p.
func Declared(rf *rules.File, p string) bool {
	if rf == nil {
		return false
	}
	for _, c := range rf.Configs {
		if c.File == p || slices.Contains(c.Sources, p) || slices.Contains(c.IgnorePaths, p) {
			return true
		}
	}
	for _, d := range rf.Dynamics {
		if d.File == p {
			return true
		}
	}
	return false
}

// Guard returns "" when sel may be narrowed for eco at all (docs/dynamic-layer.md §2), or the reason it may not.
func Guard(sel model.Selection, changes []Change, eco string) string {
	isSource, ok := sourceFile[eco]
	if !ok {
		return "no dynamic layer for ecosystem " + eco
	}
	if sel.Scope == model.TierFull {
		return "the selection is full (never narrowed)"
	}
	for _, w := range sel.Widening {
		if w.Tier == model.TierFull || w.Reason == model.ReasonGlobalInput {
			return fmt.Sprintf("widening %s on %s is global or full", w.Reason, w.Path)
		}
	}
	if len(changes) == 0 || len(changes) != len(sel.Changed) {
		return "the change set is not classified"
	}
	for _, c := range changes {
		var why string
		switch {
		case c.Status != 'M':
			why = fmt.Sprintf("status %c (only modifications of existing files are observable: an addition can shadow a module, a deletion or rename changes loading)", c.Status)
		case c.OldMode != c.NewMode:
			why = fmt.Sprintf("mode changed %s -> %s", c.OldMode, c.NewMode)
		case c.NewMode != "100644" && c.NewMode != "100755":
			why = "mode " + c.NewMode + " is not a regular file"
		case !isSource(c.Path):
			if !docFile(c) {
				why = "not a " + eco + " source file the collector observes, nor an unreferenced documentation file"
			}
		case !isSourceRole(c.BaseRole) || !isSourceRole(c.HeadRole):
			why = fmt.Sprintf("role %s/%s at base/head is not source or test", roleName(c.BaseRole), roleName(c.HeadRole))
		case c.Global:
			why = "global input"
		case c.Claimed:
			why = "claimed as a manifest, configuration, lockfile or build input"
		case c.Declared:
			why = "named by a bgraph.toml [[config]] or [[dynamic]] declaration"
		}
		if why != "" {
			return c.Path + ": " + why
		}
	}
	return ""
}

// docFile: a modified documentation or harmless file (classify.Doc / classify.Harmless) that nothing in the graph references, that
// no ecosystem gives a role, and that no bgraph.toml entry names (docs/dynamic-layer.md §2). Tests can only see it by reading it, which
// the collector observes: like a source file, it stays a changed file for the per-unit check.
func docFile(c Change) bool {
	return (classify.Doc(c.Path) || classify.Harmless(c.Path)) && !c.Referenced && !c.OtherRole && !c.Rule && !c.Declared &&
		!c.Global && !c.Claimed && c.BaseRole == "" && c.HeadRole == ""
}

func isSourceRole(r model.Role) bool { return r == model.RoleSource || r == model.RoleTest }

func roleName(r model.Role) string {
	if r == "" {
		return "none"
	}
	return string(r)
}

// Pick returns the record of (eco, unit, env): the latest one. ok is false when there is none; others counts the records of
// the unit for other environments.
func Pick(recs []Record, eco, unit, env string) (rec Record, ok bool, others int) {
	for _, r := range recs {
		if r.Eco != eco || r.Unit != unit {
			continue
		}
		if r.Env != env {
			others++
			continue
		}
		if !ok || newer(r, rec) {
			rec, ok = r, true
		}
	}
	return rec, ok, others
}

// Usable returns "" when r may be used for unit u at the base (docs/dynamic-layer.md §3), or why not.
func Usable(r Record, files []string, p Params) string {
	switch {
	case r.Collector != p.Collector:
		return fmt.Sprintf("recorded by collector %s, current is %s", r.Collector, p.Collector)
	case r.Env != p.Env:
		return "recorded in another environment"
	case !r.Pass:
		return "the recorded run did not pass"
	case !r.Complete:
		return "incomplete record: " + r.Incomplete
	}
	for _, f := range files {
		if _, ok := r.Deps[f]; !ok {
			return "the record does not list the unit's own file " + f
		}
	}
	deps := make([]string, 0, len(r.Deps))
	for d := range r.Deps {
		deps = append(deps, d)
	}
	slices.Sort(deps)
	for _, d := range deps {
		switch b := p.BaseOID(d); {
		case b == "":
			return "stale: dependency " + d + " does not exist at the base"
		case b != r.Deps[d]:
			return "stale: dependency " + d + " differs at the base"
		}
	}
	return ""
}

// Narrow applies the dynamic layer to sel (docs/dynamic-layer.md): every widened unit of p.Eco with a usable record whose
// dependencies contain no changed file is moved from Tests to Narrowed. Other kept widened units of p.Eco get a Dynamic note
// saying why. When a guard forbids narrowing the selection is returned with a dynamic_skipped warning and nothing else changes.
func Narrow(sel model.Selection, changes []Change, p Params) model.Selection {
	hasCandidate := false
	for _, t := range sel.Tests {
		if t.Widened && p.UnitEco[t.Unit] == p.Eco {
			hasCandidate = true
			break
		}
	}
	if !hasCandidate {
		return sel // nothing the dynamic layer could remove
	}
	if why := Guard(sel, changes, p.Eco); why != "" {
		return Skip(sel, p.Eco, why)
	}
	out := sel
	out.Tests = nil
	out.Narrowed = slices.Clone(sel.Narrowed)
	for _, t := range sel.Tests {
		if !t.Widened || p.UnitEco[t.Unit] != p.Eco {
			out.Tests = append(out.Tests, t)
			continue
		}
		keep := ""
		rec, ok, others := Pick(p.Records, p.Eco, t.Unit, p.Env)
		files := t.Files
		if len(files) == 0 && !t.Whole {
			files = []string{t.Unit}
		}
		switch {
		case t.Whole:
			keep = "whole-project unit: no per-unit record"
		case !ok && others > 0:
			keep = fmt.Sprintf("no record for the current environment (%d for other environments)", others)
		case !ok:
			keep = "no record"
		default:
			if why := Usable(rec, files, p); why != "" {
				keep = "record not usable: " + why
			} else {
				var hit []string
				for _, c := range sel.Changed {
					if _, ok := rec.Deps[c]; ok {
						hit = append(hit, c)
					}
				}
				if len(hit) > 0 {
					keep = "recorded dependencies include the changed file " + strings.Join(hit, ", ")
				}
			}
		}
		if keep != "" {
			t.Dynamic = "kept by the dynamic layer: " + keep
			out.Tests = append(out.Tests, t)
			continue
		}
		out.Narrowed = append(out.Narrowed, model.NarrowedTest{Unit: t.Unit, Recorded: rec.Recorded, Commit: rec.Commit,
			Reason: fmt.Sprintf("record by %s: none of the %d changed files is among its %d recorded dependencies", rec.Collector,
				len(sel.Changed), len(rec.Deps))})
	}
	if len(out.Tests) == 0 && out.Scope != model.TierFull {
		out.Scope = model.TierNone // every unit was removed: nothing is affected (an empty non-none scope means "run everything")
	}
	if out.IndexStats != nil {
		st := make(map[string]int, len(out.IndexStats)+1)
		for k, v := range out.IndexStats {
			st[k] = v
		}
		st["narrowed"] = len(out.Narrowed)
		out.IndexStats = st
	}
	return out
}

// Skip returns sel with a dynamic_skipped warning for eco.
func Skip(sel model.Selection, eco, why string) model.Selection {
	sel.Warnings = append(slices.Clone(sel.Warnings), model.Widening{Tier: model.TierNone, Reason: model.ReasonDynamicSkipped,
		Detail: eco + ": " + why})
	return sel
}

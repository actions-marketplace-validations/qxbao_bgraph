// Package selector ties everything into one Selection (docs/plan.md §4.3): change set → classification → union graph of base/current →
// backward walk → test units → widening. Every "don't know" branch widens and never returns empty.
package selector

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/classify"
	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/graph"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/tree"
)

// DefaultMaxChanged is the default threshold for an oversized diff (docs/plan.md §4.3 step 1).
const DefaultMaxChanged = 5000

// Changed is a file that changed between the base and the current tree. Status: 'A','M','D','T'. Mode follows git ("100644", "120000", "160000").
type Changed struct {
	Path    string
	Status  byte
	OldMode string
	NewMode string
}

// Input is the input of Select.
type Input struct {
	Base, Head string
	Changed    []Changed
	BaseTree   tree.Tree // nil: no base → full
	HeadTree   tree.Tree
	Ecos       []eco.Ecosystem
	Scanner    eco.Scanner
	Rules      *rules.File // nil: no bgraph.toml
	RulesErr   error       // broken bgraph.toml → full
	// PreviouslyFailed: ids of test units that failed on the previous run (supplied by the caller); always re-run if they still exist.
	PreviouslyFailed []string
	MaxChanged       int // 0 → Rules.Options.MaxChanged or DefaultMaxChanged
}

type state struct {
	in   Input
	sel  model.Selection
	full bool
}

func (s *state) widen(w model.Widening) {
	s.sel.Widening = append(s.sel.Widening, w)
	if w.Tier == model.TierFull {
		s.full = true
	}
}

// Select computes the selection.
func Select(ctx context.Context, in Input) model.Selection {
	s := &state{in: in}
	s.sel = model.Selection{Base: in.Base, Head: in.Head, Scope: model.TierNone, Changed: []string{}}
	changed := normalize(in.Changed)
	for _, c := range changed {
		s.sel.Changed = append(s.sel.Changed, c.Path)
	}
	if len(changed) == 0 {
		return s.sel
	}
	if in.RulesErr != nil {
		return s.finishFull(model.Widening{Tier: model.TierFull, Reason: model.ReasonUserRule, Path: "bgraph.toml", Detail: in.RulesErr.Error()})
	}
	if in.BaseTree == nil {
		return s.finishFull(model.Widening{Tier: model.TierFull, Reason: model.ReasonNoBase, Detail: "no base tree"})
	}
	maxChanged := in.MaxChanged
	if maxChanged <= 0 && in.Rules != nil && in.Rules.Options.MaxChanged > 0 {
		maxChanged = in.Rules.Options.MaxChanged
	}
	if maxChanged <= 0 {
		maxChanged = DefaultMaxChanged
	}
	if len(changed) > maxChanged {
		return s.finishFull(model.Widening{Tier: model.TierFull, Reason: model.ReasonDiffTooLarge,
			Detail: fmt.Sprintf("%d changed files > threshold %d", len(changed), maxChanged)})
	}

	// Analyze both trees with every ecosystem.
	var all []*eco.Analysis
	headA := map[string]*eco.Analysis{}
	var projects []eco.Project
	var units []model.TestUnit
	for _, e := range in.Ecos {
		if err := ctx.Err(); err != nil {
			return s.finishFull(model.Widening{Tier: model.TierFull, Reason: model.ReasonInternalError, Detail: err.Error()})
		}
		ba, err := e.Analyze(ctx, in.BaseTree, in.Scanner)
		if err != nil {
			return s.finishFull(model.Widening{Tier: model.TierFull, Reason: model.ReasonInternalError, Detail: e.Name() + " (base): " + err.Error()})
		}
		ha, err := e.Analyze(ctx, in.HeadTree, in.Scanner)
		if err != nil {
			return s.finishFull(model.Widening{Tier: model.TierFull, Reason: model.ReasonInternalError, Detail: e.Name() + ": " + err.Error()})
		}
		all = append(all, ba, ha)
		headA[e.Name()] = ha
		projects = append(projects, ha.Projects...)
		projects = append(projects, ba.Projects...)
		units = append(units, ha.Tests...)
	}
	g := graph.Build(all...)
	for _, e := range g.BadPatterns() {
		s.widen(model.Widening{Tier: model.TierFull, Reason: model.ReasonInternalError, Path: e.From, Detail: "invalid glob: " + e.To})
	}

	var seeds []graph.Seed
	type ruleTarget struct {
		target string
		reason string
	}
	var ruleTargets []ruleTarget

	for _, c := range changed {
		p := c.Path
		if c.OldMode == "160000" || c.NewMode == "160000" {
			s.widen(model.Widening{Tier: model.TierFull, Reason: model.ReasonSubmodule, Path: p, Detail: "submodule pointer changed"})
			continue
		}
		// 1. User rules match first.
		skip, ruled := false, false
		if in.Rules != nil {
			for _, r := range in.Rules.Match(p) {
				switch {
				case r.AffectsAll:
					s.widen(model.Widening{Tier: model.TierFull, Reason: model.ReasonUserRule, Path: p, Detail: ruleDetail(r)})
				case r.AffectsNone:
					if g.IsNode(p) {
						s.sel.Warnings = append(s.sel.Warnings, model.Widening{Tier: model.TierNone, Reason: model.ReasonRuleRejected, Path: p,
							Detail: ruleDetail(r) + `: affects="none" ignored because the file is a graph node`})
					} else {
						skip = true
					}
				default:
					ruled = true
					for _, t := range r.Affects {
						ruleTargets = append(ruleTargets, ruleTarget{t, ruleDetail(r) + " matches " + p})
					}
				}
			}
		}
		if skip {
			continue
		}
		// A rule with targets already declared the file's impact (docs/plan.md §6.7: rules override the widening table): no further widening; still walk backwards if it is a node.
		if ruled {
			if g.IsNode(p) {
				seeds = append(seeds, graph.Seed{Path: p, Reason: "changed"})
			}
			continue
		}
		// 2. Global inputs.
		if w, ok := classify.Global(p); ok {
			s.widen(w)
			continue
		}
		// 3. Ecosystem manifests / configs / lockfiles.
		claimed := false
		for _, e := range in.Ecos {
			ws, ok := e.ClassifyChange(p, in.BaseTree, in.HeadTree, headA[e.Name()])
			if ok {
				claimed = true
				for _, w := range ws {
					s.widen(w)
				}
			}
		}
		// 4. Graph node: walk backwards (even if already widened because of config: the union does no harm).
		if g.IsNode(p) {
			seeds = append(seeds, graph.Seed{Path: p, Reason: "changed"})
			// Code/data file that nothing depends on: walking backwards reaches no test, but a test may still use it outside the graph
			// (read via fs, run via child_process). Nobody known → widen to the project (docs/plan.md §4.3 step 2, §6.7).
			if r := g.Role(p); !claimed && r != model.RoleTest && r != model.RoleConfig && r != model.RoleManifest && r != model.RoleLock &&
				!g.HasDependents(p) {
				if pr, ok := eco.NearestProject(projects, p); ok {
					s.widen(model.Widening{Tier: model.TierModule, Reason: model.ReasonUnreferenced, Path: p, Scope: pr.Dir,
						Detail:  "no file in the graph depends on this file; a test may read/run it outside the graph",
						Suggest: suggestRule(p)})
				} else {
					s.widen(model.Widening{Tier: model.TierFull, Reason: model.ReasonUnreferenced, Path: p,
						Detail: "no file depends on it and it is outside every project", Suggest: suggestRule(p)})
				}
			}
			continue
		}
		if claimed || classify.Harmless(p) || classify.Doc(p) {
			continue
		}
		// 5. Unknown file: widen to the project containing it, full if outside every project.
		if pr, ok := eco.NearestProject(projects, p); ok {
			s.widen(model.Widening{Tier: model.TierModule, Reason: model.ReasonUnknownFile, Path: p, Scope: pr.Dir,
				Detail: "file belongs to no ecosystem and nobody references it", Suggest: suggestRule(p)})
		} else {
			s.widen(model.Widening{Tier: model.TierFull, Reason: model.ReasonUnknownFile, Path: p,
				Detail: "file is outside every project and nobody references it", Suggest: suggestRule(p)})
		}
	}

	// Unresolved import: a changed file within its scope → the file containing the import counts as affected.
	for _, u := range g.Unresolved {
		if u.ScopeTier == model.TierNone {
			continue // report only: the ecosystem already encoded the dependency as an edge (glob)
		}
		hit := ""
		for _, c := range changed {
			if eco.Within(c.Path, u.ScopeDir) {
				hit = c.Path
				break
			}
		}
		if hit == "" {
			continue
		}
		w := model.Widening{Tier: u.ScopeTier, Reason: unresolvedReason(u.Class), Path: u.File, Scope: u.ScopeDir,
			Detail: fmt.Sprintf("%s:%d %q unresolved; %s changed within scope", u.File, u.Line, u.Spec, hit)}
		if u.Detail != "" {
			w.Detail += " (" + u.Detail + ")"
		}
		s.widen(w)
		seeds = append(seeds, graph.Seed{Path: u.File, Reason: string(u.Class) + " import, " + hit + " changed in " + scopeName(u.ScopeDir)})
	}

	if s.full {
		return s.finishFull()
	}

	reach := g.Walk(seeds)
	// Warnings of the current tree relevant to the selection (the file with the problem is on the backward path).
	for _, e := range in.Ecos {
		if ha := headA[e.Name()]; ha != nil {
			for _, w := range ha.Warnings {
				if _, ok := reach[w.Path]; ok {
					s.sel.Warnings = append(s.sel.Warnings, w)
				}
			}
		}
	}
	units = DedupUnits(units)
	chosen := map[string]*model.SelectedTest{}
	choose := func(u model.TestUnit, reason string) {
		st, ok := chosen[u.ID]
		if !ok {
			st = &model.SelectedTest{Unit: u.ID, Project: u.Project, Runner: u.Runner, Files: u.Files, Whole: u.Whole}
			chosen[u.ID] = st
		}
		if !slices.Contains(st.Reasons, reason) {
			st.Reasons = append(st.Reasons, reason)
		}
	}
	for _, u := range units {
		if f, ok := ReachedFile(u, reach); ok {
			choose(u, strings.Join(reach.Chain(f), " ; "))
		}
	}
	// Scope widening (module/package): every test unit in the scope.
	for _, w := range s.sel.Widening {
		if w.Tier != model.TierModule && w.Tier != model.TierPackage {
			continue
		}
		if isUnresolvedReason(w.Reason) {
			continue // already handled via a seed: only the dependents of the file containing the import are affected
		}
		for _, u := range units {
			if unitWithin(u, w.Scope) {
				choose(u, fmt.Sprintf("widen %s (%s) due to %s", scopeName(w.Scope), w.Reason, w.Path))
			}
		}
	}
	// User rules with targets.
	for _, rt := range ruleTargets {
		var pat *glob.Pattern
		if strings.ContainsAny(rt.target, "*?[{") {
			pat, _ = glob.Compile(rt.target)
		}
		for _, u := range units {
			if pat == nil && unitWithin(u, path.Clean(rt.target)) {
				choose(u, rt.reason)
				continue
			}
			if pat != nil {
				for _, f := range u.Files {
					if pat.Match(f) {
						choose(u, rt.reason)
						break
					}
				}
			}
		}
	}
	// "Whole project" unit (test file set unknown): any meaningful change may touch an unknown test of the project.
	if len(seeds) > 0 || len(s.sel.Widening) > 0 {
		for _, u := range units {
			if u.Whole {
				choose(u, model.ReasonConfigIsCode+": test file set of project "+scopeName(u.Project)+" is unknown (test pattern computed by code)")
			}
		}
	}
	// Previously failed tests.
	for _, id := range in.PreviouslyFailed {
		for _, u := range units {
			if u.ID == id {
				choose(u, "previously_failed")
				s.sel.AlwaysRun = append(s.sel.AlwaysRun, id)
			}
		}
	}
	// New tests.
	for _, c := range changed {
		if c.Status == 'A' && g.Role(c.Path) == model.RoleTest {
			s.sel.AlwaysRun = append(s.sel.AlwaysRun, c.Path)
		}
	}
	slices.Sort(s.sel.AlwaysRun)
	s.sel.AlwaysRun = slices.Compact(s.sel.AlwaysRun)

	ids := make([]string, 0, len(chosen))
	for id := range chosen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		s.sel.Tests = append(s.sel.Tests, *chosen[id])
	}
	s.sel.Scope = model.TierNone
	if len(s.sel.Tests) > 0 {
		s.sel.Scope = model.TierGraph
		for _, w := range s.sel.Widening {
			// Unresolved imports are handled via seeds (the file containing the import counts as changed, then walk backwards): the selection is still computed
			// from the graph, so the scope is not raised; the widening is still listed in Widening for explanation.
			if isUnresolvedReason(w.Reason) && w.Tier != model.TierFull {
				continue
			}
			s.sel.Scope = model.Max(s.sel.Scope, w.Tier)
		}
	}
	s.sel.IndexStats = map[string]int{"units": len(units), "seeds": len(seeds), "reached": len(reach), "unresolved": len(g.Unresolved)}
	return s.sel
}

func (s *state) finishFull(ws ...model.Widening) model.Selection {
	for _, w := range ws {
		s.widen(w)
	}
	s.sel.Scope = model.TierFull
	s.sel.Tests = nil
	return s.sel
}

func normalize(in []Changed) []Changed {
	out := make([]Changed, 0, len(in))
	for _, c := range in {
		c.Path = path.Clean(strings.ReplaceAll(c.Path, "\\", "/"))
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Changed) int { return strings.Compare(a.Path, b.Path) })
	return slices.CompactFunc(out, func(a, b Changed) bool { return a.Path == b.Path })
}

// DedupUnits sorts test units by ID and drops duplicates (the same unit reported by the base and the head analysis).
func DedupUnits(units []model.TestUnit) []model.TestUnit {
	units = slices.Clone(units)
	slices.SortFunc(units, func(a, b model.TestUnit) int { return strings.Compare(a.ID, b.ID) })
	return slices.CompactFunc(units, func(a, b model.TestUnit) bool { return a.ID == b.ID })
}

// ReachedFile returns the first file of u that the backward walk reached.
func ReachedFile(u model.TestUnit, reach graph.Reach) (string, bool) {
	for _, f := range u.Files {
		if _, ok := reach[f]; ok {
			return f, true
		}
	}
	return "", false
}

func unitWithin(u model.TestUnit, dir string) bool {
	if eco.Within(u.Project, dir) {
		return true
	}
	for _, f := range u.Files {
		if eco.Within(f, dir) {
			return true
		}
	}
	return false
}

func ruleDetail(r rules.Rule) string {
	d := fmt.Sprintf("bgraph.toml rule #%d", r.Index+1)
	if r.Reason != "" {
		d += " (" + r.Reason + ")"
	}
	return d
}

func suggestRule(p string) string {
	return fmt.Sprintf("[[rule]]\npaths = [%q]\naffects = [\"<project or glob test>\"]  # or \"none\" if certainly harmless", p)
}

func scopeName(dir string) string {
	if dir == "" || dir == "." {
		return "repo"
	}
	return dir
}

func unresolvedReason(c model.UnresolvedClass) string {
	switch c {
	case model.UnresolvedDynamic:
		return model.ReasonDynamicImport
	case model.UnresolvedAmbiguous:
		return model.ReasonAmbiguousImport
	case model.UnresolvedUncertainScan:
		return model.ReasonUncertainScan
	}
	return model.ReasonUnknownImport
}

func isUnresolvedReason(r string) bool {
	switch r {
	case model.ReasonDynamicImport, model.ReasonAmbiguousImport, model.ReasonUncertainScan, model.ReasonUnknownImport:
		return true
	}
	return false
}

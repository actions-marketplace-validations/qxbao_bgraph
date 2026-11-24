// Package selector ties everything into one Selection (docs/architecture.md, "Pipeline"): change set → classification → union graph of base/current →
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

// DefaultMaxChanged is the default threshold for an oversized diff ([options] max_changed, docs/configuration.md).
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
	// Exclude: changed files the caller declares are inputs of no test (an embedding program's own state files in the working
	// tree). A changed file matching one is dropped from the change set (Selection.Changed included) before classification,
	// unless something references it or it is more than data (see keepExcluded): then it stays and a rule_rejected warning
	// says why.
	Exclude []*glob.Pattern
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

// ChangeInfo is how the selector classified one changed file; the dynamic layer's guards (docs/dynamic-layer.md §2) read it.
type ChangeInfo struct {
	Changed
	// Roles: ecosystem name → the file's role in that ecosystem's analysis of the base and of the head tree ("" when unclaimed).
	Roles map[string][2]model.Role
	// Global: the file matched the built-in global widening table.
	Global bool
	// Claimed: ecosystems whose ClassifyChange claimed the file as their manifest/config/lockfile/other non-source input.
	Claimed []string
	// Rule: a bgraph.toml [[rule]] matched the file.
	Rule bool
	// Dependents: some file of the graph depends on it (a direct or glob edge points to it).
	Dependents bool
	// Linked: the file is a symbolic link, or other paths read it through one (docs/architecture.md, "Paths, case, symlinks and
	// LFS"); its aliases were classified as changed files too.
	Linked bool
}

// Result is Select's output together with the details the dynamic layer needs.
type Result struct {
	Selection model.Selection
	// Changes: one entry per changed file, in the order of Selection.Changed. Empty when the selection ended full before the
	// classification (no base, broken bgraph.toml, oversized diff, analysis error).
	Changes []ChangeInfo
	// UnitEco: selected unit ID → its ecosystem.
	UnitEco map[string]string
}

// Select computes the selection.
func Select(ctx context.Context, in Input) model.Selection {
	return Run(ctx, in).Selection
}

// Run computes the selection and returns it with the classification details (see Result).
func Run(ctx context.Context, in Input) Result {
	var res Result
	res.Selection = run(ctx, in, &res)
	return res
}

func run(ctx context.Context, in Input, res *Result) model.Selection {
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
	g := graph.Build(append(all, eco.LinkAnalysis(in.BaseTree), eco.LinkAnalysis(in.HeadTree))...)
	links := linkSides(in.BaseTree, in.HeadTree)
	// A file read through symbolic links changes every path that reads it (its aliases): they are classified as changed files
	// too (a manifest seen through a directory link, a glob over the link directory, a scoped unresolved import...).
	if len(in.Exclude) > 0 {
		changed = s.exclude(changed, g, links, all)
		if len(changed) == 0 {
			return s.sel
		}
	}
	work, aliasOf := withAliases(changed, g, in.BaseTree, in.HeadTree)
	info := make(map[string]*ChangeInfo, len(work))
	for _, c := range work {
		ci := &ChangeInfo{Changed: c, Roles: map[string][2]model.Role{}, Dependents: g.HasDependents(c.Path)}
		for i, e := range in.Ecos {
			ci.Roles[e.Name()] = [2]model.Role{all[2*i].Roles[c.Path], all[2*i+1].Roles[c.Path]}
		}
		_, isLink := links[c.Path]
		ci.Linked = isLink || len(g.Aliases(c.Path)) > 0 || aliasOf[c.Path] != ""
		info[c.Path] = ci
		if aliasOf[c.Path] == "" {
			res.Changes = append(res.Changes, *ci) // replaced below once the classification is complete
		}
	}
	defer func() {
		for i := range res.Changes {
			res.Changes[i] = *info[res.Changes[i].Path]
		}
	}()
	for _, e := range g.BadPatterns() {
		s.widen(model.Widening{Tier: model.TierFull, Reason: model.ReasonInternalError, Path: e.From, Detail: "invalid glob: " + e.To})
	}

	var seeds []graph.Seed
	type ruleTarget struct {
		target string
		reason string
	}
	var ruleTargets []ruleTarget

	widenedLink := map[string]bool{}
	widenLink := func(l tree.Link, why string) {
		if widenedLink[l.Path] {
			return
		}
		widenedLink[l.Path] = true
		w := model.Widening{Tier: model.TierFull, Reason: model.ReasonSymlinkOutside, Path: l.Path,
			Detail: fmt.Sprintf("symlink %s -> %s: %s; %s", l.Path, strings.TrimSpace(l.Target), l.Problem, why)}
		if pr, ok := eco.NearestProject(projects, l.Path); ok {
			w.Tier, w.Scope = model.TierModule, pr.Dir
		}
		s.widen(w)
	}
	for _, c := range work {
		p := c.Path
		seedReason := "changed"
		if a := aliasOf[p]; a != "" {
			seedReason = "changed (reads " + a + " through a symlink)"
		}
		if c.OldMode == "160000" || c.NewMode == "160000" {
			s.widen(model.Widening{Tier: model.TierFull, Reason: model.ReasonSubmodule, Path: p, Detail: "submodule pointer changed"})
			continue
		}
		// 1. User rules match first.
		skip, ruled := false, false
		if in.Rules != nil {
			for _, r := range in.Rules.Match(p) {
				info[p].Rule = true
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
		// A rule with targets already declared the file's impact (docs/architecture.md, "Rules come first": rules override the widening table): no further widening; still walk backwards if it is a node.
		if ruled {
			if g.IsNode(p) {
				seeds = append(seeds, graph.Seed{Path: p, Reason: seedReason})
			}
			continue
		}
		// A file under the target of a link the trees follow only partly (a cycle, an oversized expansion): what reads it through
		// the link is not fully known.
		for _, l := range links.partial() {
			if eco.Within(p, l.Real) {
				widenLink(l, p+" changed under its target")
			}
		}
		// A symbolic link changed (added, removed, retargeted, type change): what it pointed to and what it points to now count as
		// changed for whoever reads them through it; a link that cannot be followed widens (symlink_outside).
		if side, ok := links[p]; ok {
			for i, l := range side {
				if l == nil {
					continue
				}
				if l.Problem != "" {
					widenLink(*l, "the link changed")
				}
				t := in.BaseTree
				if i == 1 {
					t = in.HeadTree
				}
				for _, f := range linkTargetFiles(t, *l) {
					seeds = append(seeds, graph.Seed{Path: f, Reason: "changed (target of changed symlink " + p + ")"})
				}
			}
			if !in.BaseTree.Has(p) && !in.HeadTree.Has(p) {
				// A directory link or a link that cannot be followed is not a file of either tree: its effect is fully described above.
				continue
			}
		}
		// 2. Global inputs.
		if w, ok := classify.Global(p); ok {
			info[p].Global = true
			s.widen(w)
			continue
		}
		// 3. Ecosystem manifests / configs / lockfiles.
		claimed := false
		for _, e := range in.Ecos {
			ws, ok := e.ClassifyChange(p, in.BaseTree, in.HeadTree, headA[e.Name()])
			if ok {
				claimed = true
				info[p].Claimed = append(info[p].Claimed, e.Name())
				for _, w := range ws {
					s.widen(w)
					for _, sd := range w.Seeds {
						seeds = append(seeds, graph.Seed{Path: sd, Reason: w.Reason + " " + p})
					}
				}
			}
		}
		// 4. Graph node: walk backwards (even if already widened because of config: the union does no harm).
		if g.IsNode(p) {
			seeds = append(seeds, graph.Seed{Path: p, Reason: seedReason})
			// Code/data file that nothing depends on: walking backwards reaches no test, but a test may still use it outside the graph
			// (read via fs, run via child_process). Nobody known → widen to the project (unreferenced_file, docs/architecture.md).
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
		for _, c := range work {
			if eco.Triggers(u, c.Path) {
				if u.SkipTests && g.Role(c.Path) == model.RoleTest && !g.Referenced(c.Path) {
					continue
				}
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
	// The precise walk: from the changed files only (docs/dynamic-layer.md §1). Seeds of unresolved imports are added after every
	// changed-file seed, so the changed-file seeds are exactly those whose reason starts with "changed" (a changed file, a path that
	// reads it through a symbolic link, the targets of a changed link).
	var changedSeeds []graph.Seed
	for _, sd := range seeds {
		if strings.HasPrefix(sd.Reason, "changed") {
			changedSeeds = append(changedSeeds, sd)
		}
	}
	reachP := g.Walk(changedSeeds)
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
	precise := map[string]bool{}
	res.UnitEco = map[string]string{}
	// choose selects u; exact: u is chosen precisely (docs/dynamic-layer.md §1), not only because of a widening.
	choose := func(u model.TestUnit, reason string, exact bool) {
		st, ok := chosen[u.ID]
		if !ok {
			st = &model.SelectedTest{Unit: u.ID, Project: u.Project, Runner: u.Runner, Files: u.Files, Whole: u.Whole, Package: u.Package,
				Config: u.Config, Dir: u.Dir, BinDir: u.BinDir, RunnerPaths: u.RunnerPaths, TestModules: u.TestModules}
			chosen[u.ID] = st
			res.UnitEco[u.ID] = u.Eco
		}
		if exact {
			precise[u.ID] = true
		}
		if !slices.Contains(st.Reasons, reason) {
			st.Reasons = append(st.Reasons, reason)
		}
	}
	for _, u := range units {
		if f, ok := ReachedFile(u, reach); ok {
			_, exact := ReachedFile(u, reachP)
			choose(u, strings.Join(reach.Chain(f), " ; "), exact)
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
			if w.Eco != "" && u.Eco != w.Eco {
				continue
			}
			if (w.Exact && u.Project == w.Scope) || (!w.Exact && unitWithin(u, w.Scope)) {
				choose(u, fmt.Sprintf("widen %s (%s) due to %s", scopeName(w.Scope), w.Reason, w.Path), false)
			}
		}
	}
	// User rules with targets. A target that names no unit of the current tree is a configuration error (a stale or mistyped
	// directory, a glob over files no unit has): the rule cannot say what the change affects, so the selection is full.
	for _, rt := range ruleTargets {
		matched := false
		for _, u := range units {
			if ruleTargetMatches(u, rt.target) {
				matched = true
				choose(u, rt.reason, true)
			}
		}
		if !matched {
			s.widen(model.Widening{Tier: model.TierFull, Reason: model.ReasonUserRule, Path: rules.FileName,
				Detail: fmt.Sprintf("%s: affects target %q matches no test unit", rt.reason, rt.target)})
			return s.finishFull()
		}
	}
	// "Whole project" unit (test file set unknown): any meaningful change may touch an unknown test of the project.
	if len(seeds) > 0 || len(s.sel.Widening) > 0 {
		for _, u := range units {
			if u.Whole {
				choose(u, model.ReasonConfigIsCode+": test file set of project "+scopeName(u.Project)+" is unknown (test pattern computed by code)", false)
			}
		}
	}
	// Previously failed tests.
	for _, id := range in.PreviouslyFailed {
		for _, u := range units {
			if u.ID == id {
				choose(u, "previously_failed", true)
				s.sel.AlwaysRun = append(s.sel.AlwaysRun, id)
			}
		}
	}
	// New tests.
	for _, c := range work {
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
		st := *chosen[id]
		// A new test file is always run (AlwaysRun): precise even if it was only reached through a widening.
		st.Widened = !precise[id] && !slices.Contains(s.sel.AlwaysRun, id)
		s.sel.Tests = append(s.sel.Tests, st)
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

// ruleTargetMatches: unit u is selected by the [[rule]] affects target t: its unit ID, a directory holding the unit's project,
// one of its files or its directory (TestUnit.Dir), or a glob matching one of its files.
func ruleTargetMatches(u model.TestUnit, t string) bool {
	if t == u.ID {
		return true
	}
	if strings.ContainsAny(t, "*?[{") {
		pat, err := glob.Compile(t)
		if err != nil {
			return false
		}
		return slices.ContainsFunc(u.Files, pat.Match)
	}
	d := path.Clean(t)
	return unitWithin(u, d) || (u.Dir != "" && eco.Within(u.Dir, d))
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

// linkSide holds a path's symbolic link in the base tree ([0]) and in the head tree ([1]); nil where it is not a link.
type linkSide [2]*tree.Link

type linkMap map[string]linkSide

// linkSides collects the symbolic links of both trees.
func linkSides(base, head tree.Tree) linkMap {
	m := linkMap{}
	for i, t := range []tree.Tree{base, head} {
		for _, l := range tree.LinksOf(t) {
			sd := m[l.Path]
			sd[i] = &l
			m[l.Path] = sd
		}
	}
	return m
}

// partial returns the directory links (of either tree) that are followed only partly: a cycle or an oversized expansion.
func (m linkMap) partial() []tree.Link {
	var out []tree.Link
	for _, sd := range m {
		for _, l := range sd {
			if l != nil && l.Real != "" && l.Problem != "" {
				out = append(out, *l)
			}
		}
	}
	slices.SortFunc(out, func(a, b tree.Link) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// linkTargetFiles returns the files of t the link resolves to: its target file, or every file under its target directory.
func linkTargetFiles(t tree.Tree, l tree.Link) []string {
	if l.Real == "" {
		return nil
	}
	if !l.Dir {
		return []string{l.Real}
	}
	var out []string
	for _, f := range t.Files() {
		if eco.Within(f, l.Real) {
			out = append(out, f)
		}
	}
	return out
}

// withAliases returns changed followed by the aliases of its files (paths that read a changed file through a symbolic link) that
// are not changed themselves, and alias → the changed file it reads.
func withAliases(changed []Changed, g *graph.Graph, base, head tree.Tree) ([]Changed, map[string]string) {
	work := slices.Clone(changed)
	aliasOf := map[string]string{}
	seen := map[string]bool{}
	for _, c := range changed {
		seen[c.Path] = true
	}
	for _, c := range changed {
		for _, a := range g.Aliases(c.Path) {
			if seen[a] {
				continue
			}
			seen[a] = true
			aliasOf[a] = c.Path
			st := byte('M')
			switch inB, inH := base.Has(a), head.Has(a); {
			case !inB:
				st = 'A'
			case !inH:
				st = 'D'
			}
			work = append(work, Changed{Path: a, Status: st, OldMode: c.OldMode, NewMode: c.NewMode})
		}
	}
	return work, aliasOf
}

// exclude drops the changed files matching in.Exclude that keepExcluded lets go from changed and from
// the selection's Changed list.
func (s *state) exclude(changed []Changed, g *graph.Graph, links linkMap, all []*eco.Analysis) []Changed {
	dropped := map[string]bool{}
	var kept []Changed
	for _, c := range changed {
		var pat *glob.Pattern
		for _, p := range s.in.Exclude {
			if p.Match(c.Path) {
				pat = p
				break
			}
		}
		if pat == nil {
			kept = append(kept, c)
			continue
		}
		if why := keepExcluded(c, g, links, all); why != "" {
			s.sel.Warnings = append(s.sel.Warnings, model.Widening{Tier: model.TierNone, Reason: model.ReasonRuleRejected, Path: c.Path,
				Detail: fmt.Sprintf("excluded by the caller (%s) but kept: %s", pat, why)})
			kept = append(kept, c)
			continue
		}
		dropped[c.Path] = true
	}
	if len(dropped) > 0 {
		s.sel.Changed = slices.DeleteFunc(s.sel.Changed, func(p string) bool { return dropped[p] })
	}
	return kept
}

// keepExcluded says why a changed file matching Input.Exclude must stay in the change set ("" when it may be dropped): something
// references it (an incoming edge: import, embed, include, a path literal...), it is code, a test, a manifest, a config, a
// lockfile or generated output in some ecosystem, or a symbolic link or submodule is involved. A plain data file is dropped even
// when a project, module or package owns it (an implicit edge from a virtual node, see Graph.Referenced): the caller declared it
// an input of no test.
func keepExcluded(c Changed, g *graph.Graph, links linkMap, all []*eco.Analysis) string {
	if _, isLink := links[c.Path]; isLink || len(g.Aliases(c.Path)) > 0 {
		return "a symbolic link is involved"
	}
	if c.OldMode == "160000" || c.NewMode == "160000" {
		return "a submodule"
	}
	if g.Referenced(c.Path) {
		return "other files reference it"
	}
	for _, a := range all {
		switch r := a.Roles[c.Path]; r {
		case "", model.RoleData, model.RoleUnknown:
		default:
			return fmt.Sprintf("its role is %s", r)
		}
	}
	return ""
}

package golang

import (
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/tree"
)

// harmlessVerbs: go.mod / go.work directives whose change cannot affect a build or a test run.
var harmlessVerbs = map[string]bool{"retract": true}

// ClassifyChange classifies go.mod, go.sum, go.work, vendored code, linter configuration, and .go files outside every module
// (docs/ecosystems/go.md; go.sum: lines only added are harmless, anything else is level L0).
func (e *Eco) ClassifyChange(p string, base, head tree.Tree, a *eco.Analysis) ([]model.Widening, bool) {
	if underNodeModules(p) {
		return nil, false
	}
	bm, hm := e.modulesOf(base), e.modulesOf(head)
	dir, b := path.Dir(p), path.Base(p)
	isMod := func(d string) bool { return bm.byDir[d] != nil || hm.byDir[d] != nil }
	switch {
	case b == "go.mod" && isMod(dir):
		return classifyGoMod(p, base, head, bm, hm), true
	case b == "go.sum" && isMod(dir):
		// go.sum only verifies what go.mod selects: lines only added change no build (level L1). A removed or changed line (a
		// build may now fail verification) widens like L0: its own module's build and, in workspace mode, every module of the
		// workspace read it.
		why := "go.sum lines removed or changed"
		switch {
		case e.rules.LockLevel() == rules.LockL0:
			why = "bgraph.toml [lockfile] level"
		default:
			if sumOnlyAdds(p, base, head) {
				return nil, true
			}
		}
		dirs, exact := moduleScope(dir, coMembers, bm, hm)
		return scoped(model.ReasonLockChanged, p, dir, "go.sum at level L0 ("+why+")", dirs, exact), true
	case (b == "go.work" || b == "go.work.sum") && moduleDirOK(dir):
		// Normally a global input (full) before ecosystems are asked; kept for callers that rule it otherwise.
		var out []model.Widening
		for _, d := range workspaceDirs(p, bm, hm) {
			out = append(out, model.Widening{Tier: model.TierModule, Reason: model.ReasonManifestChanged, Path: p, Scope: d, Eco: Name,
				Detail: "workspace file changed"})
		}
		return out, true
	case isLinterConfig(b):
		return nil, true
	}
	if m := vendorOwner(p, bm, hm); m != "" {
		// A module's vendor/ directory is read by that module's build only; a workspace's by every module of the workspace.
		dirs, exact := moduleScope(m, workspaceMembers, bm, hm)
		return scoped(model.ReasonLockChanged, p, m, "vendored dependency changed", dirs, exact), true
	}
	// A .go file under no go.mod in a repository with Go modules: a GOPATH-layout package beside them, not analyzed → full. In a
	// repository without any go.mod there are no Go units to affect: the file is left to the general rules (an unowned file).
	if strings.HasSuffix(p, ".go") && bm.innermost(dir) == nil && hm.innermost(dir) == nil && (len(bm.list) > 0 || len(hm.list) > 0) {
		return []model.Widening{{Tier: model.TierFull, Reason: model.ReasonUnknownFile, Path: p, Eco: Name,
			Detail: ".go file under no go.mod (GOPATH layout is not analyzed)"}}, true
	}
	return nil, false
}

// vendorOwner returns the module directory whose vendor/ directory contains p ("" when none).
func vendorOwner(p string, ms ...*modules) string {
	for d := path.Dir(p); d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if path.Base(d) != "vendor" {
			continue
		}
		md := path.Dir(d)
		for _, m := range ms {
			if m.byDir[md] != nil || m.workDirs[md] {
				return md
			}
		}
	}
	return ""
}

// scoped returns one module widening per directory of dirs (dir is the module of the changed file p). exact: each covers the units
// of that module only (Widening.Exact), not those of modules nested below it.
func scoped(reason, p, dir, detail string, dirs []string, exact bool) []model.Widening {
	var out []model.Widening
	for _, d := range dedupDirs(dirs) {
		w := model.Widening{Tier: model.TierModule, Reason: reason, Path: p, Scope: d, Eco: Name, Detail: detail, Exact: exact}
		if d != dir {
			w.Detail += " (module " + d + " builds with it)"
		}
		out = append(out, w)
	}
	return out
}

// moduleScope returns the modules whose build reads a changed input of module (or workspace) directory dir: dir itself and the
// modules related returns for it, each widened exactly (exact=true): a module nested below dir is a separate build and is not
// selected unless it is related. Fail-closed exceptions:
//
//   - a module nested below dir whose go.mod cannot be read on one side is included, with its own related modules (whether it
//     builds with dir, through a local replace for example, is unknown);
//   - when a go.work in dir or one of its ancestors cannot be read, workspace membership is unknown: the scope is dir's whole
//     sub-tree (exact=false), nested modules included.
func moduleScope(dir string, related func(string, ...*modules) []string, bm, hm *modules) (dirs []string, exact bool) {
	dirs = append([]string{dir}, related(dir, bm, hm)...)
	exact = true
	for _, ms := range []*modules{bm, hm} {
		for _, w := range ms.works {
			if w.mf == nil && eco.Within(dir, w.dir) {
				exact = false
			}
		}
		for _, m := range ms.list {
			if m.mf == nil && m.dir != dir && eco.Within(m.dir, dir) {
				dirs = append(dirs, m.dir)
				dirs = append(dirs, related(m.dir, bm, hm)...)
			}
		}
	}
	return dedupDirs(dirs), exact
}

// coMembers returns the other modules of every go.work that uses module dir (workspace mode reads every module's go.sum).
func coMembers(dir string, mss ...*modules) []string {
	var out []string
	for _, ms := range mss {
		for _, w := range ms.works {
			if slices.Contains(w.uses, dir) {
				out = append(out, w.uses...)
			}
		}
	}
	return out
}

// workspaceMembers returns the modules of the go.work files in directory dir (a workspace's vendor/ directory applies to them).
func workspaceMembers(dir string, mss ...*modules) []string {
	var out []string
	for _, ms := range mss {
		for _, w := range ms.works {
			if w.dir == dir {
				out = append(out, w.uses...)
			}
		}
	}
	return out
}

func classifyGoMod(p string, base, head tree.Tree, bm, hm *modules) []model.Widening {
	dir := path.Dir(p)
	widen := func(detail string, dirs ...string) []model.Widening {
		return scoped(model.ReasonManifestChanged, p, dir, detail, dirs, false)
	}
	reach := reachers(dir, bm, hm)
	om, nm := bm.byDir[dir], hm.byDir[dir]
	if om == nil || nm == nil {
		// Added or deleted: packages move between this module and the enclosing one.
		dirs := append([]string{dir}, reach...)
		for _, ms := range []*modules{bm, hm} {
			if m := ms.innermost(path.Dir(dir)); m != nil && dir != "." {
				dirs = append(dirs, m.dir)
				dirs = append(dirs, reachers(m.dir, bm, hm)...)
			}
		}
		return widen("go.mod added or deleted: module boundaries changed", dirs...)
	}
	if om.mf == nil || nm.mf == nil {
		return widen("go.mod unreadable on one side (L0)", append([]string{dir}, reach...)...)
	}
	var changed []string
	for _, v := range changedVerbs(om.mf, nm.mf) {
		if !harmlessVerbs[v] {
			changed = append(changed, v)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	detail := "directives changed: " + strings.Join(changed, ", ")
	if slices.Contains(changed, "module") {
		// Import paths of the whole repository may resolve differently.
		return widen(detail, ".")
	}
	// The module's own build and the builds of the modules that build with it (reachers: local replaces, transitively, and every
	// go.work using it, which also covers a go or toolchain change seen by the workspace). A nested module that only imports this
	// module's packages builds them from its own requirements, not from this go.mod; a change to those packages' files still
	// reaches it through the import edges.
	dirs, exact := moduleScope(dir, reachers, bm, hm)
	return scoped(model.ReasonManifestChanged, p, dir, detail, dirs, exact)
}

// reachers returns the module directories that build with module dir's go.mod: modules that replace a path with it (transitively)
// and the other modules of every go.work that uses it.
func reachers(dir string, mss ...*modules) []string {
	seen := map[string]bool{dir: true}
	queue := []string{dir}
	var out []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		var next []string
		for _, ms := range mss {
			for _, m := range ms.list {
				if m.mf == nil {
					continue
				}
				for _, r := range m.mf.LocalReplaces() {
					if d, ok := localDir(m.dir, r.dir); ok && d == cur {
						next = append(next, m.dir)
					}
				}
			}
			for _, w := range ms.works {
				if slices.Contains(w.uses, cur) {
					next = append(next, w.uses...)
				}
			}
		}
		for _, d := range next {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
				queue = append(queue, d)
			}
		}
	}
	slices.Sort(out)
	return out
}

// workspaceDirs: the modules a go.work (or go.work.sum) file applies to, in either tree; its own directory when none is known.
func workspaceDirs(p string, mss ...*modules) []string {
	dir := path.Dir(p)
	dirs := []string{dir}
	for _, ms := range mss {
		for _, w := range ms.works {
			if w.dir == dir {
				dirs = append(dirs, w.uses...)
			}
		}
	}
	return dedupDirs(dirs)
}

func dedupDirs(dirs []string) []string {
	out := slices.Clone(dirs)
	slices.Sort(out)
	return slices.Compact(out)
}

// sumOnlyAdds: go.sum p exists in both trees and every line of the base file is still in the head file (lines were only
// added, as `go mod tidy` or `go get` do for new requirements; the go.mod change, if any, is classified on its own).
func sumOnlyAdds(p string, base, head tree.Tree) bool {
	bb, err := base.Read(p)
	if err != nil {
		return false
	}
	hb, err := head.Read(p)
	if err != nil {
		return false
	}
	have := map[string]bool{}
	for _, l := range strings.Split(string(hb), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			have[l] = true
		}
	}
	for _, l := range strings.Split(string(bb), "\n") {
		if l = strings.TrimSpace(l); l != "" && !have[l] {
			return false
		}
	}
	return true
}

package golang

import (
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// harmlessVerbs: go.mod / go.work directives whose change cannot affect a build or a test run.
var harmlessVerbs = map[string]bool{"retract": true}

// ClassifyChange classifies go.mod, go.sum, go.work, vendored code, linter configuration, and .go files outside every module
// (plan §6.6, §6.8 level L0).
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
		return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonLockChanged, Path: p, Scope: dir, Eco: Name,
			Detail: "go.sum at level L0 (checksums not compared)"}}, true
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
		return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonLockChanged, Path: p, Scope: m, Eco: Name,
			Detail: "vendored dependency changed"}}, true
	}
	if strings.HasSuffix(p, ".go") && bm.innermost(dir) == nil && hm.innermost(dir) == nil {
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

func classifyGoMod(p string, base, head tree.Tree, bm, hm *modules) []model.Widening {
	dir := path.Dir(p)
	widen := func(detail string, dirs ...string) []model.Widening {
		var out []model.Widening
		for _, d := range dedupDirs(dirs) {
			w := model.Widening{Tier: model.TierModule, Reason: model.ReasonManifestChanged, Path: p, Scope: d, Eco: Name, Detail: detail}
			if d != dir {
				w.Detail += " (module " + d + " builds with it)"
			}
			out = append(out, w)
		}
		return out
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
	return widen(detail, append([]string{dir}, reach...)...)
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

package swift

import (
	"path"
	"slices"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

func (*Eco) ClassifyChange(f string, base, head tree.Tree, a *eco.Analysis) ([]model.Widening, bool) {
	dir := path.Dir(f)
	manifestFile := isManifest(f)
	lock := path.Base(f) == "Package.resolved"
	if !manifestFile && !lock && !possibleConfig(f) {
		if path.Ext(f) == ".swift" && a != nil && len(a.Projects) > 0 {
			if _, ok := eco.NearestProject(a.Projects, dir); !ok {
				return []model.Widening{{Path: f, Tier: model.TierFull, Reason: model.ReasonUnreferenced,
					Detail: "Swift source is outside every Swift package"}}, true
			}
		}
		return nil, false
	}

	before, after := loadProjects(base), loadProjects(head)
	old, cur := nearest(before, dir), nearest(after, dir)
	if manifestFile {
		old, cur = before[dir], after[dir]
	}
	if old == nil && cur == nil {
		return nil, false
	}

	reason, detail := model.ReasonTestConfig, "SwiftPM test or build configuration changed"
	switch {
	case manifestFile:
		reason, detail = model.ReasonManifestChanged, "SwiftPM manifest changed"
		if old != nil && cur != nil && old.opaque == "" && cur.opaque == "" && old.m != nil && cur.m != nil {
			if old.m.canonical == cur.m.canonical {
				return nil, true
			}
			if old.m.nonTest == cur.m.nonTest {
				reason, detail = model.ReasonTestConfig, "SwiftPM test target configuration changed"
			}
		}
		for _, p := range []*project{cur, old} {
			if p != nil && p.opaque != "" {
				reason, detail = model.ReasonConfigIsCode, p.opaque
				break
			}
		}
	case lock:
		// Package.resolved belongs to the package whose resolver writes it.
		// Also accept lockfiles below .swiftpm (Xcode workspace resolution).
		if (old == nil || !packageLock(f, old.dir)) && (cur == nil || !packageLock(f, cur.dir)) {
			return nil, false
		}
		reason, detail = model.ReasonLockChanged, "SwiftPM lockfile at level L0"
	default:
		if (old == nil || !swiftSpecial(f, old.dir)) && (cur == nil || !swiftSpecial(f, cur.dir)) {
			return nil, false
		}
	}

	dirs := map[string]bool{}
	for _, p := range []*project{old, cur} {
		if p != nil {
			dirs[p.dir] = true
		}
	}
	if manifestFile && (old == nil || cur == nil) {
		// Adding or deleting a nested manifest changes the parent package's
		// ownership of the subtree, even though the nested package is separate.
		for _, side := range []map[string]*project{before, after} {
			if up := nearest(side, path.Dir(dir)); dir != "." && up != nil {
				dirs[up.dir] = true
			}
		}
	}

	for changed := true; changed; {
		changed = false
		for _, side := range []map[string]*project{before, after} {
			for d, p := range side {
				if dirs[d] {
					continue
				}
				for target := range dirs {
					if p.opaque != "" || slices.Contains(p.deps, target) || matchesMissing(target, p.missing) {
						dirs[d], changed = true, true
						break
					}
				}
			}
		}
	}

	scopes := make([]string, 0, len(dirs))
	for d := range dirs {
		scopes = append(scopes, d)
	}
	slices.Sort(scopes)
	var ws []model.Widening
	for _, d := range scopes {
		ws = append(ws, model.Widening{Tier: model.TierModule, Reason: reason, Path: f, Scope: d, Eco: Runner,
			Exact: true, Detail: detail, Seeds: []string{sharedNode(d)}})
	}
	return ws, true
}

func possibleConfig(f string) bool {
	return path.Ext(f) == ".xctestplan" || hasComponent(f, ".swiftpm")
}

func hasComponent(f, component string) bool {
	for ; f != "."; f = path.Dir(f) {
		if path.Base(f) == component {
			return true
		}
	}
	return false
}

func packageLock(f, dir string) bool {
	return path.Dir(f) == dir || eco.Within(f, path.Join(dir, ".swiftpm"))
}

func matchesMissing(dir string, missing []string) bool {
	for _, m := range missing {
		if eco.Within(dir, m) {
			return true
		}
	}
	return false
}

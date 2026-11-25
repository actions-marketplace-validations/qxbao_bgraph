package dart

import (
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// ClassifyChange claims Dart manifests, lockfiles and configuration (docs/ecosystems/dart.md, "Changes to manifests,
// configuration and lockfiles"). Dart sources are not claimed: they are graph nodes.
func (*Eco) ClassifyChange(f string, base, head tree.Tree, _ *eco.Analysis) ([]model.Widening, bool) {
	name := path.Base(f)
	switch {
	case name == "pubspec.yaml", name == "pubspec_overrides.yaml", name == "pubspec.lock", name == "dart_test.yaml",
		name == "l10n.yaml", isBuildYAML(name), name == "analysis_options.yaml", name == "melos.yaml":
	default:
		return nil, false
	}
	rb, rh := loadRepo(base), loadRepo(head)
	if rb == nil && rh == nil {
		return nil, false
	}
	sides := []*repo{rb, rh}
	dir := path.Dir(f)
	switch name {
	case "analysis_options.yaml":
		// Read by the analyzer only; the compiler and the test runners ignore it.
		return nil, true
	case "melos.yaml":
		// melos bootstrap links the packages it covers (any package below the file).
		return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonTestConfig, Path: f, Scope: dir, Eco: Name,
			Detail: "melos configuration changed: package linking and scripts of every package below it"}}, true
	}
	var old, cur *pkg
	if rb != nil {
		old = rb.pkgs[dir]
	}
	if rh != nil {
		cur = rh.pkgs[dir]
	}
	if old == nil && cur == nil {
		return nil, false // not at a package root: an ordinary file
	}
	s := scopes{sides: sides, exact: map[string]bool{}, tree: map[string]bool{}}
	reason, detail := model.ReasonTestConfig, ""
	switch {
	case name == "pubspec.yaml":
		reason, detail = model.ReasonManifestChanged, "pubspec.yaml changed"
		if old != nil && cur != nil && old.raw != nil && cur.raw != nil {
			keys := changedKeys(old.raw, cur.raw)
			var relevant []string
			for _, k := range keys {
				if !harmlessKeys[k] {
					relevant = append(relevant, k)
				}
			}
			if len(relevant) == 0 {
				return nil, true
			}
			detail = "pubspec.yaml changed: " + strings.Join(relevant, ", ")
		}
		s.add(dir)
		s.resolution(dir)
		s.dependents()
		for _, p := range []*pkg{old, cur} {
			if p != nil && p.raw == nil {
				// Unreadable: nothing is known about its workspace; every package below it may be a member.
				s.tree[dir] = true
				detail += " (" + p.err + ")"
			}
		}
		if old == nil || cur == nil {
			// A package appeared or disappeared: the enclosing package's file set changes too.
			for _, r := range sides {
				if up := r.nearest(path.Dir(dir)); dir != "." && up != nil {
					s.add(up.dir)
				}
			}
		}
	case name == "pubspec_overrides.yaml":
		reason, detail = model.ReasonManifestChanged, "pubspec_overrides.yaml changed"
		s.add(dir)
		s.resolution(dir)
		s.dependents()
	case name == "pubspec.lock":
		reason, detail = model.ReasonLockChanged, "pubspec.lock at level L0"
		s.add(dir)
		for _, r := range sides {
			if r != nil {
				s.addAll(r.allMembers(dir))
				if p := r.pkgs[dir]; p != nil && p.raw == nil {
					s.tree[dir] = true // an unreadable root may have members below it
				}
			}
		}
	case isBuildYAML(name):
		// Builders a package defines can apply to its dependents (auto_apply: dependents).
		detail = name + " changed: build_runner code generation"
		s.add(dir)
		s.dependents()
	default: // dart_test.yaml, l10n.yaml
		detail = name + " changed"
		s.add(dir)
	}
	var ws []model.Widening
	for _, d := range slices.Sorted(maps.Keys(s.tree)) {
		ws = append(ws, model.Widening{Tier: model.TierModule, Reason: reason, Path: f, Scope: d, Eco: Name, Detail: detail})
	}
	for _, d := range slices.Sorted(maps.Keys(s.exact)) {
		if !s.tree[d] {
			ws = append(ws, model.Widening{Tier: model.TierModule, Reason: reason, Path: f, Scope: d, Eco: Name, Exact: true, Detail: detail})
		}
	}
	return ws, true
}

// scopes collects the package directories a change widens to: exact (that package's own units) or tree (every package below).
type scopes struct {
	sides []*repo
	exact map[string]bool
	tree  map[string]bool
}

func (s *scopes) add(d string) { s.exact[d] = true }

func (s *scopes) addAll(ds []string) {
	for _, d := range ds {
		s.exact[d] = true
	}
}

// resolution adds the packages that share a resolution with the package at dir: the members of a workspace rooted there,
// and for a member, its workspace root and every other member (one pubspec.lock, one version solve).
func (s *scopes) resolution(dir string) {
	for _, r := range s.sides {
		if r == nil {
			continue
		}
		root := r.resolutionRoot(dir)
		s.add(root)
		s.addAll(r.allMembers(root))
		s.addAll(r.allMembers(dir))
		if p := r.pkgs[root]; p != nil && p.member {
			// A member whose workspace root is unknown (the root's pubspec does not parse or does not list it): every
			// package below the nearest enclosing package may share its resolution.
			s.tree[r.enclosing(root)] = true
		}
		if p := r.pkgs[root]; p != nil && p.raw == nil {
			s.tree[root] = true // an unreadable root may have members below it
		}
	}
}

// dependents adds, transitively, every package that depends on one already in the set (by path or by name, in either
// tree), together with the packages sharing its resolution.
func (s *scopes) dependents() {
	for changed := true; changed; {
		changed = false
		names := map[string]bool{}
		for _, r := range s.sides {
			if r == nil {
				continue
			}
			for d := range s.exact {
				if p := r.pkgs[d]; p != nil && p.name != "" {
					names[p.name] = true
				}
			}
		}
		for _, r := range s.sides {
			if r == nil {
				continue
			}
			for _, d := range slices.Sorted(maps.Keys(r.pkgs)) {
				if s.exact[d] {
					continue
				}
				if dependsOn(r.pkgs[d], s.exact, names) {
					s.add(d)
					s.resolution(d)
					changed = true
				}
			}
		}
	}
}

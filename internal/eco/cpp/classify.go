package cpp

import (
	"path"
	"reflect"
	"slices"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

func configSystem(f string) string {
	switch path.Base(f) {
	case "meson_options.txt", "meson.options":
		return Meson
	case "CMakePresets.json", "CMakeUserPresets.json":
		return CMake
	}
	switch path.Ext(f) {
	case ".cmake":
		return CMake
	case ".wrap":
		return Meson
	}
	return ""
}

func (*Eco) ClassifyChange(f string, base, head tree.Tree, _ *eco.Analysis) ([]model.Widening, bool) {
	if reason := dependencyManagerReason(f); reason != "" {
		return classifyDependencyManager(f, reason, base, head)
	}
	system := manifestSystem(f)
	manifest := system != ""
	if !manifest {
		system = configSystem(f)
		if system == "" {
			return nil, false
		}
	}
	before, after := discover(base), discover(head)
	affected := map[string][2]*project{}
	for i, projects := range [][]*project{before, after} {
		for _, p := range projects {
			if p.system != system || !eco.Within(f, p.dir) {
				continue
			}
			pair := affected[p.dir]
			pair[i] = p
			affected[p.dir] = pair
		}
	}
	if len(affected) == 0 {
		return nil, false
	}
	if manifest && harmlessManifest(affected, path.Dir(f)) {
		return nil, true
	}
	dirs := make([]string, 0, len(affected))
	for dir := range affected {
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)
	var ws []model.Widening
	for _, d := range dirs {
		reason, detail := model.ReasonTestConfig, "C/C++ build or test configuration changed"
		if manifest {
			reason, detail = model.ReasonManifestChanged, "C/C++ build declarations changed"
		}
		for _, p := range affected[d] {
			if p != nil && p.opaque != "" {
				reason, detail = model.ReasonConfigIsCode, p.opaque
			}
		}
		p := project{system: system, dir: d}
		ws = append(ws, model.Widening{Tier: model.TierGraph, Reason: reason, Path: f, Scope: d, Eco: Name,
			Exact: true, Detail: detail, Seeds: []string{p.node("inputs", "")}})
	}
	return ws, true
}

func dependencyManagerReason(f string) string {
	switch path.Base(f) {
	case "conan.lock":
		return model.ReasonLockChanged
	case "conanfile.txt", "conanfile.py", "vcpkg.json", "vcpkg-configuration.json":
		return model.ReasonManifestChanged
	}
	return ""
}

func classifyDependencyManager(f, reason string, base, head tree.Tree) ([]model.Widening, bool) {
	dir := path.Dir(f)
	scopes := map[string]bool{}
	for _, side := range []tree.Tree{base, head} {
		for _, p := range discover(side) {
			if eco.Within(p.dir, dir) || eco.Within(f, p.dir) {
				scopes[p.dir] = true
			}
		}
	}
	dirs := make([]string, 0, len(scopes))
	for d := range scopes {
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)
	var result []model.Widening
	for _, d := range dirs {
		result = append(result, model.Widening{
			Path: f, Tier: model.TierModule, Reason: reason, Scope: d, Exact: true, Eco: Name,
			Detail: "C/C++ package manager input changed",
		})
	}
	return result, len(result) > 0
}

func harmlessManifest(affected map[string][2]*project, dir string) bool {
	visited := false
	for _, pair := range affected {
		a, b := pair[0], pair[1]
		if a == nil || b == nil || a.opaque != "" || b.opaque != "" || a.visited[dir] != b.visited[dir] {
			return false
		}
		if a.visited[dir] {
			visited = true
			if !equivalent(a, b, dir) {
				return false
			}
		}
	}
	return visited
}

func equivalent(a, b *project, dir string) bool {
	ac := slices.Clone(a.scripts[dir].calls)
	bc := slices.Clone(b.scripts[dir].calls)
	for i := range ac {
		ac[i].Line = 0
	}
	for i := range bc {
		bc[i].Line = 0
	}
	return reflect.DeepEqual(ac, bc)
}

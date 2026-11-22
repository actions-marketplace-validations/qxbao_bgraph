package node

import (
	"bytes"
	"encoding/json"
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// harmlessPkgKeys are top-level package.json keys whose change is provably harmless to tests.
var harmlessPkgKeys = map[string]bool{"version": true, "description": true, "keywords": true, "author": true, "contributors": true,
	"homepage": true, "repository": true, "bugs": true, "license": true, "funding": true, "maintainers": true, "publishConfig": true}

// ClassifyChange classifies Node manifests/configs/lockfiles (docs/ecosystems/node.md; lockfile levels in docs/lockfiles.md, see classifyLock).
func (e *Eco) ClassifyChange(p string, base, head tree.Tree, a *eco.Analysis) ([]model.Widening, bool) {
	if inNodeModules(p) {
		return nil, false
	}
	var projects []eco.Project
	if a != nil {
		projects = a.Projects
	}
	scopeOf := func(p string) string {
		if pr, ok := eco.NearestProject(projects, p); ok {
			return pr.Dir
		}
		return path.Dir(p)
	}
	base1 := path.Base(p)
	switch {
	case base1 == "package.json":
		dir := path.Dir(p)
		if keys, ok := changedTopKeys(base, head, p); ok {
			if len(keys) == 0 {
				return nil, true
			}
			only := true
			for _, k := range keys {
				if !harmlessPkgKeys[k] {
					only = false
				}
			}
			if only {
				return nil, true
			}
			return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonManifestChanged, Path: p, Scope: dir,
				Detail: "keys changed: " + strings.Join(keys, ", ")}}, true
		}
		return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonManifestChanged, Path: p, Scope: dir}}, true
	case isLockfile(p):
		return e.classifyLock(p, base, head, a), true
	case isTSConfig(p):
		dirs := []string{path.Dir(p)}
		dirs = append(dirs, tsconfigExtenders(base, p)...)
		dirs = append(dirs, tsconfigExtenders(head, p)...)
		slices.Sort(dirs)
		dirs = slices.Compact(dirs)
		var ws []model.Widening
		for _, d := range dirs {
			ws = append(ws, model.Widening{Tier: model.TierModule, Reason: model.ReasonTestConfig, Path: p, Scope: d, Detail: "tsconfig/jsconfig changed"})
		}
		return ws, true
	case configKind(p) != "":
		return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonTestConfig, Path: p, Scope: scopeOf(p),
			Detail: configKind(p) + " config changed"}}, true
	}
	return nil, false
}

// changedTopKeys returns the top-level keys that differ between two JSON versions; ok=false if one side is unreadable
// (added/deleted files also return ok=false → widen).
func changedTopKeys(base, head tree.Tree, p string) ([]string, bool) {
	if base == nil || head == nil {
		return nil, false
	}
	ob, err1 := base.Read(p)
	nb, err2 := head.Read(p)
	if err1 != nil || err2 != nil {
		return nil, false
	}
	var o, n map[string]json.RawMessage
	if json.Unmarshal(ob, &o) != nil || json.Unmarshal(nb, &n) != nil {
		return nil, false
	}
	var keys []string
	for k, v := range o {
		if w, ok := n[k]; !ok || !jsonEqual(v, w) {
			keys = append(keys, k)
		}
	}
	for k := range n {
		if _, ok := o[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys, true
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}

// tsconfigExtenders returns the directory of every tsconfig/jsconfig in t whose `extends` (transitively) points to target.
func tsconfigExtenders(t tree.Tree, target string) []string {
	if t == nil {
		return nil
	}
	var cfgs []string
	for _, f := range t.Files() {
		if isTSConfig(f) && !inNodeModules(f) {
			cfgs = append(cfgs, f)
		}
	}
	parents := map[string][]string{} // cfg → files it extends (repo-relative paths only)
	for _, c := range cfgs {
		b, err := t.Read(c)
		if err != nil {
			continue
		}
		for _, ext := range extendsOf(b) {
			if !strings.HasPrefix(ext, "./") && !strings.HasPrefix(ext, "../") {
				// extends to a package: hard to know if it is a workspace; superset: treat it as a possible target when the name matches the suffix
				if strings.HasSuffix(target, strings.TrimPrefix(ext, "@")) {
					parents[c] = append(parents[c], target)
				}
				continue
			}
			pth := path.Join(path.Dir(c), ext)
			if !strings.HasSuffix(pth, ".json") && !t.Has(pth) {
				pth += ".json"
			}
			parents[c] = append(parents[c], pth)
		}
	}
	affected := map[string]bool{target: true}
	for changed := true; changed; {
		changed = false
		for c, ps := range parents {
			if affected[c] {
				continue
			}
			for _, p := range ps {
				if affected[p] {
					affected[c] = true
					changed = true
					break
				}
			}
		}
	}
	var dirs []string
	for c := range affected {
		if c != target {
			dirs = append(dirs, path.Dir(c))
		}
	}
	slices.Sort(dirs)
	return dirs
}

// extendsOf reads the extends field (string or array) of a JSONC tsconfig; error → nil.
func extendsOf(b []byte) []string {
	s := stripJSComments(string(b))
	idx := keyIndexes(s, "extends")
	var out []string
	for _, i := range idx {
		if v, ok := literalValue(s[i:]); ok {
			out = append(out, v)
		} else if arr, ok := stringArray(s[i:]); ok {
			out = append(out, arr...)
		}
	}
	return out
}

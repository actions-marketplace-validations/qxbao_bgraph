package rust

import (
	"path"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/qxbao/bgraph/internal/classify"
	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// harmlessPackageKeys are [package] (and [workspace.package]) keys whose change cannot affect a test run.
var harmlessPackageKeys = map[string]bool{"description": true, "keywords": true, "categories": true, "homepage": true, "repository": true,
	"documentation": true, "authors": true, "license": true, "license-file": true, "readme": true, "publish": true, "exclude": true,
	"include": true, "metadata": true, "default-run": true}

type keyLevel int

const (
	levelNone keyLevel = iota
	levelPackage
	levelWorkspace
)

// keyLevelOf classifies a changed manifest key ("package.description", "dependencies", "workspace.members"...).
func keyLevelOf(key string) keyLevel {
	top, sub, _ := strings.Cut(key, ".")
	switch top {
	case "package", "project":
		switch {
		case harmlessPackageKeys[sub]:
			return levelNone
		case sub == "resolver":
			return levelWorkspace // feature resolution is decided at the workspace root
		}
		return levelPackage // name, version, edition, build, links, auto*, rust-version, workspace...
	case "workspace":
		sub0, rest, _ := strings.Cut(sub, ".")
		switch {
		case sub0 == "metadata" || sub0 == "default-members":
			return levelNone
		case sub0 == "package" && rest != "" && harmlessPackageKeys[rest]:
			return levelNone
		}
		return levelWorkspace
	case "badges":
		return levelNone
	case "lib", "bin", "test", "bench", "example", "lints":
		return levelPackage
	}
	// dependencies, dev-/build-dependencies, target.*, features, patch, replace, profile, cargo-features and anything unknown:
	// dependency/feature unification and build settings are workspace-wide.
	return levelWorkspace
}

// changedKeys returns the manifest keys that differ, descending one level into [package] and [workspace] (and [workspace.package]).
func changedKeys(ob, nb map[string]any) []string {
	tops := map[string]bool{}
	for k := range ob {
		tops[k] = true
	}
	for k := range nb {
		tops[k] = true
	}
	var keys []string
	for k := range tops {
		if reflect.DeepEqual(ob[k], nb[k]) {
			continue
		}
		if k == "package" || k == "project" || k == "workspace" {
			if ot, nt := tbl(ob[k]), tbl(nb[k]); ot != nil && nt != nil {
				subs := map[string]bool{}
				for s := range ot {
					subs[s] = true
				}
				for s := range nt {
					subs[s] = true
				}
				for s := range subs {
					if reflect.DeepEqual(ot[s], nt[s]) {
						continue
					}
					if k == "workspace" && s == "package" && tbl(ot[s]) != nil && tbl(nt[s]) != nil {
						inner := map[string]bool{}
						for x := range tbl(ot[s]) {
							inner[x] = true
						}
						for x := range tbl(nt[s]) {
							inner[x] = true
						}
						for x := range inner {
							if !reflect.DeepEqual(tbl(ot[s])[x], tbl(nt[s])[x]) {
								keys = append(keys, "workspace.package."+x)
							}
						}
						continue
					}
					keys = append(keys, k+"."+s)
				}
				continue
			}
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func isHarmlessConfig(base string) bool {
	switch base {
	case "clippy.toml", ".clippy.toml", "rustfmt.toml", ".rustfmt.toml", "deny.toml":
		return true
	}
	return false
}

func isDocOrHarmless(p string) bool { return classify.Doc(p) || classify.Harmless(p) }

// isCargoConfig: .cargo/config or .cargo/config.toml.
func isCargoConfig(p string) bool {
	b := path.Base(p)
	return (b == "config" || b == "config.toml") && path.Base(path.Dir(p)) == ".cargo"
}

// ClassifyChange classifies Cargo manifests, Cargo.lock and cargo/tool configuration (plan §6.3, §6.8 level L0).
func (e *Eco) ClassifyChange(p string, base, head tree.Tree, a *eco.Analysis) ([]model.Widening, bool) {
	b := path.Base(p)
	switch {
	case b == "Cargo.toml":
		if underTarget(p, head, base) {
			return nil, false
		}
		return classifyManifest(p, base, head, a), true
	case b == "Cargo.lock":
		if underTarget(p, head, base) {
			return nil, false
		}
		dir := path.Dir(p)
		return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonLockChanged, Path: p, Scope: dir, Eco: Name,
			Detail: "lockfile at level L0 (closure not compared)"}}, true
	case isCargoConfig(p):
		dir := path.Dir(path.Dir(p))
		if dir == "." {
			return []model.Widening{{Tier: model.TierFull, Reason: model.ReasonGlobalInput, Path: p,
				Detail: "cargo config at the repo root"}}, true
		}
		return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonTestConfig, Path: p, Scope: dir, Eco: Name,
			Detail: "cargo config changed"}}, true
	case isHarmlessConfig(b):
		return nil, true
	}
	return nil, false
}

// underTarget: p lies below a target/ directory that sits next to a Cargo.toml (cargo's build output, not a project).
func underTarget(p string, ts ...tree.Tree) bool {
	for d := path.Dir(p); d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if path.Base(d) != "target" {
			continue
		}
		for _, t := range ts {
			if t != nil && t.Has(path.Join(path.Dir(d), "Cargo.toml")) {
				return true
			}
		}
	}
	return false
}

func readManifest(t tree.Tree, p string) (map[string]any, bool) {
	if t == nil || !t.Has(p) {
		return nil, false
	}
	bs, err := t.Read(p)
	if err != nil {
		return nil, false
	}
	var doc map[string]any
	if toml.Unmarshal(bs, &doc) != nil {
		return nil, false
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, true
}

// workspaceRoot returns the nearest ancestor-or-self directory of dir whose Cargo.toml has a [workspace] table ("" if none).
func workspaceRoot(t tree.Tree, dir string) string {
	for d := dir; ; d = path.Dir(d) {
		if doc, ok := readManifest(t, path.Join(d, "Cargo.toml")); ok && tbl(doc["workspace"]) != nil {
			return d
		}
		if d == "." || d == "/" || d == "" {
			return ""
		}
	}
}

func classifyManifest(p string, base, head tree.Tree, a *eco.Analysis) []model.Widening {
	dir := path.Dir(p)
	wsWiden := func(detail string) model.Widening {
		root := ""
		if head != nil {
			root = workspaceRoot(head, dir)
		}
		if root == "" && base != nil {
			root = workspaceRoot(base, dir)
		}
		if root == "" {
			root = dir
		}
		return model.Widening{Tier: model.TierModule, Reason: model.ReasonManifestChanged, Path: p, Scope: root, Eco: Name, Detail: detail}
	}
	ob, ok1 := readManifest(base, p)
	nb, ok2 := readManifest(head, p)
	if !ok1 || !ok2 {
		return []model.Widening{wsWiden("Cargo.toml added, deleted or unreadable on one side: workspace membership and dependencies may have changed")}
	}
	var pkgKeys, wsKeys []string
	for _, k := range changedKeys(ob, nb) {
		switch keyLevelOf(k) {
		case levelPackage:
			pkgKeys = append(pkgKeys, k)
		case levelWorkspace:
			wsKeys = append(wsKeys, k)
		}
	}
	var out []model.Widening
	isPkg := false
	var projects []eco.Project
	if a != nil {
		projects = a.Projects
	}
	for _, pr := range projects {
		if pr.Eco == Name && pr.Dir == dir {
			isPkg = true
		}
	}
	if len(pkgKeys) > 0 && !isPkg {
		wsKeys = append(wsKeys, pkgKeys...) // a manifest without a package of its own (virtual workspace): the whole workspace
		pkgKeys = nil
	}
	if len(wsKeys) > 0 {
		out = append(out, wsWiden("workspace-wide keys changed (dependency/feature unification): "+strings.Join(wsKeys, ", ")))
	}
	if len(pkgKeys) > 0 {
		dirs := append([]string{dir}, reverseDeps(a, dir)...)
		for _, d := range dirs {
			detail := "keys changed: " + strings.Join(pkgKeys, ", ")
			if d != dir {
				detail += " (reverse dependency of " + dir + ")"
			}
			out = append(out, model.Widening{Tier: model.TierModule, Reason: model.ReasonManifestChanged, Path: p, Scope: d, Eco: Name, Detail: detail})
		}
	}
	return out
}

// nodeDir extracts the package directory of a virtual node "<rust:DIR::...>".
func nodeDir(n string) (string, bool) {
	rest, ok := strings.CutPrefix(n, "<rust:")
	if !ok {
		return "", false
	}
	d, _, ok := strings.Cut(rest, "::")
	return d, ok
}

// reverseDeps returns the directories of the packages that (transitively) depend on package dir, from the dependency edges of a.
func reverseDeps(a *eco.Analysis, dir string) []string {
	if a == nil {
		return nil
	}
	rev := map[string][]string{}
	for _, e := range a.Edges {
		if !strings.HasPrefix(e.Spec, "dependency ") {
			continue
		}
		f, ok1 := nodeDir(e.From)
		t, ok2 := nodeDir(e.To)
		if ok1 && ok2 && f != t {
			rev[t] = append(rev[t], f)
		}
	}
	seen := map[string]bool{dir: true}
	queue := []string{dir}
	var out []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, f := range rev[cur] {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
				queue = append(queue, f)
			}
		}
	}
	slices.Sort(out)
	return out
}

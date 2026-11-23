package node

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/lockfile"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/tree"
)

// l1Formats are the lockfiles whose closure passed the differential check against the package manager (docs/lockfiles.md);
// every other lockfile stays at level L0.
var l1Formats = map[string]bool{"pnpm-lock.yaml": true, "package-lock.json": true, "yarn.lock": true, "bun.lock": true}

// pkgNode is the virtual node of an external package, the target of every import that resolves to it (level L2 only).
func pkgNode(name string) string { return "<npm:" + name + ">" }

func (e *Eco) lockLevel() string {
	if e.Rules == nil {
		return rules.LockL1
	}
	return e.Rules.LockLevel()
}

// toolchainPrefixes / toolchainNames: packages that run, transform or type-check tests without being imported by them
// (runners, transpilers, bundlers, their plugins and presets, type packages). A change in one cannot be narrowed to its
// importers (level L2 falls back to L1).
var (
	toolchainNames = map[string]bool{"jest": true, "vitest": true, "mocha": true, "ava": true, "tap": true, "uvu": true,
		"playwright": true, "puppeteer": true, "cypress": true, "typescript": true, "ts-node": true, "ts-jest": true, "tsx": true,
		"esbuild": true, "esbuild-register": true, "vite": true, "webpack": true, "rollup": true, "rolldown": true, "parcel": true,
		"babel-jest": true, "jsdom": true, "happy-dom": true, "c8": true, "nyc": true, "source-map-support": true, "tslib": true,
		"cross-env": true, "dotenv": true, "node-gyp": true, "karma": true, "jasmine": true, "chai": true, "sinon": true,
		"turbo": true, "nx": true, "lerna": true, "sucrase": true, "unplugin": true, "core-js": true, "regenerator-runtime": true}
	toolchainPrefixes = []string{"@types/", "@jest/", "jest-", "@vitest/", "vitest-", "@babel/", "babel-", "@swc/", "swc-",
		"@playwright/", "@vitejs/", "vite-", "@rollup/", "rollup-", "@esbuild", "esbuild-", "eslint", "@eslint", "@typescript-eslint/",
		"prettier", "@testing-library/", "mocha-", "karma-", "webpack-", "@webpack", "ts-", "@sveltejs/vite", "@vue/compiler",
		"@vue/test-utils", "@storybook/", "storybook", "@nx/", "@turbo/", "postcss", "tailwindcss", "@tailwindcss/", "@cypress/"}
)

func isToolchain(name string) bool {
	if toolchainNames[name] {
		return true
	}
	for _, p := range toolchainPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return strings.Contains(name, "-plugin-") || strings.Contains(name, "-preset-") || strings.HasSuffix(name, "-plugin") ||
		strings.HasSuffix(name, "-preset") || strings.Contains(name, "/plugin-") || strings.Contains(name, "/preset-") ||
		strings.Contains(name, "-loader")
}

// packageOf returns the package name of a bare specifier ("@a/b/c" → "@a/b", "x/y" → "x").
func packageOf(spec string) string {
	parts := strings.SplitN(spec, "/", 3)
	if strings.HasPrefix(spec, "@") && len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

func readLock(t tree.Tree, p string) (*lockfile.Graph, error) {
	if t == nil || !t.Has(p) {
		return nil, fmt.Errorf("lockfile absent")
	}
	b, err := t.Read(p)
	if err != nil {
		return nil, err
	}
	// yarn v1 reads the workspace manifests, yarn Berry the root manifest's resolutions and .yarnrc.yml catalogs, from
	// the same tree.
	return lockfile.ParseFiles(p, path.Dir(p), b, t)
}

// classifyLock compares the resolved dependency closure of every project of the lockfile p (docs/lockfiles.md).
//
// L1: a project whose closure changed → every test of the project, and every file of the project counts as changed so
// that the tests of other projects importing it are selected too (dependents through workspace links are already in
// the lockfile closure). The lockfile's root project holds the workspace tooling: when its closure changes, every project
// under the lockfile's directory is selected (its scope contains them). A project of the tree that is not a lockfile
// project belongs to the nearest lockfile project containing it (its dependencies resolve from there).
//
// L2 (opt-in): within such a project, only the importers of the direct dependencies whose closure changed, unless a guard
// falls back to L1 (local/workspace dependency, install scripts, peer dependencies, toolchain or @types packages, a
// dependency no file of the project imports, an undeclared import of a changed package).
func (e *Eco) classifyLock(p string, base, head tree.Tree, a *eco.Analysis) []model.Widening {
	dir := path.Dir(p)
	l0 := func(detail string) []model.Widening {
		return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonLockChanged, Path: p, Scope: dir, Eco: Name,
			Detail: "lockfile at level L0 (" + detail + ")"}}
	}
	level := e.lockLevel()
	switch {
	case level == rules.LockL0:
		return l0("bgraph.toml [lockfile] level")
	case path.Base(p) == "bun.lockb":
		return l0("the binary bun.lockb is not read")
	case !l1Formats[path.Base(p)]:
		return l0("closure not compared for this format")
	}
	bg, err := readLock(base, p)
	if err != nil {
		return l0("base: " + err.Error())
	}
	hg, err := readLock(head, p)
	if err != nil {
		return l0("head: " + err.Error())
	}
	d, err := lockfile.Compare(bg, hg)
	if err != nil {
		return l0(err.Error())
	}
	var projects []eco.Project
	var edges []model.Edge
	var unres []model.Unresolved
	var roles map[string]model.Role
	if a != nil {
		projects, edges, unres, roles = a.Projects, a.Edges, a.Unresolved, a.Roles
	}
	mentions := configMentions(head, roles, dir)
	// Projects whose dependency resolution changed in a way level L2 cannot narrow.
	widen := map[string]string{} // project dir → detail
	narrow := map[string][]string{}
	for _, pr := range d.ChangedProjects() {
		if !eco.Within(pr, dir) {
			continue
		}
		detail := "dependency closure changed (L1)"
		if pr == dir {
			detail = "dependency closure of the lockfile's root project changed: workspace tooling (L1)"
		}
		if level == rules.LockL2 {
			seeds, why := l2Seeds(d, pr, edges, mentions)
			if why == "" {
				narrow[pr] = seeds
				continue
			}
			detail += "; L2 not applicable: " + why
		}
		widen[pr] = detail
	}
	// An import of a package the project does not declare resolves to whatever is hoisted: a changed package with that name
	// affects the importing project.
	for _, u := range unres {
		if !strings.HasPrefix(u.Detail, "undeclared package") || !eco.Within(u.File, dir) || !isBareSpec(u.Spec) {
			continue
		}
		name := packageOf(u.Spec)
		if !d.Names[name] {
			continue
		}
		pd := dir
		if pr, ok := eco.NearestProject(projects, u.File); ok && eco.Within(pr.Dir, dir) {
			pd = pr.Dir
		}
		if _, ok := widen[pd]; !ok {
			widen[pd] = fmt.Sprintf("undeclared import of changed package %q in %s (L1)", name, u.File)
		}
	}
	var out []model.Widening
	dirs := make([]string, 0, len(widen))
	for pr := range widen {
		dirs = append(dirs, pr)
	}
	slices.Sort(dirs)
	names := d.ChangedNames()
	if len(names) > 8 {
		names = append(names[:8], "...")
	}
	changedNames := strings.Join(names, ", ")
	for _, pr := range dirs {
		w := model.Widening{Tier: model.TierModule, Reason: model.ReasonLockChanged, Path: p, Scope: pr, Eco: Name,
			Detail: widen[pr] + "; changed packages: " + changedNames}
		for f, r := range roles {
			if (r == model.RoleSource || r == model.RoleTest || r == model.RoleConfig || r == model.RoleData) && eco.Within(f, pr) && !inNodeModules(f) {
				w.Seeds = append(w.Seeds, f)
			}
		}
		slices.Sort(w.Seeds)
		out = append(out, w)
	}
	nd := make([]string, 0, len(narrow))
	for pr := range narrow {
		if _, ok := widen[pr]; !ok {
			nd = append(nd, pr)
		}
	}
	slices.Sort(nd)
	for _, pr := range nd {
		var deps []string
		for _, s := range narrow[pr] {
			deps = append(deps, strings.TrimSuffix(strings.TrimPrefix(s, "<npm:"), ">"))
		}
		out = append(out, model.Widening{Tier: model.TierGraph, Reason: model.ReasonLockChanged, Path: p, Scope: pr, Eco: Name,
			Detail: "dependency closure changed (L2): importers of " + strings.Join(deps, ", "), Seeds: narrow[pr]})
	}
	return out
}

// l2Seeds returns the package nodes whose importers are the only tests of project pr affected by the closure change, or
// why the change cannot be narrowed.
func l2Seeds(d *lockfile.Diff, pr string, edges []model.Edge, mentions func(string) string) ([]string, string) {
	dc := d.DirectChanges(pr)
	if dc.Risky != "" {
		return nil, dc.Risky
	}
	if len(dc.Deps) == 0 {
		return nil, "no direct dependency carries the change"
	}
	for _, b := range dc.Bins {
		if b == "*" {
			return nil, "a changed package installs executables whose names the lockfile does not record"
		}
		if f := mentions(b); f != "" {
			return nil, fmt.Sprintf("executable %q of a changed package is named in %s", b, f)
		}
	}
	// Imports from files whose nearest lockfile project is pr (a nested workspace package resolves its own copy).
	var lockProjects []eco.Project
	for dir := range d.Head.Projects {
		lockProjects = append(lockProjects, eco.Project{Dir: dir})
	}
	imported := map[string]bool{}
	for _, e := range edges {
		if !strings.HasPrefix(e.To, "<npm:") || imported[e.To] || !eco.Within(e.From, pr) {
			continue
		}
		if lp, ok := eco.NearestProject(lockProjects, e.From); ok && lp.Dir == pr {
			imported[e.To] = true
		}
	}
	var seeds []string
	for _, dep := range dc.Deps {
		if isToolchain(dep) {
			return nil, fmt.Sprintf("%q is a runner/toolchain/types package (loaded without an import)", dep)
		}
		if f := mentions(dep); f != "" {
			return nil, fmt.Sprintf("%q is named in %s (runner, plugin or script use)", dep, f)
		}
		if !imported[pkgNode(dep)] {
			return nil, fmt.Sprintf("%q is not imported by any file of the project (transitive, plugin or tool use)", dep)
		}
		seeds = append(seeds, pkgNode(dep))
	}
	return seeds, ""
}

// pkgJSONDepKeys are the package.json keys that only declare dependencies; every other key (scripts, jest, babel, mocha...)
// may load a package by name.
var pkgJSONDepKeys = map[string]bool{"name": true, "version": true, "dependencies": true, "devDependencies": true,
	"peerDependencies": true, "optionalDependencies": true, "peerDependenciesMeta": true, "dependenciesMeta": true,
	"bundledDependencies": true, "bundleDependencies": true, "overrides": true, "resolutions": true, "workspaces": true,
	"description": true, "keywords": true, "repository": true, "homepage": true, "bugs": true, "license": true, "author": true}

// configMentions returns a function reporting the first runner config file or package.json (outside its dependency
// maps) under dir that names a package as a string: such a package is loaded by name (preset, plugin, environment,
// setup file, CLI in a script), not through an import edge.
func configMentions(t tree.Tree, roles map[string]model.Role, dir string) func(string) string {
	type doc struct{ path, text string }
	var docs []doc
	loaded := false
	load := func() {
		loaded = true
		if t == nil {
			return
		}
		var paths []string
		for f, r := range roles {
			if (r == model.RoleConfig || r == model.RoleManifest) && eco.Within(f, dir) && !inNodeModules(f) {
				paths = append(paths, f)
			}
		}
		slices.Sort(paths)
		for _, f := range paths {
			b, err := t.Read(f)
			if err != nil {
				continue
			}
			text := string(b)
			if path.Base(f) == "package.json" {
				var m map[string]json.RawMessage
				if json.Unmarshal(b, &m) == nil {
					var sb strings.Builder
					for k, v := range m {
						if !pkgJSONDepKeys[k] {
							sb.WriteString(k)
							sb.WriteByte(' ')
							sb.Write(v)
							sb.WriteByte('\n')
						}
					}
					text = sb.String()
				}
			}
			docs = append(docs, doc{f, text})
		}
	}
	return func(name string) string {
		if !loaded {
			load()
		}
		for _, d := range docs {
			for i := 0; ; {
				j := strings.Index(d.text[i:], name)
				if j < 0 {
					break
				}
				j += i
				end := j + len(name)
				before := byte(' ')
				if j > 0 {
					before = d.text[j-1]
				}
				after := byte(' ')
				if end < len(d.text) {
					after = d.text[end]
				}
				if !isNameByte(before) && before != '@' && !isNameByte(after) {
					return d.path
				}
				i = j + 1
			}
		}
		return ""
	}
}

// isNameByte: a byte that can continue an npm package name.
func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
}

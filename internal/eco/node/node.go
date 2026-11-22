// Package node is bgraph's Node ecosystem (JS/TS, CJS/ESM) (docs/ecosystems/node.md).
package node

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/node/resolve"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/tree"
)

// Name is the ecosystem name.
const Name = "node"

// AnalyzerVersion: bump whenever the analysis/resolution rules change to invalidate stored analysis snapshots.
const AnalyzerVersion = "node-23"

// Eco is the Node ecosystem.
type Eco struct {
	Rules *rules.File // [[generated]], [[config]]; may be nil
}

// New creates the Node ecosystem.
func New(r *rules.File) *Eco { return &Eco{Rules: r} }

func (e *Eco) Name() string    { return Name }
func (e *Eco) Version() string { return AnalyzerVersion }

// pkgInfo is the part of package.json that the ecosystem layer needs (the resolver reads the resolution part itself).
type pkgInfo struct {
	Name            string            `json:"name"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Jest            json.RawMessage   `json:"jest"`
}

func readPkg(t tree.Tree, p string) (*pkgInfo, error) {
	b, err := t.Read(p)
	if err != nil {
		return nil, err
	}
	var pi pkgInfo
	if err := json.Unmarshal(b, &pi); err != nil {
		return nil, err
	}
	return &pi, nil
}

// Analyze analyzes tree t.
func (e *Eco) Analyze(ctx context.Context, t tree.Tree, sc eco.Scanner) (*eco.Analysis, error) {
	a := &eco.Analysis{Roles: map[string]model.Role{}}
	files := t.Files()

	// 1. Projects = every package.json outside node_modules.
	pkgs := map[string]*pkgInfo{}
	workspaces := map[string]string{}
	for _, p := range files {
		if path.Base(p) != "package.json" || inNodeModules(p) {
			continue
		}
		dir := path.Dir(p)
		a.Roles[p] = model.RoleManifest
		pi, err := readPkg(t, p)
		if err != nil {
			// broken package.json: the project still exists; every file in it is treated as uncertain.
			pi = &pkgInfo{}
			a.Unresolved = append(a.Unresolved, model.Unresolved{File: p, Spec: "package.json", Class: model.UnresolvedUnknown,
				ScopeTier: model.TierModule, ScopeDir: dir, Detail: "package.json unreadable: " + err.Error()})
		}
		pkgs[dir] = pi
		a.Projects = append(a.Projects, eco.Project{Eco: Name, Dir: dir, Name: pi.Name})
		if pi.Name != "" {
			// Superset: every package named in the repo is a workspace candidate (picking extra is harmless). Same name: keep the
			// shallowest directory, then by lexical order (deterministic).
			if old, ok := workspaces[pi.Name]; !ok || shallower(dir, old) {
				workspaces[pi.Name] = dir
			}
		}
	}
	if len(pkgs) == 0 {
		return a, nil
	}
	projDirs := make([]eco.Project, len(a.Projects))
	copy(projDirs, a.Projects)

	// 2. Per-project runner config: aliases, implicit dependencies, unreadable parts.
	// Runner config files in EVERY directory, assigned to the nearest project (nested config such as tests/unit/jest.config.mjs).
	cfgByProj := map[string][]string{}
	for _, p := range files {
		if isRunnerConfig(p) && !inNodeModules(p) {
			if pr, ok := eco.NearestProject(projDirs, p); ok {
				cfgByProj[pr.Dir] = append(cfgByProj[pr.Dir], p)
			}
		}
	}
	configs := map[string]testConfig{}
	var aliases []resolve.Alias
	for dir, pi := range pkgs {
		tc := readTestConfig(t, dir, pi.Jest, cfgByProj[dir])
		configs[dir] = tc
		aliases = append(aliases, tc.aliases...)
		for _, f := range tc.files {
			a.Roles[f] = model.RoleConfig
		}
	}
	declAliases, declSources := e.applyDeclarations(t, configs)
	aliases = append(aliases, declAliases...)
	slices.SortFunc(aliases, func(x, y resolve.Alias) int { return strings.Compare(aliasKey(x), aliasKey(y)) })
	r := resolve.New(t, resolve.Options{Workspaces: workspaces, Aliases: aliases})

	projCache := map[string]string{} // directory → nearest project
	projectOf := func(p string) string {
		d := path.Dir(p)
		if v, ok := projCache[d]; ok {
			return v
		}
		v := ""
		if pr, ok := eco.NearestProject(projDirs, p); ok {
			v = pr.Dir
		}
		projCache[d] = v
		return v
	}

	// Jest root mocks: <project or roots directory>/__mocks__/<module name>.<ext> replace the same-named external module when running tests.
	jestMocks := map[string]map[string][]string{}
	for dir := range pkgs {
		if detectRunner(pkgs, dir) != "jest" {
			continue
		}
		mockDirs := []string{path.Join(dir, "__mocks__")}
		for _, rt := range configs[dir].roots {
			mockDirs = append(mockDirs, path.Join(rt, "__mocks__"))
		}
		for _, f := range files {
			if !isJS(f) {
				continue
			}
			for _, md := range mockDirs {
				if strings.HasPrefix(f, md+"/") {
					name := strings.TrimPrefix(f, md+"/")
					name = strings.TrimSuffix(name, path.Ext(name))
					name = strings.TrimSuffix(name, "/index")
					if jestMocks[dir] == nil {
						jestMocks[dir] = map[string][]string{}
					}
					jestMocks[dir][name] = append(jestMocks[dir][name], f)
				}
			}
		}
	}

	// 3. Scan and resolve every JS/TS file belonging to a project.
	var tests []string
	for _, p := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if inNodeModules(p) {
			continue
		}
		proj := projectOf(p)
		if owner, ok := snapshotOwner(p); ok && proj != "" {
			a.Roles[p] = model.RoleData
			a.Edges = append(a.Edges, model.Edge{From: owner, To: p, Kind: model.EdgeImplicit, Provenance: model.ProvScan, Spec: "snapshot"})
			continue
		}
		if isTSConfig(p) {
			a.Roles[p] = model.RoleConfig
			continue
		}
		if isLockfile(p) {
			a.Roles[p] = model.RoleLock
			continue
		}
		if !isJS(p) && !isSFC(p) {
			continue
		}
		if proj == "" {
			continue // JS outside every package.json: not in this ecosystem (the selector treats it as an unknown file)
		}
		// Runner config in an ancestor project (jest.config.js at the monorepo root) also applies to files in child packages: union.
		inConfig := false
		for d := proj; ; d = path.Dir(d) {
			if tc, ok := configs[d]; ok && tc.isTest(p) {
				inConfig = true
				break
			}
			if d == "." || d == "/" || d == "" {
				break
			}
		}
		if isTestFile(p, detectRunner(pkgs, proj)) || inConfig {
			a.Roles[p] = model.RoleTest
			tests = append(tests, p)
		} else if _, ok := a.Roles[p]; !ok {
			a.Roles[p] = model.RoleSource
		}
		res, err := sc.Scan(t, p)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", p, err)
		}
		if res.Uncertain {
			a.Unresolved = append(a.Unresolved, model.Unresolved{File: p, Spec: "*", Class: model.UnresolvedUncertainScan,
				ScopeTier: model.TierModule, ScopeDir: "", Detail: res.UncertainReason})
		}
		for _, imp := range res.Imports {
			e.addImport(a, r, t, p, proj, imp, jestMocks[proj])
		}
	}
	for _, ce := range r.ConfigErrors() {
		dir := projectOf(ce.Path)
		a.Unresolved = append(a.Unresolved, model.Unresolved{File: ce.Path, Spec: path.Base(ce.Path), Class: model.UnresolvedUnknown,
			ScopeTier: model.TierModule, ScopeDir: dir, Detail: "config unreadable: " + ce.Err.Error()})
		// Every file in the project may be mis-resolved: attach the broken config file as a dependency of every test in the project.
	}

	// 4. Test units and implicit dependencies.
	isTest := make(map[string]bool, len(tests))
	for _, tf := range tests {
		isTest[tf] = true
		dir := projectOf(tf)
		a.Tests = append(a.Tests, model.TestUnit{Eco: Name, ID: tf, Project: dir, Files: []string{tf}, Runner: detectRunner(pkgs, dir)})
	}
	// Test-file pattern computed by code (template/variable): unknown which files are tests → "whole project" unit (the runner runs the whole project).
	// Without it, an unrecognized test would never be selected (babel: testRegex is a template with expressions).
	for dir, tc := range configs {
		if tc.testsOpaque {
			a.Tests = append(a.Tests, model.TestUnit{Eco: Name, ID: wholeUnitID(dir), Project: dir, Runner: detectRunner(pkgs, dir), Whole: true})
		}
	}
	for dir, tc := range configs {
		var projTests []string
		for _, tf := range tests {
			if projectOf(tf) == dir || (eco.Within(tf, dir) && dir == ".") {
				projTests = append(projTests, tf)
			}
		}
		targets := append([]string{}, tc.files...)
		targets = append(targets, tc.implicit...)
		// __mocks__ at the project root or adjacent to the module: a dependency of every test in the project (superset).
		for _, p := range files {
			if strings.Contains("/"+p, "/__mocks__/") && projectOf(p) == dir {
				targets = append(targets, p)
				if _, ok := a.Roles[p]; !ok {
					a.Roles[p] = model.RoleSource
				}
			}
		}
		slices.Sort(targets)
		targets = slices.Compact(targets)
		globs := slices.Clone(tc.implicitGlobs)
		slices.Sort(globs)
		globs = slices.Compact(globs)
		for _, tf := range projTests {
			// An exact path (may contain "[id]", "{x}") is a regular edge; a glob (with literal parts escaped) is a pattern edge.
			// A test file named in the config (include, exclude, shard lists) selects tests: it is not an input of the other
			// tests (if one imports it, that is a scanned edge of its own).
			for _, tg := range targets {
				if tg != tf && !isTest[tg] {
					a.Edges = append(a.Edges, model.Edge{From: tf, To: tg, Kind: model.EdgeImplicit, Provenance: model.ProvScan, Spec: "test config"})
				}
			}
			for _, g := range globs {
				a.Edges = append(a.Edges, model.Edge{From: tf, To: g, Kind: model.EdgePattern, Provenance: model.ProvScan, Spec: "test config"})
			}
			for _, op := range tc.opaque {
				a.Unresolved = append(a.Unresolved, model.Unresolved{File: tf, Spec: op, Class: model.UnresolvedUnknown,
					ScopeTier: model.TierModule, ScopeDir: dir, Detail: model.ReasonConfigIsCode + ": " + op})
			}
		}
	}
	if len(declSources) > 0 {
		// [[config]] sources: their unresolved imports are what the declaration describes (their dynamic, glob and URL
		// lookups were already skipped in addImport). Their static imports stay, and they stay graph nodes, so editing them
		// still reaches every test through the config.
		a.Unresolved = slices.DeleteFunc(a.Unresolved, func(u model.Unresolved) bool { return declSources[u.File] })
	}
	return a, nil
}

func aliasKey(a resolve.Alias) string {
	if a.Regex != nil {
		return "r:" + a.Regex.String() + strings.Join(a.Targets, ",")
	}
	return "p:" + a.Prefix + strings.Join(a.Targets, ",")
}

func shallower(a, b string) bool {
	da, db := strings.Count(a, "/"), strings.Count(b, "/")
	if da != db {
		return da < db
	}
	return a < b
}

func edgeKind(k model.ImportKind) model.EdgeKind {
	switch k {
	case model.ImportTypeOnly:
		return model.EdgeTypeOnly
	case model.ImportReExport:
		return model.EdgeReExport
	case model.ImportMock:
		return model.EdgeMock
	}
	return model.EdgeImport
}

// addImport resolves an import and records an edge or an Unresolved.
func (e *Eco) addImport(a *eco.Analysis, r *resolve.Resolver, t tree.Tree, from, proj string, imp model.ImportRef, mocks map[string][]string) {
	// A [[config]] source computes the declared keys: its computed lookups (glob, dynamic import, new URL) are described
	// by the declaration, not dependencies of every test.
	if (imp.Kind == model.ImportGlob || imp.Kind == model.ImportURL || imp.Dynamic) && e.declaredSource(from) {
		return
	}
	unres := func(class model.UnresolvedClass, scope, detail string) {
		a.Unresolved = append(a.Unresolved, model.Unresolved{File: from, Spec: imp.Spec, Class: class, Line: imp.Line,
			ScopeTier: model.TierModule, ScopeDir: scope, Detail: detail})
	}
	switch {
	case imp.Kind == model.ImportGlob:
		spec := imp.Spec
		if strings.HasPrefix(spec, "!") {
			return // negation only removes files: ignoring it is a superset
		}
		var g string
		switch {
		case strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../"):
			g = path.Join(path.Dir(from), spec)
		case strings.HasPrefix(spec, "/"):
			g = path.Join(proj, spec)
		default:
			unres(model.UnresolvedDynamic, "", "non-relative glob")
			return
		}
		if g == ".." || strings.HasPrefix(g, "../") {
			unres(model.UnresolvedDynamic, "", "glob outside the repo")
			return
		}
		a.Edges = append(a.Edges, model.Edge{From: from, To: g, Kind: model.EdgePattern, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
		return
	case imp.Dynamic:
		if imp.Spec == "" {
			unres(model.UnresolvedDynamic, "", "argument is not a constant")
			return
		}
		res := r.ResolvePattern(from, imp.Spec, imp.Suffix)
		if res.Class != resolve.ClassInternal {
			unres(model.UnresolvedDynamic, "", "static prefix cannot be resolved: "+res.Detail)
			return
		}
		for _, f := range res.Files {
			a.Edges = append(a.Edges, model.Edge{From: from, To: f, Kind: model.EdgeImport, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
		}
		// New file matches the pattern: glob edge to the prefix's directory.
		if dir := patternDir(from, imp.Spec, res.Files); dir != "" {
			a.Edges = append(a.Edges, model.Edge{From: from, To: escapeGlob(dir) + "/**", Kind: model.EdgePattern, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
		} else {
			unres(model.UnresolvedDynamic, "", "cannot determine the pattern's directory")
		}
		return
	}
	// new URL('.', import.meta.url) / new URL('./dir/', ...): a directory reference (usually to build __dirname or read files
	// in it) → depends on every file under the directory (superset).
	if imp.Kind == model.ImportURL && (imp.Spec == "." || imp.Spec == ".." || strings.HasSuffix(imp.Spec, "/")) {
		d := path.Join(path.Dir(from), imp.Spec)
		if d == ".." || strings.HasPrefix(d, "../") {
			unres(model.UnresolvedUnknown, "", "URL outside the repo")
			return
		}
		g := "**"
		if d != "." {
			g = escapeGlob(d) + "/**"
		}
		a.Edges = append(a.Edges, model.Edge{From: from, To: g, Kind: model.EdgePattern, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
		return
	}
	res := r.Resolve(from, imp.Spec)
	// SFC: `src="x.ts"`, `@import "a.css"` are relative first (HTML/CSS syntax), even without "./".
	if isSFC(from) && !strings.HasPrefix(imp.Spec, ".") && !strings.HasPrefix(imp.Spec, "/") && !strings.HasPrefix(imp.Spec, "#") {
		if rel := r.Resolve(from, "./"+imp.Spec); rel.Class == resolve.ClassInternal {
			if res.Class == resolve.ClassInternal {
				rel.Files = append(rel.Files, res.Files...)
			}
			res = rel
		}
	}
	// Jest: an external (bare) module with a root mock <root>/__mocks__/<name> — when running tests Jest uses the mock instead of the real module
	// (even when the "real" module is a build output absent from the tree). The edge to the mock is an exact dependency.
	if files := mocks[imp.Spec]; len(files) > 0 && isBareSpec(imp.Spec) {
		for _, f := range files {
			a.Edges = append(a.Edges, model.Edge{From: from, To: f, Kind: model.EdgeMock, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
		}
		if res.Class != resolve.ClassInternal {
			// L2: the mock may load the real package (jest.requireActual, which resolves to the mock path here too).
			if res.Class == resolve.ClassExternal && res.Package != "" && e.lockLevel() == rules.LockL2 {
				a.Edges = append(a.Edges, model.Edge{From: from, To: pkgNode(res.Package), Kind: edgeKind(imp.Kind), Line: imp.Line, Spec: imp.Spec,
					Provenance: model.ProvScan})
			}
			return // external/generated/unknown: at test run time Jest loads the mock, not this module
		}
		res.AlsoGenerated = false
	}
	switch res.Class {
	case resolve.ClassInternal:
		k := edgeKind(imp.Kind)
		for _, f := range res.Files {
			a.Edges = append(a.Edges, model.Edge{From: from, To: f, Kind: k, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
		}
		if res.AlsoGenerated {
			e.addGenerated(a, from, imp, res)
		}
		if res.CaseMismatch {
			a.Warnings = append(a.Warnings, model.Widening{Tier: model.TierNone, Reason: model.ReasonCaseMismatch, Path: from,
				Detail: fmt.Sprintf("%s:%d %q matches %s only when ignoring case (works on Windows/macOS, breaks on Linux)",
					from, imp.Line, imp.Spec, strings.Join(res.Files, ", "))})
		}
		// A probe is not an edge: the current tree is always fully re-resolved, so a new file shadowing an old one already shows up as an
		// import edge to the new file (a changed file). res.Probes is for a future incremental index.
	case resolve.ClassExternal:
		// Level L2 narrows a lockfile change to the importers of the changed package: the import is an edge to its node.
		if res.Package != "" && e.lockLevel() == rules.LockL2 {
			a.Edges = append(a.Edges, model.Edge{From: from, To: pkgNode(res.Package), Kind: edgeKind(imp.Kind), Line: imp.Line, Spec: imp.Spec,
				Provenance: model.ProvScan})
		}
	case resolve.ClassBuiltin:
	case resolve.ClassGenerated:
		e.addGenerated(a, from, imp, res)
	default: // unknown
		if strings.HasPrefix(res.Detail, "undeclared package") && isBareSpec(imp.Spec) {
			// A bare specifier that fails to resolve can only point into the repo through a root-directory mechanism not yet modeled
			// (NODE_PATH, moduleDirectories, babel module-resolver...): the target file then has a path ending in spec,
			// spec.<ext> or lying under spec/. A glob edge to those paths stays fail-closed without widening the whole project.
			// Virtual node per spec so multiple importers share one pattern set.
			v := bareNode(imp.Spec)
			a.Edges = append(a.Edges, model.Edge{From: from, To: v, Kind: model.EdgeImport, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
			esc := escapeGlob(imp.Spec)
			for _, g := range []string{"**/" + esc, "**/" + esc + ".*", "**/" + esc + "/**"} {
				a.Edges = append(a.Edges, model.Edge{From: v, To: g, Kind: model.EdgePattern, Spec: imp.Spec, Provenance: model.ProvScan})
			}
			// For reporting only (bgraph unresolved): ScopeTier none → the selector does not seed.
			a.Unresolved = append(a.Unresolved, model.Unresolved{File: from, Spec: imp.Spec, Class: model.UnresolvedUnknown, Line: imp.Line,
				ScopeTier: model.TierNone, ScopeDir: "", Detail: res.Detail + " (tracked via glob **/" + imp.Spec + ")"})
			return
		}
		scope := proj
		if strings.HasPrefix(imp.Spec, "./") || strings.HasPrefix(imp.Spec, "../") {
			d := path.Join(path.Dir(from), path.Dir(imp.Spec))
			if d == ".." || strings.HasPrefix(d, "../") {
				d = ""
			}
			scope = d
		}
		unres(model.UnresolvedUnknown, scope, res.Detail)
	}
}

// patternDir: the directory holding the static part of a dynamic import ("./locales/en-" from src/a.ts → "src/locales").
func patternDir(from, spec string, files []string) string {
	if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
		d := path.Join(path.Dir(from), spec)
		if !strings.HasSuffix(spec, "/") {
			d = path.Dir(d)
		}
		if d == ".." || strings.HasPrefix(d, "../") {
			return ""
		}
		return d
	}
	// alias/#imports: use the common directory of the matched files.
	if len(files) == 0 {
		return ""
	}
	d := path.Dir(files[0])
	for _, f := range files[1:] {
		for !eco.Within(f, d) {
			d = path.Dir(d)
		}
	}
	return d
}

func escapeGlob(s string) string {
	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune(`*?[]{}\`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// detectRunner guesses the runner of project dir from its own dependencies, then the root's.
func detectRunner(pkgs map[string]*pkgInfo, dir string) string {
	for d := dir; ; d = path.Dir(d) {
		if pi, ok := pkgs[d]; ok {
			for _, name := range []string{"vitest", "jest", "@playwright/test", "mocha", "ava"} {
				if pi.DevDependencies[name] != "" || pi.Dependencies[name] != "" {
					return strings.TrimPrefix(strings.TrimSuffix(name, "/test"), "@")
				}
			}
		}
		if d == "." || d == "/" || d == "" {
			break
		}
	}
	return "node-test"
}

// isBareSpec: a package name (may carry a subpath), not relative/absolute/#/URL.
func isBareSpec(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "#") || strings.Contains(s, ":") {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// bareNode is the virtual node of an undeclared bare specifier (matches no file path in the repo).
func bareNode(spec string) string { return "<bare:" + spec + ">" }

// addGenerated records the dependency on package res.PackageDir's build output: per [[generated]] (glob edge to source) if
// a rule exists, otherwise an Unresolved scoped to that package (any change in the package affects the importer).
func (e *Eco) addGenerated(a *eco.Analysis, from string, imp model.ImportRef, res resolve.Result) {
	if e.Rules != nil {
		var srcs []string
		for _, g := range e.Rules.Generated {
			hit := path.Clean(g.Owner) == res.PackageDir
			for _, o := range g.Outputs {
				if pre := o.StaticPrefix(); pre != "" && (eco.Within(pre, res.PackageDir) || eco.Within(res.PackageDir, pre)) {
					hit = true
				}
			}
			if hit {
				srcs = append(srcs, g.RawSources...)
			}
		}
		if len(srcs) > 0 {
			for _, s := range srcs {
				a.Edges = append(a.Edges, model.Edge{From: from, To: s, Kind: model.EdgePattern, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvRule})
			}
			return
		}
	}
	detail := fmt.Sprintf("build output of package %q is not in the tree", res.Package)
	if res.Detail != "" {
		detail = res.Detail
	}
	a.Unresolved = append(a.Unresolved, model.Unresolved{File: from, Spec: imp.Spec, Class: model.UnresolvedUnknown, Line: imp.Line,
		ScopeTier: model.TierModule, ScopeDir: res.PackageDir, Detail: model.ReasonGeneratedOutput + ": " + detail})
}

// wholeUnitID is the identifier of the "whole test suite of project dir" unit.
func wholeUnitID(dir string) string { return "<all-tests:" + dir + ">" }

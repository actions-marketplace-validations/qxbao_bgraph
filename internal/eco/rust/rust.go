// Package rust is bgraph's Rust ecosystem, at Cargo-target granularity (docs/plan.md §6.3).
//
// Inside a crate the mod/use graph is not a sound dependency relation (trait impls are crate-global, macro_rules! is textually scoped,
// inventory/linkme register at link time, #[cfg] changes the module tree), so the safe unit is the Cargo target. The module tree is used
// only to decide which target OWNS a file.
//
// # Test units
//
// One model.TestUnit per Cargo target that `cargo test` runs. Eco is "rust", Runner "cargo", Project is the package directory
// (repo-relative, "." for the repo root) and Package is the package name. The unit ID is built by UnitID:
//
//	<pkgdir>::lib                 unit tests of the library
//	<pkgdir>::doc                 doc-tests of the library (only when the package has a library and doctest is not disabled)
//	<pkgdir>::bin:<name>          unit tests of a binary
//	<pkgdir>::test:<name>         an integration test
//	<pkgdir>::bench:<name>        a benchmark target (run once in test mode)
//	<pkgdir>::example:<name>      an example
//
// The unit's Files hold a single virtual node, "<rust:" + ID + ">", so selection is reachability of that node. The virtual node
// of the library is both the unit and the dependency target of every other target of the package and of dependent packages.
//
// # Graph (From depends on To)
//
//   - a target depends on every file it owns (module tree from its root file, #[path], literal include!/include_str!/include_bytes!);
//   - bins, tests, benches, examples and the doc unit depend on the library of their package;
//     tests, benches and examples also depend on every bin of the package (Cargo builds them, CARGO_BIN_EXE_*);
//   - every target of package P depends on the library of each workspace/path dependency of P (normal, dev and build alike);
//   - package-level files (build.rs and what it reads, files under the package directory that no target owns) are dependencies
//     of every target of the package, except unowned .rs files under tests/ (benches/, examples/), which only the
//     integration tests (benches, examples) depend on;
//   - a target whose module tree cannot be determined lexically depends on every file of the package.
//
// The package directory excludes nested packages and target/.
package rust

import (
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	rsscan "github.com/qxbao/bgraph/internal/scan/rust"
	"github.com/qxbao/bgraph/internal/tree"
)

// Name is the ecosystem name.
const Name = "rust"

// Runner is the runner name of the units.
const Runner = "cargo"

// AnalyzerVersion: bump whenever the analysis rules change to invalidate stored analysis snapshots.
const AnalyzerVersion = "rust-2"

// Eco is the Rust ecosystem.
type Eco struct{}

// New creates the Rust ecosystem. The rules file is accepted for symmetry with the other ecosystems; Rust has no declarations yet.
func New(_ *rules.File) *Eco { return &Eco{} }

func (e *Eco) Name() string    { return Name }
func (e *Eco) Version() string { return AnalyzerVersion }

// Kinds of Cargo targets in unit IDs.
const (
	KindLib     = "lib"
	KindDoc     = "doc"
	KindBin     = "bin"
	KindTest    = "test"
	KindBench   = "bench"
	KindExample = "example"
)

// UnitID returns the test-unit ID of a Cargo target: "<pkgDir>::<kind>" for lib and doc, "<pkgDir>::<kind>:<name>" otherwise.
func UnitID(pkgDir, kind, name string) string {
	if kind == KindLib || kind == KindDoc || name == "" {
		return pkgDir + "::" + kind
	}
	return pkgDir + "::" + kind + ":" + name
}

// ParseUnitID is the inverse of UnitID. ok=false when id is not a Rust unit ID.
func ParseUnitID(id string) (pkgDir, kind, name string, ok bool) {
	pkgDir, rest, found := strings.Cut(id, "::")
	if !found || pkgDir == "" {
		return "", "", "", false
	}
	kind, name, _ = strings.Cut(rest, ":")
	switch kind {
	case KindLib, KindDoc:
		return pkgDir, kind, "", name == ""
	case KindBin, KindTest, KindBench, KindExample:
		return pkgDir, kind, name, name != ""
	}
	return "", "", "", false
}

// Node returns the virtual graph node of a unit ID.
func Node(id string) string { return "<rust:" + id + ">" }

// pkgNode is the virtual node of a package-level group ("pkg", "tests", "benches", "examples", "all").
func pkgNode(dir, group string) string { return "<rust:" + dir + "::" + group + ">" }

// Analyze analyzes tree t.
func (e *Eco) Analyze(ctx context.Context, t tree.Tree, sc eco.Scanner) (*eco.Analysis, error) {
	a := &eco.Analysis{Roles: map[string]model.Role{}}
	ws, err := loadWorkspace(t)
	if err != nil {
		return nil, err
	}
	if len(ws.manifests) == 0 {
		return a, nil
	}
	b := &builder{t: t, sc: sc, a: a, ws: ws, seen: map[edgeKey]bool{}}
	for _, m := range ws.manifests {
		a.Roles[m.path] = model.RoleManifest
		if m.err != nil {
			a.Unresolved = append(a.Unresolved, model.Unresolved{File: m.path, Spec: "Cargo.toml", Class: model.UnresolvedUnknown,
				ScopeTier: model.TierFull, ScopeDir: m.dir, Detail: "Cargo.toml unreadable: " + m.err.Error()})
		}
	}
	for _, p := range t.Files() {
		if path.Base(p) == "Cargo.lock" && !ws.skip(p) {
			a.Roles[p] = model.RoleLock
		}
	}
	for _, p := range ws.pkgs {
		a.Projects = append(a.Projects, eco.Project{Eco: Name, Dir: p.dir, Name: p.name})
	}
	b.assignFiles()
	for _, p := range ws.pkgs {
		p.targets = discoverTargets(t, p, b.fileset[p.dir])
	}
	for _, p := range ws.pkgs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := b.analyzePackage(p); err != nil {
			return nil, err
		}
	}
	b.finishRoles()
	return a, nil
}

type edgeKey struct {
	from, to string
	kind     model.EdgeKind
}

type builder struct {
	t    tree.Tree
	sc   eco.Scanner
	a    *eco.Analysis
	ws   *workspaces
	seen map[edgeKey]bool
	// fileset: package directory → files whose nearest enclosing package is that package.
	fileset map[string][]string
	// owners: file → kinds of the targets that own it (for roles).
	owners map[string]map[string]bool
}

func (b *builder) edge(from, to string, kind model.EdgeKind, spec string, line int) {
	k := edgeKey{from, to, kind}
	if b.seen[k] {
		return
	}
	b.seen[k] = true
	b.a.Edges = append(b.a.Edges, model.Edge{From: from, To: to, Kind: kind, Spec: spec, Line: line, Provenance: model.ProvScan})
}

func (b *builder) role(p string, r model.Role) {
	if old, ok := b.a.Roles[p]; !ok || old == model.RoleData || old == model.RoleUnknown {
		b.a.Roles[p] = r
	}
}

func (b *builder) own(p, kind string) {
	if b.owners == nil {
		b.owners = map[string]map[string]bool{}
	}
	if b.owners[p] == nil {
		b.owners[p] = map[string]bool{}
	}
	b.owners[p][kind] = true
}

// assignFiles attributes every file to its nearest enclosing package (nested packages and target/ excluded).
func (b *builder) assignFiles() {
	b.fileset = map[string][]string{}
	byDir := map[string]string{} // directory → owning package dir ("" none), memoized
	var nearest func(d string) string
	nearest = func(d string) string {
		if v, ok := byDir[d]; ok {
			return v
		}
		v := ""
		if _, ok := b.ws.pkgs[d]; ok {
			v = d
		} else if d != "." && d != "/" && d != "" {
			v = nearest(path.Dir(d))
		}
		byDir[d] = v
		return v
	}
	for _, f := range b.t.Files() {
		d := path.Dir(f)
		pd := nearest(d)
		if pd == "" {
			if _, ok := b.ws.pkgs["."]; ok {
				pd = "."
			} else {
				continue
			}
		}
		rel := f
		if pd != "." {
			rel = strings.TrimPrefix(f, pd+"/")
		}
		if rel == "target" || strings.HasPrefix(rel, "target/") || b.ws.skip(f) {
			continue
		}
		b.fileset[pd] = append(b.fileset[pd], f)
	}
}

// walkResult is what one ownership walk found.
type walkResult struct {
	files    []string // owned files (excluding the root itself)
	includes []includeEdge
	dirs     []string // directories whose files are modules of the target (automod::dir!), relative to the repo root
	unknown  []string // reasons the module tree could not be determined
}

type includeEdge struct {
	file string
	spec string
	line int
}

// walk follows the module tree from root. modDir is the directory in which `mod x;` declarations of root are looked up.
func (b *builder) walk(p *pkg, root string) walkResult {
	var res walkResult
	seen := map[string]bool{}
	var visit func(file, modDir string)
	visit = func(file, modDir string) {
		if seen[file] {
			return
		}
		seen[file] = true
		if file != root {
			res.files = append(res.files, file)
		}
		sr, err := b.sc.Scan(b.t, file)
		if err != nil {
			res.unknown = append(res.unknown, fmt.Sprintf("%s: scan failed: %v", file, err))
			return
		}
		if sr.Uncertain {
			res.unknown = append(res.unknown, fmt.Sprintf("%s: %s", file, sr.UncertainReason))
			return
		}
		if sr.Rust == nil {
			return
		}
		for _, m := range sr.Rust.Mods {
			if m.Unknown != "" {
				res.unknown = append(res.unknown, fmt.Sprintf("%s:%d mod %s: %s", file, m.Line, m.Name, m.Unknown))
				continue
			}
			if m.Path != "" {
				base := path.Dir(file)
				if len(m.Nest) > 0 {
					base = path.Join(modDir, path.Join(m.Nest...))
				}
				target := path.Join(base, strings.ReplaceAll(m.Path, "\\", "/"))
				if b.t.Has(target) {
					visit(target, path.Dir(target)) // a file loaded through #[path] behaves like a mod.rs
				}
				continue
			}
			base := modDir
			if len(m.Nest) > 0 {
				base = path.Join(modDir, path.Join(m.Nest...))
			}
			for _, cand := range []string{path.Join(base, m.Name+".rs"), path.Join(base, m.Name, "mod.rs")} {
				if b.t.Has(cand) {
					visit(cand, path.Join(base, m.Name))
				}
			}
		}
		for _, inc := range sr.Rust.Includes {
			var target string
			switch inc.Kind {
			case model.RustIncludeOutDir:
				continue // build script output: covered by the build script being a package-level input
			case model.RustIncludeLiteral:
				target = path.Join(path.Dir(file), strings.ReplaceAll(inc.Path, "\\", "/"))
			case model.RustIncludeManifest:
				target = path.Join(p.dir, strings.ReplaceAll(inc.Path, "\\", "/"))
			case model.RustIncludeDir:
				res.dirs = append(res.dirs, path.Join(p.dir, strings.ReplaceAll(inc.Path, "\\", "/")))
				continue
			default:
				res.unknown = append(res.unknown, fmt.Sprintf("%s:%d %s! with a non-literal path", file, inc.Line, inc.Macro))
				continue
			}
			if target == ".." || strings.HasPrefix(target, "../") || strings.HasPrefix(target, "/") {
				b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: file, Spec: inc.Path, Class: model.UnresolvedUnknown, Line: inc.Line,
					ScopeTier: model.TierFull, ScopeDir: "", Detail: inc.Macro + "! reads a file outside the repository"})
				continue
			}
			if target == "." {
				target = ""
			}
			res.includes = append(res.includes, includeEdge{target, inc.Macro + "!(" + inc.Path + ")", inc.Line})
			if inc.Macro == "include" && strings.HasSuffix(target, ".rs") && b.t.Has(target) {
				visit(target, modDir) // included items live in the including module
			}
		}
	}
	visit(root, path.Dir(root))
	return res
}

func (b *builder) analyzePackage(p *pkg) error {
	// Targets and their owned files.
	owned := map[string]bool{}
	libNode := ""
	var binNodes []string
	for _, tg := range p.targets {
		if tg.kind == KindLib {
			libNode = tg.node
		}
		if tg.kind == KindBin {
			binNodes = append(binNodes, tg.node)
		}
	}
	var testNodes, benchNodes, exampleNodes []string
	for _, tg := range p.targets {
		switch tg.kind {
		case KindTest:
			testNodes = append(testNodes, tg.node)
		case KindBench:
			benchNodes = append(benchNodes, tg.node)
		case KindExample:
			exampleNodes = append(exampleNodes, tg.node)
		}
	}

	pkgN := pkgNode(p.dir, "pkg")
	allN := pkgNode(p.dir, "all")
	needAll := false
	for _, tg := range p.targets {
		if tg.unit {
			b.a.Tests = append(b.a.Tests, model.TestUnit{Eco: Name, ID: UnitID(p.dir, tg.kind, tg.name), Project: p.dir,
				Files: []string{tg.node}, Runner: Runner, Package: p.name})
		}
		if tg.root != "" {
			var res walkResult
			if b.t.Has(tg.root) {
				res = b.walk(p, tg.root)
			}
			owned[tg.root] = true
			b.own(tg.root, tg.kind)
			b.edge(tg.node, tg.root, model.EdgeImplicit, "cargo target", 0)
			for _, f := range res.files {
				owned[f] = true
				b.own(f, tg.kind)
				b.edge(tg.node, f, model.EdgeImplicit, "cargo target", 0)
			}
			for _, d := range res.dirs {
				// automod::dir!: a module per file of the directory; the target owns every file under it (superset of the generated modules).
				for _, f := range b.fileset[p.dir] {
					if strings.HasPrefix(f, d+"/") {
						owned[f] = true
						b.own(f, tg.kind)
						b.edge(tg.node, f, model.EdgeImplicit, "automod::dir!", 0)
					}
				}
			}
			for _, inc := range res.includes {
				if inc.file == "" {
					b.edge(tg.node, pkgNode(p.dir, "all"), model.EdgeInclude, inc.spec, inc.line)
					needAll = true
					continue
				}
				b.edge(tg.node, inc.file, model.EdgeInclude, inc.spec, inc.line)
				owned[inc.file] = true // owned by the including target, no longer a package-level file
				if !b.t.Has(inc.file) && b.t.IsDir(inc.file) {
					b.edge(tg.node, escapeGlob(inc.file)+"/**", model.EdgePattern, inc.spec, inc.line)
				}
				b.role(inc.file, dataRole(inc.file))
			}
			if len(res.unknown) > 0 {
				needAll = true
				b.edge(tg.node, allN, model.EdgeImplicit, "module tree unknown: "+res.unknown[0], 0)
				for _, u := range res.unknown {
					b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: tg.root, Spec: u, Class: model.UnresolvedDynamic,
						ScopeTier: model.TierNone, Detail: "target " + UnitID(p.dir, tg.kind, tg.name) + " depends on every file of the package: " + u})
				}
			}
		}
		// Linking.
		if tg.kind != KindLib && libNode != "" {
			b.edge(tg.node, libNode, model.EdgeImplicit, "links the library", 0)
		}
		if tg.kind == KindTest || tg.kind == KindBench || tg.kind == KindExample {
			for _, bn := range binNodes {
				b.edge(tg.node, bn, model.EdgeImplicit, "cargo builds the binaries for it", 0)
			}
		}
		b.edge(tg.node, pkgN, model.EdgeImplicit, "package files", 0)
		for _, dep := range p.deps {
			if q := b.ws.pkgs[dep]; q != nil && q != p {
				for _, qt := range q.targets {
					if qt.kind == KindLib || !hasLib(q) {
						b.edge(tg.node, qt.node, model.EdgeImplicit, "dependency "+q.name, 0)
					}
				}
			}
		}
	}

	// Build script: a package-level input; what it reads is its dependency.
	build := p.buildScript(b.t)
	if build != "" {
		owned[build] = true
		b.role(build, model.RoleSource)
		b.edge(pkgN, build, model.EdgeImplicit, "build script", 0)
		res := b.walk(p, build)
		for _, f := range res.files {
			owned[f] = true
			b.own(f, "build")
			b.edge(build, f, model.EdgeImplicit, "build script module", 0)
		}
		for _, inc := range res.includes {
			if inc.file == "" {
				b.edge(build, allN, model.EdgeInclude, inc.spec, inc.line)
				needAll = true
				continue
			}
			b.edge(build, inc.file, model.EdgeInclude, inc.spec, inc.line)
			owned[inc.file] = true
			b.role(inc.file, dataRole(inc.file))
		}
		if len(res.unknown) > 0 {
			needAll = true
			b.edge(build, allN, model.EdgeImplicit, "module tree unknown: "+res.unknown[0], 0)
		}
		if b.readBuildScriptPaths(p, build, owned) {
			needAll = true
		}
	}

	// Package-level files: everything in the package no target owns.
	var cat = map[string]string{} // file → group
	groupTargets := map[string][]string{"tests": testNodes, "benches": benchNodes, "examples": exampleNodes}
	for _, f := range b.fileset[p.dir] {
		if owned[f] || f == build || isManifestFile(f) || b.skipPackageLevel(p, f) {
			continue
		}
		rel := f
		if p.dir != "." {
			rel = strings.TrimPrefix(f, p.dir+"/")
		}
		group := "pkg"
		// Only unowned .rs files under tests/ (benches/, examples/) are confined to that group: they can only be compiled
		// by those targets (a src/ module reaching them needs #[path], which makes it owned). Data files there (fixtures,
		// snapshots) may also be read at runtime by unit tests in src/, so they stay package-level.
		for _, g := range []string{"tests", "benches", "examples"} {
			if strings.HasSuffix(f, ".rs") && strings.HasPrefix(rel, g+"/") && len(groupTargets[g]) > 0 {
				group = g
			}
		}
		cat[f] = group
		b.edge(pkgNode(p.dir, group), f, model.EdgeImplicit, "package file", 0)
		switch {
		case strings.HasSuffix(f, ".rs") && group != "pkg" && group != "examples":
			b.role(f, model.RoleTest)
		case strings.HasSuffix(f, ".rs"):
			b.role(f, model.RoleSource)
		default:
			b.role(f, model.RoleData)
		}
	}
	for g, nodes := range groupTargets {
		for _, n := range nodes {
			b.edge(n, pkgNode(p.dir, g), model.EdgeImplicit, "unowned "+g+"/ files", 0)
		}
	}
	if needAll {
		for _, f := range b.fileset[p.dir] {
			if isManifestFile(f) {
				continue
			}
			b.edge(allN, f, model.EdgeImplicit, "package file", 0)
			if _, ok := b.a.Roles[f]; !ok {
				if strings.HasSuffix(f, ".rs") {
					b.role(f, model.RoleSource)
				} else {
					b.role(f, model.RoleData)
				}
			}
		}
	}
	return nil
}

func hasLib(p *pkg) bool {
	for _, t := range p.targets {
		if t.kind == KindLib {
			return true
		}
	}
	return false
}

func isManifestFile(f string) bool {
	switch path.Base(f) {
	case "Cargo.toml", "Cargo.lock":
		return true
	}
	return false
}

// skipPackageLevel: files that cannot be inputs of a test and must not select every target (docs, licences, tool configs, cargo config).
func (b *builder) skipPackageLevel(p *pkg, f string) bool {
	if isHarmlessConfig(path.Base(f)) || isDocOrHarmless(f) {
		return true
	}
	if strings.Contains("/"+f, "/.cargo/") {
		return true
	}
	return false
}

// readBuildScriptPaths turns the string literals of the build script into dependencies of the build script: a literal that names an
// existing file or directory (relative to the package directory, where build scripts run) is read by the script. Literals that
// escape the repository become Unresolved(full). Returns true when the script's inputs cannot be bounded.
func (b *builder) readBuildScriptPaths(p *pkg, build string, owned map[string]bool) bool {
	src, err := b.t.Read(build)
	if err != nil {
		return true
	}
	strs, ok := rsscan.Strings(src)
	if !ok {
		return true
	}
	seen := map[string]bool{}
	for _, s := range strs {
		s = strings.TrimPrefix(strings.TrimPrefix(s, "cargo::rerun-if-changed="), "cargo:rerun-if-changed=")
		if s == "" || strings.ContainsAny(s, "\n\x00") || len(s) > 512 || seen[s] {
			continue
		}
		seen[s] = true
		s = strings.ReplaceAll(s, "\\", "/")
		if strings.HasPrefix(s, "/") || (len(s) > 1 && s[1] == ':') {
			continue // absolute paths: cannot name a repo file portably
		}
		target := path.Join(p.dir, s)
		switch {
		case target == ".." || strings.HasPrefix(target, "../"):
			if strings.HasPrefix(s, "..") {
				b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: build, Spec: s, Class: model.UnresolvedUnknown,
					ScopeTier: model.TierFull, ScopeDir: "", Detail: "build script names a path outside the repository"})
			}
		case b.t.Has(target):
			if target != build {
				b.edge(build, target, model.EdgeInclude, "build script reads "+s, 0)
				owned[target] = true
				b.role(target, dataRole(target))
			}
		case b.t.IsDir(target):
			g := escapeGlob(target) + "/**"
			if target == "." {
				g = "**"
			}
			b.edge(build, g, model.EdgePattern, "build script reads "+s, 0)
		case strings.HasPrefix(s, "../"):
			// Inside the repository but absent: a file added later is still a dependency.
			b.edge(build, target, model.EdgeInclude, "build script reads "+s, 0)
		}
	}
	return false
}

func dataRole(p string) model.Role {
	if strings.HasSuffix(p, ".rs") {
		return model.RoleSource
	}
	return model.RoleData
}

func escapeGlob(s string) string {
	var sb strings.Builder
	for _, c := range s {
		if strings.ContainsRune(`*?[]{}\`, c) {
			sb.WriteByte('\\')
		}
		sb.WriteRune(c)
	}
	return sb.String()
}

// finishRoles refines .rs roles: a file owned only by test/bench targets is a test file.
func (b *builder) finishRoles() {
	var files []string
	for f := range b.owners {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		kinds := b.owners[f]
		if !strings.HasSuffix(f, ".rs") {
			b.role(f, model.RoleData)
			continue
		}
		testOnly := true
		for k := range kinds {
			if k != KindTest && k != KindBench {
				testOnly = false
			}
		}
		if testOnly {
			b.a.Roles[f] = model.RoleTest
		} else if _, ok := b.a.Roles[f]; !ok {
			b.a.Roles[f] = model.RoleSource
		}
	}
	slices.SortFunc(b.a.Edges, func(x, y model.Edge) int {
		if c := strings.Compare(x.From, y.From); c != 0 {
			return c
		}
		return strings.Compare(x.To, y.To)
	})
}

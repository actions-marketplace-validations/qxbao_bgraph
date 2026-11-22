// Package golang is bgraph's Go ecosystem, at package granularity, with pure resolution (docs/ecosystems/go.md): go.mod, go.work and
// go/parser (ImportsOnly) only; the go command is never run.
//
// `go test` compiles a directory's package, its in-package _test.go files and its external _test package together, so selecting a
// subset of test functions is not sound (init order, TestMain, shared state): the safe unit is the package directory.
//
// # Test units
//
// One model.TestUnit per directory that holds at least one _test.go file (any build constraint), belongs to a module and is not under
// vendor/. Eco is "go", Runner "go", Project is the module directory (the directory of the go.mod, "." for the repo root) and Package
// the import path. The unit ID is built by UnitID: "go:<dir>", dir being the repo-relative package directory ("go:." for the root).
// The unit's Files hold a single virtual node, Node(dir) = "<go:dir>".
//
// # Graph (From depends on To)
//
// Every package has two virtual nodes: the library node LibNode(dir) = "<golib:dir>" (what an importer compiles) and the unit node
// Node(dir) (what `go test` compiles: the library plus the test files and their imports).
//
//   - <go:dir> → <golib:dir>, the _test.go files of the directory, the packages they import, their //go:embed patterns and the
//     existing repo paths their string literals name (fixture_data);
//   - <golib:dir> → every other file directly in the directory (any extension: .s, .c, .h, .syso, data), the files of
//     sub-directories that are not packages (testdata/, fixtures/, _x/, .x/: the nearest ancestor package of the same module owns
//     them), the packages its non-test files import, their //go:embed patterns, and the module node;
//   - <golib:dir> of a cgo package → every C/C++/ObjC/header/assembly file of the repository (#include and #cgo -I may reach
//     anywhere);
//   - <gomod:moddir> → the files of the module that no package owns (module-level files: every package of the module depends on them).
//
// Build constraints and GOOS/GOARCH file suffixes are ignored: every file counts (the union of every configuration). Import paths
// resolve against every module of the repository and every local replace target (longest prefix; all of them, so a path claimed by
// two modules depends on both), then against the module's vendor/ directory. An in-repo path with no package becomes a virtual node
// "<goimp:path>" with a pattern edge to the .go files of the directory a package would live in, so a package added later is caught.
// Paths no module claims are the standard library (no dot) or external (go.sum widens their changes).
//
// A directory subtree that holds another ecosystem's project manifest (package.json, pyproject.toml, setup.py, setup.cfg, Cargo.toml,
// pom.xml, build.gradle(.kts), settings.gradle(.kts), *.csproj/*.fsproj/*.vbproj) and no .go file anywhere below it is not owned by
// Go: its files are neither package files nor module-level files, so a frontend change under a root go.mod selects no Go unit.
// //go:embed patterns and test string literals still reach files there (they do not depend on ownership). A Go test that reads such
// a file through a path built at run time is not seen: the same gap class as run-time-built paths in general.
//
// A test that builds or runs another package of the repository by a literal path or import path ("../cmd/x", "./cmd/...",
// "example.com/m/cmd/x", also as a token of a longer literal such as "go run ./cmd/x"), or a testscript file under testdata/
// (*.txtar, script/*.txt) whose `go` or `exec` lines name an in-repo import path, depends on that package's whole closure. A
// program built before the tests run (by a Makefile) and run by a relative path that does not exist in the tree
// ("../../../alertmanager", in a test or in a helper package the test imports) is tied to the main packages `go build` names
// after the path's last element (binaryLiteral); a program found by another name or by a path built at run time is a gap.
//
// # Known over-selection
//
// A module-level widening (go.mod, go.sum, vendor/ change) is exact (model.Widening.Exact): it covers the units of the module and of
// the modules that build with it, not those of a module nested in its directory that does not (ClassifyChange). An unresolved import
// that fires on any change in a module is scoped to the module directory, and a trigger scope cannot exclude sub-directories: a
// change in a module nested inside it fires it too.
package golang

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/qxbao/bgraph/internal/classify"
	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/tree"
)

// Name is the ecosystem name.
const Name = "go"

// Runner is the runner name of the units.
const Runner = "go"

// AnalyzerVersion: bump whenever the analysis rules change to invalidate stored analysis snapshots.
const AnalyzerVersion = "go-9"

// Eco is the Go ecosystem.
type Eco struct {
	mu    sync.Mutex
	mods  map[tree.Tree]*modules // module layout per tree, for ClassifyChange
	rules *rules.File
}

// New creates the Go ecosystem; rf ([lockfile] level) may be nil.
func New(rf *rules.File) *Eco { return &Eco{rules: rf} }

func (e *Eco) Name() string    { return Name }
func (e *Eco) Version() string { return AnalyzerVersion }

// UnitID returns the test-unit ID of the package in directory dir ("." for the repo root).
func UnitID(dir string) string { return "go:" + dir }

// ParseUnitID is the inverse of UnitID. ok=false when id is not a Go unit ID.
func ParseUnitID(id string) (dir string, ok bool) {
	dir, ok = strings.CutPrefix(id, "go:")
	if !ok || dir == "" || dir != path.Clean(dir) || strings.HasPrefix(dir, "/") || dir == ".." || strings.HasPrefix(dir, "../") {
		return "", false
	}
	return dir, true
}

// Node returns the unit node of the package in dir (TestUnit.Files).
func Node(dir string) string { return "<go:" + dir + ">" }

// LibNode returns the library node of the package in dir.
func LibNode(dir string) string { return "<golib:" + dir + ">" }

// ModNode returns the node of a module's module-level files.
func ModNode(dir string) string { return "<gomod:" + dir + ">" }

func impNode(p string) string { return "<goimp:" + p + ">" }

// cgoPattern: what a cgo package may compile or #include.
const cgoPattern = "**/*.{c,h,cc,cpp,cxx,hh,hpp,hxx,m,mm,s,S,sx,f,F,for,f90,syso,swig,swigcxx,inc,def}"

// pkg is a package directory.
type pkg struct {
	dir    string
	mod    *module
	vendor bool
	tests  bool     // has _test.go files
	gofs   []string // .go files directly in the directory
	owned  []string // other files it owns (directly or in non-package sub-directories)
}

// Analyze analyzes tree t.
func (e *Eco) Analyze(ctx context.Context, t tree.Tree, sc eco.Scanner) (*eco.Analysis, error) {
	a := &eco.Analysis{Roles: map[string]model.Role{}}
	ms := loadModules(t)
	e.remember(t, ms)
	if len(ms.list) == 0 {
		return a, nil
	}
	b := &builder{t: t, sc: sc, a: a, ms: ms, seen: map[edgeKey]bool{}, pkgs: map[string]*pkg{}}
	for _, m := range ms.list {
		a.Projects = append(a.Projects, eco.Project{Eco: Name, Dir: m.dir, Name: m.path})
		a.Roles[m.gomod] = model.RoleManifest
		if t.Has(path.Join(m.dir, "go.sum")) {
			a.Roles[path.Join(m.dir, "go.sum")] = model.RoleLock
		}
		if m.err != nil {
			// The module path is unknown, so imports of its packages cannot be resolved: any change in the module selects every
			// package of the module (and their dependents) through the module node.
			a.Unresolved = append(a.Unresolved, model.Unresolved{File: ModNode(m.dir), Spec: "go.mod", Class: model.UnresolvedUnknown,
				ScopeTier: model.TierModule, ScopeDir: m.dir, Detail: "go.mod unreadable: " + m.err.Error()})
		}
	}
	for _, w := range ms.works {
		a.Roles[w.file] = model.RoleManifest
	}
	b.assign()
	if err := b.analyzePackages(ctx); err != nil {
		return nil, err
	}
	slices.SortFunc(a.Edges, func(x, y model.Edge) int {
		if c := strings.Compare(x.From, y.From); c != 0 {
			return c
		}
		if c := strings.Compare(x.To, y.To); c != 0 {
			return c
		}
		return strings.Compare(string(x.Kind), string(y.Kind))
	})
	slices.SortFunc(a.Tests, func(x, y model.TestUnit) int { return strings.Compare(x.ID, y.ID) })
	return a, nil
}

// remember keeps the module layout of a tree for ClassifyChange (bounded: trees are few per selection).
func (e *Eco) remember(t tree.Tree, ms *modules) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.mods == nil || len(e.mods) >= 8 {
		e.mods = map[tree.Tree]*modules{}
	}
	e.mods[t] = ms
}

func (e *Eco) modulesOf(t tree.Tree) *modules {
	if t == nil {
		return &modules{byDir: map[string]*module{}}
	}
	e.mu.Lock()
	ms, ok := e.mods[t]
	e.mu.Unlock()
	if ok {
		return ms
	}
	ms = loadModules(t)
	e.remember(t, ms)
	return ms
}

type edgeKey struct {
	from, to string
	kind     model.EdgeKind
}

type builder struct {
	t    tree.Tree
	sc   eco.Scanner
	a    *eco.Analysis
	ms   *modules
	seen map[edgeKey]bool
	pkgs map[string]*pkg
	// orphans: module dir → module-level files.
	orphans map[string][]string
	// hidden: directory holding .go files that the go command does not match with ./... (under testdata/, _x/, .x/) → the node
	// that owns them. Such a directory can still be imported by an explicit path.
	hidden map[string]string
	// bins: program name → directories of the main packages `go build` names so (see binaries); nil until first needed.
	bins map[string][]string
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
	if _, ok := b.a.Roles[p]; !ok {
		b.a.Roles[p] = r
	}
}

// skippedDir: a directory component that the go command never treats as a package (and never holds a module of the repository).
func notPackageComponent(c string) bool {
	return c == "testdata" || c == "node_modules" || strings.HasPrefix(c, ".") || strings.HasPrefix(c, "_")
}

// packageDirOK: d may be a package directory (no component is testdata, node_modules, or starts with "." or "_").
func packageDirOK(d string) bool {
	if d == "." {
		return true
	}
	for _, c := range strings.Split(d, "/") {
		if notPackageComponent(c) {
			return false
		}
	}
	return true
}

func underNodeModules(p string) bool {
	return strings.HasPrefix(p, "node_modules/") || strings.Contains(p, "/node_modules/")
}

func isVendorDir(d string) bool {
	return d == "vendor" || strings.HasPrefix(d, "vendor/") || strings.Contains(d, "/vendor/") || strings.HasSuffix(d, "/vendor")
}

// assign finds the package directories and gives every other file of a module to its owner: the nearest ancestor-or-self package
// of the same module, or the module itself.
func (b *builder) assign() {
	files := b.t.Files()
	modOf := map[string]*module{}
	var innermost func(d string) *module
	innermost = func(d string) *module {
		if m, ok := modOf[d]; ok {
			return m
		}
		m := b.ms.byDir[d]
		if m == nil && d != "." && d != "" && d != "/" {
			m = innermost(path.Dir(d))
		}
		modOf[d] = m
		return m
	}
	// Package directories.
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") || underNodeModules(f) {
			continue
		}
		d := path.Dir(f)
		if !packageDirOK(d) {
			continue
		}
		m := innermost(d)
		if m == nil {
			continue // GOPATH layout: ClassifyChange widens
		}
		p := b.pkgs[d]
		if p == nil {
			p = &pkg{dir: d, mod: m, vendor: isVendorDir(strings.TrimPrefix(strings.TrimPrefix(d, m.dir), "/"))}
			b.pkgs[d] = p
		}
		p.gofs = append(p.gofs, f)
		if strings.HasSuffix(f, "_test.go") {
			p.tests = true
		}
	}
	// Owners.
	ownerOf := map[string]*pkg{}
	var owner func(d string, m *module) *pkg
	owner = func(d string, m *module) *pkg {
		if p, ok := ownerOf[d]; ok {
			return p
		}
		var p *pkg
		if q := b.pkgs[d]; q != nil && q.mod == m {
			p = q
		} else if d != m.dir && d != "." {
			p = owner(path.Dir(d), m)
		}
		ownerOf[d] = p
		return p
	}
	foreign := foreignRoots(files)
	b.orphans = map[string][]string{}
	b.hidden = map[string]string{}
	for _, f := range files {
		if underNodeModules(f) {
			continue
		}
		d := path.Dir(f)
		m := innermost(d)
		if m == nil {
			continue
		}
		if q := b.pkgs[d]; q != nil && strings.HasSuffix(f, ".go") {
			continue // a .go file of a package: analyzed with the package
		}
		if b.excluded(f, m) || underForeign(d, foreign) {
			continue
		}
		if strings.HasSuffix(f, ".go") && b.hidden[d] == "" {
			if p := owner(d, m); p != nil {
				b.hidden[d] = LibNode(p.dir)
			} else {
				b.hidden[d] = ModNode(m.dir)
			}
		}
		if p := owner(d, m); p != nil {
			if d == p.dir && skipDoc(f) {
				continue // documentation directly in a package directory: not an input unless something references it
			}
			p.owned = append(p.owned, f)
			continue
		}
		if skipDoc(f) && !hasTestdata(f) {
			continue
		}
		b.orphans[m.dir] = append(b.orphans[m.dir], f)
	}
}

// foreignManifests are the project manifests of the other ecosystems.
var foreignManifests = map[string]bool{"package.json": true, "pyproject.toml": true, "setup.py": true, "setup.cfg": true,
	"Cargo.toml": true, "pom.xml": true, "build.gradle": true, "build.gradle.kts": true, "settings.gradle": true,
	"settings.gradle.kts": true}

func isForeignManifest(base string) bool {
	switch path.Ext(base) {
	case ".csproj", ".fsproj", ".vbproj":
		return true
	}
	return foreignManifests[base]
}

// foreignRoots returns the directories that hold another ecosystem's project manifest and no .go file anywhere below them.
func foreignRoots(files []string) map[string]bool {
	hasGo := map[string]bool{}
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		for d := path.Dir(f); !hasGo[d]; d = path.Dir(d) {
			hasGo[d] = true
			if d == "." || d == "/" || d == "" {
				break
			}
		}
	}
	out := map[string]bool{}
	for _, f := range files {
		if d := path.Dir(f); isForeignManifest(path.Base(f)) && !hasGo[d] && !underNodeModules(f) {
			out[d] = true
		}
	}
	return out
}

// underForeign: directory d lies in the subtree of a foreign root.
func underForeign(d string, roots map[string]bool) bool {
	if len(roots) == 0 {
		return false
	}
	for ; ; d = path.Dir(d) {
		if roots[d] {
			return true
		}
		if d == "." || d == "/" || d == "" {
			return false
		}
	}
}

// excluded: files of a module that are never inputs of a package: go.mod/go.sum/go.work (classified by ClassifyChange) and linter
// configuration.
func (b *builder) excluded(f string, m *module) bool {
	switch base := path.Base(f); {
	case base == "go.mod" || base == "go.sum" || base == "go.work" || base == "go.work.sum":
		return b.ms.byDir[path.Dir(f)] != nil || b.ms.workDirs[path.Dir(f)]
	case isLinterConfig(base):
		return true
	}
	return false
}

func isLinterConfig(base string) bool {
	switch base {
	case ".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json":
		return true
	}
	return false
}

func skipDoc(f string) bool { return classify.Doc(f) || classify.Harmless(f) }

func hasTestdata(f string) bool {
	return strings.HasPrefix(f, "testdata/") || strings.Contains(f, "/testdata/")
}

func (b *builder) analyzePackages(ctx context.Context) error {
	dirs := make([]string, 0, len(b.pkgs))
	for d := range b.pkgs {
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)
	for _, d := range dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.analyzePackage(b.pkgs[d])
	}
	for _, m := range b.ms.list {
		mn := ModNode(m.dir)
		for _, f := range b.orphans[m.dir] {
			b.edge(mn, f, model.EdgeImplicit, "module-level file", 0)
			b.role(f, model.RoleData)
		}
	}
	return nil
}

func (b *builder) analyzePackage(p *pkg) {
	lib, unit := LibNode(p.dir), Node(p.dir)
	if p.tests && !p.vendor {
		b.a.Tests = append(b.a.Tests, model.TestUnit{Eco: Name, ID: UnitID(p.dir), Project: p.mod.dir, Files: []string{unit}, Dir: p.dir,
			Runner: Runner, Package: p.mod.importPath(p.dir)})
	}
	b.edge(unit, lib, model.EdgeImplicit, "go test compiles the package", 0)
	b.edge(lib, ModNode(p.mod.dir), model.EdgeImplicit, "module "+p.mod.dir, 0)
	for _, f := range p.gofs {
		test := strings.HasSuffix(f, "_test.go")
		from := lib
		if test {
			from = unit
			b.role(f, model.RoleTest)
		} else {
			b.role(f, model.RoleSource)
		}
		b.edge(from, f, model.EdgeImplicit, "package file", 0)
		sr, err := b.sc.Scan(b.t, f)
		if err != nil || sr.Uncertain || sr.Go == nil {
			why := "no scan result"
			if err != nil {
				why = err.Error()
			} else if sr.Uncertain {
				why = sr.UncertainReason
			}
			// The file's imports are unknown: any change in its module seeds it (its package and every dependent).
			b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: f, Spec: "<imports>", Class: model.UnresolvedUncertainScan,
				ScopeTier: model.TierModule, ScopeDir: p.mod.dir, Detail: "imports cannot be read: " + why})
			continue
		}
		for _, im := range sr.Imports {
			if im.Spec == "C" {
				b.edge(from, cgoPattern, model.EdgePattern, "cgo", im.Line)
				continue
			}
			b.resolveImport(p, f, from, im, true)
		}
		for _, em := range sr.Go.Embeds {
			b.embed(p, from, em)
		}
		if test {
			b.fixtures(p, unit, sr.Go.Strings)
		} else {
			for _, s := range sr.Go.Strings {
				b.binaryLiteral(p, from, s)
			}
		}
	}
	if p.tests && !p.vendor {
		b.scripts(p, unit)
	}
	for _, f := range p.owned {
		b.edge(lib, f, model.EdgeImplicit, "package file", 0)
		b.role(f, model.RoleData)
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		// A .go file the package does not compile (testdata/, _x/): a test may still build or run it, so what it imports is an
		// input of the package. Unreadable imports cannot compile either: ignored.
		if sr, err := b.sc.Scan(b.t, f); err == nil && !sr.Uncertain && sr.Go != nil {
			for _, im := range sr.Imports {
				if im.Spec != "C" {
					b.resolveImport(p, f, lib, im, false)
				}
			}
		}
	}
}

// resolveImport adds the edges of one import of file f (package p) from node from. compiled: f is a file of the package (not a
// program under testdata/ or _x/).
func (b *builder) resolveImport(p *pkg, f, from string, im model.ImportRef, compiled bool) {
	spec := im.Spec
	cands := b.ms.candidates(spec)
	found := false
	for _, c := range cands {
		if b.pkgs[c] == nil {
			continue
		}
		found = true
		if c != p.dir || from != LibNode(p.dir) { // an external test package importing its own package: unit → library
			b.edge(from, LibNode(c), model.EdgeImport, spec, im.Line)
		}
	}
	for _, c := range cands {
		// A directory the go command skips in patterns but that an explicit import path can name (a package under testdata/):
		// its files belong to its owner, which is the dependency.
		if n := b.hidden[c]; n != "" && b.pkgs[c] == nil {
			found = true
			if n != from {
				b.edge(from, n, model.EdgeImport, spec, im.Line)
			}
		}
	}
	for _, vd := range b.ms.vendorDirs(p.mod) {
		c := path.Join(vd, spec)
		if q := b.pkgs[c]; q != nil && c != p.dir {
			b.edge(from, LibNode(c), model.EdgeImport, spec, im.Line)
			found = true
		}
	}
	if found || len(cands) == 0 {
		return // resolved, or standard library / external module (go.sum covers version changes)
	}
	n := impNode(spec)
	b.edge(from, n, model.EdgeImport, spec, im.Line)
	for _, c := range cands {
		b.edge(n, escapeGlob(c)+"/*.go", model.EdgePattern, "package that does not exist yet", 0)
	}
	if !compiled {
		// A program under testdata/ or _x/: not built by the package, so a missing import cannot be a generated package it needs.
		b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: f, Spec: spec, Class: model.UnresolvedUnknown, Line: im.Line,
			ScopeTier: model.TierNone, Detail: "in-repo import path with no package (" + strings.Join(cands, ", ") +
				"); a package added there later is a dependency"})
		return
	}
	// A package the build needs but the tree does not hold: generated (ignored by git) or broken. Its generator inputs are unknown,
	// so any change in the importer's module, or in the module the package would belong to, selects the importer (fail-closed).
	scopes := []string{p.mod.dir}
	for _, c := range cands {
		d := ""
		if m := b.ms.innermost(c); m != nil {
			d = m.dir
		}
		if !slices.Contains(scopes, d) {
			scopes = append(scopes, d)
		}
	}
	for _, sd := range scopes {
		b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: f, Spec: spec, Class: model.UnresolvedUnknown, Line: im.Line,
			ScopeTier: model.TierModule, ScopeDir: sd, Detail: "in-repo import path with no package in the tree (" + strings.Join(cands, ", ") +
				"): generated or missing; a package added there later is also a dependency"})
	}
}

// embed adds the pattern edges of one //go:embed pattern. A pattern matching a directory embeds it recursively.
func (b *builder) embed(p *pkg, from string, em model.GoEmbed) {
	if em.Unknown != "" || em.Pattern == "" {
		b.edge(from, dirGlob(p.dir), model.EdgePattern, "//go:embed (unparsed: "+em.Unknown+")", em.Line)
		return
	}
	pat := strings.TrimPrefix(em.Pattern, "all:")
	g := embedGlob(pat)
	if p.dir != "." {
		g = escapeGlob(p.dir) + "/" + g
	}
	spec := "//go:embed " + em.Pattern
	b.edge(from, g, model.EdgePattern, spec, em.Line)
	b.edge(from, g+"/**", model.EdgePattern, spec, em.Line)
}

// embedGlob converts a path.Match pattern (embed syntax) to the glob package's syntax: * ? [..] and \ keep their meaning, braces are
// literal.
func embedGlob(pat string) string {
	var sb strings.Builder
	for _, c := range pat {
		if c == '{' || c == '}' {
			sb.WriteByte('\\')
		}
		sb.WriteRune(c)
	}
	return sb.String()
}

// fixtures adds the edges of the string literals of a test file. Each literal, and each of its white-space separated tokens (a
// literal may hold a whole command line, "go run ./cmd/x"), is tried as:
//
//   - a path relative to the package directory (go test runs in it) naming an existing file or directory: a fixture_data edge (a
//     pattern edge for a directory). The package's own directory and its ancestors are not data references;
//   - a package directory ("../cmd/x", as in `go build ../cmd/x` or `go run`): also the package's library node, so the whole closure
//     of the program the test builds is an input; "dir/..." (a go command package pattern) names every package below dir;
//   - an in-repo import path ("example.com/m/cmd/x", "example.com/m/cmd/..."): the library node of the package(s) it names.
func (b *builder) fixtures(p *pkg, unit string, strs []string) {
	for _, s := range strs {
		s = strings.ReplaceAll(s, "\\", "/")
		b.pathLiteral(p, unit, s)
		toks := strings.Fields(s)
		if len(toks) > 1 {
			for _, tok := range toks {
				b.pathLiteral(p, unit, tok)
			}
		}
		for _, tok := range toks {
			b.importLiteral(unit, tok)
		}
	}
}

// pathLiteral handles one literal (or token) as a path relative to the package directory.
func (b *builder) pathLiteral(p *pkg, unit, s string) {
	if s == "" || strings.HasPrefix(s, "/") || strings.Contains(s, "://") || (len(s) > 1 && s[1] == ':') {
		return
	}
	all := false
	if s == "..." {
		s, all = ".", true
	} else if t, ok := strings.CutSuffix(s, "/..."); ok {
		s, all = t, true
	}
	target := path.Join(p.dir, s)
	if target == ".." || strings.HasPrefix(target, "../") {
		return
	}
	if all {
		b.packagesBelow(unit, target, p.dir, s+"/...")
		return
	}
	if eco.Within(p.dir, target) {
		return
	}
	b.binaryLiteral(p, unit, s)
	switch {
	case b.t.Has(target):
		b.edge(unit, target, model.EdgeFixtureData, s, 0)
	case b.t.IsDir(target):
		b.edge(unit, dirGlob(target), model.EdgePattern, "fixture directory "+s, 0)
		if b.pkgs[target] != nil {
			b.edge(unit, LibNode(target), model.EdgeImplicit, "test may build package "+s, 0)
		}
	}
}

// binaryLiteral handles a relative path literal that names no file or directory of the tree but whose last element is the name of
// a program built from the repository ("../../../alertmanager", "./bin/amtool.exe"; not a bare "./x"), typically built by a Makefile before the
// tests run): from (a test unit, or the library node of a helper package that runs the program for the tests) depends on the
// closure of every main package `go build` would name so. The literal's directory is not resolved: a helper runs it relative to
// the directory of whichever test calls it.
func (b *builder) binaryLiteral(p *pkg, from, s string) {
	// "../x" (up from the package directory) or "./dir/x"; a bare "./x" is as often a working-directory data path ("./caddy").
	if rest, ok := strings.CutPrefix(s, "./"); ok && !strings.Contains(strings.TrimPrefix(rest, "./"), "/") {
		return
	} else if !ok && !strings.HasPrefix(s, "../") {
		return
	}
	if t := path.Join(p.dir, s); b.t.Has(t) || b.t.IsDir(t) {
		return
	}
	name := strings.TrimSuffix(path.Base(s), ".exe")
	if name == "" || name == "." || name == ".." {
		return
	}
	for _, d := range b.binaries()[name] {
		if d != p.dir {
			b.edge(from, LibNode(d), model.EdgeImplicit, "may run the program built from "+d+" ("+s+")", 0)
		}
	}
}

// binaries maps a program name to the directories of the non-vendored main packages `go build` names so: the directory's last
// element, or for a module's root directory the last element of the module path (before a /vN major-version suffix).
func (b *builder) binaries() map[string][]string {
	if b.bins != nil {
		return b.bins
	}
	b.bins = map[string][]string{}
	for d, q := range b.pkgs {
		if q.vendor {
			continue
		}
		for _, f := range q.gofs {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			sr, err := b.sc.Scan(b.t, f)
			if err != nil || sr.Go == nil {
				continue
			}
			if sr.Go.Package == "main" {
				name := path.Base(d)
				if d == q.mod.dir && q.mod.path != "" {
					elems := strings.Split(q.mod.path, "/")
					name = elems[len(elems)-1]
					if len(elems) > 1 && len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
						name = elems[len(elems)-2]
					}
				}
				b.bins[name] = append(b.bins[name], d)
			}
			break
		}
	}
	for _, ds := range b.bins {
		slices.Sort(ds)
	}
	return b.bins
}

// importLiteral handles one token as an in-repo import path or "<import path>/..." pattern.
func (b *builder) importLiteral(unit, tok string) {
	if tok == "" || strings.ContainsAny(tok, "@\\") || tok[0] == '.' || tok[0] == '/' || tok[0] == '-' || !strings.Contains(tok, "/") {
		return
	}
	pat, all := strings.CutSuffix(tok, "/...")
	for _, c := range b.ms.candidates(pat) {
		if all {
			b.packagesBelow(unit, c, "", tok)
		} else if b.pkgs[c] != nil && !b.pkgs[c].vendor {
			b.edge(unit, LibNode(c), model.EdgeImplicit, "test may build package "+tok, 0)
		}
	}
}

// packagesBelow makes unit depend on the library node of every non-vendored package in directory dir or below it, except self.
func (b *builder) packagesBelow(unit, dir, self, spec string) {
	for d, q := range b.pkgs {
		if eco.Within(d, dir) && d != self && !q.vendor {
			b.edge(unit, LibNode(d), model.EdgeImplicit, "test builds packages "+spec, 0)
		}
	}
}

// scriptExt: testscript files (txtar archives of shell-like commands run by rsc.io/script / testscript).
func isScript(f string) bool {
	switch path.Ext(f) {
	case ".txtar":
		return true
	case ".txt":
		switch path.Base(path.Dir(f)) {
		case "script", "scripts", "testscript", "testscripts":
			return true
		}
	}
	return false
}

// scripts reads the testscript files under testdata/ owned by package p: the in-repo import paths named on `go ...` and `exec ...`
// command lines make the unit depend on those packages (a script may build and run them).
func (b *builder) scripts(p *pkg, unit string) {
	for _, f := range p.owned {
		if !hasTestdata(f) || !isScript(f) {
			continue
		}
		src, err := b.t.Read(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(src), "\n") {
			fs := strings.Fields(line)
			for len(fs) > 0 && (fs[0] == "!" || fs[0] == "?" || strings.HasPrefix(fs[0], "[")) {
				fs = fs[1:]
			}
			if len(fs) < 2 || (fs[0] != "go" && fs[0] != "exec") {
				continue
			}
			for _, tok := range fs[1:] {
				b.importLiteral(unit, strings.Trim(tok, "'\""))
			}
		}
	}
}

// dirGlob matches every file under directory d.
func dirGlob(d string) string {
	if d == "." {
		return "**"
	}
	return escapeGlob(d) + "/**"
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

// module is one go.mod.
type module struct {
	dir   string
	gomod string
	path  string // module path ("" when unreadable)
	mf    *modFile
	err   error
}

func (m *module) importPath(dir string) string {
	if m.path == "" {
		return ""
	}
	if dir == m.dir {
		return m.path
	}
	rel := dir
	if m.dir != "." {
		rel = strings.TrimPrefix(dir, m.dir+"/")
	}
	return m.path + "/" + rel
}

// work is one go.work.
type work struct {
	file string
	dir  string
	mf   *modFile
	err  error
	uses []string // module directories
}

// prefix maps an import-path prefix to a directory of the repository.
type prefix struct {
	path, dir string
}

type modules struct {
	list     []*module
	byDir    map[string]*module
	works    []*work
	workDirs map[string]bool
	prefixes []prefix
}

// moduleDirOK: a go.mod / go.work in directory d is a module (workspace) of the repository: no component is vendor, testdata,
// node_modules, or starts with "." or "_".
func moduleDirOK(d string) bool {
	if d == "." {
		return true
	}
	for _, c := range strings.Split(d, "/") {
		if c == "vendor" || notPackageComponent(c) {
			return false
		}
	}
	return true
}

func loadModules(t tree.Tree) *modules {
	ms := &modules{byDir: map[string]*module{}, workDirs: map[string]bool{}}
	for _, f := range t.Files() {
		base := path.Base(f)
		if base != "go.mod" && base != "go.work" {
			continue
		}
		d := path.Dir(f)
		if !moduleDirOK(d) {
			continue
		}
		src, err := t.Read(f)
		var mf *modFile
		if err == nil {
			mf, err = parseModFile(src)
		}
		if base == "go.work" {
			w := &work{file: f, dir: d, mf: mf, err: err}
			ms.works = append(ms.works, w)
			ms.workDirs[d] = true
			continue
		}
		m := &module{dir: d, gomod: f, mf: mf, err: err}
		if err == nil {
			m.path = mf.Module()
			if m.path == "" {
				m.err = fmt.Errorf("no module directive")
			}
		}
		ms.list = append(ms.list, m)
		ms.byDir[d] = m
	}
	for _, m := range ms.list {
		if m.path != "" {
			ms.prefixes = append(ms.prefixes, prefix{m.path, m.dir})
		}
		if m.mf != nil {
			for _, r := range m.mf.LocalReplaces() {
				if d, ok := localDir(m.dir, r.dir); ok {
					ms.prefixes = append(ms.prefixes, prefix{r.old, d})
				}
			}
		}
	}
	for _, w := range ms.works {
		if w.mf == nil {
			continue
		}
		for _, u := range w.mf.Uses() {
			if d, ok := localDir(w.dir, u); ok {
				w.uses = append(w.uses, d)
			}
		}
		for _, r := range w.mf.LocalReplaces() {
			if d, ok := localDir(w.dir, r.dir); ok {
				ms.prefixes = append(ms.prefixes, prefix{r.old, d})
			}
		}
	}
	return ms
}

// candidates returns the directories import path spec may name inside the repository: one per module path / local replace that is a
// prefix of it (the union, so ambiguity over-selects).
func (ms *modules) candidates(spec string) []string {
	var out []string
	for _, pr := range ms.prefixes {
		rest, ok := strings.CutPrefix(spec, pr.path)
		if !ok || (rest != "" && rest[0] != '/') {
			continue
		}
		d := path.Join(pr.dir, strings.TrimPrefix(rest, "/"))
		if d == ".." || strings.HasPrefix(d, "../") || slices.Contains(out, d) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// vendorDirs returns the vendor directories that apply to module m: its own and those of the workspaces that use it.
func (ms *modules) vendorDirs(m *module) []string {
	out := []string{path.Join(m.dir, "vendor")}
	for _, w := range ms.works {
		if slices.Contains(w.uses, m.dir) {
			out = append(out, path.Join(w.dir, "vendor"))
		}
	}
	return out
}

// innermost returns the module containing p (a file or directory path), nil when none.
func (ms *modules) innermost(p string) *module {
	for d := p; ; d = path.Dir(d) {
		if m := ms.byDir[d]; m != nil {
			return m
		}
		if d == "." || d == "/" || d == "" {
			return nil
		}
	}
}

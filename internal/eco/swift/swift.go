// Package swift models SwiftPM package and target ownership without executing
// a Swift manifest. IDs are swift:<repo-relative-dir>::test:<target> and
// swift:<repo-relative-dir>::all for an opaque package.
package swift

import (
	"context"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/classify"
	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

const (
	Runner          = "swift"
	AnalyzerVersion = "swift-7"
)

type Eco struct{}

func New() *Eco              { return &Eco{} }
func (*Eco) Name() string    { return Runner }
func (*Eco) Version() string { return AnalyzerVersion }

func UnitID(dir, target string) string {
	if dir == "" {
		dir = "."
	}
	if target == "" {
		return "swift:" + encodeIDPart(dir) + "::all"
	}
	return "swift:" + encodeIDPart(dir) + "::test:" + encodeIDPart(target)
}

func ParseUnitID(id string) (dir, target string, ok bool) {
	body, valid := strings.CutPrefix(id, "swift:")
	if !valid {
		return "", "", false
	}
	// The separator may also occur in a directory name. Escape it in
	// either path component before constructing an ID.
	if d, found := strings.CutSuffix(body, "::all"); found {
		return decodeIDPart(d), "", d != ""
	}
	i := strings.LastIndex(body, "::test:")
	if i <= 0 || i+len("::test:") == len(body) {
		return "", "", false
	}
	return decodeIDPart(body[:i]), decodeIDPart(body[i+len("::test:"):]), true
}

var encodeID = strings.NewReplacer("%", "%25", ":", "%3A")
var decodeID = strings.NewReplacer("%25", "%", "%3A", ":")

func encodeIDPart(s string) string { return encodeID.Replace(s) }
func decodeIDPart(s string) string { return decodeID.Replace(s) }

func UnitNode(dir, target string) string { return "<" + UnitID(dir, target) + ">" }
func targetNode(dir string, t target) string {
	if t.kind == "testTarget" {
		return UnitNode(dir, t.name)
	}
	return "<swift:" + encodeIDPart(dir) + "::target:" + encodeIDPart(t.name) + ">"
}
func libNode(dir string) string    { return "<swift:" + encodeIDPart(dir) + "::library>" }
func sharedNode(dir string) string { return "<swift:" + encodeIDPart(dir) + "::shared>" }

type project struct {
	dir, primary  string
	m             *manifest
	manifestFiles []string
	opaque        string
	whole         bool
	native        bool
	runtimeTools  bool
	deps          []string
	missing       []string
	targets       map[string]target
	testModules   []string
}

var variant = regexp.MustCompile(`^Package@swift-[0-9]+(?:\.[0-9]+){0,2}\.swift$`)
var filterable = regexp.MustCompile("^[A-Za-z_][A-Za-z0-9_]*$")

func isManifest(file string) bool {
	n := path.Base(file)
	return n == "Package.swift" || variant.MatchString(n)
}

func loadProjects(t tree.Tree) map[string]*project {
	if t == nil {
		return nil
	}
	var projects map[string]*project
	record := func(file string) {
		if !isManifest(file) {
			return
		}
		if projects == nil {
			projects = map[string]*project{}
		}
		dir := path.Dir(file)
		p := projects[dir]
		if p == nil {
			p = &project{dir: dir, primary: path.Join(dir, "Package.swift")}
			projects[dir] = p
		}
		if !slices.Contains(p.manifestFiles, file) {
			p.manifestFiles = append(p.manifestFiles, file)
		}
	}
	for _, file := range t.Files() {
		record(file)
	}
	for _, link := range tree.LinksOf(t) {
		record(link.Path)
	}
	for _, p := range projects {
		for _, file := range p.manifestFiles {
			if file != p.primary {
				p.opaque = "toolchain-specific Swift manifest " + file
				break
			}
		}
		src, err := t.Read(p.primary)
		if err == nil {
			p.m, err = parseManifest(src)
		}
		if err != nil && p.opaque == "" {
			p.opaque = err.Error()
		}
		if p.m == nil {
			continue
		}
		p.targets = make(map[string]target, len(p.m.targets))
		for _, tg := range p.m.targets {
			p.targets[tg.name] = tg
			if tg.kind == "testTarget" {
				p.testModules = append(p.testModules, tg.name)
			}
		}
		slices.Sort(p.testModules)
		p.whole = p.m.whole
		for _, tg := range p.m.targets {
			if tg.path != "" {
				if d, ok := resolvePath(p.dir, tg.path); !ok || !eco.Within(d, p.dir) {
					p.opaque = "unsupported Swift target path " + tg.path
				}
			}
		}
		for _, local := range p.m.local {
			d, ok := resolvePath(p.dir, local)
			switch {
			case !ok:
				p.missing = append(p.missing, ".")
			case projects[d] != nil:
				p.deps = append(p.deps, d)
			default:
				found := false
				for _, f := range t.FoldLookup(path.Join(d, "Package.swift")) {
					if q := projects[path.Dir(f)]; q != nil {
						p.deps = append(p.deps, q.dir)
						found = true
					}
				}
				if !found {
					p.missing = append(p.missing, d)
				}
			}
		}
	}
	return projects
}

func resolvePath(dir, rel string) (string, bool) {
	if rel == "" || path.IsAbs(rel) || strings.ContainsAny(rel, "\x00\\") || strings.HasPrefix(rel, "~") || strings.Contains(rel, ":") {
		return "", false
	}
	d := path.Join(dir, rel)
	return d, d != ".." && !strings.HasPrefix(d, "../")
}

func nearest(projects map[string]*project, dir string) *project {
	for ; ; dir = path.Dir(dir) {
		if p := projects[dir]; p != nil {
			return p
		}
		if dir == "." {
			return nil
		}
	}
}

type builder struct {
	t        tree.Tree
	sc       eco.Scanner
	packages map[string]*project
	a        *eco.Analysis
	files    map[string][]string
	foreign  map[string]bool
	dirFold  map[string][]string
	// reported holds the target inputs already recorded as unresolved, so a
	// declaration resolved against several candidate roots is reported once.
	reported map[string]bool
}

func (*Eco) Analyze(ctx context.Context, t tree.Tree, sc eco.Scanner) (*eco.Analysis, error) {
	a := &eco.Analysis{}
	ps := loadProjects(t)
	if len(ps) == 0 {
		return a, nil
	}
	a.Roles = map[string]model.Role{}
	b := builder{t: t, sc: sc, packages: ps, a: a, files: map[string][]string{}, foreign: map[string]bool{}, dirFold: map[string][]string{}, reported: map[string]bool{}}
	b.groupFiles()
	dirs := make([]string, 0, len(ps))
	for d := range ps {
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)
	for _, d := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.analyze(ps[d])
	}
	return a, nil
}

func foreignManifest(file string) bool {
	switch path.Base(file) {
	case "package.json", "Cargo.toml", "go.mod", "pyproject.toml", "composer.json", "pom.xml", "build.gradle", "build.gradle.kts":
		return true
	}
	switch path.Ext(file) {
	case ".csproj", ".fsproj", ".vbproj":
		return true
	}
	return false
}

func (b *builder) groupFiles() {
	seenDirs := map[string]bool{}
	for _, f := range b.t.Files() {
		dir := path.Dir(f)
		for d := dir; d != "." && !seenDirs[d]; d = path.Dir(d) {
			seenDirs[d] = true
			key := strings.ToLower(d)
			b.dirFold[key] = append(b.dirFold[key], d)
		}
		p := nearest(b.packages, dir)
		if p == nil {
			if path.Ext(f) == ".swift" {
				b.a.Roles[f] = model.RoleSource
			}
			continue
		}
		b.files[p.dir] = append(b.files[p.dir], f)
		if path.Base(f) == "main.swift" || (path.Ext(f) == ".swift" && eco.Within(f, path.Join(p.dir, "Snippets"))) {
			p.runtimeTools = true
		}
		if dir != p.dir && foreignManifest(f) {
			b.foreign[dir] = true
		}
		if path.Base(f) == "LinuxMain.swift" || path.Base(f) == "XCTMain.swift" {
			p.whole = true
		}
	}
	for _, fs := range b.files {
		for _, f := range fs {
			if path.Ext(f) == ".swift" {
				for d := path.Dir(f); b.packages[d] == nil; d = path.Dir(d) {
					delete(b.foreign, d)
				}
			}
		}
	}
}

func (b *builder) foreignFile(p *project, f string) bool {
	for d := path.Dir(f); d != p.dir; d = path.Dir(d) {
		if b.foreign[d] {
			return true
		}
	}
	return false
}

func (b *builder) analyze(p *project) {
	dir := p.dir
	shared := sharedNode(dir)
	lib := libNode(dir)
	var name string
	if p.m != nil {
		name = p.m.name
	}
	b.a.Projects = append(b.a.Projects, eco.Project{Eco: Runner, Dir: dir, Name: name})
	for _, f := range p.manifestFiles {
		b.a.Roles[f] = model.RoleManifest
	}
	b.edge(lib, shared, model.EdgeImplicit, "Swift package inputs")
	for _, d := range p.deps {
		b.edge(shared, libNode(d), model.EdgeProjectRef, "local Swift package dependency")
	}
	for _, d := range p.missing {
		b.unresolved(shared, d, d, model.UnresolvedUnknown, "local Swift dependency is missing or outside the Git tree")
	}
	if p.opaque != "" {
		b.addTest(p, target{}, dir)
		manifest := p.primary
		if !b.t.Has(manifest) && len(p.manifestFiles) > 0 {
			manifest = p.manifestFiles[0]
		}
		b.edge(shared, manifest, model.EdgeImplicit, "opaque Swift manifest")
		b.unresolved(manifest, "PackageDescription", ".", model.UnresolvedDynamic, model.ReasonConfigIsCode+": "+p.opaque)
		b.a.Warnings = append(b.a.Warnings, model.Widening{Reason: model.ReasonConfigIsCode, Path: manifest, Scope: dir, Eco: Runner, Exact: true, Detail: p.opaque})
		for _, f := range b.files[dir] {
			if !isManifest(f) {
				b.ownCommon(p, f)
			}
		}
		return
	}

	for _, tg := range p.m.targets {
		node := targetNode(dir, tg)
		if tg.kind == "testTarget" {
			roots := b.targetRoots(p, tg)
			root := roots[0]
			for _, d := range roots {
				if b.t.IsDir(d) {
					root = d
					break
				}
			}
			b.addTest(p, tg, root)
		} else {
			b.edge(lib, node, model.EdgeImplicit, "Swift package target "+tg.name)
		}
		b.edge(node, shared, model.EdgeImplicit, "Swift package inputs")
		for _, dep := range tg.deps {
			if d, ok := p.targets[dep]; ok {
				b.edge(node, targetNode(dir, d), model.EdgeProjectRef, "Swift target dependency "+dep)
			}
		}
	}

	claimed := map[string]bool{}
	for _, tg := range p.m.targets {
		b.ownTarget(p, tg, claimed)
	}
	for _, f := range b.files[dir] {
		if claimed[f] || isManifest(f) || b.foreignFile(p, f) {
			continue
		}
		if swiftSpecial(f, dir) {
			b.ownCommon(p, f)
		} else if !classify.Harmless(f) && (!classify.Doc(f) || runtimeDataPath(f, dir)) {
			b.ownCommon(p, f)
		}
	}
	if p.m.opaqueTool || p.m.whole || p.native {
		// Plugins, macros and native/binary inputs can read files outside their
		// target directories. Every target also sees the package build closure.
		b.unresolved(shared, "Swift build tool", ".", model.UnresolvedDynamic, "Swift plugin, macro, native or binary build input")
		for _, tg := range p.m.targets {
			if tg.kind == "testTarget" {
				b.edge(targetNode(dir, tg), lib, model.EdgeProjectRef, "Swift build tool closure")
			}
		}
	}
}

func (b *builder) addTest(p *project, tg target, dir string) {
	node := UnitNode(p.dir, tg.name)
	whole := p.opaque != "" || p.whole || !filterable.MatchString(tg.name)
	var modules []string
	if !whole {
		modules = p.testModules
	}
	b.a.Tests = append(b.a.Tests, model.TestUnit{
		Eco: Runner, ID: UnitID(p.dir, tg.name), Project: p.dir, Package: tg.name, Runner: Runner, Dir: dir,
		Files: []string{node}, Whole: whole, TestModules: modules,
	})
	b.edge(node, sharedNode(p.dir), model.EdgeImplicit, "Swift package inputs")
}

func (b *builder) ownCommon(p *project, file string) {
	switch {
	case path.Base(file) == "Package.resolved" && packageLock(file, p.dir):
		b.a.Roles[file] = model.RoleLock
		return
	case swiftSpecial(file, p.dir):
		b.a.Roles[file] = model.RoleConfig
		return
	case path.Ext(file) == ".swift":
		b.a.Roles[file] = model.RoleSource
	default:
		b.a.Roles[file] = model.RoleData
	}
	b.edge(sharedNode(p.dir), file, model.EdgeImplicit, "unowned Swift package input")
	if path.Ext(file) == ".swift" {
		b.references(p, file, sharedNode(p.dir))
	}
}

func (b *builder) unresolved(file, spec, dir string, class model.UnresolvedClass, detail string) {
	b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{
		File: file, Spec: spec, Class: class, ScopeTier: model.TierGraph, ScopeDir: dir, Detail: detail,
	})
}

func (b *builder) edge(from, to string, kind model.EdgeKind, spec string) {
	if from != to {
		b.a.Edges = append(b.a.Edges, model.Edge{From: from, To: to, Kind: kind, Spec: spec, Provenance: model.ProvScan})
	}
}

func (b *builder) targetRoots(p *project, t target) []string {
	if t.path != "" {
		d := path.Join(p.dir, t.path)
		return append([]string{d}, b.alternateDirs(d)...)

	}
	pools := []string{"Sources", "Source", "src", "srcs"}
	switch t.kind {
	case "testTarget":
		pools = append([]string{"Tests"}, pools...)
	case "plugin":
		pools = []string{"Plugins"}
	}

	var roots []string
	for _, pool := range pools {
		root := path.Join(p.dir, pool, t.name)
		roots = append(roots, root)
		roots = append(roots, b.alternateDirs(root)...)
	}

	// SwiftPM selects the first existing source pool. A single target
	// of the corresponding kind may occupy that pool when no directory
	// bears its name, including a test target in a non-Tests source pool.
	if p.m.flatLayout && singleInPool(p, t) {
		for _, pool := range pools {
			flat := path.Join(p.dir, pool)
			alternates := b.alternateDirs(flat)
			if !b.t.IsDir(flat) && len(alternates) == 0 {
				continue
			}
			if !b.t.IsDir(path.Join(flat, t.name)) {
				roots = append(append([]string{flat}, alternates...), roots...)
			}
			break
		}
	}
	return roots
}

func (b *builder) alternateDirs(dir string) []string {
	var out []string
	for _, d := range b.dirFold[strings.ToLower(dir)] {
		if d != dir {
			out = append(out, d)
		}
	}
	return out
}

func singleInPool(p *project, t target) bool {
	count := 0
	for _, other := range p.m.targets {
		samePool := sourcePool(t.kind) == sourcePool(other.kind)
		if samePool {
			count++
		}
	}
	return count == 1
}

func sourcePool(kind string) string {
	switch kind {
	case "testTarget", "plugin":
		return kind
	default:
		return "Sources"
	}
}

func (b *builder) ownTarget(p *project, tg target, claimed map[string]bool) {
	node := targetNode(p.dir, tg)
	for _, d := range b.targetRoots(p, tg) {
		excludes := b.paths(node, d, tg.excludes)
		sources := b.paths(node, d, tg.sources)
		for _, source := range sources {
			if !eco.Within(source, d) {
				b.referencePath(node, source, "declared Swift target source")
			}
		}
		for _, r := range b.paths(node, d, tg.resources) {
			b.referencePath(node, r, "declared Swift target resource")
			b.referencePath(sharedNode(p.dir), r, "Swift package runtime resource")
		}
		// SwiftPM walks target roots recursively. A nested package or foreign
		// manifest does not partition the sources of the enclosing target.
		for _, f := range filesBelow(b.t, d) {
			if (path.Dir(f) == p.dir && isManifest(f)) || swiftSpecial(f, p.dir) || underAny(f, excludes) {
				continue
			}
			if implicitResource(f, d) {
				claimed[f] = true
				b.edge(node, f, model.EdgeFixtureData, "implicit Swift target resource")
				b.edge(sharedNode(p.dir), f, model.EdgeFixtureData, "Swift package runtime resource")
				if b.a.Roles[f] == "" {
					b.a.Roles[f] = model.RoleData
				}
				continue
			}
			if tg.sources != nil && !underAny(f, sources) {
				continue
			}
			if path.Ext(f) != ".swift" && !nativeSource(f) {
				continue
			}
			claimed[f] = true
			b.edge(node, f, model.EdgeImplicit, "Swift target "+tg.name)
			if path.Ext(f) == ".swift" {
				// Legacy manually curated test entry points can alter XCTest IDs.
				b.references(p, f, node)
			} else {
				b.edge(sharedNode(p.dir), f, model.EdgeImplicit, "Swift native build input")
				p.native = true
			}
			if tg.kind == "testTarget" {
				b.a.Roles[f] = model.RoleTest
			} else if b.a.Roles[f] != model.RoleTest {
				b.a.Roles[f] = model.RoleSource
			}
		}
	}
}

func (b *builder) paths(node, base string, rels []string) []string {
	var out []string
	for _, rel := range rels {
		if f, ok := resolvePath(base, rel); ok {
			out = append(out, f)
		} else if key := node + "\n" + rel; !b.reported[key] {
			b.reported[key] = true
			b.unresolved(node, rel, ".", model.UnresolvedUnknown, "Swift target input is outside the Git tree")
		}
	}
	return out
}

func underAny(file string, roots []string) bool {
	for _, d := range roots {
		if eco.Within(file, d) {
			return true
		}
	}
	return false
}

func filesBelow(t tree.Tree, dir string) []string {
	files := t.Files()
	if dir == "." {
		return files
	}
	prefix := dir + "/"
	start, _ := slices.BinarySearch(files, prefix)
	end := start
	for end < len(files) && strings.HasPrefix(files[end], prefix) {
		end++
	}
	return files[start:end]
}

func runtimeDataPath(f, dir string) bool {
	for _, root := range []string{"Tests", "TestsData", "TestData", "testdata", "Fixtures", "fixtures", "Resources", "Sources"} {
		if eco.Within(f, path.Join(dir, root)) {
			return true
		}
	}
	return false
}

func nativeSource(f string) bool {
	switch path.Ext(f) {
	case ".h", ".hh", ".hpp", ".h++", ".hp", ".hxx", ".H", ".ipp", ".def", ".apinotes", ".modulemap",
		".m", ".mm", ".c", ".C", ".cc", ".cpp", ".cxx", ".c++", ".s", ".S":
		return true
	}
	return false
}

// SwiftPM processes localized folders and some resource types implicitly.
// Include the Apple build system's rules even when the analysis runs elsewhere.
func implicitResource(f, root string) bool {
	for ; f != root; f = path.Dir(f) {
		switch strings.ToLower(path.Ext(f)) {
		case ".lproj", ".nib", ".xib", ".storyboard", ".xcassets", ".xcstrings", ".xcdatamodeld",
			".xcdatamodel", ".xcmappingmodel", ".metal", ".xcprivacy":
			return true
		}
	}
	return false
}

func swiftSpecial(f, dir string) bool {
	// Only the package's own lockfile is special. Any other Package.resolved
	// (a test fixture, for instance) is ordinary package data.
	if path.Base(f) == "Package.resolved" && packageLock(f, dir) {
		return true
	}
	prefix := path.Join(dir, ".swiftpm")
	return eco.Within(f, prefix) || path.Ext(f) == ".xctestplan"
}

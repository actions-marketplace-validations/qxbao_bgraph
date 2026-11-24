package cpp

import (
	"context"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/classify"
	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	scan "github.com/qxbao/bgraph/internal/scan/cpp"
	"github.com/qxbao/bgraph/internal/tree"
)

const AnalyzerVersion = "cpp-4"

type Eco struct{}

func New() *Eco              { return &Eco{} }
func (*Eco) Name() string    { return Name }
func (*Eco) Version() string { return AnalyzerVersion }

func manifestName(system string) string {
	if system == CMake {
		return "CMakeLists.txt"
	}
	return "meson.build"
}

func manifestSystem(file string) string {
	switch path.Base(file) {
	case "CMakeLists.txt":
		return CMake
	case "meson.build":
		return Meson
	default:
		return ""
	}
}

func discover(t tree.Tree) []*project {
	if t == nil {
		return nil
	}
	scripts := map[string]map[string]script{CMake: {}, Meson: {}}
	read := func(file string) {
		system := manifestSystem(file)
		if system == "" {
			return
		}
		dir := path.Dir(file)
		if _, ok := scripts[system][dir]; ok {
			return
		}
		b, err := t.Read(file)
		s := script{file: file, err: err}
		if err == nil {
			if system == CMake {
				s.calls, s.err = scan.CMake(b)
			} else {
				s.calls, s.err = scan.Meson(b)
			}
		}
		scripts[system][dir] = s
	}
	for _, f := range t.Files() {
		read(f)
	}
	for _, l := range tree.LinksOf(t) {
		read(l.Path)
	}

	included := map[string]bool{}
	for dir, s := range scripts[CMake] {
		for _, call := range s.calls {
			if call.Name == "add_subdirectory" && len(call.Args) > 0 {
				if child, ok := local(dir, call.Args[0].Text); ok && child != dir && eco.Within(child, dir) {
					included[child] = true
				}
			}
		}
	}
	var projects []*project
	for _, system := range []string{CMake, Meson} {
		for dir, script := range scripts[system] {
			if system == CMake && included[dir] && !slices.ContainsFunc(script.calls, func(c scan.Call) bool {
				return c.Name == "cmake_minimum_required"
			}) {
				// A nested project() also changes CMake's variable scope. An
				// included list needs independent setup to be a runner root.
				continue
			}
			if !isRoot(system, dir, script, scripts[system]) {
				continue
			}
			p := &project{system: system, dir: dir, scripts: scripts[system],
				visited: map[string]bool{}, targets: map[string]*target{}, tests: map[string]string{}, mesonValues: map[string][]string{}}
			p.read(dir)
			p.finish(t)
			if system == CMake && p.opaque == "" && len(p.tests) > 0 {
				var enabled bool
				for _, c := range script.calls {
					enabled = enabled || c.Name == "enable_testing"
				}
				if !enabled {
					p.fail(script.file, 1, "CTest enable_testing was not proved at the build root")
				}
			}
			projects = append(projects, p)
		}
	}
	slices.SortFunc(projects, func(a, b *project) int {
		return strings.Compare(a.dir+"\x00"+a.system, b.dir+"\x00"+b.system)
	})
	return projects
}

func isRoot(system, dir string, s script, scripts map[string]script) bool {
	// Meson requires project() to be the first statement. An unreadable
	// top-level file can still represent a suite and needs a fallback.
	if system == Meson {
		return len(s.calls) > 0 && s.calls[0].Name == "project" || dir == "." && s.err != nil
	}
	// CMake supplies an implicit project() when configuring a top-level
	// list file. Nested lists may also declare a separate project.
	if dir == "." || slices.ContainsFunc(s.calls, func(c scan.Call) bool { return c.Name == "project" }) {
		return true
	}
	for d := path.Dir(dir); ; d = path.Dir(d) {
		if _, ok := scripts[d]; ok {
			return false
		}
		if d == "." {
			return true
		}
	}
}

func unitNode(system, dir, name string) string {
	return "<" + UnitID(system, dir, name) + ">"
}

func (p *project) node(kind, name string) string {
	return "<" + Name + ":" + p.system + ":" + encodeID.Replace(p.dir) + "::" + kind + ":" + encodeID.Replace(name) + ">"
}

type builder struct {
	t            tree.Tree
	a            *eco.Analysis
	roots        map[string]map[string]*project
	owned        map[*project]map[string]bool
	foreign      map[string]bool
	includeFiles map[string][]string
}

func (*Eco) Analyze(ctx context.Context, t tree.Tree, sc eco.Scanner) (*eco.Analysis, error) {
	a := &eco.Analysis{}
	projects := discover(t)
	if len(projects) == 0 {
		return a, nil
	}
	a.Roles = map[string]model.Role{}
	b := builder{t: t, a: a, roots: map[string]map[string]*project{CMake: {}, Meson: {}}, owned: map[*project]map[string]bool{}, foreign: foreignRoots(t), includeFiles: map[string][]string{}}
	for _, p := range projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.roots[p.system][p.dir] = p
		b.build(p)
	}
	for _, f := range t.Files() {
		key := strings.ToLower(path.Base(f))
		b.includeFiles[key] = append(b.includeFiles[key], f)
	}
	for _, f := range t.Files() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, system := range []string{CMake, Meson} {
			if p := b.owner(system, f); p != nil {
				b.common(p, f)
			}
		}
		if scan.Native(f) {
			b.claim(f, model.RoleSource)
			b.includes(sc, f)
		}
	}
	return a, nil
}

func (b *builder) claim(file string, role model.Role) {
	old := b.a.Roles[file]
	if old == "" || old == model.RoleData || role == model.RoleTest || role == model.RoleManifest {
		b.a.Roles[file] = role
	}
}

func (b *builder) edge(from, to string, kind model.EdgeKind, spec string) {
	if from != to {
		b.a.Edges = append(b.a.Edges, model.Edge{From: from, To: to, Kind: kind, Spec: spec, Provenance: model.ProvScan})
	}
}

func (b *builder) unresolved(node, spec, detail string, wants model.UnresolvedClass) {
	b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: node, Spec: spec, Detail: detail,
		Class: wants, ScopeTier: model.TierGraph, ScopeDir: "."})
}

func (b *builder) build(p *project) {
	b.a.Projects = append(b.a.Projects, eco.Project{Eco: Name, Dir: p.dir})
	shared := p.node("inputs", "")
	b.owned[p] = map[string]bool{}
	for _, d := range slices.Sorted(maps.Keys(p.visited)) {
		f := path.Join(d, manifestName(p.system))
		b.claim(f, model.RoleManifest)
	}
	runner := MesonRunner
	if p.system == CMake {
		runner = CTestRunner
	}
	addTest := func(name string) string {
		node := unitNode(p.system, p.dir, name)
		b.a.Tests = append(b.a.Tests, model.TestUnit{Eco: Name, ID: UnitID(p.system, p.dir, name), Project: p.dir,
			Runner: runner, Package: name, Whole: name == "", Files: []string{node}})
		b.edge(node, shared, model.EdgeImplicit, "C/C++ project inputs")
		return node
	}
	if p.opaque != "" {
		addTest("")
		b.unresolved(shared, p.system+" build graph", p.opaque, model.UnresolvedDynamic)
		return
	}
	testTargets := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(p.tests)) {
		owner := p.tests[name]
		testTargets[owner] = true
		b.edge(addTest(name), p.node("target", owner), model.EdgeProjectRef, "registered test executable")
	}
	for _, name := range slices.Sorted(maps.Keys(p.targets)) {
		tg := p.targets[name]
		node := p.node("target", name)
		for _, f := range tg.files {
			b.edge(node, f, model.EdgeImplicit, "declared C/C++ target source")
			if isTranslationUnit(f) {
				b.owned[p][f] = true
				role := model.RoleSource
				if testTargets[name] {
					role = model.RoleTest
				}
				b.claim(f, role)
			}
		}
		for _, dep := range tg.deps {
			b.edge(node, p.node("target", dep), model.EdgeProjectRef, "declared C/C++ target dependency")
		}
		if tg.executable && !testTargets[name] {
			b.edge(shared, node, model.EdgeProjectRef, "project executable can be run by tests")
		}
	}
}

func (b *builder) owner(system, f string) *project {
	for dir := path.Dir(f); ; dir = path.Dir(dir) {
		if p := b.roots[system][dir]; p != nil {
			return p
		}
		if dir == "." || b.foreign[dir] && !scan.Native(f) {
			return nil
		}
	}
}

func (b *builder) common(p *project, f string) {
	if b.owned[p][f] || configSystem(f) != "" && configSystem(f) != p.system {
		return
	}
	if classify.Harmless(f) && !scan.Native(f) {
		return
	}
	role := model.RoleData
	switch {
	case manifestSystem(f) != "":
		if manifestSystem(f) == p.system {
			b.claim(f, model.RoleManifest)
		}
		return
	case configSystem(f) != "":
		role = model.RoleConfig
	case dependencyManagerReason(f) == model.ReasonLockChanged:
		role = model.RoleLock
	case dependencyManagerReason(f) != "":
		role = model.RoleManifest
	case scan.Native(f):
		role = model.RoleSource
	}
	b.claim(f, role)
	b.edge(p.node("inputs", ""), f, model.EdgeImplicit, "shared C/C++ project input")
}

func isTranslationUnit(f string) bool {
	switch strings.ToLower(path.Ext(f)) {
	case ".c", ".cc", ".cpp", ".cxx", ".c++", ".cp", ".m", ".mm":
		return true
	}
	return false
}

func foreignRoots(t tree.Tree) map[string]bool {
	out := map[string]bool{}
	for _, f := range t.Files() {
		switch path.Base(f) {
		case "package.json", "Cargo.toml", "go.mod", "pyproject.toml", "composer.json", "Package.swift",
			"pom.xml", "build.gradle", "build.gradle.kts", "Gemfile":
			out[path.Dir(f)] = true
		}
	}
	for _, f := range t.Files() {
		if scan.Native(f) {
			for d := path.Dir(f); d != "."; d = path.Dir(d) {
				delete(out, d)
			}
		}
	}
	return out
}

func (b *builder) includes(sc eco.Scanner, f string) {
	if strings.EqualFold(path.Ext(f), ".s") {
		b.unresolved(f, "assembly source", "assembler directives can load build inputs", model.UnresolvedUncertainScan)
		return
	}
	var r model.ScanResult
	var err error
	if sc == nil {
		var data []byte
		data, err = b.t.Read(f)
		if err == nil {
			r = scan.Source(data)
		}
	} else {
		r, err = sc.Scan(b.t, f)
	}
	if err != nil {
		b.unresolved(f, "C/C++ source", err.Error(), model.UnresolvedUncertainScan)
		return
	}
	if r.Uncertain {
		b.unresolved(f, "C/C++ source", r.UncertainReason, model.UnresolvedUncertainScan)
		return
	}
	for _, ref := range r.Imports {
		spec := ref.Spec
		// Include paths can vary by target and platform. Also consider
		// any project-relative interpretation of the same literal tail.
		tail := path.Base(spec)
		if tail == "." || tail == ".." || tail == "/" {
			b.unresolved(f, spec, "C/C++ include has no literal path tail", model.UnresolvedUnknown)
			continue
		}
		for _, matched := range b.includeFiles[strings.ToLower(tail)] {
			// A case-insensitive match is only plausible for a C/C++ header.
			// Another file must match exactly, and an extensionless system
			// include (<version>, <vector>) names a standard header.
			if !scan.Native(matched) && (path.Base(matched) != tail ||
				ref.Kind == model.ImportSystem && path.Ext(tail) == "") {
				continue
			}
			b.edge(f, matched, model.EdgeImport, spec)
			if !scan.Native(matched) && ref.Suffix != "embed" {
				b.unresolved(matched, spec, "nonstandard C/C++ include can reference additional inputs", model.UnresolvedUncertainScan)
			}
		}
	}
}

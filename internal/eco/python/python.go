// Package python is bgraph's Python ecosystem, at test-file granularity (docs/plan.md §6.2).
//
// # Test units
//
// One model.TestUnit per pytest test file: Eco "python", Runner "pytest", ID = the file, Project = the Python project directory (the
// nearest directory with pyproject.toml, setup.py, setup.cfg or a pytest config file; "." otherwise) and Config = the pytest config
// file pytest would use (empty when there is none). A file is a test when its name matches python_files (default test_*.py,
// *_test.py), it lies under testpaths (when set), and no directory on its way matches norecursedirs. With --doctest-modules in addopts
// every module of that scope is a unit too. When python_files/testpaths are computed (tox substitution) the project also gets a
// "whole project" unit. Projects driven by Django's runner (tests/runtests.py or manage.py, no pytest config) use Runner "django"
// and the unittest pattern test*.py. A conftest.py that implements a pytest collection hook (pytest_collect_file,
// pytest_collect_directory, pytest_pycollect_makemodule, pytest_ignore_collect), itself or through a repo plugin it loads, and
// a repo -p / pytest11 plugin that does, make their scope directory a "directory unit" (ID and Files = the directory; pytest
// collects every item below it), because files that are not test_*.py modules may be test items there (see collect.go).
//
// # Graph (From depends on To)
//
//   - a module depends on the files its imports execute: the module (or package __init__.py) found under ANY candidate import root
//     (repo root, project dirs, src/ layouts, pyproject/setup.cfg package locations, pytest pythonpath, the rootdir-insertion directory
//     of every test and conftest, literal sys.path targets), every ancestor package's __init__.py, PEP 420 namespace packages, .pyi
//     stubs; the union over all roots is taken. An import no root resolves is matched by suffix anywhere in the repo through a virtual
//     <pymod:a.b> node with glob edges (so a file added later is caught); if nothing in the repo matches, a standard-library or declared
//     dependency is external (no edge), anything else keeps the virtual node and is only reported;
//   - every Python file depends on the __init__.py of each of its ancestor directories (loading a module runs its parent packages);
//   - every test depends on every ancestor conftest.py, on the modules named by -p in addopts and by pytest11 entry points declared
//     in the repo;
//   - dynamic imports with a static module prefix depend on everything under that package; fully dynamic imports and non-evaluable
//     sys.path changes are Unresolved at project scope.
package python

import (
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/tree"
)

// Name is the ecosystem name.
const Name = "python"

// Runner names of the units.
const (
	RunnerPytest = "pytest"
	RunnerDjango = "django"
)

// AnalyzerVersion: bump whenever the analysis rules change to invalidate stored analysis snapshots.
const AnalyzerVersion = "python-4"

// Eco is the Python ecosystem.
type Eco struct {
	rules *rules.File // [[dynamic]]; may be nil
}

// New creates the Python ecosystem. [[rule]] entries are applied by the selector; [[dynamic]] entries are read here.
func New(r *rules.File) *Eco { return &Eco{rules: r} }

func (e *Eco) Name() string    { return Name }
func (e *Eco) Version() string { return AnalyzerVersion }

// WholeUnitID is the ID of the "whole test suite of project dir" unit.
func WholeUnitID(dir string) string { return "<py-all-tests:" + dir + ">" }

type edgeKey struct {
	from, to string
	kind     model.EdgeKind
}

type builder struct {
	t  tree.Tree
	sc eco.Scanner
	a  *eco.Analysis

	pyList  []string        // every .py file in analysis scope, sorted
	fileSet map[string]bool // .py and .pyi files
	pyDirs  map[string]bool // directories with a .py/.pyi file below them ("." included)
	byLast  map[string][]string
	markers map[string]bool // project marker directories

	cfgAt    map[string]*pytestCfg
	cfgFor   map[string]*pytestCfg
	declared declared
	roots    []rootSpec
	rootSeen map[string]bool
	cache    map[string]resolved
	seen     map[edgeKey]bool
	projOf   map[string]string

	initChain map[string][]string
	pluginMod []string // modules of pytest11 entry points declared in the repo

	dyn     []rules.Dynamic // [[dynamic]] declarations
	dynUsed []bool

	hookCache map[string]bool // file -> names a pytest collection hook (collect.go)
}

// moduleTriggers: the files the import system can load. A dynamic import (or a sys.path edit) can only ever load these, so a change to
// anything else (data, docs, configuration) cannot reach it through the dynamic site.
var moduleTriggers = []string{"**/*.py", "**/*.pyi", "**/*.pyc", "**/*.so", "**/*.pyd", "**/*.so.*"}

// skipDir: paths below these directories are never repo code.
func skipPath(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "node_modules", "site-packages", "__pycache__":
			return true
		}
	}
	return false
}

var markerFiles = map[string]bool{"pyproject.toml": true, "setup.py": true, "setup.cfg": true, "pytest.ini": true, ".pytest.ini": true,
	"pytest.toml": true, ".pytest.toml": true}

// Analyze analyzes tree t.
func (e *Eco) Analyze(ctx context.Context, t tree.Tree, sc eco.Scanner) (*eco.Analysis, error) {
	a := &eco.Analysis{Roles: map[string]model.Role{}}
	b := &builder{t: t, sc: sc, a: a, fileSet: map[string]bool{}, pyDirs: map[string]bool{}, byLast: map[string][]string{},
		markers: map[string]bool{}, cfgAt: map[string]*pytestCfg{}, cfgFor: map[string]*pytestCfg{}, rootSeen: map[string]bool{},
		cache: map[string]resolved{}, seen: map[edgeKey]bool{}, projOf: map[string]string{}, initChain: map[string][]string{},
		hookCache: map[string]bool{}}
	if e.rules != nil {
		b.dyn = e.rules.Dynamics
		b.dynUsed = make([]bool, len(b.dyn))
	}
	files := t.Files()
	var meta []string
	for _, p := range files {
		switch path.Ext(p) {
		case ".py", ".pyi":
			if skipPath(p) {
				continue
			}
			b.fileSet[p] = true
			if strings.HasSuffix(p, ".py") {
				b.pyList = append(b.pyList, p)
			}
			for d := path.Dir(p); !b.pyDirs[d]; d = path.Dir(d) {
				b.pyDirs[d] = true
				if d == "." {
					break
				}
			}
			stem := strings.TrimSuffix(strings.TrimSuffix(p, ".pyi"), ".py")
			last := path.Base(stem)
			if last == "__init__" {
				stem = path.Dir(stem)
				last = path.Base(stem)
			}
			b.byLast[last] = append(b.byLast[last], stem)
		}
		base := path.Base(p)
		if markerFiles[base] {
			b.markers[path.Dir(p)] = true
		}
		if base == "tox.ini" || base == "Pipfile" || isRequirementsFile(p) {
			meta = append(meta, p)
		}
		if markerFiles[base] && base != "pytest.ini" {
			meta = append(meta, p)
		}
	}
	if len(b.pyList) == 0 {
		return a, nil // no Python in this tree: nothing to do
	}
	for _, p := range meta {
		if path.Base(p) == "tox.ini" && !b.markers[path.Dir(p)] {
			if loadPytestAt(t, path.Dir(p)) != nil {
				b.markers[path.Dir(p)] = true
			}
		}
	}
	// A setup.py / setup.cfg / pyproject.toml inside a package (a directory with __init__.py) of an enclosing project is a
	// nested build configuration (numpy.distutils `configuration()` per subpackage in old scikit-learn, scipy...), not a
	// separate project: the package belongs to the enclosing project. Treating it as a project would shrink every
	// project-scoped widening and the pytest rootdir to the subpackage.
	for d := range b.markers {
		if d == "." || !b.fileSet[d+"/__init__.py"] || t.Has(d+"/pytest.ini") || t.Has(d+"/.pytest.ini") ||
			t.Has(d+"/pytest.toml") || t.Has(d+"/.pytest.toml") || loadPytestAt(t, d) != nil {
			continue // a pytest config file there is a real rootdir for the tests below it
		}
		for anc := path.Dir(d); ; anc = path.Dir(anc) {
			if b.markers[anc] {
				delete(b.markers, d)
				break
			}
			if anc == "." {
				break
			}
		}
	}
	b.declared = loadDeclared(t, meta)

	// Projects.
	markerDirs := make([]string, 0, len(b.markers))
	for d := range b.markers {
		markerDirs = append(markerDirs, d)
	}
	sort.Strings(markerDirs)
	needRoot := false
	for _, p := range b.pyList {
		if b.project(path.Dir(p)) == "." && !b.markers["."] {
			needRoot = true
			break
		}
	}
	if needRoot {
		a.Projects = append(a.Projects, eco.Project{Eco: Name, Dir: "."})
	}
	for _, d := range markerDirs {
		a.Projects = append(a.Projects, eco.Project{Eco: Name, Dir: d})
	}

	// Import roots (part 1: layouts, pytest pythonpath, conventional src dirs).
	b.addRoot(rootSpec{dir: "."})
	for _, d := range markerDirs {
		b.addRoot(rootSpec{dir: d})
		for _, sub := range []string{"src", "lib", "python"} {
			b.addRoot(rootSpec{dir: path.Join(d, sub)})
		}
		l := readLayout(t, d)
		for _, r := range l.roots {
			b.addRoot(rootSpec{dir: r})
		}
		for _, m := range l.maps {
			b.addRoot(rootSpec{prefix: strings.Split(m.name, "."), dir: m.dir})
		}
		for _, name := range []string{"pyproject.toml", "setup.cfg", "setup.py"} {
			if p := path.Join(d, name); t.Has(p) {
				b.pluginMod = append(b.pluginMod, pluginModules(t, p)...)
			}
		}
	}
	for _, d := range markerDirs {
		if c := b.cfgAtDir(d); c != nil {
			for _, pp := range c.pythonpath {
				b.addRoot(rootSpec{dir: joinDir(c.dir, pp)})
			}
		}
	}

	// Scan everything and collect units; roots from tests, conftests and sys.path literals.
	type fileInfo struct {
		scan  model.ScanResult
		unit  *unitInfo
		isCnf bool
	}
	infos := make(map[string]*fileInfo, len(b.pyList))
	for _, p := range b.pyList {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res, err := sc.Scan(t, p)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", p, err)
		}
		fi := &fileInfo{scan: res, isCnf: path.Base(p) == "conftest.py"}
		fi.unit = b.unitFor(p)
		infos[p] = fi
		if fi.unit != nil || fi.isCnf {
			b.addRoot(rootSpec{dir: b.insertionDir(p)})
		}
		for _, im := range res.Imports {
			if im.Kind == model.ImportSysPath && !im.Dynamic {
				for _, d := range b.sysPathDirs(p, im.Spec) {
					b.addRoot(rootSpec{dir: d})
				}
			}
		}
	}
	slices.SortFunc(b.roots, func(x, y rootSpec) int { return strings.Compare(x.key(), y.key()) })

	// Entry-point plugins and -p plugins resolve like imports (heuristic: no fallback).
	var epFiles []string
	for _, m := range b.pluginMod {
		epFiles = append(epFiles, b.heuristicFiles(m, nil)...)
	}
	slices.Sort(epFiles)
	epFiles = slices.Compact(epFiles)
	cfgPlugins := map[string][]string{}

	for _, p := range b.pyList {
		fi := infos[p]
		proj := b.project(path.Dir(p))
		role := model.RoleSource
		switch {
		case fi.unit != nil:
			role = model.RoleTest
		case path.Base(p) == "setup.py":
			role = model.RoleManifest
		case isHarmlessScript(p):
			role = model.RoleConfig
		case path.Base(p) == "noxfile.py":
			role = model.RoleConfig
		}
		b.setRole(p, role)
		if fi.scan.Uncertain {
			a.Unresolved = append(a.Unresolved, model.Unresolved{File: p, Spec: "*", Class: model.UnresolvedUncertainScan,
				ScopeTier: model.TierModule, ScopeDir: ".", Detail: fi.scan.UncertainReason, TriggerGlobs: moduleTriggers})
		}
		for _, im := range fi.scan.Imports {
			b.addImport(p, proj, im)
		}
		for _, init := range b.inits(path.Dir(p)) {
			if init != p {
				b.edge(p, init, model.EdgeImplicit, "package init", 0)
			}
		}
		if fi.unit == nil {
			continue
		}
		u := fi.unit
		a.Tests = append(a.Tests, model.TestUnit{Eco: Name, ID: p, Project: proj, Files: []string{p}, Runner: u.runner, Config: u.config})
		for d := path.Dir(p); ; d = path.Dir(d) {
			if cf := path.Join(d, "conftest.py"); b.fileSet[cf] && cf != p {
				b.edge(p, cf, model.EdgeConftest, "conftest", 0)
			}
			if d == "." {
				break
			}
		}
		if u.cfg != nil {
			fs, ok := cfgPlugins[u.cfg.file]
			if !ok {
				for _, pl := range u.cfg.plugins {
					fs = append(fs, b.heuristicFiles(pl, nil)...)
				}
				cfgPlugins[u.cfg.file] = fs
			}
			for _, f := range fs {
				b.edge(p, f, model.EdgeImport, "pytest -p plugin", 0)
			}
		}
		for _, f := range epFiles {
			b.edge(p, f, model.EdgeImport, "pytest11 entry point", 0)
		}
	}
	// Directory units for pytest collection hooks (collect.go).
	conftests := map[string]model.ScanResult{}
	rootSet := map[string]bool{}
	for _, p := range b.pyList {
		fi := infos[p]
		if fi.isCnf {
			conftests[p] = fi.scan
		}
		if fi.unit != nil {
			rd := b.project(path.Dir(p))
			if fi.unit.cfg != nil {
				rd = fi.unit.cfg.dir
			}
			rootSet[rd] = true
		}
	}
	for _, c := range b.cfgAt {
		if c == nil {
			continue
		}
		if _, ok := cfgPlugins[c.file]; !ok {
			var fs []string
			for _, pl := range c.plugins {
				fs = append(fs, b.heuristicFiles(pl, nil)...)
			}
			cfgPlugins[c.file] = fs
		}
	}
	var rootdirs []string
	for d := range rootSet {
		rootdirs = append(rootdirs, d)
	}
	sort.Strings(rootdirs)
	for _, d := range b.collectHookDirs(conftests, epFiles, cfgPlugins, rootdirs) {
		b.addDirUnit(d, epFiles, cfgPlugins)
	}
	// Whole-project units for configs whose collection rules are computed.
	var cfgDirs []string
	for d, c := range b.cfgAt {
		if c != nil && len(c.opaque) > 0 {
			cfgDirs = append(cfgDirs, d)
		}
	}
	sort.Strings(cfgDirs)
	for _, d := range cfgDirs {
		c := b.cfgAt[d]
		a.Tests = append(a.Tests, model.TestUnit{Eco: Name, ID: WholeUnitID(d), Project: d, Runner: RunnerPytest, Whole: true, Config: c.file})
		a.Unresolved = append(a.Unresolved, model.Unresolved{File: c.file, Spec: strings.Join(c.opaque, "; "), Class: model.UnresolvedUnknown,
			ScopeTier: model.TierNone, Detail: "pytest collection settings are computed: the project's whole suite is one unit"})
	}
	for i, d := range b.dyn {
		if !b.dynUsed[i] {
			why := "matches no dynamic import site (stale declaration?): the widening stays"
			if !b.fileSet[d.File] {
				why = "is not a Python file of the repo: the declaration is inert"
			}
			a.Unresolved = append(a.Unresolved, model.Unresolved{File: rules.FileName, Spec: fmt.Sprintf("[[dynamic]] #%d file = %q", i+1, d.File),
				Class: model.UnresolvedUnknown, ScopeTier: model.TierNone, Detail: "[[dynamic]] " + why})
		}
	}
	b.configRoles(files)
	slices.SortFunc(a.Edges, func(x, y model.Edge) int {
		if c := strings.Compare(x.From, y.From); c != 0 {
			return c
		}
		return strings.Compare(x.To, y.To)
	})
	return a, nil
}

func (b *builder) setRole(p string, r model.Role) {
	old, ok := b.a.Roles[p]
	if !ok || r == model.RoleTest || old == model.RoleSource {
		b.a.Roles[p] = r
	}
}

func (b *builder) edge(from, to string, kind model.EdgeKind, spec string, line int) {
	if from == to {
		return
	}
	k := edgeKey{from, to, kind}
	if b.seen[k] {
		return
	}
	b.seen[k] = true
	b.a.Edges = append(b.a.Edges, model.Edge{From: from, To: to, Kind: kind, Spec: spec, Line: line, Provenance: model.ProvScan})
}

// project returns the project directory of the files in dir: the nearest marker directory, "." otherwise.
func (b *builder) project(dir string) string {
	if v, ok := b.projOf[dir]; ok {
		return v
	}
	v := "."
	if b.markers[dir] {
		v = dir
	} else if dir != "." && dir != "" {
		v = b.project(path.Dir(dir))
	}
	b.projOf[dir] = v
	return v
}

// cfgAtDir is the pytest configuration stored in exactly dir (nil: none).
func (b *builder) cfgAtDir(dir string) *pytestCfg {
	if c, ok := b.cfgAt[dir]; ok {
		return c
	}
	c := loadPytestAt(b.t, dir)
	b.cfgAt[dir] = c
	return c
}

// pytestCfgFor is the configuration pytest uses for a file in dir: the nearest ancestor directory that has one.
func (b *builder) pytestCfgFor(dir string) *pytestCfg {
	if c, ok := b.cfgFor[dir]; ok {
		return c
	}
	var c *pytestCfg
	if c = b.cfgAtDir(dir); c == nil && dir != "." && dir != "" {
		c = b.pytestCfgFor(path.Dir(dir))
	}
	b.cfgFor[dir] = c
	return c
}

// inits returns the __init__.py(i) files of dir and every ancestor directory.
func (b *builder) inits(dir string) []string {
	if v, ok := b.initChain[dir]; ok {
		return v
	}
	var out []string
	if dir != "." && dir != "" {
		out = append(out, b.inits(path.Dir(dir))...)
	}
	for _, n := range []string{"__init__.py", "__init__.pyi"} {
		if f := path.Join(dir, n); b.fileSet[f] {
			out = append(out, f)
		}
	}
	b.initChain[dir] = out
	return out
}

// insertionDir is the directory pytest puts on sys.path (prepend/append import mode) for a test or conftest file: the first ancestor
// directory without __init__.py.
func (b *builder) insertionDir(p string) string {
	d := path.Dir(p)
	for d != "." && b.fileSet[path.Join(d, "__init__.py")] {
		d = path.Dir(d)
	}
	return d
}

// sysPathDirs turns the Spec of an evaluated sys.path entry into candidate directories.
func (b *builder) sysPathDirs(from, spec string) []string {
	if rest, ok := strings.CutPrefix(spec, "cwd:"); ok {
		// relative to the working directory: the repo root or the project directory of the file (or the file's own directory)
		return []string{path.Clean(rest), path.Join(b.project(path.Dir(from)), rest), path.Join(path.Dir(from), rest)}
	}
	return []string{path.Join(path.Dir(from), spec)}
}

func isHarmlessScript(p string) bool {
	switch path.Base(p) {
	case "conf.py":
		d := path.Base(path.Dir(p))
		return d == "docs" || d == "doc" || d == "source"
	}
	return false
}

// configRoles gives roles to the non-Python files the ecosystem owns, so that they are graph nodes (and never "unreferenced").
func (b *builder) configRoles(files []string) {
	for _, p := range files {
		base := path.Base(p)
		switch {
		case base == "pytest.ini" || base == ".pytest.ini" || base == "pytest.toml" || base == ".pytest.toml" || base == "tox.ini":
			b.a.Roles[p] = model.RoleConfig
		case base == "pyproject.toml" || base == "setup.cfg" || base == "Pipfile":
			b.a.Roles[p] = model.RoleManifest
		case isLockfile(base) || isRequirementsFile(p):
			b.a.Roles[p] = model.RoleLock
		}
	}
}

func isLockfile(base string) bool {
	switch base {
	case "poetry.lock", "uv.lock", "pdm.lock", "Pipfile.lock", "pylock.toml":
		return true
	}
	return strings.HasPrefix(base, "pylock.") && strings.HasSuffix(base, ".toml")
}

// ---- test units ----

type unitInfo struct {
	runner string
	config string
	cfg    *pytestCfg
}

// unitFor decides whether p is a test unit and how its runner is launched.
func (b *builder) unitFor(p string) *unitInfo {
	dir := path.Dir(p)
	base := path.Base(p)
	if base == "conftest.py" {
		return nil
	}
	cfg := b.pytestCfgFor(dir)
	proj := b.project(dir)
	if cfg == nil {
		if driver, root := b.djangoDriver(proj); driver != "" {
			if b.djangoTest(p, root) {
				return &unitInfo{runner: RunnerDjango, config: driver}
			}
			return nil
		}
	}
	rootdir := proj
	if cfg != nil {
		rootdir = cfg.dir
	}
	if !eco.Within(p, rootdir) {
		return nil
	}
	rel := p
	if rootdir != "." {
		rel = strings.TrimPrefix(p, rootdir+"/")
	}
	if !b.collectable(cfg, rel, rootdir) {
		return nil
	}
	isTest := false
	for _, pat := range cfg.patterns() {
		if fnmatchEx(pat, p) {
			isTest = true
			break
		}
	}
	if !isTest && !(cfg != nil && cfg.doctest && base != "setup.py" && base != "__main__.py") {
		return nil
	}
	u := &unitInfo{runner: RunnerPytest, cfg: cfg}
	if cfg != nil {
		u.config = cfg.file
	}
	return u
}

// collectable applies norecursedirs, testpaths and --ignore to a file path relative to the rootdir.
func (b *builder) collectable(cfg *pytestCfg, rel, rootdir string) bool {
	dirs := strings.Split(path.Dir(rel), "/")
	if path.Dir(rel) == "." {
		dirs = nil
	}
	pats := cfg.norecursePatterns()
	for i, d := range dirs {
		if d == "__pycache__" {
			return false
		}
		full := path.Join(rootdir, strings.Join(dirs[:i+1], "/"))
		for _, pat := range pats {
			if fnmatchEx(pat, full) {
				return false
			}
		}
		if b.t.Has(path.Join(full, "pyvenv.cfg")) {
			return false
		}
	}
	if cfg == nil {
		return true
	}
	if len(cfg.testpaths) > 0 && !underTestpaths(cfg.testpaths, rel) {
		return false
	}
	for _, ig := range cfg.ignore {
		if rel == ig || strings.HasPrefix(rel, ig+"/") {
			return false
		}
	}
	for _, ig := range cfg.ignoreGlob {
		if fnmatchEx(ig, path.Join(rootdir, rel)) {
			return false
		}
	}
	return true
}

func underTestpaths(testpaths []string, rel string) bool {
	for _, tp := range testpaths {
		tp = path.Clean(strings.ReplaceAll(tp, "\\", "/"))
		if tp == "." {
			return true
		}
		g, err := glob.Compile(tp)
		if err != nil {
			return true // cannot interpret: do not drop the file
		}
		for d := rel; ; d = path.Dir(d) {
			if g.Match(d) {
				return true
			}
			if d == "." || d == "/" || d == "" {
				break
			}
		}
	}
	return false
}

// djangoDriver returns the Django test driver of project proj (tests/runtests.py or manage.py) and the directory test labels are
// relative to.
func (b *builder) djangoDriver(proj string) (driver, root string) {
	for _, c := range []struct{ file, root string }{
		{"tests/runtests.py", "tests"}, {"runtests.py", "."}, {"manage.py", "."},
	} {
		if f := path.Join(proj, c.file); b.fileSet[f] {
			return f, path.Join(proj, c.root)
		}
	}
	return "", ""
}

// djangoTest: a file the Django test runner discovers (unittest pattern test*.py in a chain of packages below root).
func (b *builder) djangoTest(p, root string) bool {
	if !strings.HasPrefix(path.Base(p), "test") || !eco.Within(p, root) {
		return false
	}
	rel := p
	if root != "." {
		rel = strings.TrimPrefix(p, root+"/")
	}
	d := path.Dir(rel)
	if d == "." {
		return true
	}
	parts := strings.Split(d, "/")
	for i := range parts {
		if !b.fileSet[path.Join(root, strings.Join(parts[:i+1], "/"), "__init__.py")] {
			return false
		}
	}
	return true
}

// ---- imports ----

func (b *builder) unres(from, spec string, class model.UnresolvedClass, tier model.Tier, scope string, line int, detail string) {
	b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: from, Spec: spec, Class: class, Line: line, ScopeTier: tier, ScopeDir: scope, Detail: detail})
}

func splitNames(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// heuristicFiles resolves a dotted name (a string in the code: patch target, plugin, settings entry) to files: the union of every
// prefix that is a module. Nothing found: no files.
func (b *builder) heuristicFiles(spec string, names []string) []string {
	if spec == "" || strings.HasPrefix(spec, ".") {
		return nil
	}
	comps := strings.Split(spec, ".")
	var out []string
	for k := len(comps); k >= 1; k-- {
		var n []string
		if k == len(comps) {
			n = names
		}
		if r := b.resolveAbs(comps[:k], n); r.found {
			out = append(out, r.files...)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (b *builder) addImport(from, proj string, im model.ImportRef) {
	if im.Kind == model.ImportSysPath {
		if im.Dynamic {
			b.dynSite(from, "sys.path", proj, im.Line, true,
				"sys.path is changed by an expression bgraph cannot evaluate: imports may resolve from an unknown directory")
		}
		return
	}
	kind := model.EdgeImport
	heuristic := im.Kind == model.ImportMock
	if heuristic {
		kind = model.EdgeMock
	}
	if im.Dynamic {
		if im.Kind == model.ImportPlugin || im.Spec == "" {
			b.dynSite(from, im.Spec, proj, im.Line, false, "module name is not a constant")
			return
		}
		b.dynamicHead(from, proj, im)
		return
	}
	names := splitNames(im.Suffix)
	if strings.HasPrefix(im.Spec, ".") {
		b.relativeImport(from, proj, im, names, kind)
		return
	}
	if heuristic {
		for _, f := range b.heuristicFiles(im.Spec, names) {
			b.edge(from, f, kind, im.Spec, im.Line)
		}
		return
	}
	comps := strings.Split(im.Spec, ".")
	if r := b.resolveAbs(comps, names); r.found {
		for _, f := range r.files {
			b.edge(from, f, kind, im.Spec, im.Line)
		}
		return
	}
	// No root has it. A module of that name somewhere in the repo (an unknown root: PYTHONPATH, conftest tricks, a plugin that
	// edits sys.path) is tracked by glob so that files added later are caught.
	switch {
	case b.suffixMatches(comps):
		b.moduleGlobs(from, im, comps, names)
	case b.isExternal(comps[0]):
	default:
		b.moduleGlobs(from, im, comps, names)
		b.unres(from, im.Spec, model.UnresolvedUnknown, model.TierNone, "", im.Line,
			"undeclared module not found in the repo (tracked via glob **/"+strings.Join(comps, "/")+")")
	}
}

// moduleGlobs links from to the virtual node of an unresolved absolute module, with glob edges to every path that could be it.
func (b *builder) moduleGlobs(from string, im model.ImportRef, comps, names []string) {
	v := moduleNode(strings.Join(comps, "."))
	b.edge(from, v, model.EdgeImport, im.Spec, im.Line)
	for _, g := range modulePatterns(comps) {
		b.patternEdge(v, g, im.Spec)
	}
	for _, n := range names {
		if n != "*" && n != "" {
			for _, g := range modulePatterns(append(slices.Clone(comps), n))[len(comps):] {
				b.patternEdge(v, g, im.Spec)
			}
		}
	}
}

func (b *builder) patternEdge(from, glob, spec string) {
	b.edge(from, glob, model.EdgePattern, spec, 0)
}

func (b *builder) relativeImport(from, proj string, im model.ImportRef, names []string, kind model.EdgeKind) {
	level := 0
	for level < len(im.Spec) && im.Spec[level] == '.' {
		level++
	}
	var rel []string
	if rest := im.Spec[level:]; rest != "" {
		rel = strings.Split(rest, ".")
	}
	base := path.Dir(from)
	for i := 1; i < level; i++ {
		if base == "." {
			b.unres(from, im.Spec, model.UnresolvedUnknown, model.TierModule, proj, im.Line, "relative import climbs above the repository root")
			return
		}
		base = path.Dir(base)
	}
	var files []string
	if b.resolveAt(base, rel, false, names, &files) {
		slices.Sort(files)
		for _, f := range slices.Compact(files) {
			b.edge(from, f, kind, im.Spec, im.Line)
		}
		return
	}
	if kind == model.EdgeMock {
		return
	}
	// The target is not in the tree (generated, a compiled extension, or a mistake): an edge to the exact paths catches the file when it is added.
	d := joinPath(base, rel...)
	for _, f := range []string{d + ".py", d + ".pyi", d + "/__init__.py", d + "/__init__.pyi"} {
		b.edge(from, f, kind, im.Spec, im.Line)
	}
	b.unres(from, im.Spec, model.UnresolvedUnknown, model.TierNone, "", im.Line, "relative import target not in the tree (tracked via exact path edges)")
}

// dynamicHead handles importlib.import_module("pkg.plugins." + name): the importer depends on everything under the package.
func (b *builder) dynamicHead(from, proj string, im model.ImportRef) {
	comps := strings.Split(im.Spec, ".")
	if !strings.HasSuffix(im.Spec, ".") {
		comps = comps[:len(comps)-1] // the last component is only a prefix of a module name
	}
	if len(comps) == 0 || comps[0] == "" {
		b.dynSite(from, im.Spec, proj, im.Line, false, "module name is not a constant")
		return
	}
	hit := false
	for _, r := range b.roots {
		if len(comps) < len(r.prefix) || !slices.Equal(comps[:len(r.prefix)], r.prefix) {
			continue
		}
		d := joinPath(r.dir, comps[len(r.prefix):]...)
		if b.pyDirs[d] {
			hit = true
			g := escapeGlob(d) + "/**"
			if d == "." {
				g = "**"
			}
			b.edge(from, g, model.EdgePattern, im.Spec, im.Line)
		}
	}
	if hit {
		for _, f := range b.resolveAbs(comps, nil).files {
			b.edge(from, f, model.EdgeImport, im.Spec, im.Line)
		}
		return
	}
	if b.isExternal(comps[0]) {
		return
	}
	b.dynSite(from, im.Spec, proj, im.Line, false, "module name has a static prefix that is not a package of the repo")
}

// dynSite records a dynamic import or sys.path site: described by a [[dynamic]] declaration (pattern edges to what it loads, nothing
// else), or an Unresolved at project scope that fires only when a loadable file changes.
func (b *builder) dynSite(from, spec, proj string, line int, sysPath bool, detail string) {
	for i, d := range b.dyn {
		if d.File != from || d.SysPathOnly && !sysPath || len(d.Lines) > 0 && !slices.Contains(d.Lines, line) {
			continue
		}
		b.dynUsed[i] = true
		for _, g := range d.RawLoads {
			k := edgeKey{from, g, model.EdgePattern}
			if !b.seen[k] {
				b.seen[k] = true
				b.a.Edges = append(b.a.Edges, model.Edge{From: from, To: g, Kind: model.EdgePattern, Line: line, Spec: "bgraph.toml [[dynamic]]", Provenance: model.ProvRule})
			}
		}
		return
	}
	// Repo scope, not the project: a name-based import can load any module on sys.path, and in-repo modules of other
	// projects (or of a parent project, when the "project" is a nested build config) are on it in a dev environment.
	_ = proj
	b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: from, Spec: spec, Class: model.UnresolvedDynamic, Line: line,
		ScopeTier: model.TierModule, ScopeDir: ".", Detail: detail, TriggerGlobs: moduleTriggers})
}

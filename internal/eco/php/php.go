// Package php models each Composer/PHPUnit/Pest project as one runnable unit.
// Unit IDs are "<repo-relative-project-dir>::php" and use "." for the root.
package php

import (
	"context"
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	phpscan "github.com/qxbao/bgraph/internal/scan/php"
	"github.com/qxbao/bgraph/internal/tree"
)

const (
	Name          = "php"
	RunnerPHPUnit = "phpunit"
	RunnerPest    = "pest"

	// AnalyzerVersion invalidates snapshots when PHP analysis changes.
	AnalyzerVersion = "php-8"
)

func UnitID(dir string) string { return path.Clean(dir) + "::php" }

func ParseUnitID(id string) (string, bool) {
	d, ok := strings.CutSuffix(id, "::php")
	if !ok || d == "" || excludedPath(d) || path.Clean(d) != d || !repoPath(d) {
		return "", false
	}
	return d, true
}

func virtual(dir string) string { return "<php:" + UnitID(dir) + ">" }

type Eco struct{}

func New(_ *rules.File) *Eco { return &Eco{} }
func (*Eco) Name() string    { return Name }
func (*Eco) Version() string { return AnalyzerVersion }

type project struct {
	dir, name, runner, config, binDir, vendorDir string
	manifest                                     manifest
	customSuite                                  bool
	problem                                      string
	testPaths                                    []string
}

type analysis struct {
	t           tree.Tree
	a           *eco.Analysis
	projects    map[string]*project
	foreign     map[string]bool
	paths       []string
	scanTasks   []scanTask
	basenames   map[string][]string
	aliases     map[string]string
	vendorRoots map[string][]*project
}

func (*Eco) Analyze(ctx context.Context, t tree.Tree, sc eco.Scanner) (*eco.Analysis, error) {
	a := &eco.Analysis{Roles: map[string]model.Role{}}
	b := analysis{t: t, a: a, projects: map[string]*project{}, paths: t.Files()}
	var configs []string
	for _, f := range b.paths {
		n := path.Base(f)
		if n != "composer.json" && !testConfig(n) || excludedPath(f) {
			continue
		}
		d := path.Dir(f)
		switch {
		case n == "composer.json":
			b.projects[d] = b.loadProject(d)
		default:
			configs = append(configs, f)
		}
	}
	// A custom vendor-dir can contain installed packages with their own manifests.
	b.indexVendors()
	for d := range b.projects {
		if b.vendored(d) {
			delete(b.projects, d)
		}
	}
	b.indexVendors()
	for _, f := range configs {
		d := path.Dir(f)
		if b.projects[d] == nil && !b.vendored(f) {
			b.projects[d] = b.loadProject(d)
		}
	}
	if len(b.projects) == 0 {
		return a, nil
	}
	b.foreign = b.foreignRoots()
	b.aliases = tree.AliasesOf(t)

	dirs := make([]string, 0, len(b.projects))
	for d := range b.projects {
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)
	for _, d := range dirs {
		p := b.projects[d]
		a.Projects = append(a.Projects, eco.Project{Eco: Name, Dir: d, Name: p.name})
		b.composerEdges(p)
		b.configEdges(p)
	}
	for _, f := range b.paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if b.aliases[f] != "" {
			for _, owner := range b.vendorProjects(f) {
				b.edge(virtual(owner.dir), f, model.EdgeImplicit, "Composer vendor symlink", 0)
			}
		}
		p := b.fileOwner(f)
		if p == nil {
			continue
		}
		switch path.Base(f) {
		case "composer.json":
			// Composer metadata changes are classified without seeding ownership.
			a.Roles[f] = model.RoleManifest
			continue
		case "composer.lock":
			a.Roles[f] = model.RoleLock
		default:
			a.Roles[f] = model.RoleData
			switch {
			case testConfig(path.Base(f)):
				a.Roles[f] = model.RoleConfig
			case testFile(f):
				a.Roles[f] = model.RoleTest
				p.testPaths = append(p.testPaths, f)
			case phpscan.Source(f):
				a.Roles[f] = model.RoleSource
			}
		}
		b.edge(virtual(p.dir), f, model.EdgeImplicit, "PHP project ownership", 0)
		if phpscan.Source(f) && sc != nil {
			b.queueInclude(p, f)
		}
	}
	seen := map[scanTask]bool{}
	for i := 0; sc != nil && i < len(b.scanTasks); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		task := b.scanTasks[i]
		if !seen[task] {
			seen[task] = true
			b.includes(task.project, task.file, sc)
		}
	}
	for _, d := range dirs {
		p := b.projects[d]
		u := model.TestUnit{Eco: Name, ID: UnitID(d), Dir: d, Project: d,
			Files: []string{virtual(d)}, Runner: p.runner, Config: p.config, BinDir: b.runnerBinDir(p)}
		if p.customSuite {
			// Composer controls the arguments and layout of this suite.
			u.Files = nil
			u.Whole = true
		} else if p.config == "" {
			if len(p.testPaths) == 0 {
				continue
			}
			u.RunnerPaths = b.testOperands(p)
		} else if p.runner == RunnerPest && !t.IsDir(path.Join(p.dir, "tests")) && t.IsDir(path.Join(p.dir, "test")) {
			u.RunnerPaths = []string{path.Join(p.dir, "test")}
		}
		a.Tests = append(a.Tests, u)
	}
	return a, nil
}

func (b *analysis) loadProject(d string) *project {
	p := &project{dir: d, runner: RunnerPHPUnit, vendorDir: path.Join(d, "vendor")}
	// PHPUnit 9.3+ default configuration lookup order.
	for _, n := range []string{"phpunit.xml", "phpunit.dist.xml", "phpunit.xml.dist"} {
		if f := path.Join(d, n); b.t.Has(f) {
			p.config = f
			break
		}
	}
	f := path.Join(d, "composer.json")
	if b.t.Has(f) {
		m, err := readManifest(b.t, f)
		if err != nil {
			p.problem = "unreadable or invalid composer.json"
		} else {
			p.manifest = m
			p.name = m.Name
			runner, customSuite := m.scriptSuite(p.config, func(arg string) bool {
				operand, ok := localPath(d, arg)
				return ok && (phpscan.Source(operand) || b.t.IsDir(operand))
			})
			p.customSuite = customSuite
			if m.has("pestphp/pest") || runner == RunnerPest {
				p.runner = RunnerPest
			}
			if m.Config.VendorDir != "" {
				if vendor, ok := localPath(d, m.Config.VendorDir); ok && !eco.Within(d, vendor) {
					p.vendorDir = vendor
				} else {
					p.problem = "unsupported Composer vendor-dir"
				}
			}
			if m.Config.BinDir != "" {
				var ok bool
				p.binDir, ok = localPath(d, strings.ReplaceAll(m.Config.BinDir, "{$vendor-dir}", m.vendorDirOrDefault()))
				if !ok {
					p.binDir = "/"
					p.problem = "unsupported Composer bin-dir"
				}
			}
		}
		if p.binDir == "" {
			if p.problem == "unsupported Composer vendor-dir" {
				p.binDir = "/"
			} else {
				p.binDir = path.Join(p.vendorDir, "bin")
			}
		}
	}
	for _, n := range []string{"Pest.php", "tests/Pest.php", "test/Pest.php"} {
		if b.t.Has(path.Join(d, n)) {
			p.runner = RunnerPest
			break
		}
	}
	return p
}

// runnerBinDir returns where the runner of p is installed. A PHPUnit config
// without its own composer.json (dev/tests/unit/phpunit.xml.dist) runs with
// the binaries of the enclosing Composer project, not a global PATH lookup.
func (b *analysis) runnerBinDir(p *project) string {
	for q := p; q != nil; {
		if q.binDir != "" {
			return q.binDir
		}
		if q.dir == "." {
			break
		}
		q = b.owner(path.Dir(q.dir))
	}
	return ""
}

func (b *analysis) owner(d string) *project {
	for {
		if p := b.projects[d]; p != nil {
			return p
		}
		if d == "." {
			return nil
		}
		d = path.Dir(d)
	}
}

func (b *analysis) fileOwner(f string) *project {
	if excludedPath(f) || b.vendored(f) {
		return nil
	}
	p := b.owner(path.Dir(f))
	if p != nil {
		for d := path.Dir(f); d != p.dir; d = path.Dir(d) {
			if b.foreign[d] {
				return nil
			}
		}
	}
	return p
}

func (b *analysis) vendored(f string) bool {
	return len(b.vendorProjects(f)) > 0
}

func (b *analysis) vendorProjects(f string) []*project {
	for d := f; ; d = path.Dir(d) {
		if owners := b.vendorRoots[d]; len(owners) > 0 {
			return owners
		}
		if d == "." {
			return nil
		}
	}
}

func (b *analysis) indexVendors() {
	b.vendorRoots = map[string][]*project{}
	for _, p := range b.projects {
		if p.binDir != "" {
			b.vendorRoots[p.vendorDir] = append(b.vendorRoots[p.vendorDir], p)
		}
	}
}

func (b *analysis) foreignRoots() map[string]bool {
	foreign := map[string]bool{}
	phpRoots := map[string]bool{}
	for _, f := range b.paths {
		if excludedPath(f) || b.vendored(f) {
			continue
		}
		d := path.Dir(f)
		switch path.Base(f) {
		case "package.json", "go.mod", "Cargo.toml", "pyproject.toml", "pom.xml", "build.gradle", "build.gradle.kts":
			foreign[d] = true
		}
		if phpscan.Source(f) {
			if p := b.owner(d); p != nil {
				for ; d != p.dir && !phpRoots[d]; d = path.Dir(d) {
					phpRoots[d] = true
				}
			}
		}
	}
	for d := range phpRoots {
		delete(foreign, d)
	}
	return foreign
}

func (b *analysis) testOperands(p *project) []string {
	var groups []string
	for _, n := range []string{"tests", "test"} {
		d := path.Join(p.dir, n)
		// PHPUnit recursively walks a directory operand. Use individual files if
		// scanning the whole directory would include an independent suite.
		if !slices.ContainsFunc(filesWithin(b.paths, d), func(f string) bool {
			return phpscan.Source(f) && b.fileOwner(f) != p
		}) {
			groups = append(groups, d)
		}
	}
	var out []string
	for _, f := range p.testPaths {
		group := f
		for _, d := range groups {
			if eco.Within(f, d) && (strings.HasSuffix(f, "Test.php") || strings.HasSuffix(f, ".phpt")) {
				group = d
				break
			}
		}
		out = append(out, group)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// testFile matches PHPUnit's default, case-sensitive "Test.php" suffix, so
// sources such as Latest.php or Contest.php are not test operands.
func testFile(f string) bool {
	return strings.HasSuffix(path.Base(f), "Test.php") || strings.HasSuffix(strings.ToLower(f), ".phpt")
}

func testConfig(n string) bool {
	switch n {
	case "phpunit.xml", "phpunit.xml.dist", "phpunit.dist.xml":
		return true
	}
	return false
}

func excludedPath(p string) bool {
	for _, s := range strings.Split(p, "/") {
		switch s {
		case "vendor", "node_modules", ".git":
			return true
		}
	}
	return false
}

func repoPath(p string) bool {
	return p != "" && p != ".." && !strings.HasPrefix(p, "../") && !strings.ContainsAny(p, ":\\\x00") && !path.IsAbs(p)
}

// localPath interprets both path separator spellings in Composer/PHPUnit inputs.
func localPath(dir, raw string) (string, bool) {
	raw = strings.ReplaceAll(raw, "\\", "/")
	if raw == "" || strings.ContainsAny(raw, "$~\x00:") || path.IsAbs(raw) {
		return "", false
	}
	p := path.Join(dir, raw)
	return p, repoPath(p)
}

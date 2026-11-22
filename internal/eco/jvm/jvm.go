// Package jvm is bgraph's JVM ecosystem (Maven and Gradle), at module/project granularity (docs/plan.md §6.4).
//
// Below the module the class graph is not a sound dependency relation (reflection, ServiceLoader, annotation processors, Spring
// component scanning, generated sources), so the safe unit is the Maven module or Gradle project; file-level knowledge is used only
// to decide which module OWNS a file.
//
// # Test units
//
// One model.TestUnit per Maven module and per Gradle project (also those without tests: the runner simply runs nothing there).
// Eco is "jvm"; Runner is "maven" or "gradle"; Project is the module directory (repo-relative, "." for the repo root); Files holds the
// single virtual node of the unit. The unit ID is built by UnitID:
//
//	maven:<dir>                 a Maven module, <dir> its directory ("maven:." is the root module)
//	gradle:<builddir><path>     a Gradle project; <path> is the project path (":" root project, ":core:api"), <builddir> the
//	                            directory of the build (the settings directory) and is empty for a build at the repo root, so
//	                            "gradle::core:api" is :core:api of the root build and "gradle:build-logic:convention" is :convention
//	                            of the build in build-logic/ (a build directory never contains ':').
//
// Package is the Maven "groupId:artifactId" or the Gradle project path. Config encodes how to launch the build: see LaunchConfig.
// The virtual node of a unit is "<jvm:" + ID + ">", see Node.
//
// # Graph (From depends on To)
//
//   - a unit depends on every file under its directory, except the directories of nested units and build output (Maven target/,
//     Gradle build/ and .gradle/): sources, resources, build files and every other file ("a non-code file change selects the whole
//     module");
//   - files that sit in the directory of an ENCLOSING unit but outside its src/ (shared configuration, test data, script plugins) are
//     dependencies of every nested unit, because a nested module can read them through "../";
//   - a unit depends on the unit of every module it references: Maven <dependency> of any scope/type/classifier, <plugin> that is a
//     reactor module and the plugin's <dependencies>, build <extension>, BOM imports; Gradle project(":x"), projects.x.y accessors,
//     testFixtures(project(..)), platform(project(..)) and any other configuration. A reference the parser cannot place makes the
//     unit depend on every unit of its build;
//   - a Maven module depends on the POM files of its local parent chain (<parent> through relativePath);
//   - a Gradle project depends on every project of the builds included with includeBuild.
//
// # Widening
//
// Changes to the shared build definition (Maven parent/aggregator POMs unless only <version> changed, .mvn/**, profiles that declare
// modules, a remote parent's version; Gradle settings and root scripts, buildSrc/, build-logic/, version catalogs, the wrapper,
// gradle.properties, cross-project configuration) widen to every JVM unit of the build (full when the build is the repo root).
// Dynamic constructs that hide the project list (Gradle include in a loop) are Unresolved at full scope.
package jvm

import (
	"context"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/qxbao/bgraph/internal/classify"
	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/tree"
)

// Name is the ecosystem name.
const Name = "jvm"

// Runner names of the units.
const (
	RunnerMaven  = "maven"
	RunnerGradle = "gradle"
)

// AnalyzerVersion: bump whenever the analysis rules change to invalidate stored analysis snapshots.
const AnalyzerVersion = "jvm-3"

// Eco is the JVM ecosystem.
type Eco struct {
	mu     sync.Mutex
	mcache map[tree.Tree]*mvnModel
	gcache map[tree.Tree]*gradleModel
}

// New creates the JVM ecosystem. The rules file is accepted for symmetry with the other ecosystems; JVM has no declarations yet.
func New(_ *rules.File) *Eco { return &Eco{} }

func (e *Eco) Name() string    { return Name }
func (e *Eco) Version() string { return AnalyzerVersion }

// UnitID returns the unit ID of a Maven module ("maven:<dir>") or of a Gradle project ("gradle:<buildDir><path>"). For Maven the
// build directory and the project path are ignored; for Gradle dir is the build directory ("." or "" for the repo root).
func UnitID(kind, dir, projectPath string) string {
	if kind == RunnerMaven {
		return "maven:" + dir
	}
	if dir == "." {
		dir = ""
	}
	return "gradle:" + dir + projectPath
}

// ParseUnitID is the inverse of UnitID: kind is RunnerMaven or RunnerGradle; for Maven dir is the module directory and projectPath is
// empty; for Gradle dir is the build directory ("." for the repo root) and projectPath the project path.
func ParseUnitID(id string) (kind, dir, projectPath string, ok bool) {
	if d, found := strings.CutPrefix(id, "maven:"); found {
		return RunnerMaven, d, "", d != ""
	}
	rest, found := strings.CutPrefix(id, "gradle:")
	if !found {
		return "", "", "", false
	}
	i := strings.IndexByte(rest, ':')
	if i < 0 {
		return "", "", "", false
	}
	dir, projectPath = rest[:i], rest[i:]
	if dir == "" {
		dir = "."
	}
	return RunnerGradle, dir, projectPath, true
}

// Node returns the virtual graph node of a unit ID.
func Node(id string) string { return "<jvm:" + id + ">" }

// LaunchConfig encodes TestUnit.Config: the build file that marks the root of the build the unit runs in (the reactor root POM, the
// settings script or the single build script) and, after a '|', the wrapper script (mvnw/gradlew) that applies, both repo-relative.
// The wrapper part is empty when the repo ships none. The command runs in the directory of the build file.
func LaunchConfig(buildFile, wrapper string) string { return buildFile + "|" + wrapper }

// ParseLaunchConfig is the inverse of LaunchConfig.
func ParseLaunchConfig(c string) (buildFile, wrapper string) {
	buildFile, wrapper, _ = strings.Cut(c, "|")
	return
}

// unit is one Maven module or Gradle project of the analyzed tree.
type unit struct {
	kind   string
	id     string
	dir    string
	node   string
	pkg    string
	config string
	// own: the build definition files of the unit (pom.xml, build script); their role is manifest.
	own []string
	// shareSrc: the unit compiles nothing from src/ (a Maven aggregator/parent with packaging pom), so the non-code files there
	// are build configuration (checkstyle, license headers, assembly descriptors) that nested units may read.
	shareSrc bool
}

type edgeKey struct {
	from, to string
	kind     model.EdgeKind
}

type builder struct {
	t    tree.Tree
	a    *eco.Analysis
	seen map[edgeKey]bool
}

func newBuilder(t tree.Tree, a *eco.Analysis) *builder {
	return &builder{t: t, a: a, seen: map[edgeKey]bool{}}
}

func (b *builder) edge(from, to string, kind model.EdgeKind, spec string) {
	k := edgeKey{from, to, kind}
	if b.seen[k] {
		return
	}
	b.seen[k] = true
	b.a.Edges = append(b.a.Edges, model.Edge{From: from, To: to, Kind: kind, Spec: spec, Provenance: model.ProvScan})
}

func (b *builder) role(p string, r model.Role) {
	if old, ok := b.a.Roles[p]; !ok || old == model.RoleData || old == model.RoleUnknown {
		b.a.Roles[p] = r
	}
}

// Analyze analyzes tree t.
func (e *Eco) Analyze(ctx context.Context, t tree.Tree, _ eco.Scanner) (*eco.Analysis, error) {
	a := &eco.Analysis{Roles: map[string]model.Role{}}
	hasMaven, hasGradle := false, false
	for _, p := range t.Files() {
		switch path.Base(p) {
		case "pom.xml":
			hasMaven = true
		case "settings.gradle", "settings.gradle.kts", "build.gradle", "build.gradle.kts":
			hasGradle = true
		}
	}
	if !hasMaven && !hasGradle {
		return a, nil
	}
	b := newBuilder(t, a)
	var units []*unit
	if hasMaven {
		m := e.mavenModel(t)
		units = append(units, m.analyze(b)...)
	}
	if hasGradle {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		g := e.gradleModel(t)
		units = append(units, g.analyze(b)...)
	}
	for _, u := range units {
		a.Projects = append(a.Projects, eco.Project{Eco: Name, Dir: u.dir, Name: u.pkg})
		a.Tests = append(a.Tests, model.TestUnit{Eco: Name, ID: u.id, Project: u.dir, Files: []string{u.node}, Runner: u.kind,
			Package: u.pkg, Config: u.config})
	}
	b.own(units)
	slices.SortFunc(a.Edges, func(x, y model.Edge) int {
		if c := strings.Compare(x.From, y.From); c != 0 {
			return c
		}
		return strings.Compare(x.To, y.To)
	})
	return a, nil
}

// skipFile: files that are never inputs of a unit (VCS metadata, vendored node_modules).
func skipFile(p string) bool {
	q := "/" + p + "/"
	return strings.Contains(q, "/.git/") || strings.Contains(q, "/node_modules/")
}

// outputDir: the first path segment of rel is build output of the unit kind.
func outputDir(kind, rel string) bool {
	top, _, _ := strings.Cut(rel, "/")
	if kind == RunnerMaven {
		return top == "target"
	}
	return top == "build" || top == ".gradle"
}

func isScript(base string) bool {
	switch base {
	case "pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts":
		return true
	}
	return false
}

// own attributes every file to the unit(s) of each kind whose directory is the nearest enclosing one, and shares the files of
// enclosing units with nested ones.
func (b *builder) own(units []*unit) {
	for _, kind := range []string{RunnerMaven, RunnerGradle} {
		byDir := map[string][]*unit{}
		for _, u := range units {
			if u.kind == kind {
				byDir[u.dir] = append(byDir[u.dir], u)
			}
		}
		if len(byDir) == 0 {
			continue
		}
		// descendants[dir] = units strictly nested below dir.
		dirs := make([]string, 0, len(byDir))
		for d := range byDir {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		descendants := map[string][]*unit{}
		for _, d := range dirs {
			for _, d2 := range dirs {
				if d2 != d && eco.Within(d2, d) {
					descendants[d] = append(descendants[d], byDir[d2]...)
				}
			}
		}
		nearest := map[string]string{} // directory → nearest unit directory ("" none)
		var near func(d string) string
		near = func(d string) string {
			if v, ok := nearest[d]; ok {
				return v
			}
			v := ""
			if _, ok := byDir[d]; ok {
				v = d
			} else if d != "." && d != "/" && d != "" {
				v = near(path.Dir(d))
			}
			nearest[d] = v
			return v
		}
		for _, f := range b.t.Files() {
			if skipFile(f) {
				continue
			}
			ud := near(path.Dir(f))
			if ud == "" {
				continue
			}
			rel := f
			if ud != "." {
				rel = strings.TrimPrefix(f, ud+"/")
			}
			if outputDir(kind, rel) {
				continue
			}
			base := path.Base(f)
			role := fileRole(rel)
			for _, u := range byDir[ud] {
				b.edge(u.node, f, model.EdgeImplicit, "module file")
			}
			b.role(f, role)
			// Shared with nested units: not under src/ (unless the unit compiles nothing from there and the file is no code), not a
			// build definition of the enclosing unit, not documentation.
			inSrc := rel == "src" || strings.HasPrefix(rel, "src/")
			srcConfig := role == model.RoleData && slices.ContainsFunc(byDir[ud], func(u *unit) bool { return u.shareSrc })
			if inSrc && !srcConfig || isScript(base) && path.Dir(rel) == "." ||
				classify.Doc(f) || classify.Harmless(f) {
				continue
			}
			for _, d := range descendants[ud] {
				b.edge(d.node, f, model.EdgeImplicit, "file of enclosing module "+ud)
			}
		}
		for _, u := range units {
			if u.kind != kind {
				continue
			}
			for _, f := range u.own {
				if b.t.Has(f) {
					b.a.Roles[f] = model.RoleManifest
				}
			}
		}
	}
}

var sourceExts = map[string]bool{".java": true, ".kt": true, ".kts": true, ".scala": true, ".groovy": true, ".gradle": true, ".clj": true}

// fileRole: java-like files under src/test (or src/*Test*, src/it) are tests, other source files are sources, everything else data.
func fileRole(rel string) model.Role {
	ext := path.Ext(rel)
	if !sourceExts[ext] {
		return model.RoleData
	}
	if strings.HasPrefix(rel, "src/") {
		set, _, _ := strings.Cut(strings.TrimPrefix(rel, "src/"), "/")
		ls := strings.ToLower(set)
		if ls == "test" || ls == "it" || strings.Contains(ls, "test") && ls != "testfixtures" {
			return model.RoleTest
		}
	}
	return model.RoleSource
}

// scopeWiden builds the widening of a shared-build-definition change: every JVM unit within root (full when root is the repo root).
func scopeWiden(reason, p, root, detail string) model.Widening {
	if root == "." || root == "" {
		return model.Widening{Tier: model.TierFull, Reason: reason, Path: p, Eco: Name, Detail: detail}
	}
	return model.Widening{Tier: model.TierModule, Reason: reason, Path: p, Scope: root, Eco: Name, Detail: detail}
}

// ClassifyChange classifies Maven POMs/.mvn and Gradle build definitions (plan §6.4).
func (e *Eco) ClassifyChange(p string, base, head tree.Tree, a *eco.Analysis) ([]model.Widening, bool) {
	if ws, ok := e.classifyMaven(p, base, head); ok {
		return ws, true
	}
	return e.classifyGradle(p, base, head)
}

func (e *Eco) mavenModel(t tree.Tree) *mvnModel {
	e.mu.Lock()
	defer e.mu.Unlock()
	if m, ok := e.mcache[t]; ok {
		return m
	}
	m := loadMaven(t)
	if e.mcache == nil {
		e.mcache = map[tree.Tree]*mvnModel{}
	}
	e.mcache[t] = m
	return m
}

func (e *Eco) gradleModel(t tree.Tree) *gradleModel {
	e.mu.Lock()
	defer e.mu.Unlock()
	if m, ok := e.gcache[t]; ok {
		return m
	}
	m := loadGradle(t)
	if e.gcache == nil {
		e.gcache = map[tree.Tree]*gradleModel{}
	}
	e.gcache[t] = m
	return m
}

// wrapperFor returns the wrapper script (name "mvnw" or "gradlew") nearest to dir, looking in dir and its ancestors ("" if none).
func wrapperFor(t tree.Tree, dir, name string) string {
	for d := dir; ; d = path.Dir(d) {
		w := path.Join(d, name)
		if t.Has(w) {
			return w
		}
		if d == "." || d == "/" || d == "" {
			return ""
		}
	}
}

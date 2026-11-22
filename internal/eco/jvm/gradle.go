package jvm

import (
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// gproj is a Gradle project.
type gproj struct {
	path   string // ":a:b", ":" for the root project
	dir    string
	b      *gbuild
	script string // build.gradle(.kts) of the project ("" none)
}

// gbuild is one Gradle build: a settings directory with its projects.
type gbuild struct {
	dir      string
	settings string // settings script ("" for a build without one)
	// scripts are the settings script and the scripts it applies: a change to any of them changes the build structure.
	scripts  []string
	projects map[string]*gproj
	order    []*gproj
	included []*gbuild
	incDirs  []string
	rootName string
	dynamic  []string
}

// gradleModel is the Gradle part of one tree.
type gradleModel struct {
	t      tree.Tree
	builds []*gbuild // sorted by dir
	byDir  map[string]*gbuild
	// cross: build scripts that configure other projects (allprojects/subprojects/project(..) {}), by script path.
	cross map[string]string
	// refs caches the lexed references of each script.
	refs map[string]*scriptRefs
}

func isGradleScript(base string) bool {
	return base == "build.gradle" || base == "build.gradle.kts"
}

func isSettingsScript(base string) bool {
	return base == "settings.gradle" || base == "settings.gradle.kts"
}

func skipGradlePath(p string) bool {
	if skipFile(p) {
		return true
	}
	q := "/" + path.Dir(p) + "/"
	return strings.Contains(q, "/build/") || strings.Contains(q, "/.gradle/") || strings.Contains(q, "/buildSrc/")
}

func readScript(t tree.Tree, p string) (string, bool) {
	b, err := t.Read(p)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func loadGradle(t tree.Tree) *gradleModel {
	m := &gradleModel{t: t, byDir: map[string]*gbuild{}, cross: map[string]string{}, refs: map[string]*scriptRefs{}}
	var settings, scripts []string
	hasFile := func(p string) bool { return t.Has(p) }
	for _, p := range t.Files() {
		base := path.Base(p)
		if !isSettingsScript(base) && !isGradleScript(base) || skipGradlePath(p) {
			continue
		}
		// A script under src/ of an enclosing Gradle project directory is a test resource, not a build.
		if underSrc(p, hasFile) {
			continue
		}
		if isSettingsScript(base) {
			settings = append(settings, p)
		} else {
			scripts = append(scripts, p)
		}
	}
	// Builds with a settings script.
	for _, sp := range settings {
		dir := path.Dir(sp)
		if _, dup := m.byDir[dir]; dup {
			continue
		}
		b := &gbuild{dir: dir, settings: sp, projects: map[string]*gproj{}}
		m.byDir[dir] = b
		m.builds = append(m.builds, b)
		src, ok := readScript(t, sp)
		if !ok {
			b.dynamic = append(b.dynamic, "settings script unreadable")
			continue
		}
		si := parseSettings(src, strings.HasSuffix(sp, ".kts"))
		b.scripts = append(b.scripts, sp)
		b.rootName = si.rootName
		if b.rootName == "" {
			b.rootName = path.Base(dir)
		}
		b.dynamic = append(b.dynamic, si.dynamic...)
		// Applied scripts may declare includes too.
		for depth, queue := 0, si.applied; depth < 3 && len(queue) > 0; depth++ {
			var nextQ []string
			for _, ap := range queue {
				full := path.Join(dir, ap)
				if !t.Has(full) {
					continue
				}
				b.scripts = append(b.scripts, full)
				if s2, ok := readScript(t, full); ok {
					sub := parseSettings(s2, strings.HasSuffix(full, ".kts"))
					si.includes = append(si.includes, sub.includes...)
					si.flat = append(si.flat, sub.flat...)
					si.includeBuilds = append(si.includeBuilds, sub.includeBuilds...)
					for k, v := range sub.dirs {
						si.dirs[k] = v
					}
					b.dynamic = append(b.dynamic, sub.dynamic...)
					nextQ = append(nextQ, sub.applied...)
				}
			}
			queue = nextQ
		}
		m.buildProjects(b, si)
	}
	// Single-project builds: a build script outside every project of the builds above.
	covered := map[string]bool{}
	for _, b := range m.builds {
		for _, pr := range b.order {
			covered[pr.dir] = true
		}
	}
	for _, sp := range scripts {
		dir := path.Dir(sp)
		if covered[dir] {
			continue
		}
		if _, dup := m.byDir[dir]; dup {
			continue
		}
		b := &gbuild{dir: dir, projects: map[string]*gproj{}}
		m.byDir[dir] = b
		m.builds = append(m.builds, b)
		m.buildProjects(b, settingsInfo{dirs: map[string]string{}})
	}
	sort.Slice(m.builds, func(i, j int) bool { return m.builds[i].dir < m.builds[j].dir })
	for _, b := range m.builds {
		for _, d := range b.incDirs {
			if ib, ok := m.byDir[d]; ok && ib != b {
				b.included = append(b.included, ib)
			}
		}
		for _, pr := range b.order {
			if pr.script == "" {
				continue
			}
			sr := m.scriptRefs(pr.script)
			if sr.cross != "" {
				m.cross[pr.script] = sr.cross
			}
		}
	}
	return m
}

// underSrc: p lies under src/ of an enclosing directory that itself holds a build script or settings script.
func underSrc(p string, has func(string) bool) bool {
	for d := path.Dir(path.Dir(p)); ; d = path.Dir(d) {
		for _, n := range []string{"build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts", "pom.xml"} {
			if has(path.Join(d, n)) {
				rel := strings.TrimPrefix(strings.TrimPrefix(p, d), "/")
				if rel == "src" || strings.HasPrefix(rel, "src/") {
					return true
				}
			}
		}
		if d == "." || d == "/" || d == "" {
			return false
		}
	}
}

// buildProjects creates the projects of b from the parsed settings: the root, every include (with its implied ancestors) and
// includeFlat, then resolves their directories.
func (m *gradleModel) buildProjects(b *gbuild, si settingsInfo) {
	names := map[string]bool{":": true}
	flatDir := map[string]string{}
	add := func(pp string) {
		segs := strings.Split(strings.TrimPrefix(pp, ":"), ":")
		for i := range segs {
			if segs[i] == "" {
				return
			}
			names[":"+strings.Join(segs[:i+1], ":")] = true
		}
	}
	for _, inc := range si.includes {
		add(normProjectPath(inc))
	}
	for _, f := range si.flat {
		pp := normProjectPath(f)
		if strings.Count(pp, ":") == 1 {
			names[pp] = true
			flatDir[pp] = path.Join("..", strings.TrimPrefix(pp, ":"))
		}
	}
	paths := make([]string, 0, len(names))
	for n := range names {
		paths = append(paths, n)
	}
	sort.Slice(paths, func(i, j int) bool {
		di, dj := strings.Count(paths[i], ":"), strings.Count(paths[j], ":")
		if paths[i] == ":" {
			di = 0
		}
		if paths[j] == ":" {
			dj = 0
		}
		if di != dj {
			return di < dj
		}
		return paths[i] < paths[j]
	})
	dirOf := map[string]string{}
	for _, pp := range paths {
		var dir string
		switch {
		case pp == ":":
			dir = b.dir
		case si.dirs[pp] != "":
			dir = path.Join(b.dir, si.dirs[pp])
		case flatDir[pp] != "":
			dir = path.Join(b.dir, flatDir[pp])
		default:
			parent := ":"
			if i := strings.LastIndexByte(pp, ':'); i > 0 {
				parent = pp[:i]
			}
			dir = path.Join(dirOf[parent], pp[strings.LastIndexByte(pp, ':')+1:])
		}
		dirOf[pp] = dir
		pr := &gproj{path: pp, dir: dir, b: b}
		for _, n := range []string{"build.gradle.kts", "build.gradle"} {
			if m.t.Has(path.Join(dir, n)) {
				pr.script = path.Join(dir, n)
				break
			}
		}
		b.projects[pp] = pr
		b.order = append(b.order, pr)
	}
	for _, ib := range si.includeBuilds {
		if r, ok := stripRootDir(ib); ok {
			b.incDirs = append(b.incDirs, path.Join(b.dir, cleanRel(r)))
		}
	}
}

// scope is the directory that contains the build directory and every project directory of the build (a projectDir or includeFlat
// project may lie outside the settings directory): the scope of a widening that concerns the whole build.
func (gb *gbuild) scope() string {
	dirs := []string{gb.dir}
	for _, pr := range gb.order {
		dirs = append(dirs, pr.dir)
	}
	return commonDir(dirs)
}

// rootAbove returns the build whose directory is the deepest at or above dir (nil when none).
func (m *gradleModel) rootAbove(dir string) *gbuild {
	var best *gbuild
	for _, b := range m.builds {
		if eco.Within(dir, b.dir) && (best == nil || len(b.dir) > len(best.dir)) {
			best = b
		}
	}
	return best
}

func (m *gradleModel) isRootScript(p string) (*gbuild, bool) {
	base := path.Base(p)
	b, ok := m.byDir[path.Dir(p)]
	if !ok {
		return nil, false
	}
	if isGradleScript(base) {
		// The root project's build script.
		if pr, ok := b.projects[":"]; ok && pr.script == p {
			return b, true
		}
		return nil, false
	}
	for _, s := range b.scripts {
		if s == p {
			return b, true
		}
	}
	return nil, false
}

// accessorName converts a project name to its type-safe accessor spelling (kebab/snake/dot to camelCase).
func accessorName(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	for i := range parts {
		if i > 0 && parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

func looseName(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// resolveAccessor resolves the segments of projects.a.b... to project paths (the longest chain of known projects; several when
// names collide after normalization).
func (b *gbuild) resolveAccessor(segs []string) []string {
	cur := []string{":"}
	// The first segment may also name the root project.
	if len(segs) > 0 && b.rootName != "" && looseName(segs[0]) == looseName(b.rootName) {
		if _, ok := b.projects[":"]; ok {
			cur = []string{":"}
			segs = segs[1:]
		}
	}
	matched := false
	for _, s := range segs {
		var next []string
		for _, c := range cur {
			for _, pr := range b.order {
				if pr.path == ":" {
					continue
				}
				i := strings.LastIndexByte(pr.path, ':')
				parent := pr.path[:i]
				if parent == "" {
					parent = ":"
				}
				if parent == c && looseName(accessorName(pr.path[i+1:])) == looseName(s) {
					next = append(next, pr.path)
				}
			}
		}
		if len(next) == 0 {
			break
		}
		cur = next
		matched = true
	}
	if !matched && !(len(segs) == 0) {
		return nil
	}
	return cur
}

// analyze adds the Gradle part of the graph to b and returns the units.
func (m *gradleModel) analyze(b *builder) []*unit {
	var units []*unit
	byProj := map[*gproj]*unit{}
	// A project gets a unit when it has a build script or any file of its own (nested projects excluded).
	hasOwn := m.ownDirs()
	for _, gb := range m.builds {
		buildFile := gb.settings
		if buildFile == "" {
			if pr := gb.projects[":"]; pr != nil {
				buildFile = pr.script
			}
		}
		wrapper := wrapperFor(m.t, gb.dir, "gradlew")
		cfg := LaunchConfig(buildFile, wrapper)
		for _, pr := range gb.order {
			if pr.script == "" && !hasOwn[pr.dir] {
				continue
			}
			id := UnitID(RunnerGradle, gb.dir, pr.path)
			u := &unit{kind: RunnerGradle, id: id, dir: pr.dir, node: Node(id), pkg: pr.path, config: cfg}
			if pr.script != "" {
				u.own = append(u.own, pr.script)
			}
			if pr.path == ":" {
				u.own = append(u.own, gb.scripts...)
			}
			units = append(units, u)
			byProj[pr] = u
		}
		for _, d := range gb.dynamic {
			if gb.settings == "" {
				continue
			}
			b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: gb.settings, Spec: d, Class: model.UnresolvedDynamic,
				ScopeTier: model.TierFull, ScopeDir: gb.scope(), Detail: "the project list of this build cannot be determined: " + d})
		}
	}
	for _, gb := range m.builds {
		for _, pr := range gb.order {
			u := byProj[pr]
			if u == nil {
				continue
			}
			for _, ib := range gb.included {
				for _, ip := range ib.order {
					if iu := byProj[ip]; iu != nil {
						b.edge(u.node, iu.node, model.EdgeProjectRef, "included build "+ib.dir)
					}
				}
			}
		}
		for _, pr := range gb.order {
			if pr.script != "" {
				m.scriptEdges(b, gb, pr, pr.script, byProj, 0)
			}
		}
		m.pluginEdges(b, gb, byProj)
	}
	return units
}

// pluginSourceExt: files of a plugin build (buildSrc/, build-logic/) that can add project dependencies to the projects applying
// the plugin.
var pluginSourceExt = []string{".gradle", ".kts", ".kt", ".java", ".groovy"}

// pluginEdges: a convention plugin in buildSrc/ or build-logic/ can add project(":x") dependencies to every project that applies
// it, and which projects apply which plugin is not modelled, so every project of the build depends on every project such a plugin
// names (an unplaceable reference: on every project of the build).
func (m *gradleModel) pluginEdges(b *builder, gb *gbuild, byProj map[*gproj]*unit) {
	var dirs []string
	for _, d := range []string{"buildSrc", "build-logic"} {
		dirs = append(dirs, path.Join(gb.dir, d))
	}
	root := gb.projects[":"]
	var to []*gproj
	all := false
	for _, f := range m.t.Files() {
		in := false
		for _, d := range dirs {
			if eco.Within(f, d) {
				in = true
				break
			}
		}
		if !in || !slices.ContainsFunc(pluginSourceExt, func(e string) bool { return strings.HasSuffix(f, e) }) {
			continue
		}
		rel := strings.TrimPrefix(f, gb.dir+"/")
		if outputDir(RunnerGradle, rel) || strings.Contains(f, "/build/") || strings.Contains(f, "/.gradle/") {
			continue
		}
		for _, r := range m.scriptRefs(f).refs {
			switch r.kind {
			case refPath:
				if !strings.HasPrefix(r.val, ":") || root == nil {
					all = true // relative to the applying project, which is not known
					continue
				}
				to = append(to, gb.lookup(r.val, root)...)
			case refStrPath:
				to = append(to, gb.resolveStrPath(r.val)...)
			case refAccessor:
				ts := gb.resolveAccessor(strings.Split(r.val, "."))
				if len(ts) == 0 {
					all = true
				}
				for _, p := range ts {
					to = append(to, gb.projects[p])
				}
			case refUnplaceable:
				all = true
			}
		}
	}
	if all {
		to = gb.order
	}
	if len(to) == 0 {
		return
	}
	for _, from := range gb.order {
		fu := byProj[from]
		if fu == nil {
			continue
		}
		for _, t := range to {
			if tu := byProj[t]; tu != nil && tu != fu {
				b.edge(fu.node, tu.node, model.EdgeProjectRef, "project reference in a convention plugin (buildSrc/build-logic)")
			}
		}
	}
}

// ownDirs: project directories that hold at least one file of their own.
func (m *gradleModel) ownDirs() map[string]bool {
	dirs := map[string]bool{}
	for _, gb := range m.builds {
		for _, pr := range gb.order {
			dirs[pr.dir] = false
		}
	}
	nearest := map[string]string{}
	var near func(d string) string
	near = func(d string) string {
		if v, ok := nearest[d]; ok {
			return v
		}
		v := ""
		if _, ok := dirs[d]; ok {
			v = d
		} else if d != "." && d != "/" && d != "" {
			v = near(path.Dir(d))
		}
		nearest[d] = v
		return v
	}
	for _, f := range m.t.Files() {
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
		if outputDir(RunnerGradle, rel) {
			continue
		}
		dirs[ud] = true
	}
	return dirs
}

// scriptEdges adds the dependency edges declared by script (of project pr) and, recursively, by the scripts it applies.
func (m *gradleModel) scriptEdges(b *builder, gb *gbuild, pr *gproj, script string, byProj map[*gproj]*unit, depth int) {
	sr := m.scriptRefs(script)
	targets := func(sc scopeInfo) []*gproj {
		switch sc.kind {
		case scopeBuild:
			return gb.order
		case scopeAll:
			return append([]*gproj{pr}, gb.descendants(pr)...)
		case scopeSub:
			return gb.descendants(pr)
		case scopeProject:
			if ts := gb.lookup(sc.arg, pr); len(ts) > 0 {
				return ts
			}
			return gb.order // the configured project is not placeable: any project may be affected
		}
		return []*gproj{pr}
	}
	addEdges := func(sc scopeInfo, to []*gproj, spec string) {
		for _, from := range targets(sc) {
			fu := byProj[from]
			if fu == nil {
				continue
			}
			for _, t := range to {
				if tu := byProj[t]; tu != nil && tu != fu {
					b.edge(fu.node, tu.node, model.EdgeProjectRef, spec)
				}
			}
		}
	}
	// Projects configured by this script depend on it (it is a file of the project that owns it).
	for _, sc := range sr.configures {
		for _, q := range targets(sc) {
			if qu := byProj[q]; qu != nil && q != pr {
				b.edge(qu.node, script, model.EdgeInclude, "configured by "+script)
			}
		}
	}
	for _, r := range sr.refs {
		switch r.kind {
		case refPath:
			addEdges(r.sc, gb.lookup(r.val, pr), "project("+r.val+")")
		case refStrPath:
			addEdges(r.sc, gb.resolveStrPath(r.val), "reference "+r.val)
		case refAccessor:
			ts := gb.resolveAccessor(strings.Split(r.val, "."))
			if len(ts) == 0 {
				addEdges(r.sc, gb.order, "projects."+r.val+" (unplaceable accessor: depends on every project of the build)")
			} else {
				var to []*gproj
				for _, p := range ts {
					to = append(to, gb.projects[p])
				}
				addEdges(r.sc, to, "projects."+r.val)
			}
		case refUnplaceable:
			addEdges(r.sc, gb.order, r.val+" (unplaceable: depends on every project of the build)")
		case refFile:
			for _, from := range targets(r.sc) {
				fu := byProj[from]
				if fu == nil {
					continue
				}
				target := path.Join(from.dir, r.val)
				if r.rootRel {
					target = path.Join(gb.dir, r.val)
				}
				if eco.Within(target, from.dir) || target == ".." || strings.HasPrefix(target, "../") {
					continue
				}
				if m.t.Has(target) {
					b.edge(fu.node, target, model.EdgeInclude, "file "+r.val)
				} else {
					b.edge(fu.node, escapeGlob(target)+"/**", model.EdgePattern, "directory "+r.val)
				}
			}
		case refApply:
			full := ""
			if r.rootRel {
				full = path.Join(gb.dir, r.val)
			} else {
				full = path.Join(pr.dir, r.val)
			}
			if !m.t.Has(full) && !r.rootRel {
				if alt := path.Join(gb.dir, r.val); m.t.Has(alt) {
					full = alt
				}
			}
			if !m.t.Has(full) {
				continue
			}
			if pu := byProj[pr]; pu != nil {
				b.edge(pu.node, full, model.EdgeInclude, "apply from "+r.val)
			}
			if depth < 3 {
				m.scriptEdges(b, gb, pr, full, byProj, depth+1)
			}
		}
	}
}

// descendants returns the projects whose path lies below pr's.
func (gb *gbuild) descendants(pr *gproj) []*gproj {
	var out []*gproj
	prefix := pr.path + ":"
	if pr.path == ":" {
		prefix = ":"
	}
	for _, o := range gb.order {
		if o != pr && strings.HasPrefix(o.path, prefix) {
			out = append(out, o)
		}
	}
	return out
}

// lookup resolves a literal project reference as written in the script of from: absolute (":x") or relative to the project.
func (gb *gbuild) lookup(ref string, from *gproj) []*gproj {
	var out []*gproj
	seen := map[string]bool{}
	add := func(p string) {
		p = normProjectPath(p)
		if pr, ok := gb.projects[p]; ok && !seen[p] {
			seen[p] = true
			out = append(out, pr)
		}
	}
	if strings.HasPrefix(ref, ":") {
		add(ref)
		return out
	}
	add(":" + ref)
	if from != nil && from.path != ":" {
		add(from.path + ":" + ref)
	}
	return out
}

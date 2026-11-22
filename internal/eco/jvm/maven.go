package jvm

import (
	"path"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// parentRef is a <parent> element.
type parentRef struct {
	g, a, v    string
	relPath    string // raw <relativePath> ("" when absent or empty)
	relDefault bool   // no <relativePath> element: "../pom.xml"
	relEmpty   bool   // <relativePath/>: the parent is looked up in repositories only
}

// pomInfo is one parsed POM that is a module of the analyzed tree.
type pomInfo struct {
	path, dir string
	root      *xnode
	err       error
	g, a, v   string // own coordinates, raw
	packaging string
	parent    *parentRef
	props     map[string]string
	// modules are the raw <module>/<subproject> entries, including those of profiles; profileModules: some come from a profile.
	modules        []string
	profileModules bool
	// localParent is the POM path of the parent found in the tree ("" when remote or absent).
	localParent string
	// declaredBy are the POMs that list this module.
	declaredBy []string
}

// mvnModel is the Maven part of one tree.
type mvnModel struct {
	t     tree.Tree
	poms  map[string]*pomInfo // by POM path
	byDir map[string]*pomInfo
	order []*pomInfo // sorted by path
	ga    map[string][]*pomInfo
	// conflicts: directories that hold two distinct module POMs.
	conflicts []string
	// children[pom path] = module POMs whose local parent is that POM.
	children map[string][]*pomInfo
	// comp[pom path] = component id (modules/parent connectivity); compRoot[id] = common ancestor directory.
	comp     map[string]int
	compRoot map[int]string
}

func skipMavenPath(p string, hasPom func(string) bool) bool {
	if skipFile(p) {
		return true
	}
	for d := path.Dir(p); d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if path.Base(d) == "target" && hasPom(path.Join(path.Dir(d), "pom.xml")) {
			return true
		}
	}
	return false
}

// nestedFixture: pom lies under src/ (test resources, invoker ITs, archetype templates) of an enclosing POM directory.
func nestedFixture(p string, hasPom func(string) bool) bool {
	for d := path.Dir(path.Dir(p)); ; d = path.Dir(d) {
		if hasPom(path.Join(d, "pom.xml")) {
			rel := strings.TrimPrefix(strings.TrimPrefix(p, d), "/")
			if rel == "src" || strings.HasPrefix(rel, "src/") {
				return true
			}
		}
		if d == "." || d == "/" || d == "" {
			return false
		}
	}
}

func loadMaven(t tree.Tree) *mvnModel {
	m := &mvnModel{t: t, poms: map[string]*pomInfo{}, byDir: map[string]*pomInfo{}, ga: map[string][]*pomInfo{},
		children: map[string][]*pomInfo{}, comp: map[string]int{}, compRoot: map[int]string{}}
	hasPom := func(p string) bool { return t.Has(p) }
	var queue []string
	for _, p := range t.Files() {
		if path.Base(p) != "pom.xml" || skipMavenPath(p, hasPom) || nestedFixture(p, hasPom) {
			continue
		}
		queue = append(queue, p)
	}
	add := func(p string) *pomInfo {
		if pi, ok := m.poms[p]; ok {
			return pi
		}
		d := path.Dir(p)
		if other, ok := m.byDir[d]; ok && other.path != p {
			m.conflicts = append(m.conflicts, p)
			return nil
		}
		pi := parsePom(t, p)
		m.poms[p] = pi
		m.byDir[d] = pi
		for _, s := range pi.modules {
			if mp := modulePomPath(d, s); mp != "" && t.Has(mp) && !skipFile(mp) {
				queue = append(queue, mp)
			}
		}
		return pi
	}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		add(p)
	}
	for _, pi := range m.poms {
		m.order = append(m.order, pi)
	}
	sort.Slice(m.order, func(i, j int) bool { return m.order[i].path < m.order[j].path })
	for _, pi := range m.order {
		for _, s := range pi.modules {
			if mp := modulePomPath(pi.dir, s); mp != "" {
				if c, ok := m.poms[mp]; ok && c != pi {
					c.declaredBy = append(c.declaredBy, pi.path)
				}
			}
		}
	}
	for _, pi := range m.order {
		pi.localParent = m.findParent(pi)
		if pi.localParent != "" {
			m.children[pi.localParent] = append(m.children[pi.localParent], pi)
		}
	}
	for _, pi := range m.order {
		g := m.effGroup(pi)
		a := m.interp(pi, pi.a)
		k := g + ":" + a
		m.ga[k] = append(m.ga[k], pi)
	}
	m.components()
	return m
}

// modulePomPath resolves a <module> entry relative to the declaring directory dir.
func modulePomPath(dir, s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\\", "/"))
	if s == "" || strings.Contains(s, "${") {
		return ""
	}
	p := path.Join(dir, s)
	if strings.HasSuffix(p, ".xml") {
		return p
	}
	return path.Join(p, "pom.xml")
}

func parsePom(t tree.Tree, p string) *pomInfo {
	pi := &pomInfo{path: p, dir: path.Dir(p), props: map[string]string{}}
	b, err := t.Read(p)
	if err != nil {
		pi.err = err
		return pi
	}
	root, err := parseXML(b)
	if err != nil {
		pi.err = err
		return pi
	}
	if root.name != "project" {
		pi.err = errNotPom
		return pi
	}
	pi.root = root
	pi.g, pi.a, pi.v = root.childText("groupId"), root.childText("artifactId"), root.childText("version")
	pi.packaging = root.childText("packaging")
	if pn := root.child("parent"); pn != nil {
		pr := &parentRef{g: pn.childText("groupId"), a: pn.childText("artifactId"), v: pn.childText("version")}
		if rp := pn.child("relativePath"); rp == nil {
			pr.relDefault = true
		} else if strings.TrimSpace(rp.text) == "" {
			pr.relEmpty = true
		} else {
			pr.relPath = rp.text
		}
		pi.parent = pr
	}
	if props := root.child("properties"); props != nil {
		for _, k := range props.kids {
			pi.props[k.name] = k.text
		}
	}
	collect := func(n *xnode, fromProfile bool) {
		for _, tag := range []struct{ list, item string }{{"modules", "module"}, {"subprojects", "subproject"}} {
			for _, mn := range n.child(tag.list).children(tag.item) {
				pi.modules = append(pi.modules, mn.text)
				if fromProfile {
					pi.profileModules = true
				}
			}
		}
	}
	collect(root, false)
	for _, prof := range root.child("profiles").children("profile") {
		collect(prof, true)
	}
	return pi
}

type constErr string

func (e constErr) Error() string { return string(e) }

const errNotPom = constErr("root element is not <project>")

// findParent resolves the local parent POM path of pi ("" when the parent is remote or absent).
func (m *mvnModel) findParent(pi *pomInfo) string {
	pr := pi.parent
	if pr == nil || pr.relEmpty {
		return ""
	}
	cand := path.Join(pi.dir, "../pom.xml")
	if !pr.relDefault {
		rp := strings.ReplaceAll(pr.relPath, "\\", "/")
		cand = path.Join(pi.dir, rp)
		if !strings.HasSuffix(cand, ".xml") {
			cand = path.Join(cand, "pom.xml")
		}
	}
	c, ok := m.poms[cand]
	if !ok || c == pi || c.err != nil {
		return ""
	}
	// Maven accepts the file only when its coordinates match the declared parent.
	if c.a != pr.a {
		return ""
	}
	if g := c.g; g != "" && pr.g != "" && g != pr.g && !strings.Contains(g, "${") && !strings.Contains(pr.g, "${") {
		return ""
	}
	return cand
}

// chain returns pi followed by its local ancestors.
func (m *mvnModel) chain(pi *pomInfo) []*pomInfo {
	var out []*pomInfo
	seen := map[string]bool{}
	for cur := pi; cur != nil && !seen[cur.path]; {
		seen[cur.path] = true
		out = append(out, cur)
		if cur.localParent == "" {
			break
		}
		cur = m.poms[cur.localParent]
	}
	return out
}

func (m *mvnModel) effGroup(pi *pomInfo) string {
	g := pi.g
	if g == "" && pi.parent != nil {
		g = pi.parent.g
	}
	return m.interp(pi, g)
}

// lookup finds the value of property name for pi (builtin project.* values first, then <properties> up the local parent chain).
func (m *mvnModel) lookup(pi *pomInfo, name string) (string, bool) {
	switch name {
	case "project.groupId", "pom.groupId":
		g := pi.g
		if g == "" && pi.parent != nil {
			g = pi.parent.g
		}
		return g, g != ""
	case "project.artifactId", "pom.artifactId":
		return pi.a, pi.a != ""
	case "project.version", "pom.version", "version":
		v := pi.v
		if v == "" && pi.parent != nil {
			v = pi.parent.v
		}
		return v, v != ""
	case "project.packaging":
		if pi.packaging == "" {
			return "jar", true
		}
		return pi.packaging, true
	case "project.parent.groupId", "parent.groupId":
		if pi.parent != nil {
			return pi.parent.g, pi.parent.g != ""
		}
	case "project.parent.artifactId", "parent.artifactId":
		if pi.parent != nil {
			return pi.parent.a, pi.parent.a != ""
		}
	case "project.parent.version", "parent.version":
		if pi.parent != nil {
			return pi.parent.v, pi.parent.v != ""
		}
	case "project.basedir", "basedir", "pom.basedir":
		return ".", true // paths are interpreted relative to the module directory
	case "maven.multiModuleProjectDirectory":
		return relDir(pi.dir, m.reactorRoot(pi).dir), true
	case "project.parent.basedir", "parent.basedir":
		if pi.localParent != "" {
			return relDir(pi.dir, path.Dir(pi.localParent)), true
		}
	}
	for _, c := range m.chain(pi) {
		if v, ok := c.props[name]; ok {
			return v, true
		}
	}
	return "", false
}

// interp substitutes ${...} references in s; unresolvable ones stay in the result.
func (m *mvnModel) interp(pi *pomInfo, s string) string {
	for i := 0; i < 10 && strings.Contains(s, "${"); i++ {
		var sb strings.Builder
		changed := false
		for len(s) > 0 {
			j := strings.Index(s, "${")
			if j < 0 {
				sb.WriteString(s)
				break
			}
			k := strings.IndexByte(s[j:], '}')
			if k < 0 {
				sb.WriteString(s)
				break
			}
			sb.WriteString(s[:j])
			name := s[j+2 : j+k]
			if v, ok := m.lookup(pi, name); ok && v != "${"+name+"}" {
				sb.WriteString(v)
				changed = true
			} else {
				sb.WriteString(s[j : j+k+1])
			}
			s = s[j+k+1:]
		}
		s = sb.String()
		if !changed {
			break
		}
	}
	return s
}

// components groups POMs connected by module declarations and local parent links and records the common ancestor directory of each.
func (m *mvnModel) components() {
	parent := map[string]string{}
	var find func(x string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	union := func(a, b string) { parent[find(a)] = find(b) }
	for _, pi := range m.order {
		find(pi.path)
		for _, d := range pi.declaredBy {
			union(pi.path, d)
		}
		if pi.localParent != "" {
			union(pi.path, pi.localParent)
		}
	}
	ids := map[string]int{}
	dirs := map[int][]string{}
	for _, pi := range m.order {
		r := find(pi.path)
		id, ok := ids[r]
		if !ok {
			id = len(ids)
			ids[r] = id
		}
		m.comp[pi.path] = id
		dirs[id] = append(dirs[id], pi.dir)
	}
	for id, ds := range dirs {
		m.compRoot[id] = commonDir(ds)
	}
}

// commonDir is the deepest directory containing every dir.
func commonDir(dirs []string) string {
	c := dirs[0]
	for _, d := range dirs[1:] {
		for !eco.Within(d, c) {
			c = path.Dir(c)
		}
	}
	return c
}

// reactorRoot returns the topmost POM that (transitively) lists pi as a module, or pi itself.
func (m *mvnModel) reactorRoot(pi *pomInfo) *pomInfo {
	cur := pi
	seen := map[string]bool{}
	for !seen[cur.path] {
		seen[cur.path] = true
		if len(cur.declaredBy) == 0 {
			return cur
		}
		ds := append([]string(nil), cur.declaredBy...)
		sort.Strings(ds)
		next := m.poms[ds[0]]
		if next == nil {
			return cur
		}
		cur = next
	}
	return cur
}

func (m *mvnModel) isShared(pi *pomInfo) bool {
	return len(pi.modules) > 0 || len(m.children[pi.path]) > 0
}

// ref is a coordinate a POM refers to.
type mref struct {
	g, a string
	what string // "dependency", "plugin", "extension", "BOM import"
}

const defaultPluginGroup = "org.apache.maven.plugins"

// refs lists every module-capable coordinate referenced by pi: dependencies (not those that only manage a version), plugins,
// plugin dependencies, extensions. inherited: the references a child module inherits from pi, which leaves out <pluginManagement>
// (a managed plugin applies only where a module declares it, and that declaration is the module's own reference).
func (pi *pomInfo) refs(inherited bool) []mref {
	var out []mref
	if pi.root == nil {
		return nil
	}
	pi.root.walk(func(x *xnode, anc []*xnode) {
		if len(anc) < 2 {
			return
		}
		par := anc[len(anc)-1]
		switch x.name {
		case "dependency":
			if par.name != "dependencies" {
				return
			}
			managed := len(anc) >= 3 && anc[len(anc)-2].name == "dependencyManagement"
			scope := x.childText("scope")
			if managed && scope != "import" {
				return
			}
			what := "dependency"
			if managed {
				what = "BOM import"
			}
			out = append(out, mref{g: x.childText("groupId"), a: x.childText("artifactId"), what: what})
		case "plugin":
			if par.name != "plugins" {
				return
			}
			if inherited && len(anc) >= 3 && anc[len(anc)-2].name == "pluginManagement" {
				return
			}
			g := x.childText("groupId")
			if g == "" {
				g = defaultPluginGroup
			}
			out = append(out, mref{g: g, a: x.childText("artifactId"), what: "plugin"})
		case "extension":
			if par.name != "extensions" {
				return
			}
			out = append(out, mref{g: x.childText("groupId"), a: x.childText("artifactId"), what: "extension"})
		}
	})
	return out
}

// externalPaths lists the values of source/resource directory elements of pi (raw).
func (pi *pomInfo) externalPaths() []string {
	var out []string
	if pi.root == nil {
		return nil
	}
	pi.root.walk(func(x *xnode, anc []*xnode) {
		switch x.name {
		case "sourceDirectory", "testSourceDirectory", "scriptSourceDirectory", "directory", "source":
			if x.text == "" || len(x.kids) > 0 {
				return
			}
			if x.name == "directory" {
				// <resource><directory> / <testResource><directory>; <build><directory> is the output directory (target).
				if len(anc) == 0 {
					return
				}
				par := anc[len(anc)-1].name
				if par != "resource" && par != "testResource" {
					return
				}
			}
			if x.name == "source" && !(len(anc) > 0 && anc[len(anc)-1].name == "sources") {
				return
			}
			out = append(out, x.text)
		}
	})
	return out
}

// analyze adds the Maven part of the graph to b and returns the units.
func (m *mvnModel) analyze(b *builder) []*unit {
	var units []*unit
	byPath := map[string]*unit{}
	for _, c := range m.conflicts {
		b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: c, Spec: path.Base(c), Class: model.UnresolvedUnknown,
			ScopeTier: model.TierFull, ScopeDir: "", Detail: "two POM files in one directory: the module directory cannot identify the module"})
	}
	for _, pi := range m.order {
		id := UnitID(RunnerMaven, pi.dir, "")
		pkg := m.effGroup(pi) + ":" + m.interp(pi, pi.a)
		root := m.reactorRoot(pi)
		buildFile := root.path
		u := &unit{kind: RunnerMaven, id: id, dir: pi.dir, node: Node(id), pkg: pkg, own: []string{pi.path},
			config: LaunchConfig(buildFile, wrapperFor(m.t, root.dir, "mvnw"))}
		units = append(units, u)
		byPath[pi.path] = u
		b.a.Roles[pi.path] = model.RoleManifest
	}
	for _, pi := range m.order {
		u := byPath[pi.path]
		if pi.err != nil {
			// A POM that cannot be read: its dependencies are unknown, so it depends on every other module.
			for _, o := range units {
				if o != u {
					b.edge(u.node, o.node, model.EdgeProjectRef, "unreadable POM "+pi.path+": "+pi.err.Error())
				}
			}
			continue
		}
		for _, anc := range m.chain(pi)[1:] {
			b.edge(u.node, anc.path, model.EdgeImplicit, "parent POM")
		}
		// A module inherits the <dependencies>, <build><plugins> and <extensions> of its local parent chain, so the references of
		// every ancestor POM are its own (interpolated in the child, as in the effective POM).
		var refs []mref
		for i, src := range m.chain(pi) {
			refs = append(refs, src.refs(i > 0)...)
		}
		for _, r := range refs {
			g, a := m.interp(pi, r.g), m.interp(pi, r.a)
			if !strings.Contains(g, "${") && !strings.Contains(a, "${") {
				for _, d := range m.ga[g+":"+a] {
					if d != pi {
						b.edge(u.node, byPath[d.path].node, model.EdgeProjectRef, r.what+" "+g+":"+a)
					}
				}
				continue
			}
			// The coordinate cannot be resolved: it may name a module of this repo.
			var cands []*pomInfo
			for _, d := range m.order {
				if d == pi {
					continue
				}
				if strings.Contains(g, "${") || m.effGroup(d) == g {
					if strings.Contains(a, "${") || m.interp(d, d.a) == a {
						cands = append(cands, d)
					}
				}
			}
			if len(cands) > 0 {
				for _, d := range cands {
					b.edge(u.node, byPath[d.path].node, model.EdgeProjectRef, r.what+" "+g+":"+a+" (unresolved coordinate)")
				}
				b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: pi.path, Spec: g + ":" + a, Class: model.UnresolvedUnknown,
					ScopeTier: model.TierModule, ScopeDir: m.compRoot[m.comp[pi.path]],
					Detail: r.what + " coordinate has an unresolvable property and may name a module of this repository"})
			}
		}
		m.externalEdges(b, pi, u)
	}
	return units
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

// externalEdges adds the dependencies of u on source/resource directories that lie outside the module directory.
func (m *mvnModel) externalEdges(b *builder, pi *pomInfo, u *unit) {
	for _, raw := range pi.externalPaths() {
		v := m.interp(pi, raw)
		if strings.Contains(v, "${") {
			if strings.HasPrefix(v, "${project.build.") || strings.HasPrefix(v, "${project.reporting.") {
				continue // under target/
			}
			b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: pi.path, Spec: raw, Class: model.UnresolvedUnknown,
				ScopeTier: model.TierModule, ScopeDir: m.compRoot[m.comp[pi.path]],
				Detail: "source/resource directory with an unresolvable property"})
			continue
		}
		v = strings.ReplaceAll(v, "\\", "/")
		if strings.HasPrefix(v, "/") || (len(v) > 1 && v[1] == ':') {
			continue
		}
		target := path.Join(pi.dir, v)
		if eco.Within(target, pi.dir) || target == ".." || strings.HasPrefix(target, "../") {
			continue // owned by the module anyway, or outside the repository
		}
		if m.t.Has(target) {
			b.edge(u.node, target, model.EdgeInclude, "source file "+raw)
		} else {
			b.edge(u.node, escapeGlob(target)+"/**", model.EdgePattern, "source directory "+raw)
		}
	}
}

// relDir returns the path of directory to relative to directory from (both repo-relative, "." for the root).
func relDir(from, to string) string {
	if from == to {
		return "."
	}
	fp, tp := strings.Split(from, "/"), strings.Split(to, "/")
	if from == "." {
		fp = nil
	}
	if to == "." {
		tp = nil
	}
	i := 0
	for i < len(fp) && i < len(tp) && fp[i] == tp[i] {
		i++
	}
	var parts []string
	for j := i; j < len(fp); j++ {
		parts = append(parts, "..")
	}
	parts = append(parts, tp[i:]...)
	if len(parts) == 0 {
		return "."
	}
	return strings.Join(parts, "/")
}

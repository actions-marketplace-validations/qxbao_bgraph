package python

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/lockfile"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/tree"
)

// l1Formats are the lockfiles whose closure passed the differential check against the package manager (docs/lockfiles.md);
// every other dependency file stays at level L0.
var l1Formats = map[string]bool{"uv.lock": true, "poetry.lock": true}

// pytestPluginDists are distributions known to register a pytest11 entry point although their name does not mention
// pytest: pytest loads every installed plugin in every run, imported or not.
var pytestPluginDists = map[string]bool{"pluggy": true, "iniconfig": true, "hypothesis": true, "anyio": true, "faker": true,
	"syrupy": true, "typeguard": true, "time-machine": true, "inline-snapshot": true, "logfire": true, "pyfakefs": true,
	"requests-mock": true, "respx": true, "xdoctest": true, "nbval": true, "teamcity-messages": true, "tavern": true,
	"schemathesis": true, "polyfactory": true, "mimesis": true, "jaxtyping": true}

// pluginCandidate reports whether the distribution (normalized name) may be a pytest plugin.
func pluginCandidate(name string) bool {
	return pytestPluginDists[name] || strings.Contains(name, "pytest")
}

// pluginClosure returns the names of the packages reachable from a package that may be a pytest plugin (the plugin and
// what it imports when pytest loads it).
func pluginClosure(g *lockfile.Graph) map[string]bool {
	out := map[string]bool{}
	for id, n := range g.Nodes {
		if n.Project != "" || !pluginCandidate(n.Name) {
			continue
		}
		for _, r := range g.Packages(id) {
			if m, ok := g.Nodes[r]; ok && m.Project == "" {
				out[m.Name] = true
			}
		}
	}
	return out
}

func readLock(t tree.Tree, p string) (*lockfile.Graph, error) {
	if t == nil || !t.Has(p) {
		return nil, fmt.Errorf("lockfile absent")
	}
	b, err := t.Read(p)
	if err != nil {
		return nil, err
	}
	return lockfile.Parse(p, path.Dir(p), b)
}

// classifyLock compares the resolved dependency closure of every project of the uv.lock or poetry.lock p (level L1,
// docs/lockfiles.md).
//
// A lockfile project (uv: a workspace member or path dependency in the repo, and the lockfile's own directory; poetry:
// the lockfile's directory) whose closure changed → every test under its directory, and every file of it counts as
// changed so that importers in other projects follow. The lockfile's root project carries the dependency groups uv
// installs with every member: when its closure changes, every project under the lockfile's directory is selected.
//
// The environment is shared (one virtual environment per lockfile), so a package outside a project's closure can still
// reach its tests:
//   - pytest loads every installed plugin: a changed package that may be a pytest plugin (named after pytest, or a known
//     plugin distribution) or that such a package depends on selects every project under the lockfile's directory;
//   - an import no package of the project's closure (nor of the root's) provides by name may come from any package of the
//     environment: when packages changed, such a project is selected.
func (e *Eco) classifyLock(p string, base, head tree.Tree, a *eco.Analysis) []model.Widening {
	dir := path.Dir(p)
	l0 := func(detail string) []model.Widening {
		return []model.Widening{w(model.ReasonLockChanged, p, projectDir(a, p), "lockfile at level L0 ("+detail+")")}
	}
	if e.rules.LockLevel() == rules.LockL0 {
		return l0("bgraph.toml [lockfile] level")
	}
	if a == nil {
		return l0("no analysis")
	}
	bg, err := readLock(base, p)
	if err != nil {
		return l0("base: " + err.Error())
	}
	hg, err := readLock(head, p)
	if err != nil {
		return l0("head: " + err.Error())
	}
	d, err := lockfile.Compare(bg, hg)
	if err != nil {
		return l0(err.Error())
	}
	type wid struct {
		detail string
		exact  bool
	}
	widen := map[string]wid{}
	covered := func(pd string) bool {
		for s, x := range widen {
			if (x.exact && s == pd) || (!x.exact && eco.Within(pd, s)) {
				return true
			}
		}
		return false
	}
	for _, pr := range d.ChangedProjects() {
		if !eco.Within(pr, dir) {
			continue
		}
		detail := "dependency closure changed (L1)"
		if pr == dir {
			detail = "dependency closure of the lockfile's root project changed: its dependency groups are installed with every project (L1)"
		}
		widen[pr] = wid{detail: detail}
	}
	names := d.ChangedNames()
	if !covered(dir) && len(names) > 0 {
		bp, hp := pluginClosure(bg), pluginClosure(hg)
		for _, n := range names {
			if bp[n] || hp[n] {
				widen[dir] = wid{detail: fmt.Sprintf("changed package %q may be (or be loaded by) a pytest plugin, which pytest loads in every test run of the lockfile's environment (L1)", n)}
				break
			}
		}
	}
	if len(names) > 0 && !covered(dir) {
		for _, pd := range e.unexplainedImports(a, dir, bg, hg) {
			if !covered(pd[0]) {
				widen[pd[0]] = wid{exact: true, detail: fmt.Sprintf("imports %q, which no package of its lockfile closure provides by name: "+
					"it may come from a changed package of the shared environment (L1)", pd[1])}
			}
		}
	}
	shown := names
	if len(shown) > 8 {
		shown = append(slices.Clone(shown[:8]), "...")
	}
	changedNames := strings.Join(shown, ", ")
	if changedNames == "" {
		changedNames = "none (edges only)"
	}
	dirs := make([]string, 0, len(widen))
	for pr := range widen {
		dirs = append(dirs, pr)
	}
	slices.Sort(dirs)
	var out []model.Widening
	for _, pr := range dirs {
		x := widen[pr]
		wd := w(model.ReasonLockChanged, p, pr, x.detail+"; changed packages: "+changedNames)
		wd.Exact = x.exact
		for f, r := range a.Roles {
			if r != model.RoleSource && r != model.RoleTest && r != model.RoleConfig && r != model.RoleData {
				continue
			}
			if (x.exact && projectDir(a, f) == pr) || (!x.exact && eco.Within(f, pr)) {
				wd.Seeds = append(wd.Seeds, f)
			}
		}
		slices.Sort(wd.Seeds)
		out = append(out, wd)
	}
	return out
}

// unexplainedImports returns, for every Python project under dir, its first external top-level import (sorted) that no
// package of its lockfile closure provides by name ([project dir, module] pairs). The closure is the one of the deepest
// lockfile project containing the project, together with the lockfile root's (its dependency groups are installed with
// every member), on both sides. A name provides a module when it is the module's name normalized, a known distribution
// of it, or a namespace distribution under it (declared.isDeclared).
func (e *Eco) unexplainedImports(a *eco.Analysis, dir string, graphs ...*lockfile.Graph) [][2]string {
	tops := map[string]map[string]bool{} // project dir → external top-level modules imported by its files
	projOf := map[string]string{}
	for _, ed := range a.Edges {
		top, ok := strings.CutPrefix(ed.To, extPrefix)
		if !ok {
			continue
		}
		fd := path.Dir(ed.From)
		pd, ok := projOf[fd]
		if !ok {
			pd = projectDir(a, ed.From)
			projOf[fd] = pd
		}
		if !eco.Within(pd, dir) {
			continue
		}
		if tops[pd] == nil {
			tops[pd] = map[string]bool{}
		}
		tops[pd][strings.TrimSuffix(top, ">")] = true
	}
	var out [][2]string
	pds := make([]string, 0, len(tops))
	for pd := range tops {
		pds = append(pds, pd)
	}
	slices.Sort(pds)
	for _, pd := range pds {
		provided := declared{}
		for _, g := range graphs {
			for _, lp := range []string{lockProject(g, pd, dir), dir} {
				id, ok := g.Projects[lp]
				if !ok {
					continue
				}
				for _, nid := range g.Packages(id) {
					if n, ok := g.Nodes[nid]; ok && n.Package != "" {
						provided[normDist(n.Package)] = true
					}
				}
			}
		}
		mods := make([]string, 0, len(tops[pd]))
		for m := range tops[pd] {
			mods = append(mods, m)
		}
		slices.Sort(mods)
		for _, m := range mods {
			if !provided.isDeclared(m) {
				out = append(out, [2]string{pd, m})
				break
			}
		}
	}
	return out
}

// lockProject returns the deepest project of g whose directory contains pd (dir, the lockfile's directory, otherwise).
func lockProject(g *lockfile.Graph, pd, dir string) string {
	best := dir
	for lp := range g.Projects {
		if eco.Within(pd, lp) && eco.Within(lp, dir) && len(lp) > len(best) {
			best = lp
		}
	}
	return best
}

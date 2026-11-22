// Package lockfile reads lockfiles into a resolved dependency graph and compares the dependency closure of every project
// between two trees (docs/plan.md §6.8, levels L1 and L2).
//
// Supported formats: pnpm-lock.yaml (lockfile versions 6.x and 9.0), package-lock.json (lockfileVersion 2 and 3, the
// "packages" section) and Cargo.lock (version 3 and 4). Anything else, or a file that does not parse, is an error: the
// caller stays at level L0.
//
// A graph has one node per installed package and one node per project (workspace member). The closure of a project is
// every node reachable from its project node, together with every resolved edge; two closures are equal when the same
// packages (by full lockfile content: version, integrity, source, resolution...) are reached through the same edges.
// A project reached through a workspace link is part of the closure of its dependents, so a dependency change of a
// workspace package also changes the closure of every project that depends on it.
package lockfile

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
)

// Formats.
const (
	FormatPnpm  = "pnpm"
	FormatNpm   = "npm"
	FormatCargo = "cargo"
)

// Node is an installed package or a project.
type Node struct {
	ID   string
	Name string // package name as it is imported (npm: the node_modules folder name; cargo: the crate name)
	// Package and Version: the real package name (differs from Name for an npm alias) and its resolved version, for reports.
	Package, Version string
	// Ident is the node's full lockfile content in canonical form: any difference means a different installed package.
	Ident string
	// Deps maps a dependency name to the node it resolves to. An edge to a missing node points to MissingID(name).
	Deps map[string]string
	// Project is the repo-relative directory of the project when the node is a project node.
	Project string
	// Scripts: the package runs install scripts (npm hasInstallScript, pnpm requiresBuild) or, when unknown, true.
	Scripts bool
	// Peer: the package has peer dependencies (its resolution depends on its dependents).
	Peer bool
	// Local: a local (non-registry) package: a workspace link, a file: or path dependency.
	Local bool
}

// Graph is a parsed lockfile.
type Graph struct {
	Format  string
	Version string
	// Extra is the canonical form of every part of the file outside the per-package graph (format version, resolution
	// settings, overrides, patches...). Two graphs with different Extra cannot be compared: level L0.
	Extra string
	Nodes map[string]*Node
	// Projects maps a project directory (repo-relative, "." for the repo root) to its node ID.
	Projects map[string]string
	// ScriptsKnown: the format records which packages run install scripts (pnpm 9.0 does not).
	ScriptsKnown bool
}

// MissingID is the node ID of a dependency the lockfile does not resolve (an optional or peer dependency not installed).
func MissingID(name string) string { return "<missing:" + name + ">" }

// Parse reads a lockfile by its base name. dir is the repo-relative directory of the lockfile.
func Parse(name, dir string, data []byte) (*Graph, error) {
	switch path.Base(name) {
	case "pnpm-lock.yaml":
		return ParsePnpm(dir, data)
	case "package-lock.json", "npm-shrinkwrap.json":
		return ParseNpm(dir, data)
	case "Cargo.lock":
		return ParseCargo(dir, data)
	}
	return nil, fmt.Errorf("unsupported lockfile %s", name)
}

// Supported reports whether Parse handles the lockfile base name.
func Supported(name string) bool {
	switch path.Base(name) {
	case "pnpm-lock.yaml", "package-lock.json", "npm-shrinkwrap.json", "Cargo.lock":
		return true
	}
	return false
}

func (g *Graph) ident(id string) string {
	if n, ok := g.Nodes[id]; ok {
		return n.Ident
	}
	return id
}

// Closure returns the closure of the node id: one string per reached node ("N <ident>") and per resolved edge
// ("E <ident> <dep name> <dep ident>").
func (g *Graph) Closure(id string) map[string]bool {
	out := map[string]bool{}
	seen := map[string]bool{id: true}
	queue := []string{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		n, ok := g.Nodes[cur]
		if !ok {
			out["N "+cur] = true
			continue
		}
		out["N "+n.Ident] = true
		for name, to := range n.Deps {
			out["E "+n.Ident+" "+name+" "+g.ident(to)] = true
			if !seen[to] {
				seen[to] = true
				queue = append(queue, to)
			}
		}
	}
	return out
}

// Packages returns the IDs of every node reachable from id (id included).
func (g *Graph) Packages(id string) []string {
	seen := map[string]bool{id: true}
	queue := []string{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if n, ok := g.Nodes[cur]; ok {
			for _, to := range n.Deps {
				if !seen[to] {
					seen[to] = true
					queue = append(queue, to)
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// Diff is the comparison of two graphs of the same lockfile.
type Diff struct {
	Base, Head *Graph
	// Changed: project directories (of either side) whose closure differs, or that exist on one side only.
	Changed map[string]bool
	// Names: names of the packages whose content differs between the sides (a node identity present on one side only).
	Names map[string]bool
}

// Compare compares two graphs. It returns an error when they cannot be compared (different format or Extra).
func Compare(base, head *Graph) (*Diff, error) {
	if base.Format != head.Format {
		return nil, fmt.Errorf("lockfile format changed (%s → %s)", base.Format, head.Format)
	}
	if base.Version != head.Version {
		return nil, fmt.Errorf("lockfile version changed (%s → %s)", base.Version, head.Version)
	}
	if base.Extra != head.Extra {
		return nil, fmt.Errorf("lockfile settings outside the package graph changed (overrides, patches, resolution settings...)")
	}
	d := &Diff{Base: base, Head: head, Changed: map[string]bool{}, Names: map[string]bool{}}
	dirs := map[string]bool{}
	for p := range base.Projects {
		dirs[p] = true
	}
	for p := range head.Projects {
		dirs[p] = true
	}
	for p := range dirs {
		bi, ok1 := base.Projects[p]
		hi, ok2 := head.Projects[p]
		if !ok1 || !ok2 || !equalSets(base.Closure(bi), head.Closure(hi)) {
			d.Changed[p] = true
		}
	}
	bid, hid := map[string]bool{}, map[string]bool{}
	for _, n := range base.Nodes {
		bid[n.Ident] = true
	}
	for _, n := range head.Nodes {
		hid[n.Ident] = true
	}
	for _, n := range base.Nodes {
		if !hid[n.Ident] && n.Project == "" {
			d.Names[n.Name] = true
		}
	}
	for _, n := range head.Nodes {
		if !bid[n.Ident] && n.Project == "" {
			d.Names[n.Name] = true
		}
	}
	return d, nil
}

// ChangedProjects returns the sorted directories of the projects whose closure changed.
func (d *Diff) ChangedProjects() []string {
	out := make([]string, 0, len(d.Changed))
	for p := range d.Changed {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ChangedNames returns the sorted names of the packages whose content changed.
func (d *Diff) ChangedNames() []string {
	out := make([]string, 0, len(d.Names))
	for p := range d.Names {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Direct describes, for one project whose closure changed, which of its direct dependencies carry the change (level L2).
type Direct struct {
	// Deps: names of the direct dependencies whose own closure changed, added or removed.
	Deps []string
	// Risky: when not empty, the change cannot be narrowed to importers of Deps (level L2 falls back to L1) and says why.
	Risky string
}

// DirectChanges computes the direct dependencies of project dir whose closure changed.
func (d *Diff) DirectChanges(dir string) Direct {
	bi, ok1 := d.Base.Projects[dir]
	hi, ok2 := d.Head.Projects[dir]
	if !ok1 || !ok2 {
		return Direct{Risky: "project added or removed in the lockfile"}
	}
	bn, hn := d.Base.Nodes[bi], d.Head.Nodes[hi]
	var out Direct
	names := map[string]bool{}
	for k := range bn.Deps {
		names[k] = true
	}
	for k := range hn.Deps {
		names[k] = true
	}
	for name := range names {
		bt, okb := bn.Deps[name]
		ht, okh := hn.Deps[name]
		if !okb || !okh {
			return Direct{Risky: fmt.Sprintf("direct dependency %q added or removed", name)}
		}
		if equalSets(d.Base.Closure(bt), d.Head.Closure(ht)) {
			continue
		}
		for _, g := range []struct {
			g  *Graph
			id string
		}{{d.Base, bt}, {d.Head, ht}} {
			if n, ok := g.g.Nodes[g.id]; ok && (n.Local || n.Project != "") {
				return Direct{Risky: fmt.Sprintf("dependency %q is a local or workspace package", name)}
			}
		}
		out.Deps = append(out.Deps, name)
	}
	slices.Sort(out.Deps)
	// Every package that differs inside the project's closure: install scripts and peer dependencies cannot be narrowed.
	bc, hc := d.Base.Packages(bi), d.Head.Packages(hi)
	bIdent, hIdent := map[string]bool{}, map[string]bool{}
	for _, id := range bc {
		bIdent[d.Base.ident(id)] = true
	}
	for _, id := range hc {
		hIdent[d.Head.ident(id)] = true
	}
	check := func(g *Graph, ids []string, other map[string]bool) string {
		for _, id := range ids {
			n, ok := g.Nodes[id]
			if !ok || other[n.Ident] {
				continue
			}
			switch {
			case n.Scripts && g.ScriptsKnown:
				return fmt.Sprintf("changed package %q runs install scripts", n.Name)
			case n.Scripts:
				return fmt.Sprintf("the lockfile format does not record install scripts (changed package %q)", n.Name)
			case n.Peer:
				return fmt.Sprintf("changed package %q has peer dependencies", n.Name)
			}
		}
		return ""
	}
	if r := check(d.Base, bc, hIdent); r != "" {
		return Direct{Deps: out.Deps, Risky: r}
	}
	if r := check(d.Head, hc, bIdent); r != "" {
		return Direct{Deps: out.Deps, Risky: r}
	}
	return out
}

// canon returns a canonical JSON form of v (map keys sorted by encoding/json).
func canon(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// join joins a lockfile-relative path to the lockfile directory; ok=false when the result leaves the repo.
func join(dir, rel string) (string, bool) {
	p := path.Join(dir, rel)
	if p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
		return p, false
	}
	return p, true
}

// ClosureChanged reports whether the closure of node bid in base differs from the closure of node hid in head.
func ClosureChanged(base, head *Graph, bid, hid string) bool {
	return !equalSets(base.Closure(bid), head.Closure(hid))
}

package lockfile

import (
	"fmt"
	"sort"

	"github.com/pelletier/go-toml/v2"
)

// FormatPoetry is the poetry.lock format.
const FormatPoetry = "poetry"

// poetryLockVersions are the [metadata] lock-version values this parser was checked against (docs/lockfiles.md).
var poetryLockVersions = map[string]bool{"1.0": true, "1.1": true, "2.0": true, "2.1": true}

// ParsePoetry reads a poetry.lock (lock-version 1.0, 1.1, 2.0, 2.1).
//
// A poetry.lock resolves one project, the pyproject.toml beside it, which is not an entry of the file: the lockfile's
// directory is the only project, and its closure is every package of the file (the union over every group, extra and
// marker). Edges come from [package.dependencies], by name (PEP 503 normalized): poetry does not record which entry a
// dependency resolved to, so a name shared by several entries (one per marker range) has an edge to each, and a name
// no entry has (a dependency whose marker or extra never applies) points to MissingID. The edge keeps the requirement
// (version constraint, markers, extras, optional), so a requirement change is a closure change.
//
// [metadata] content-hash (a hash of the pyproject.toml dependency sections) is not compared: with identical packages
// the installed set is identical. Neither is [metadata] python-versions, the project's Python range: its effect is in
// the packages locked, their markers and python-versions. Lock-version 1.x [metadata.files] artifact hashes are part of
// their package's content. Every other key (the 1.x top-level [extras], keys of a later poetry) is Extra: a change is
// level L0.
func ParsePoetry(dir string, data []byte) (*Graph, error) {
	var top map[string]any
	if err := toml.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("lockfile poetry.lock: %w", err)
	}
	meta, _ := top["metadata"].(map[string]any)
	lv, _ := meta["lock-version"].(string)
	if !poetryLockVersions[lv] {
		return nil, fmt.Errorf("lockfile poetry.lock: lock-version %q not supported", lv)
	}
	g := &Graph{Format: FormatPoetry, Version: lv, Nodes: map[string]*Node{}, Projects: map[string]string{}}
	extra := map[string]any{}
	for k, v := range top {
		switch k {
		case "package":
		case "metadata":
			for mk, mv := range meta {
				switch mk {
				case "lock-version", "content-hash", "files", "python-versions":
				default:
					extra["metadata."+mk] = mv
				}
			}
		default:
			extra[k] = v
		}
	}
	g.Extra = canonMap(extra)
	files := map[string]any{}
	if f, ok := meta["files"]; ok {
		fm, ok := f.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("lockfile poetry.lock: [metadata.files] is not a table")
		}
		for k, v := range fm {
			files[NormalizeName(k)] = v
		}
	}
	rawPkgs, _ := top["package"].([]any)
	if _, has := top["package"]; has && rawPkgs == nil {
		return nil, fmt.Errorf("lockfile poetry.lock: [[package]] is not an array of tables")
	}
	type poetryPkg struct {
		name, version string
		raw           map[string]any
		id            string
	}
	pkgs := make([]*poetryPkg, 0, len(rawPkgs))
	byName := map[string][]*poetryPkg{}
	for _, rp := range rawPkgs {
		m, ok := rp.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("lockfile poetry.lock: package entry is not a table")
		}
		name, _ := m["name"].(string)
		version, _ := m["version"].(string)
		if name == "" || version == "" {
			return nil, fmt.Errorf("lockfile poetry.lock: package without a name or version")
		}
		p := &poetryPkg{name: NormalizeName(name), version: version, raw: m}
		p.id = "poetry:" + p.name + " " + version + " " + canon(m["source"])
		pkgs = append(pkgs, p)
		byName[p.name] = append(byName[p.name], p)
	}
	rootID := "poetry-root:" + dir
	root := &Node{ID: rootID, Name: rootID, Ident: "poetry root", Deps: map[string]string{}, Project: dir, Local: true}
	for _, p := range pkgs {
		if _, dup := g.Nodes[p.id]; dup {
			return nil, fmt.Errorf("lockfile poetry.lock: duplicate package %s %s", p.name, p.version)
		}
		c := map[string]any{}
		for k, v := range p.raw {
			if k != "dependencies" && k != "description" {
				c[k] = v
			}
		}
		if f, ok := files[p.name]; ok {
			c["metadata.files"] = f
		}
		n := &Node{ID: p.id, Name: p.name, Package: p.name, Version: p.version, Ident: "poetry " + canon(c), Deps: map[string]string{}, Scripts: true}
		if src, ok := p.raw["source"].(map[string]any); ok {
			if t, _ := src["type"].(string); t == "directory" || t == "file" {
				n.Local = true
			}
		}
		if deps, ok := p.raw["dependencies"]; ok {
			dm, ok := deps.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("lockfile poetry.lock: dependencies of %s is not a table", p.name)
			}
			for dname, spec := range dm {
				dn := NormalizeName(dname)
				key := "dep " + dn + " " + canon(spec)
				hits := byName[dn]
				if len(hits) == 0 {
					n.Deps[key] = MissingID(dn)
					continue
				}
				for _, h := range hits {
					n.Deps[key+" -> "+h.version+" "+canon(h.raw["source"])] = h.id
				}
			}
		}
		g.Nodes[p.id] = n
	}
	ids := make([]string, 0, len(g.Nodes))
	for id := range g.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		root.Deps["pkg "+id] = id
	}
	g.Nodes[rootID] = root
	g.Projects[dir] = rootID
	return g, nil
}

package lockfile

import (
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// FormatUv is the uv.lock format.
const FormatUv = "uv"

// uvMaxRevision is the newest uv.lock revision whose content this parser was checked against (docs/lockfiles.md).
const uvMaxRevision = 5

// virtualRootPrefix marks the synthetic project node of a virtual workspace root (a root pyproject.toml without a
// [project] table): it depends on the root's dependency groups and requirements, recorded by name in [manifest].
const virtualRootPrefix = "uv-virtual-root:"

// NormalizeName normalizes a Python distribution name (PEP 503: lowercase, runs of "-", "_", "." become "-").
func NormalizeName(s string) string {
	var b strings.Builder
	prev := false
	for _, c := range strings.ToLower(s) {
		if c == '-' || c == '_' || c == '.' {
			if !prev {
				b.WriteByte('-')
			}
			prev = true
			continue
		}
		prev = false
		b.WriteRune(c)
	}
	return b.String()
}

// uvLocalKinds are the source kinds of a package built from a local directory (a workspace member or a path
// dependency): such a package is a project of the lockfile.
var uvLocalKinds = []string{"editable", "virtual", "directory"}

// ParseUv reads a uv.lock (version 1, revisions up to uvMaxRevision).
//
// Every package from a local directory (source editable, virtual or directory: the workspace members and path
// dependencies) inside the repo is a project, keyed by its directory. Edges are the union of the package's
// dependencies, every optional dependency (extra) and every development dependency group; markers are not evaluated
// (every edge counts) but are part of the edge, so a marker change is a closure change. A dependency names its target
// by name, plus version and source when several packages share the name; a reference that matches several packages is
// an error (level L0).
//
// The lockfile's own directory is always a project: when no package lives there (a virtual workspace root), a
// synthetic project depends on the root's dependency groups and requirements ([manifest]), which uv installs with
// every member.
//
// [options] (exclude-newer, resolution and prerelease modes), [manifest] (members, constraints, overrides,
// requirements), requires-python and the resolution, supported and required markers only steer resolution, list the
// members or bound the environments: their whole effect is in the resolved graph the closures compare (package
// versions, per-package resolution markers, edge markers). Every other top-level key (conflicts, keys of a later uv)
// is Extra: a change is level L0.
func ParseUv(dir string, data []byte) (*Graph, error) {
	var top map[string]any
	if err := toml.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("lockfile uv.lock: %w", err)
	}
	ver, ok := top["version"].(int64)
	if !ok || ver != 1 {
		return nil, fmt.Errorf("lockfile uv.lock: version %v not supported", top["version"])
	}
	var rev int64
	if r, ok := top["revision"]; ok {
		rv, ok := r.(int64)
		if !ok || rv < 0 || rv > uvMaxRevision {
			return nil, fmt.Errorf("lockfile uv.lock: revision %v not supported", r)
		}
		rev = rv
	}
	g := &Graph{Format: FormatUv, Version: fmt.Sprintf("%d.%d", ver, rev), Nodes: map[string]*Node{}, Projects: map[string]string{}}
	extra := map[string]any{}
	for k, v := range top {
		switch k {
		case "version", "revision", "package", "options", "manifest", "requires-python", "resolution-markers", "supported-markers", "required-markers":
		default:
			extra[k] = v
		}
	}
	g.Extra = canonMap(extra)
	rawPkgs, _ := top["package"].([]any)
	if _, has := top["package"]; has && rawPkgs == nil {
		return nil, fmt.Errorf("lockfile uv.lock: [[package]] is not an array of tables")
	}
	type uvPkg struct {
		name, version, source string
		raw                   map[string]any
		id                    string
	}
	pkgs := make([]*uvPkg, 0, len(rawPkgs))
	byName := map[string][]*uvPkg{}
	for _, rp := range rawPkgs {
		m, ok := rp.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("lockfile uv.lock: package entry is not a table")
		}
		name, _ := m["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("lockfile uv.lock: package without a name")
		}
		version, _ := m["version"].(string)
		p := &uvPkg{name: NormalizeName(name), version: version, source: canon(m["source"]), raw: m}
		p.id = "uv:" + p.name + " " + p.version + " " + p.source
		pkgs = append(pkgs, p)
		byName[p.name] = append(byName[p.name], p)
	}
	resolve := func(ref map[string]any) (string, error) {
		name, _ := ref["name"].(string)
		if name == "" {
			return "", fmt.Errorf("lockfile uv.lock: dependency without a name")
		}
		n := NormalizeName(name)
		var hits []*uvPkg
		for _, c := range byName[n] {
			if v, ok := ref["version"].(string); ok && c.version != v {
				continue
			}
			if s, ok := ref["source"]; ok && canon(s) != c.source {
				continue
			}
			hits = append(hits, c)
		}
		switch len(hits) {
		case 0:
			return MissingID(n), nil
		case 1:
			return hits[0].id, nil
		}
		return "", fmt.Errorf("lockfile uv.lock: dependency %q matches %d packages", canon(ref), len(hits))
	}
	addEdges := func(n *Node, kind string, list any) error {
		if list == nil {
			return nil
		}
		arr, ok := list.([]any)
		if !ok {
			return fmt.Errorf("lockfile uv.lock: %s of %s is not an array", kind, n.Name)
		}
		for _, d := range arr {
			ref, ok := d.(map[string]any)
			if !ok {
				return fmt.Errorf("lockfile uv.lock: dependency of %s is not a table", n.Name)
			}
			t, err := resolve(ref)
			if err != nil {
				return err
			}
			// The edge keeps its kind (dependency, extra, group) and every attribute (marker, extras): moving a dependency
			// between groups or changing its marker changes what an install selects.
			n.Deps[kind+" "+canon(ref)] = t
		}
		return nil
	}
	for _, p := range pkgs {
		if _, dup := g.Nodes[p.id]; dup {
			return nil, fmt.Errorf("lockfile uv.lock: duplicate package %s %s", p.name, p.version)
		}
		n := &Node{ID: p.id, Name: p.name, Package: p.name, Version: p.version, Ident: "uv " + uvIdent(p.raw), Deps: map[string]string{}, Scripts: true}
		if err := addEdges(n, "dep", p.raw["dependencies"]); err != nil {
			return nil, err
		}
		for _, key := range []string{"optional-dependencies", "dev-dependencies"} {
			v, ok := p.raw[key]
			if !ok {
				continue
			}
			groups, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("lockfile uv.lock: %s of %s is not a table", key, p.name)
			}
			for gname, list := range groups {
				if err := addEdges(n, key+":"+gname, list); err != nil {
					return nil, err
				}
			}
		}
		if src, ok := p.raw["source"].(map[string]any); ok {
			for _, k := range uvLocalKinds {
				rel, ok := src[k].(string)
				if !ok {
					continue
				}
				n.Local = true
				if pd, in := join(dir, rel); in {
					if _, dup := g.Projects[pd]; dup {
						return nil, fmt.Errorf("lockfile uv.lock: two packages in directory %s", pd)
					}
					n.Project = pd
					g.Projects[pd] = p.id
				}
			}
		}
		g.Nodes[p.id] = n
	}
	if _, ok := g.Projects[dir]; !ok {
		// A virtual workspace root: its dependency groups and requirements are installed with every member. The manifest
		// records them by name (specifiers, not resolutions): an edge to every package of that name.
		id := virtualRootPrefix + dir
		root := &Node{ID: id, Name: id, Ident: "uv virtual root", Deps: map[string]string{}, Project: dir, Local: true}
		man, _ := top["manifest"].(map[string]any)
		var reqs []any
		if r, ok := man["requirements"].([]any); ok {
			reqs = append(reqs, r...)
		}
		if dg, ok := man["dependency-groups"].(map[string]any); ok {
			for gname, list := range dg {
				r, ok := list.([]any)
				if !ok {
					return nil, fmt.Errorf("lockfile uv.lock: manifest dependency group %s is not an array", gname)
				}
				reqs = append(reqs, r...)
			}
		}
		for _, r := range reqs {
			m, ok := r.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("lockfile uv.lock: manifest requirement is not a table")
			}
			name, _ := m["name"].(string)
			n := NormalizeName(name)
			key := "manifest " + canon(m)
			if len(byName[n]) == 0 {
				root.Deps[key] = MissingID(n)
			}
			for _, c := range byName[n] {
				root.Deps[key+" "+c.id] = c.id
			}
		}
		g.Nodes[id] = root
		g.Projects[dir] = id
	}
	return g, nil
}

// uvIdent is the canonical content of a package entry without its edges (compared separately), its local metadata
// (requires-dist / requires-dev: specifiers, whose effect is in the edges) and the artifacts' upload-time and size
// (the hash identifies the artifact).
func uvIdent(m map[string]any) string {
	c := map[string]any{}
	for k, v := range m {
		switch k {
		case "dependencies", "optional-dependencies", "dev-dependencies", "metadata":
		case "sdist":
			c[k] = uvArtifact(v)
		case "wheels":
			if arr, ok := v.([]any); ok {
				out := make([]any, len(arr))
				for i, w := range arr {
					out[i] = uvArtifact(w)
				}
				c[k] = out
			} else {
				c[k] = v
			}
		default:
			c[k] = v
		}
	}
	return canon(c)
}

func uvArtifact(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := map[string]any{}
	for k, x := range m {
		if k != "upload-time" && k != "size" {
			out[k] = x
		}
	}
	return out
}

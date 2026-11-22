package lockfile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type cargoPkg struct {
	Name         string   `toml:"name"`
	Version      string   `toml:"version"`
	Source       string   `toml:"source"`
	Checksum     string   `toml:"checksum"`
	Dependencies []string `toml:"dependencies"`
}

// ParseCargo reads a Cargo.lock with version 3 or 4. Every package without a source (a workspace member or a path
// dependency) is a project candidate: Projects is keyed "<name> <version>" (not by directory, which the lockfile does
// not record); the caller maps them to directories with CargoProject.
//
// Cargo.lock records the union of every dependency kind (normal, dev, build), every target and every feature, which is
// the closure level L1 needs.
func ParseCargo(dir string, data []byte) (*Graph, error) {
	var top map[string]any
	if err := toml.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("Cargo.lock: %w", err)
	}
	var doc struct {
		Version int64      `toml:"version"`
		Package []cargoPkg `toml:"package"`
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("Cargo.lock: %w", err)
	}
	if doc.Version != 3 && doc.Version != 4 {
		return nil, fmt.Errorf("Cargo.lock: version %d not supported", doc.Version)
	}
	g := &Graph{Format: FormatCargo, Version: fmt.Sprint(doc.Version), Nodes: map[string]*Node{}, Projects: map[string]string{}, ScriptsKnown: false}
	extra := map[string]any{}
	for k, v := range top {
		if k != "package" {
			extra[k] = v
		}
	}
	g.Extra = canon(extra)
	byName := map[string][]*cargoPkg{}
	idOf := func(p *cargoPkg) string { return "cargo:" + p.Name + " " + p.Version + " " + p.Source }
	for i := range doc.Package {
		p := &doc.Package[i]
		byName[p.Name] = append(byName[p.Name], p)
	}
	resolve := func(ref string) (string, error) {
		f := strings.Fields(ref)
		if len(f) == 0 {
			return "", fmt.Errorf("Cargo.lock: empty dependency")
		}
		var hits []*cargoPkg
		for _, c := range byName[f[0]] {
			if len(f) >= 2 && c.Version != f[1] {
				continue
			}
			if len(f) >= 3 && "("+c.Source+")" != strings.Join(f[2:], " ") {
				continue
			}
			hits = append(hits, c)
		}
		if len(hits) != 1 {
			return "", fmt.Errorf("Cargo.lock: dependency %q matches %d packages", ref, len(hits))
		}
		return idOf(hits[0]), nil
	}
	for i := range doc.Package {
		p := &doc.Package[i]
		id := idOf(p)
		if _, dup := g.Nodes[id]; dup {
			return nil, fmt.Errorf("Cargo.lock: duplicate package %s %s", p.Name, p.Version)
		}
		n := &Node{ID: id, Name: p.Name, Package: p.Name, Version: p.Version, Ident: "cargo " + p.Name + " " + p.Version + " " + p.Source + " " + p.Checksum,
			Deps: map[string]string{}, Scripts: true}
		for _, d := range p.Dependencies {
			t, err := resolve(d)
			if err != nil {
				return nil, err
			}
			// Two versions of a crate may both be dependencies (renamed); key the edge by the full reference.
			n.Deps[d] = t
		}
		if p.Source == "" {
			n.Local = true
			key := p.Name + " " + p.Version
			n.Project = key
			g.Projects[key] = id
		}
		g.Nodes[id] = n
	}
	return g, nil
}

// CargoProject returns the key in Graph.Projects of the crate named name: the only source-less package with that name.
func (g *Graph) CargoProject(name string) (string, bool) {
	var hits []string
	for k := range g.Projects {
		if n, _, _ := strings.Cut(k, " "); n == name {
			hits = append(hits, k)
		}
	}
	sort.Strings(hits)
	if len(hits) != 1 {
		return "", false
	}
	return hits[0], true
}

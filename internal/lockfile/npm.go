package lockfile

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// npmIdentSkip are package-lock entry keys that describe who needs a package, not what is installed.
var npmIdentSkip = map[string]bool{"dev": true, "optional": true, "devOptional": true, "peer": true, "extraneous": true}

// ParseNpm reads a package-lock.json (or npm-shrinkwrap.json) with lockfileVersion 2 or 3. Only the "packages" section is
// used: it is the installed node_modules tree, and dependencies are resolved by Node's lookup (the package's own
// node_modules, then every ancestor directory's node_modules).
func ParseNpm(dir string, data []byte) (*Graph, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("package-lock.json: %w", err)
	}
	var ver int
	if err := json.Unmarshal(top["lockfileVersion"], &ver); err != nil || (ver != 2 && ver != 3) {
		return nil, fmt.Errorf("package-lock.json: lockfileVersion %s not supported", strings.TrimSpace(string(top["lockfileVersion"])))
	}
	var pkgs map[string]map[string]any
	if err := json.Unmarshal(top["packages"], &pkgs); err != nil || pkgs == nil {
		return nil, fmt.Errorf("package-lock.json: no packages section")
	}
	g := &Graph{Format: FormatNpm, Version: fmt.Sprint(ver), Nodes: map[string]*Node{}, Projects: map[string]string{}, ScriptsKnown: true}
	extra := map[string]any{}
	for k, v := range top {
		switch k {
		case "packages", "name", "version", "dependencies": // "dependencies": v2's legacy tree, a copy of "packages"
			continue
		}
		var x any
		_ = json.Unmarshal(v, &x)
		extra[k] = x
	}
	g.Extra = canon(extra)

	isProject := func(key string) bool {
		return key == "" || !strings.Contains("/"+key+"/", "/node_modules/")
	}
	idOf := func(key string) string { return "npm:" + key }
	// target follows link entries to the entry they point to.
	target := func(key string) string {
		for range 8 {
			e := pkgs[key]
			if e == nil || e["link"] != true {
				return key
			}
			r, _ := e["resolved"].(string)
			key = path.Clean(r)
			if key == "." {
				key = ""
			}
		}
		return key
	}
	lookup := func(from, name string) (string, bool) {
		d := from
		for {
			if path.Base(d) != "node_modules" {
				c := "node_modules/" + name
				if d != "" {
					c = d + "/node_modules/" + name
				}
				if _, ok := pkgs[c]; ok {
					return target(c), true
				}
			}
			if d == "" {
				return "", false
			}
			d = path.Dir(d)
			if d == "." {
				d = ""
			}
		}
	}
	keys := make([]string, 0, len(pkgs))
	for k := range pkgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		e := pkgs[key]
		if e["link"] == true {
			continue // the link's target is its own entry
		}
		proj := isProject(key)
		n := &Node{ID: idOf(key), Deps: map[string]string{}}
		if i := strings.LastIndex(key, "node_modules/"); i >= 0 {
			n.Name = key[i+len("node_modules/"):]
		} else {
			n.Name, _ = e["name"].(string)
		}
		n.Package, n.Version = n.Name, fmt.Sprint(e["version"])
		if pn, ok := e["name"].(string); ok && pn != "" {
			n.Package = pn
		}
		id := map[string]any{}
		for k, v := range e {
			if !npmIdentSkip[k] {
				id[k] = v
			}
		}
		if proj {
			// A project's own fields (name, version, scripts, engines...) are its package.json, compared on its own.
			n.Ident = "project " + key
			n.Local = true
			if rel, ok := join(dir, key); ok {
				n.Project = rel
				g.Projects[rel] = n.ID
			}
		} else {
			n.Ident = "npm " + n.Name + " " + canon(id)
			if r, _ := e["resolved"].(string); r != "" && !strings.Contains(r, "://") {
				n.Local = true // file: / git+ssh-less local path
			}
		}
		n.Scripts = e["hasInstallScript"] == true
		if pd, ok := e["peerDependencies"].(map[string]any); ok && len(pd) > 0 {
			n.Peer = true
		}
		sections := []string{"dependencies", "optionalDependencies", "peerDependencies"}
		if proj {
			sections = append(sections, "devDependencies")
		}
		peerMeta, _ := e["peerDependenciesMeta"].(map[string]any)
		for _, s := range sections {
			m, _ := e[s].(map[string]any)
			for name := range m {
				if _, dup := n.Deps[name]; dup {
					continue
				}
				t, ok := lookup(key, name)
				if !ok {
					optional := s == "optionalDependencies"
					if s == "peerDependencies" {
						pm, _ := peerMeta[name].(map[string]any)
						optional = pm["optional"] == true
					}
					if optional {
						continue
					}
					n.Deps[name] = MissingID(name)
					continue
				}
				n.Deps[name] = idOf(t)
			}
		}
		g.Nodes[n.ID] = n
	}
	// Edges to link targets that have no entry of their own (a link to a directory outside the lockfile's tree).
	for _, n := range g.Nodes {
		for name, t := range n.Deps {
			if _, ok := g.Nodes[t]; !ok && t != MissingID(name) {
				g.Nodes[t] = &Node{ID: t, Name: name, Ident: "npm-local " + t, Local: true}
			}
		}
	}
	if _, ok := g.Projects[dir]; !ok {
		return nil, fmt.Errorf("package-lock.json: no root package entry")
	}
	return g, nil
}

package lockfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// bunVersions are the bun.lock lockfileVersion values whose closures passed the differential check against bun.
var bunVersions = map[string]bool{"1": true}

// bunGraphKeys are the top-level keys that make up the package graph; every other key is Extra.
var bunGraphKeys = map[string]bool{"lockfileVersion": true, "workspaces": true, "packages": true,
	// overrides only steer resolution: every overridden version is the version of the package entry a dependency
	// resolves to (resolution is by name, through the package paths)
	"overrides": true,
	// catalog ranges are resolved the same way, by name: a catalog change shows in the package entries
	"catalog": true, "catalogs": true,
	// patches: compared on their own (Graph.Patches); the patched packages count as changed
	"patchedDependencies": true}

// ParseBun reads a text bun.lock (bun 1.2 and later; JSON with trailing commas). "workspaces" maps each workspace
// directory to its manifest dependencies (the projects); "packages" maps an install path ("a", or "a/b" for the copy
// of b nested under a, scoped names counting as one segment) to the package installed there. A dependency of the
// package at path p resolves, as bun does, to the entry at p/<name>, then at each shorter parent path, then at <name>;
// a workspace's own dependencies start at the workspace's package name. A peer dependency also reaches every copy of
// the package whose version the peer range accepts (the isolated linker resolves peers outside the install paths).
// Every name a workspace's package.json declares resolves too (files is required). The binary bun.lockb is not
// supported.
func ParseBun(dir string, data []byte, files Files) (*Graph, error) {
	if files == nil {
		return nil, fmt.Errorf("bun.lock: the workspace manifests are not available")
	}
	clean, err := jsoncToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("bun.lock: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(clean))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("bun.lock: %w", err)
	}
	ver := fmt.Sprint(top["lockfileVersion"])
	if !bunVersions[ver] {
		return nil, fmt.Errorf("bun.lock: lockfileVersion %s not supported", ver)
	}
	g := &Graph{Format: FormatBun, Version: ver, Nodes: map[string]*Node{}, Projects: map[string]string{}}
	extra := map[string]any{}
	for k, v := range top {
		if !bunGraphKeys[k] {
			extra[k] = v
		}
	}
	g.Extra = canonMap(extra)
	if pd, ok := top["patchedDependencies"].(map[string]any); ok {
		g.Patches = canonMap(pd)
	} else if top["patchedDependencies"] != nil {
		return nil, fmt.Errorf("bun.lock: patchedDependencies is not a map")
	}
	wss, ok := top["workspaces"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("bun.lock: no workspaces")
	}
	pkgs, _ := top["packages"].(map[string]any)
	if top["packages"] != nil && pkgs == nil {
		return nil, fmt.Errorf("bun.lock: packages is not a map")
	}

	// Workspaces: directory → project node; the workspace's package name → directory.
	wsByName := map[string]string{}
	wsKeys := make([]string, 0, len(wss))
	for k := range wss {
		wsKeys = append(wsKeys, k)
	}
	sort.Strings(wsKeys)
	for _, k := range wsKeys {
		w, ok := wss[k].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("bun.lock: workspace %q is not a map", k)
		}
		wk := k
		if wk == "" {
			wk = "."
		}
		rel, ok := join(dir, wk)
		if !ok {
			return nil, fmt.Errorf("bun.lock: workspace %q outside the repo", k)
		}
		id := "bun-project:" + rel
		name, _ := w["name"].(string)
		g.Nodes[id] = &Node{ID: id, Name: name, Package: name, Ident: "project " + rel, Project: rel, Local: true, Deps: map[string]string{}}
		g.Projects[rel] = id
		if name != "" && k != "" {
			wsByName[name] = rel
		}
	}

	// Package keys by package name (the name in the entry's identifier), for peer candidates.
	byName := map[string][]string{}
	pkgKeys := make([]string, 0, len(pkgs))
	for k := range pkgs {
		pkgKeys = append(pkgKeys, k)
	}
	sort.Strings(pkgKeys)
	for _, k := range pkgKeys {
		if arr, ok := pkgs[k].([]any); ok && len(arr) > 0 {
			if id, ok := arr[0].(string); ok {
				if name, _, ok := splitPattern(id); ok {
					byName[name] = append(byName[name], k)
				}
			}
		}
	}
	bunVersion := func(k string) string {
		arr, _ := pkgs[k].([]any)
		id, _ := arr[0].(string)
		_, ref, _ := splitPattern(id)
		if _, ok := parseSemVersion(ref); ok {
			return ref
		}
		return ""
	}

	var perr error
	var nodeFor func(key string) string
	// lookup resolves dependency name from the install path segs (most specific parent first).
	lookup := func(segs []string, name string) (string, bool) {
		for i := len(segs); i >= 0; i-- {
			k := name
			if i > 0 {
				k = strings.Join(segs[:i], "/") + "/" + name
			}
			if _, ok := pkgs[k]; ok {
				return k, true
			}
		}
		return "", false
	}
	addDeps := func(n *Node, segs []string, info map[string]any, project bool) {
		sections := []string{"dependencies", "optionalDependencies", "peerDependencies"}
		if project {
			sections = []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"}
		}
		for _, s := range sections {
			if info[s] == nil {
				continue
			}
			m, ok := info[s].(map[string]any)
			if !ok {
				perr = fmt.Errorf("bun.lock: %s of %s is not a map", s, n.ID)
				return
			}
			names := make([]string, 0, len(m))
			for dn := range m {
				names = append(names, dn)
			}
			sort.Strings(names)
			for _, dn := range names {
				if _, dup := n.Deps[dn]; dup {
					continue
				}
				if s == "peerDependencies" {
					// bun's isolated linker resolves a peer to a copy of the package that need not be the one the
					// install path reaches: every copy whose version the peer range accepts is a candidate.
					rng, _ := m[dn].(string)
					for _, k := range byName[dn] {
						if v := bunVersion(k); v != "" {
							if ok, err := semverSatisfies(v, rng); err == nil && !ok {
								continue
							}
						}
						n.Deps[dn+" (peer "+k+")"] = nodeFor(k)
					}
				}
				k, ok := lookup(segs, dn)
				if !ok {
					// A missing optional dependency or peer is not installed (a peer comes from the dependent, which is
					// in the same closure); any other missing dependency is an error.
					if s == "optionalDependencies" || s == "peerDependencies" {
						continue
					}
					perr = fmt.Errorf("bun.lock: %s: no package for %s %s", n.ID, s, dn)
					return
				}
				n.Deps[dn] = nodeFor(k)
			}
		}
	}
	nodeFor = func(key string) string {
		arr, ok := pkgs[key].([]any)
		if !ok || len(arr) == 0 {
			perr = fmt.Errorf("bun.lock: package %q is not a non-empty array", key)
			return "bun:" + key
		}
		ident, _ := arr[0].(string)
		name, ref, ok := splitPattern(ident)
		if !ok {
			perr = fmt.Errorf("bun.lock: package %q: bad identifier %q", key, ident)
			return "bun:" + key
		}
		if wp, ok := strings.CutPrefix(ref, "workspace:"); ok {
			rel, ok := join(dir, wp)
			if id, ok2 := g.Projects[rel]; ok && ok2 {
				return id
			}
			perr = fmt.Errorf("bun.lock: package %q: workspace %s is not in workspaces", key, wp)
			return "bun:" + key
		}
		id := "bun:" + key
		if _, ok := g.Nodes[id]; ok {
			return id
		}
		segs := bunSegments(key)
		n := &Node{ID: id, Name: segs[len(segs)-1], Package: name, Version: ref, Deps: map[string]string{}, Scripts: true}
		g.Nodes[id] = n
		n.Ident = "bun " + n.Name + " " + canon(arr)
		var info map[string]any
		for _, x := range arr[1:] {
			if m, ok := x.(map[string]any); ok {
				info = m
				break
			}
		}
		// A registry package is [ident, registry, info, integrity]; folders, links, tarballs and git have other shapes.
		n.Local = len(arr) != 4 || strings.Contains(ref, ":")
		if info != nil {
			if pd, ok := info["peerDependencies"].(map[string]any); ok && len(pd) > 0 {
				n.Peer = true
			}
			if b, ok := info["bin"].(map[string]any); ok {
				for k := range b {
					n.Bins = append(n.Bins, k)
				}
				sort.Strings(n.Bins)
			} else if _, ok := info["binDir"]; ok {
				n.Bins = []string{"*"}
			}
			addDeps(n, segs, info, false)
		}
		return id
	}
	for _, k := range wsKeys {
		w := wss[k].(map[string]any)
		wk := k
		if wk == "" {
			wk = "."
		}
		rel, _ := join(dir, wk)
		var segs []string
		if name, _ := w["name"].(string); name != "" && k != "" {
			segs = bunSegments(name)
		}
		pn := g.Nodes[g.Projects[rel]]
		addDeps(pn, segs, w, true)
		// The lockfile drops some of a workspace's declarations (a devDependency that is also a peer): bun installs
		// what the manifest declares, so every declared name resolves too.
		b, err := files.Read(path.Join(rel, "package.json"))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("bun.lock: workspace %s: %v", rel, err)
		}
		var man map[string]json.RawMessage
		if err := json.Unmarshal(b, &man); err != nil {
			return nil, fmt.Errorf("bun.lock: workspace %s: package.json: %v", rel, err)
		}
		for _, s := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
			var d map[string]any
			if raw, ok := man[s]; ok && json.Unmarshal(raw, &d) != nil {
				return nil, fmt.Errorf("bun.lock: workspace %s: package.json %s is not a map", rel, s)
			}
			names := make([]string, 0, len(d))
			for n := range d {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, dn := range names {
				if _, ok := pn.Deps[dn]; ok {
					continue
				}
				k, ok := lookup(segs, dn)
				if !ok {
					if s == "optionalDependencies" || s == "peerDependencies" {
						continue
					}
					return nil, fmt.Errorf("bun.lock: workspace %s: %s %s is not in the lockfile", rel, s, dn)
				}
				pn.Deps[dn] = nodeFor(k)
			}
		}
	}
	if perr != nil {
		return nil, perr
	}
	return g, nil
}

// bunSegments splits an install path into package names ("@a/b/c" → ["@a/b", "c"]).
func bunSegments(key string) []string {
	parts := strings.Split(key, "/")
	var out []string
	for i := 0; i < len(parts); i++ {
		if strings.HasPrefix(parts[i], "@") && i+1 < len(parts) {
			out = append(out, parts[i]+"/"+parts[i+1])
			i++
			continue
		}
		out = append(out, parts[i])
	}
	return out
}

// jsoncToJSON removes comments and trailing commas (outside strings) from JSONC.
func jsoncToJSON(data []byte) ([]byte, error) {
	out := make([]byte, 0, len(data))
	inStr := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inStr {
			out = append(out, c)
			if c == '\\' && i+1 < len(data) {
				i++
				out = append(out, data[i])
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			out = append(out, c)
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			end := bytes.Index(data[i+2:], []byte("*/"))
			if end < 0 {
				return nil, fmt.Errorf("unterminated comment")
			}
			i += end + 3
		case c == ',':
			j := i + 1
			for j < len(data) && (data[j] == ' ' || data[j] == '\n' || data[j] == '\r' || data[j] == '\t') {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue // trailing comma
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	if inStr {
		return nil, fmt.Errorf("unterminated string")
	}
	return out, nil
}

package lockfile

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// pnpmIdentSkip are package/snapshot keys that describe who needs a package, not what is installed. Dependency maps are
// edges, compared through the closure.
var pnpmIdentSkip = map[string]bool{"dev": true, "optional": true, "dependencies": true, "optionalDependencies": true,
	"transitivePeerDependencies": true}

// pnpmGraphKeys are the top-level keys that make up the package graph; every other key is Extra.
var pnpmGraphKeys = map[string]bool{"importers": true, "packages": true, "snapshots": true,
	// a lockfile without importers (single project) lists the root's dependencies at the top level
	"dependencies": true, "devDependencies": true, "optionalDependencies": true, "dependenciesMeta": true, "specifiers": true}

// ParsePnpm reads a pnpm-lock.yaml with lockfileVersion 6.x or 9.0.
func ParsePnpm(dir string, data []byte) (*Graph, error) {
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("pnpm-lock.yaml: %w", err)
	}
	ver := fmt.Sprint(top["lockfileVersion"])
	v9 := ver == "9.0"
	if !v9 && !strings.HasPrefix(ver, "6.") {
		return nil, fmt.Errorf("pnpm-lock.yaml: lockfileVersion %s not supported", ver)
	}
	g := &Graph{Format: FormatPnpm, Version: ver, Nodes: map[string]*Node{}, Projects: map[string]string{}, ScriptsKnown: !v9}
	extra := map[string]any{}
	for k, v := range top {
		if !pnpmGraphKeys[k] {
			extra[k] = v
		}
	}
	g.Extra = canon(extra)

	pkgs, _ := top["packages"].(map[string]any)
	snaps, _ := top["snapshots"].(map[string]any)
	importers, _ := top["importers"].(map[string]any)
	if importers == nil {
		importers = map[string]any{".": map[string]any{
			"dependencies": top["dependencies"], "devDependencies": top["devDependencies"], "optionalDependencies": top["optionalDependencies"]}}
	}
	importerDir := map[string]string{} // importer key → repo-relative dir
	for k := range importers {
		rel, ok := join(dir, k)
		if !ok {
			return nil, fmt.Errorf("pnpm-lock.yaml: importer %q outside the repo", k)
		}
		importerDir[path.Clean(k)] = rel
	}

	var perr error
	// snapKey maps a dependency reference to the key of its snapshot (9.0) or package (6.x) entry.
	snapKey := func(name, ref string) string {
		base := ref
		if i := strings.IndexByte(base, '('); i >= 0 {
			base = base[:i]
		}
		if v9 {
			if strings.LastIndexByte(base, '@') > 0 {
				return ref // alias: "string-width@4.2.3"
			}
			return name + "@" + ref
		}
		if strings.HasPrefix(ref, "/") || strings.Contains(base, ":") || (strings.Contains(base, "/") && !startsDigit(base)) {
			return ref
		}
		return "/" + name + "@" + ref
	}
	var nodeFor func(name, ref, from string) string
	nodeFor = func(name, ref, from string) string {
		if rest, ok := strings.CutPrefix(ref, "link:"); ok {
			p := path.Clean(path.Join(from, rest))
			if _, ok := importerDir[p]; ok {
				return "pnpm-importer:" + p
			}
			id := "pnpm-link:" + p
			if _, ok := g.Nodes[id]; !ok {
				g.Nodes[id] = &Node{ID: id, Name: name, Ident: "link " + p, Local: true, Deps: map[string]string{}}
			}
			return id
		}
		key := snapKey(name, ref)
		id := "pnpm:" + key
		if _, ok := g.Nodes[id]; ok {
			return id
		}
		var snap, meta map[string]any
		if v9 {
			s, ok := snaps[key]
			if !ok {
				perr = fmt.Errorf("pnpm-lock.yaml: no snapshot for %s %s", name, ref)
				return id
			}
			snap, _ = s.(map[string]any)
			mk := key
			if i := strings.IndexByte(mk, '('); i >= 0 {
				mk = mk[:i]
			}
			m, ok := pkgs[mk]
			if !ok {
				perr = fmt.Errorf("pnpm-lock.yaml: no package entry for %s", mk)
				return id
			}
			meta, _ = m.(map[string]any)
		} else {
			m, ok := pkgs[key]
			if !ok {
				perr = fmt.Errorf("pnpm-lock.yaml: no package entry for %s %s", name, ref)
				return id
			}
			meta, _ = m.(map[string]any)
			snap = meta
		}
		n := &Node{ID: id, Name: name, Deps: map[string]string{}}
		g.Nodes[id] = n
		pk := strings.TrimPrefix(key, "/")
		if i := strings.IndexByte(pk, '('); i >= 0 {
			pk = pk[:i]
		}
		if i := strings.LastIndexByte(pk, '@'); i > 0 {
			n.Package, n.Version = pk[:i], pk[i+1:]
		} else {
			n.Package, n.Version = name, pk
		}
		idm := map[string]any{}
		for k, v := range meta {
			if !pnpmIdentSkip[k] {
				idm[k] = v
			}
		}
		sidm := map[string]any{}
		if v9 {
			for k, v := range snap {
				if !pnpmIdentSkip[k] {
					sidm[k] = v
				}
			}
		}
		n.Ident = "pnpm " + name + " " + key + " " + canon(idm) + " " + canon(sidm)
		if v9 {
			n.Scripts = true // 9.0 dropped requiresBuild: unknown
		} else {
			n.Scripts = meta["requiresBuild"] == true
		}
		if pd, ok := meta["peerDependencies"].(map[string]any); ok && len(pd) > 0 {
			n.Peer = true
		}
		if strings.Contains(key, "(") {
			n.Peer = true
		}
		if res, ok := meta["resolution"].(map[string]any); ok {
			if _, ok := res["integrity"]; !ok {
				n.Local = true // directory, git or tarball resolution
			}
		}
		for _, s := range []string{"dependencies", "optionalDependencies"} {
			m, _ := snap[s].(map[string]any)
			for dn, r := range m {
				rs, ok := r.(string)
				if !ok {
					perr = fmt.Errorf("pnpm-lock.yaml: bad reference %s → %s", key, dn)
					continue
				}
				n.Deps[dn] = nodeFor(dn, rs, ".")
			}
		}
		return id
	}

	keys := make([]string, 0, len(importers))
	for k := range importers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ck := path.Clean(k)
		id := "pnpm-importer:" + ck
		rel := importerDir[ck]
		n := &Node{ID: id, Name: rel, Ident: "project " + ck, Project: rel, Local: true, Deps: map[string]string{}}
		g.Nodes[id] = n
		g.Projects[rel] = id
	}
	for _, k := range keys {
		ck := path.Clean(k)
		n := g.Nodes["pnpm-importer:"+ck]
		imp, _ := importers[k].(map[string]any)
		for _, s := range []string{"dependencies", "devDependencies", "optionalDependencies"} {
			m, _ := imp[s].(map[string]any)
			for dn, v := range m {
				var ref string
				switch x := v.(type) {
				case map[string]any:
					ref, _ = x["version"].(string)
				case string:
					ref = x
				}
				if ref == "" {
					return nil, fmt.Errorf("pnpm-lock.yaml: importer %s: no version for %s", k, dn)
				}
				n.Deps[dn] = nodeFor(dn, ref, ck)
			}
		}
	}
	if perr != nil {
		return nil, perr
	}
	return g, nil
}

func startsDigit(s string) bool { return s != "" && s[0] >= '0' && s[0] <= '9' }

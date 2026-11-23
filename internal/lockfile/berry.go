package lockfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// berryVersions are the __metadata versions whose closures passed the differential check against yarn (yarn 2 to 4;
// version 7 was not found in any sampled repository).
var berryVersions = map[string]bool{"4": true, "5": true, "6": true, "8": true, "9": true, "10": true}

var reProtocol = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// ParseBerry reads a yarn Berry (2+) lockfile. Every entry is keyed by the descriptors (name@range) that resolve to
// it; the workspaces ("name@workspace:path" resolutions) are the projects, and every dependency, of a workspace or a
// package, resolves through the descriptor index:
//   - a range without a protocol is an npm range ("^1.0.0" → "npm:^1.0.0", as lockfiles before version 8 write it);
//   - a range bound to its parent (link:, portal:, file:, relative patch: paths) carries "::locator=<parent>";
//   - "catalog:" and "catalog:<name>" ranges are read from the catalogs of .yarnrc.yml next to the lockfile.
//
// A patch entry ("name@patch:<inner descriptor>#<patch>") and the entry it patches are in each other's closure, so a
// change of either is a change of both: the patched package is changed when its patch changes, and a dependency on the
// unpatched descriptor also reaches yarn's built-in compatibility patches (typescript, fsevents, resolve). Peer
// dependencies are not edges: yarn resolves them from the dependent, which is in the same closure. A dependency that
// resolves to no entry is an error (level L0).
func ParseBerry(dir string, data []byte, files Files) (*Graph, error) {
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("yarn.lock: %w", err)
	}
	meta, ok := top["__metadata"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("yarn.lock: no __metadata")
	}
	ver := fmt.Sprint(meta["version"])
	if !berryVersions[ver] {
		return nil, fmt.Errorf("yarn.lock: __metadata version %s not supported", ver)
	}
	g := &Graph{Format: FormatBerry, Version: ver, Nodes: map[string]*Node{}, Projects: map[string]string{},
		Extra: map[string]string{"__metadata": canon(meta)}}

	type entry struct {
		key  string
		resn string
		m    map[string]any
	}
	entries := map[string]*entry{} // lockfile key → entry
	byDesc := map[string]*entry{}  // descriptor → entry
	byResn := map[string]*entry{}  // resolution (locator) → entry
	for k, v := range top {
		if k == "__metadata" {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("yarn.lock: entry %q is not a map", k)
		}
		e := &entry{key: k, m: m}
		e.resn, _ = m["resolution"].(string)
		if e.resn == "" {
			return nil, fmt.Errorf("yarn.lock: entry %q has no resolution", k)
		}
		entries[k] = e
		if _, dup := byResn[e.resn]; dup {
			return nil, fmt.Errorf("yarn.lock: two entries resolve to %s", e.resn)
		}
		byResn[e.resn] = e
		for _, d := range splitBerryKey(k) {
			if _, dup := byDesc[d]; dup {
				return nil, fmt.Errorf("yarn.lock: descriptor %s in two entries", d)
			}
			byDesc[d] = e
		}
	}

	catalogs, userExts, err := berryRC(files, dir)
	if err != nil {
		return nil, fmt.Errorf("yarn.lock: %v", err)
	}
	exts := append(userExts, builtinExtensions()...)

	// Patch links: a patch entry's inner descriptor names the entry it patches.
	patchOf := map[*entry][]*entry{} // patched entry → its patch entries
	patchBase := map[*entry]*entry{}
	for _, e := range entries {
		for _, d := range splitBerryKey(e.key) {
			_, rng, ok := splitPattern(d)
			if !ok {
				continue
			}
			inner, ok := strings.CutPrefix(rng, "patch:")
			if !ok {
				continue
			}
			if i := strings.IndexByte(inner, '#'); i >= 0 {
				inner = inner[:i]
			}
			dec, err := url.PathUnescape(inner)
			if err != nil {
				return nil, fmt.Errorf("yarn.lock: patch descriptor %s: %v", d, err)
			}
			base := byDesc[dec]
			if base == nil {
				if n, r, ok := splitPattern(dec); ok && !reProtocol.MatchString(r) {
					base = byDesc[n+"@npm:"+r]
				}
			}
			if base == nil {
				if b2, ok := byResn[dec]; ok {
					base = b2
				}
			}
			if base == nil {
				return nil, fmt.Errorf("yarn.lock: patch %s: the patched descriptor %s has no entry", d, dec)
			}
			if patchBase[e] == nil {
				patchBase[e] = base
				patchOf[base] = append(patchOf[base], e)
			}
		}
	}

	// Projects.
	for _, e := range entries {
		_, rng, ok := splitPattern(e.resn)
		if !ok {
			continue
		}
		wp, ok := strings.CutPrefix(rng, "workspace:")
		if !ok {
			continue
		}
		rel, ok := join(dir, wp)
		if !ok {
			return nil, fmt.Errorf("yarn.lock: workspace %s outside the repo", e.resn)
		}
		id := "berry-project:" + rel
		name, _, _ := splitPattern(e.resn)
		if _, dup := g.Projects[rel]; dup {
			return nil, fmt.Errorf("yarn.lock: two workspaces at %s", rel)
		}
		g.Nodes[id] = &Node{ID: id, Name: name, Package: name, Version: fmt.Sprint(e.m["version"]), Ident: "project " + rel,
			Project: rel, Local: true, Deps: map[string]string{}}
		g.Projects[rel] = id
	}

	wsByName := map[string]*entry{} // workspace name → its entry (nil when two workspaces share it)
	wsDup := map[string]bool{}
	for _, e := range entries {
		if projectOf(g, e.resn, dir) == "" {
			continue
		}
		name, _, _ := splitPattern(e.resn)
		if _, dup := wsByName[name]; dup {
			wsDup[name] = true
		}
		wsByName[name] = e
	}
	for name := range wsDup {
		delete(wsByName, name)
	}

	ress, err := berryResolutions(files, dir)
	if err != nil {
		return nil, fmt.Errorf("yarn.lock: root manifest: %v", err)
	}
	rootLocator := ""
	if id, ok := g.Projects[dir]; ok {
		rootLocator = g.Nodes[id].Name + "@workspace:."
		for _, e := range entries {
			if projectOf(g, e.resn, dir) == id {
				rootLocator = e.resn
			}
		}
	}

	var perr error
	var nodeFor func(name string, e *entry) string
	resolve := func(parent *entry, name, rng string) (*entry, error) {
		bindTo := parent.resn
		rng = strings.TrimSpace(rng) // keys lose surrounding spaces ("condition:A ? x : ")
		if r, err := applyBerryResolution(ress, parent.resn, name, rng); err != nil {
			return nil, err
		} else if r != "" {
			rng, bindTo = r, rootLocator // a resolution's own relative path is relative to the root workspace
		}
		if c, ok := strings.CutPrefix(rng, "catalog:"); ok {
			cat := catalogs[c]
			r, ok := cat[name]
			if !ok {
				return nil, fmt.Errorf("catalog range %s of %s: not in .yarnrc.yml", rng, name)
			}
			rng = r
		}
		cands := []string{name + "@" + rng}
		if !reProtocol.MatchString(rng) {
			cands = append(cands, name+"@npm:"+rng)
		}
		loc := "::locator=" + encodeURIComponent(bindTo)
		for _, c := range append([]string(nil), cands...) {
			cands = append(cands, c+loc)
		}
		var found *entry
		for _, c := range cands {
			if e, ok := byDesc[c]; ok {
				if found != nil && found != e {
					return nil, fmt.Errorf("%s@%s matches two entries", name, rng)
				}
				found = e
			}
		}
		if found == nil && strings.HasPrefix(rng, "workspace:") {
			// yarn resolves a workspace: range to the workspace of that name whatever the lockfile says (a range
			// added since the last install is not a key yet).
			found = wsByName[name]
		}
		if found == nil {
			return nil, fmt.Errorf("%s: no entry for %s@%s", parent.resn, name, rng)
		}
		return found, nil
	}
	addDeps := func(n *Node, e *entry) {
		m, _ := e.m["dependencies"].(map[string]any)
		if e.m["dependencies"] != nil && m == nil {
			perr = fmt.Errorf("yarn.lock: dependencies of %s is not a map", e.resn)
			return
		}
		for dn, r := range m {
			var rs string
			switch x := r.(type) {
			case string:
				rs = x
			case int: // an unquoted major ("debug: 4"); a float ("1.10") would have lost digits: not accepted
				rs = strconv.Itoa(x)
			default:
				perr = fmt.Errorf("yarn.lock: bad range %s → %s", e.resn, dn)
				return
			}
			t, err := resolve(e, dn, rs)
			if err != nil {
				perr = fmt.Errorf("yarn.lock: %v", err)
				return
			}
			n.Deps[dn] = nodeFor(dn, t)
		}
		// Dependencies yarn adds at resolution time (package extensions, workspaces included): the lockfile keeps the
		// package's own manifest. A workspace's version is not in the lockfile: a selector naming it applies.
		for _, x := range exts {
			if _, dup := n.Deps[x.dep]; dup {
				continue
			}
			if n.Project != "" && x.name != n.Package || n.Project == "" && !x.matches(n.Package, n.Version) {
				continue
			}
			t, err := resolve(e, x.dep, x.rng)
			if err != nil && wsByName[x.dep] != nil {
				// a range a workspace of that name satisfies resolves to it (transparent workspaces); the
				// workspace's version is not recorded, so take it
				t, err = wsByName[x.dep], nil
			}
			if err != nil {
				if x.builtin {
					continue // an extension of another yarn release, not applied here
				}
				perr = fmt.Errorf("yarn.lock: package extension %s: %v", x.sel, err)
				return
			}
			n.Deps[x.dep] = nodeFor(x.dep, t)
		}
		for i, p := range patchOf[e] {
			n.Deps[fmt.Sprintf("<patched %d>", i)] = nodeFor(n.Name, p)
		}
		if b := patchBase[e]; b != nil {
			n.Deps["<patch base>"] = nodeFor(n.Name, b)
		}
	}
	nodeFor = func(name string, e *entry) string {
		if pid := projectOf(g, e.resn, dir); pid != "" {
			return pid
		}
		id := "berry:" + name + "\x00" + e.resn
		if _, ok := g.Nodes[id]; ok {
			return id
		}
		n := &Node{ID: id, Name: name, Deps: map[string]string{}, Scripts: true}
		g.Nodes[id] = n
		n.Package, _, _ = splitPattern(e.resn)
		n.Version = fmt.Sprint(e.m["version"])
		idm := map[string]any{}
		for k, v := range e.m {
			if k != "dependencies" {
				idm[k] = v
			}
		}
		n.Ident = "berry " + name + " " + canon(idm)
		if pd, ok := e.m["peerDependencies"].(map[string]any); ok && len(pd) > 0 {
			n.Peer = true
		}
		switch b := e.m["bin"].(type) {
		case map[string]any:
			for k := range b {
				n.Bins = append(n.Bins, k)
			}
			sort.Strings(n.Bins)
		}
		_, rng, _ := splitPattern(e.resn)
		if !strings.HasPrefix(rng, "npm:") && !(strings.HasPrefix(rng, "patch:") && strings.Contains(rng, "npm%3A")) {
			n.Local = true // link:, portal:, file:, git, tarball, exec:
		}
		addDeps(n, e)
		return id
	}
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := entries[k]
		if pid := projectOf(g, e.resn, dir); pid != "" {
			addDeps(g.Nodes[pid], e)
		}
	}
	if perr != nil {
		return nil, perr
	}
	return g, nil
}

// projectOf returns the project node ID of a workspace resolution, or "".
func projectOf(g *Graph, resn, dir string) string {
	_, rng, ok := splitPattern(resn)
	if !ok {
		return ""
	}
	wp, ok := strings.CutPrefix(rng, "workspace:")
	if !ok {
		return ""
	}
	rel, _ := join(dir, wp)
	return g.Projects[rel]
}

// encodeURIComponent escapes like JavaScript's encodeURIComponent (yarn binds a descriptor to its parent locator with
// it).
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// berryResolution is one entry of the root manifest's "resolutions": [from/]name[@range] → range.
type berryResolution struct {
	from, name, rng, to string
}

var (
	reBerryResDesc = regexp.MustCompile(`^((?:@[^/@]+/)?[^/@]+)(?:@(.*))?$`)
	reBerryResFrom = regexp.MustCompile(`^((?:@[^/@]+/)?[^/@]+(?:@[^/]*)?)/((?:@[^/@]+/)?[^/@]+(?:@.*)?)$`)
)

// berryResolutions reads the "resolutions" of the root manifest in dir. yarn applies them when it resolves a
// descriptor, and the lockfile keeps the original descriptor in the dependent's entry: the overridden descriptor is
// the one to look up.
func berryResolutions(files Files, dir string) ([]berryResolution, error) {
	if files == nil {
		return nil, fmt.Errorf("root manifest not available")
	}
	b, err := files.Read(path.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	var m struct {
		Resolutions map[string]string `json:"resolutions"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("resolutions: %v", err)
	}
	var out []berryResolution
	for k, v := range m.Resolutions {
		r := berryResolution{to: v}
		desc := k
		if mm := reBerryResFrom.FindStringSubmatch(k); mm != nil {
			r.from, desc = mm[1], mm[2]
		}
		dm := reBerryResDesc.FindStringSubmatch(desc)
		if dm == nil {
			return nil, fmt.Errorf("resolution %q not supported", k)
		}
		r.name, r.rng = dm[1], dm[2]
		out = append(out, r)
	}
	return out, nil
}

// npmRange adds the default npm: protocol to a range without one.
func npmRange(r string) string {
	if reProtocol.MatchString(r) {
		return r
	}
	return "npm:" + r
}

// applyBerryResolution returns the range a resolution substitutes for dependency name@rng of the package parent
// (a locator), or "" when none applies. The most specific resolution wins (a parent, then a range).
func applyBerryResolution(ress []berryResolution, parent, name, rng string) (string, error) {
	pname, _, _ := splitPattern(parent)
	best, bestScore := "", -1
	for _, r := range ress {
		if r.name != name {
			continue
		}
		score := 0
		if r.rng != "" {
			if npmRange(r.rng) != npmRange(rng) {
				continue
			}
			score++
		}
		if r.from != "" {
			fn, fr, _ := splitPattern(r.from)
			if fr != "" {
				return "", fmt.Errorf("resolution %s/%s with a parent range is not supported", r.from, name)
			}
			if fn != pname {
				continue
			}
			score += 2
		}
		if score == bestScore && best != r.to {
			return "", fmt.Errorf("two resolutions apply to %s@%s", name, rng)
		}
		if score > bestScore {
			best, bestScore = r.to, score
		}
	}
	return best, nil
}

// splitBerryKey splits a lockfile key into its descriptors.
func splitBerryKey(k string) []string {
	parts := strings.Split(k, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// berryExtension adds dependency dep@rng to the packages its selector (name@range) matches.
type berryExtension struct {
	sel, name, selRange, dep, rng string
	builtin                       bool
}

// matches reports whether the extension applies to package name@version; a selector range this code cannot read
// applies (an extra edge only widens the closure).
func (x berryExtension) matches(name, version string) bool {
	if x.name != name {
		return false
	}
	if x.selRange == "" || x.selRange == "*" {
		return true
	}
	ok, err := semverSatisfies(version, x.selRange)
	return ok || err != nil
}

func newBerryExtension(sel, dep, rng string, builtin bool) (berryExtension, error) {
	name, r, ok := splitPattern(sel)
	if !ok {
		name, r = sel, ""
	}
	r = strings.TrimPrefix(r, "npm:")
	if name == "" || reProtocol.MatchString(r) {
		return berryExtension{}, fmt.Errorf("package extension selector %q not supported", sel)
	}
	return berryExtension{sel: sel, name: name, selRange: r, dep: dep, rng: rng, builtin: builtin}, nil
}

func builtinExtensions() []berryExtension {
	out := make([]berryExtension, 0, len(berryBuiltinExtensions))
	for _, x := range berryBuiltinExtensions {
		if e, err := newBerryExtension(x[0], x[1], x[2], true); err == nil {
			out = append(out, e)
		}
	}
	return out
}

// berryRC reads, from the .yarnrc.yml in dir, the catalogs ("" is the default catalog, "catalog:"; other keys are named
// catalogs, "catalog:<name>") and the dependencies added by packageExtensions. A missing .yarnrc.yml has neither; an
// unavailable tree is an error.
func berryRC(files Files, dir string) (map[string]map[string]string, []berryExtension, error) {
	if files == nil {
		return nil, nil, fmt.Errorf(".yarnrc.yml not available")
	}
	b, err := files.Read(path.Join(dir, ".yarnrc.yml"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]map[string]string{}, nil, nil
		}
		return nil, nil, fmt.Errorf(".yarnrc.yml: %v", err)
	}
	var rc struct {
		Catalog           map[string]string            `yaml:"catalog"`
		Catalogs          map[string]map[string]string `yaml:"catalogs"`
		PackageExtensions map[string]struct {
			Dependencies map[string]string `yaml:"dependencies"`
		} `yaml:"packageExtensions"`
	}
	if err := yaml.Unmarshal(b, &rc); err != nil {
		return nil, nil, fmt.Errorf(".yarnrc.yml: %v", err)
	}
	out := map[string]map[string]string{"": rc.Catalog}
	for k, v := range rc.Catalogs {
		out[k] = v
	}
	var exts []berryExtension
	sels := make([]string, 0, len(rc.PackageExtensions))
	for s := range rc.PackageExtensions {
		sels = append(sels, s)
	}
	sort.Strings(sels)
	for _, s := range sels {
		deps := rc.PackageExtensions[s].Dependencies
		names := make([]string, 0, len(deps))
		for d := range deps {
			names = append(names, d)
		}
		sort.Strings(names)
		for _, d := range names {
			e, err := newBerryExtension(s, d, deps[d], false)
			if err != nil {
				return nil, nil, fmt.Errorf(".yarnrc.yml: %v", err)
			}
			exts = append(exts, e)
		}
	}
	return out, exts, nil
}

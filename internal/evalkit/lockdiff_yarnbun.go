package evalkit

// Lockfile differential check for yarn.lock (classic and Berry) and bun.lock. The reference is the package manager's own
// resolution, read without bgraph's parser:
//   - yarn classic: `yarn list --json` (yarn's resolver and hoister, from the lockfile, without installing) gives the
//     node_modules layout yarn would produce, with every package's dependency names; each workspace's packages are
//     found by Node's lookup in that layout, from the workspace's own dependency names (its entry's children; the
//     root's from its manifest).
//   - yarn Berry: `yarn info -AR --json` (from the lockfile, without installing) lists every package with its resolved
//     dependencies (descriptor → locator); a workspace's packages are everything reachable from its locator.
//   - bun: `bun install --frozen-lockfile --ignore-scripts` installs the tree; each workspace's packages are found by
//     Node's lookup on disk, from its manifest's dependencies, recursively through the installed manifests.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/lockfile"
	"gopkg.in/yaml.v3"
)

func lockdiffTreeFormat(format string) bool {
	return format == lockfile.FormatYarn || format == lockfile.FormatBun
}

// keepYarnBun keeps, besides the manifests and the lockfile, what yarn and bun read to resolve and install: yarn
// settings (.yarnrc.yml: catalogs, plugins, yarnPath) with the pinned release and plugins, bun settings, and patches.
func keepYarnBun(rel string) bool {
	switch {
	case rel == ".yarnrc.yml", rel == "bunfig.toml":
		return true
	case strings.HasPrefix(rel, ".yarn/releases/"), strings.HasPrefix(rel, ".yarn/plugins/"), strings.HasPrefix(rel, ".yarn/patches/"):
		return true
	case strings.HasSuffix(rel, ".patch"):
		return true
	}
	return false
}

// matFiles reads a materialized commit (files relative to the lockfile directory) by repo-relative path.
type matFiles struct{ root, dir string }

func (m matFiles) rel(p string) (string, bool) {
	if m.dir == "." {
		return p, true
	}
	return strings.CutPrefix(p, m.dir+"/")
}

func (m matFiles) Read(p string) ([]byte, error) {
	r, ok := m.rel(p)
	if !ok {
		return nil, fs.ErrNotExist
	}
	return os.ReadFile(filepath.Join(m.root, filepath.FromSlash(r)))
}

func (m matFiles) Files() []string {
	var out []string
	filepath.WalkDir(m.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			r, _ := filepath.Rel(m.root, p)
			out = append(out, path.Join(m.dir, filepath.ToSlash(r)))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// lockdiffYarn returns each project's package set (name@version) by yarn's own resolution.
func lockdiffYarn(ctx context.Context, o LockdiffOpts, g *lockfile.Graph, dst, lockDir string) (map[string]map[string]bool, bool, error) {
	if g.Format == lockfile.FormatBerry {
		return lockdiffBerry(ctx, o, dst, lockDir)
	}
	return lockdiffYarn1(ctx, o, dst, lockDir)
}

type yarnListNode struct {
	Name     string          `json:"name"`
	Children []*yarnListNode `json:"children"`
	Shadow   bool            `json:"shadow"`
}

// vpkg is a package of yarn classic's virtual node_modules layout.
type vpkg struct {
	name, version string
	deps          []string
}

func lockdiffYarn1(ctx context.Context, o LockdiffOpts, dst, lockDir string) (map[string]map[string]bool, bool, error) {
	args := append(append([]string(nil), o.Yarn1[1:]...), "list", "--json", "--ignore-engines", "--non-interactive")
	out, errb, err := runTool(ctx, dst, o.Env, o.Yarn1[0], args...)
	var tree []*yarnListNode
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	for sc.Scan() {
		var msg struct {
			Type string `json:"type"`
			Data struct {
				Type  string          `json:"type"`
				Trees []*yarnListNode `json:"trees"`
			} `json:"data"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) == nil && msg.Type == "tree" && msg.Data.Type == "list" {
			tree = msg.Data.Trees
		}
	}
	if tree == nil {
		msg := firstLine(strings.Join(strings.Fields(string(errb)+" "+string(out)), " "))
		return nil, strings.Contains(msg, "lockfile"), fmt.Errorf("yarn list: %v: %s", err, msg)
	}
	layout := map[string]*vpkg{} // "node_modules/a/node_modules/b" → package
	var add func(prefix string, n *yarnListNode) error
	add = func(prefix string, n *yarnListNode) error {
		name, ver, ok := splitNameVersion(n.Name)
		if !ok {
			return fmt.Errorf("yarn list: bad node %q", n.Name)
		}
		p := prefix + "node_modules/" + name
		v := &vpkg{name: name, version: ver}
		layout[p] = v
		for _, c := range n.Children {
			if c.Shadow {
				dn, _, ok := splitNameVersion(c.Name)
				if !ok {
					return fmt.Errorf("yarn list: bad dependency %q", c.Name)
				}
				v.deps = append(v.deps, dn)
				continue
			}
			if err := add(p+"/", c); err != nil {
				return err
			}
		}
		return nil
	}
	for _, n := range tree {
		if err := add("", n); err != nil {
			return nil, false, err
		}
	}
	// Workspaces by name (their entry in the layout is node_modules/<name>).
	manifests := map[string]string{} // name → dir relative to dst
	rootDeps, err := manifestDeps(filepath.Join(dst, "package.json"))
	if err != nil {
		return nil, false, err
	}
	pats := map[string]bool{}
	filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "package.json" {
			return nil
		}
		rel, _ := filepath.Rel(dst, filepath.Dir(p))
		if rel == "." {
			return nil
		}
		b, _ := os.ReadFile(p)
		var pj struct{ Name, Version string }
		if json.Unmarshal(b, &pj) == nil && pj.Name != "" {
			if v, ok := layout["node_modules/"+pj.Name]; ok && v.version == pj.Version {
				if _, dup := manifests[pj.Name]; dup {
					pats[pj.Name] = true
				}
				manifests[pj.Name] = filepath.ToSlash(rel)
			}
		}
		return nil
	})
	isWorkspace := func(p string) bool {
		v := layout[p]
		return v != nil && p == "node_modules/"+v.name && manifests[v.name] != "" && !pats[v.name]
	}
	resolve := func(from, name string) string {
		for d := from; ; {
			c := "node_modules/" + name
			if d != "" {
				c = d + "/node_modules/" + name
			}
			if _, ok := layout[c]; ok {
				return c
			}
			if d == "" {
				return ""
			}
			i := strings.LastIndex(d, "/node_modules/")
			if i < 0 {
				d = ""
			} else {
				d = d[:i]
			}
		}
	}
	closure := func(from string, deps []string) map[string]bool {
		set := map[string]bool{}
		seen := map[string]bool{}
		type item struct{ from, name string }
		queue := []item{}
		for _, d := range deps {
			queue = append(queue, item{from, d})
		}
		for len(queue) > 0 {
			it := queue[0]
			queue = queue[1:]
			p := resolve(it.from, it.name)
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			v := layout[p]
			if !isWorkspace(p) {
				set[v.name+"@"+v.version] = true
			}
			for _, d := range v.deps {
				queue = append(queue, item{p, d})
			}
		}
		return set
	}
	res := map[string]map[string]bool{lockDir: closure("", rootDeps)}
	for name, dir := range manifests {
		if pats[name] {
			continue
		}
		p := "node_modules/" + name
		res[path.Join(lockDir, dir)] = closure(p, layout[p].deps)
	}
	return res, false, nil
}

// splitNameVersion splits "name@version" or "name@range" (a scoped name starts with @).
func splitNameVersion(s string) (string, string, bool) {
	i := strings.LastIndexByte(s, '@')
	if i <= 0 {
		return "", "", false
	}
	// "name@npm:other@^1" (alias range): the name ends at the first @ after the scope.
	if j := strings.IndexByte(s[1:], '@') + 1; j > 0 && j < i {
		i = j
	}
	return s[:i], s[i+1:], true
}

// manifestDeps returns the dependency names of a package.json (dependencies, devDependencies, optionalDependencies).
func manifestDeps(p string) ([]string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %v", p, err)
	}
	var out []string
	for _, s := range []string{"dependencies", "devDependencies", "optionalDependencies"} {
		var d map[string]any
		if json.Unmarshal(m[s], &d) == nil {
			for k := range d {
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

var reVirtual = regexp.MustCompile(`@virtual:[0-9a-f]+#`)

func lockdiffBerry(ctx context.Context, o LockdiffOpts, dst, lockDir string) (map[string]map[string]bool, bool, error) {
	cmd := o.YarnBerry
	if b, err := os.ReadFile(filepath.Join(dst, ".yarnrc.yml")); err == nil {
		var rc struct {
			YarnPath string `yaml:"yarnPath"`
		}
		if yaml.Unmarshal(b, &rc) == nil && rc.YarnPath != "" {
			rp := filepath.Join(dst, filepath.FromSlash(rc.YarnPath))
			if _, err := os.Stat(rp); err == nil {
				cmd = []string{"node", rp}
			}
		}
	}
	args := append(append([]string(nil), cmd[1:]...), "info", "-AR", "--json")
	out, errb, err := runTool(ctx, dst, o.Env, cmd[0], args...)
	type rec struct {
		Value    string `json:"value"`
		Children struct {
			Version      string `json:"Version"`
			Dependencies []struct {
				Locator string `json:"locator"`
			} `json:"Dependencies"`
		} `json:"children"`
	}
	recs := map[string]*rec{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	for sc.Scan() {
		var r rec
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Value != "" {
			recs[r.Value] = &r
		}
	}
	if err != nil || len(recs) == 0 {
		msg := firstLine(strings.Join(strings.Fields(string(out)+" "+string(errb)), " "))
		return nil, strings.Contains(msg, "lockfile"), fmt.Errorf("yarn info: %v: %s", err, msg)
	}
	devirt := func(l string) string { return reVirtual.ReplaceAllString(l, "@") }
	isWorkspace := func(l string) (string, bool) {
		_, r, ok := splitNameVersion(l)
		if !ok {
			return "", false
		}
		w, ok := strings.CutPrefix(r, "workspace:")
		return w, ok
	}
	res := map[string]map[string]bool{}
	for l := range recs {
		w, ok := isWorkspace(l)
		if !ok {
			continue
		}
		set := map[string]bool{}
		seen := map[string]bool{l: true}
		queue := []string{l}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			r := recs[cur]
			if r == nil {
				continue
			}
			if _, ws := isWorkspace(cur); !ws {
				name, _, _ := splitNameVersion(cur)
				set[name+"@"+r.Children.Version] = true
			}
			for _, d := range r.Children.Dependencies {
				t := devirt(d.Locator)
				if !seen[t] {
					seen[t] = true
					queue = append(queue, t)
				}
			}
		}
		res[path.Join(lockDir, w)] = set
	}
	return res, false, nil
}

// lockdiffBun installs the materialized commit with bun and reads each project's packages from node_modules.
// Every commit is installed afresh (bun does not remove packages an earlier install left, which would shadow the
// commit's own). o.BunLinker forces a linker: an isolated install (bun's package store, one directory per resolved
// package with its resolved dependencies as siblings) takes seconds where a hoisted one takes minutes on Windows, and
// both follow the resolution bun reads from the lockfile.
func lockdiffBun(ctx context.Context, o LockdiffOpts, g *lockfile.Graph, dst, lockDir string) (map[string]map[string]bool, bool, error) {
	args := append(append([]string(nil), o.Bun[1:]...), "install", "--frozen-lockfile", "--ignore-scripts", "--no-progress")
	if o.BunLinker != "" {
		args = append(args, "--linker", o.BunLinker)
	}
	out, errb, err := runTool(ctx, dst, o.Env, o.Bun[0], args...)
	if err != nil {
		msg := firstLine(strings.Join(strings.Fields(string(errb)+" "+string(out)), " "))
		return nil, strings.Contains(msg, "lockfile"), fmt.Errorf("bun install: %v: %s", err, msg)
	}
	root, err := realPath(dst)
	if err != nil {
		return nil, false, err
	}
	res := map[string]map[string]bool{}
	for p := range g.Projects {
		rel := p
		if lockDir != "." {
			rel = strings.TrimPrefix(strings.TrimPrefix(p, lockDir), "/")
		}
		if rel == "" {
			rel = "."
		}
		dir := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
			continue
		}
		set, err := nodeClosure(root, dir)
		if err != nil {
			return nil, false, err
		}
		res[p] = set
	}
	return res, false, nil
}

// realPath resolves every symbolic link and Windows junction in an absolute path (filepath.EvalSymlinks does not
// follow junctions, which bun and npm create on Windows).
func realPath(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	for range 64 {
		vol := filepath.VolumeName(p)
		parts := strings.Split(strings.TrimPrefix(p[len(vol):], string(filepath.Separator)), string(filepath.Separator))
		cur := vol + string(filepath.Separator)
		changed := false
		for i, part := range parts {
			if part == "" {
				continue
			}
			next := filepath.Join(cur, part)
			fi, err := os.Lstat(next)
			if err != nil {
				return "", err
			}
			if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
				t, err := os.Readlink(next)
				if err != nil {
					return "", err
				}
				if !filepath.IsAbs(t) {
					t = filepath.Join(cur, t)
				}
				p = filepath.Join(append([]string{t}, parts[i+1:]...)...)
				changed = true
				break
			}
			cur = next
		}
		if !changed {
			return filepath.Clean(cur), nil
		}
	}
	return "", fmt.Errorf("%s: too many links", p)
}

type installedPkg struct {
	Name                 string         `json:"name"`
	Version              string         `json:"version"`
	Dependencies         map[string]any `json:"dependencies"`
	DevDependencies      map[string]any `json:"devDependencies"`
	OptionalDependencies map[string]any `json:"optionalDependencies"`
	PeerDependencies     map[string]any `json:"peerDependencies"`
	PeerDependenciesMeta map[string]struct {
		Optional bool `json:"optional"`
	} `json:"peerDependenciesMeta"`
}

// nodeClosure returns the name@version of every installed package reachable from the project in dir by Node's module
// lookup (a package's node_modules, then every ancestor's, up to root), following real paths. A workspace package
// (a real path outside node_modules) is traversed but not listed.
func nodeClosure(root, dir string) (map[string]bool, error) {
	read := func(d string) (*installedPkg, error) {
		b, err := os.ReadFile(filepath.Join(d, "package.json"))
		if err != nil {
			return nil, err
		}
		var p installedPkg
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		return &p, nil
	}
	lookup := func(from, name string) string {
		for d := from; ; {
			if filepath.Base(d) != "node_modules" {
				c := filepath.Join(d, "node_modules", filepath.FromSlash(name))
				if _, err := os.Stat(filepath.Join(c, "package.json")); err == nil {
					if r, err := realPath(c); err == nil {
						return r
					}
				}
			}
			if d == root || len(d) <= len(root) {
				return ""
			}
			d = filepath.Dir(d)
		}
	}
	inModules := func(d string) bool {
		return strings.Contains(filepath.ToSlash(d)+"/", "/node_modules/")
	}
	set := map[string]bool{}
	me, err := read(dir)
	if err != nil {
		return nil, err
	}
	type item struct{ from, name string }
	var queue []item
	for _, m := range []map[string]any{me.Dependencies, me.DevDependencies, me.OptionalDependencies, me.PeerDependencies} {
		for n := range m {
			queue = append(queue, item{dir, n})
		}
	}
	seen := map[string]bool{dir: true}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		r := lookup(it.from, it.name)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		p, err := read(r)
		if err != nil {
			continue
		}
		if inModules(r) {
			// a manifest version may carry a "v" or "=" prefix that the registry (and the lockfile) drop
			set[p.Name+"@"+strings.TrimLeft(p.Version, "v=")] = true
		}
		for _, m := range []map[string]any{p.Dependencies, p.OptionalDependencies, p.PeerDependencies} {
			for n := range m {
				// An optional peer is not installed for the package: whatever a lookup finds there is someone
				// else's (or left over from an earlier commit of the reused install directory).
				if p.PeerDependenciesMeta[n].Optional {
					if _, dep := p.Dependencies[n]; !dep {
						continue
					}
				}
				queue = append(queue, item{r, n})
			}
		}
	}
	return set, nil
}

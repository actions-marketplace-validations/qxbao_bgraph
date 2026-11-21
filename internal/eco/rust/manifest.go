package rust

import (
	"path"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/tree"
)

// manifest is one Cargo.toml.
type manifest struct {
	path string
	dir  string
	doc  map[string]any
	err  error
}

// wsInfo is the [workspace] part of a manifest.
type wsInfo struct {
	root    string
	exclude []*glob.Pattern
	deps    map[string]any // [workspace.dependencies]
}

// pkg is a Cargo package: a manifest with a [package] table.
type pkg struct {
	dir     string
	name    string
	m       *manifest
	ws      string   // directory of the workspace root the package belongs to; "" when standalone
	deps    []string // directories of the packages it depends on (path, workspace-inherited or patched dependencies)
	targets []*target
}

// target is a Cargo target; unit=false targets are graph nodes only (cargo test does not run them).
type target struct {
	kind string
	name string
	root string // repo path of the root file; "" for the doc unit
	node string
	unit bool
}

type workspaces struct {
	t         tree.Tree
	manifests []*manifest
	roots     map[string]*wsInfo
	pkgs      map[string]*pkg
	patches   map[string]string // real package name → directory (from [patch] / [replace] path entries)
	mdirs     map[string]bool   // directories holding a Cargo.toml
}

// skip: paths that are never part of the repository's Rust projects (vendored node_modules, git internals, cargo's target/ output).
func (w *workspaces) skip(p string) bool {
	for d := path.Dir(p); d != "." && d != "/" && d != ""; d = path.Dir(d) {
		switch path.Base(d) {
		case "node_modules", ".git":
			return true
		case "target":
			if w.mdirs[path.Dir(d)] {
				return true
			}
		}
	}
	return false
}

func loadWorkspace(t tree.Tree) (*workspaces, error) {
	w := &workspaces{t: t, roots: map[string]*wsInfo{}, pkgs: map[string]*pkg{}, patches: map[string]string{}, mdirs: map[string]bool{}}
	var cands []string
	for _, p := range t.Files() {
		if path.Base(p) == "Cargo.toml" {
			cands = append(cands, p)
			w.mdirs[path.Dir(p)] = true
		}
	}
	for _, p := range cands {
		if w.skip(p) {
			continue
		}
		m := &manifest{path: p, dir: path.Dir(p)}
		b, err := t.Read(p)
		if err != nil {
			m.err = err
		} else if err := toml.Unmarshal(b, &m.doc); err != nil {
			m.err = err
		}
		w.manifests = append(w.manifests, m)
	}
	sort.Slice(w.manifests, func(i, j int) bool { return w.manifests[i].path < w.manifests[j].path })
	for _, m := range w.manifests {
		if m.err != nil {
			continue
		}
		if wt := tbl(m.doc["workspace"]); wt != nil {
			info := &wsInfo{root: m.dir, deps: tbl(wt["dependencies"])}
			for _, e := range strs(wt["exclude"]) {
				if pat, err := glob.Compile(path.Join(m.dir, e)); err == nil {
					info.exclude = append(info.exclude, pat)
				}
			}
			w.roots[m.dir] = info
		}
		w.collectPatches(m)
		pt := tbl(m.doc["package"])
		if pt == nil {
			pt = tbl(m.doc["project"]) // legacy spelling
		}
		if pt == nil {
			continue
		}
		name := str(pt["name"])
		if name == "" {
			name = path.Base(m.dir)
			if m.dir == "." {
				name = "root"
			}
		}
		w.pkgs[m.dir] = &pkg{dir: m.dir, name: name, m: m}
	}
	for _, p := range w.pkgs {
		p.ws = w.rootOf(p)
	}
	for _, p := range w.pkgs {
		p.deps = w.depDirs(p)
	}
	return w, nil
}

// collectPatches records the path entries of [patch.*] and [replace]: a registry dependency with that name resolves to the directory.
func (w *workspaces) collectPatches(m *manifest) {
	add := func(name string, v any) {
		t := tbl(v)
		if t == nil || str(t["path"]) == "" {
			return
		}
		real := str(t["package"])
		if real == "" {
			real = name
		}
		real, _, _ = strings.Cut(real, ":") // [replace] keys are "name:version"
		w.patches[real] = path.Join(m.dir, strings.ReplaceAll(str(t["path"]), "\\", "/"))
	}
	for _, src := range tbl(m.doc["patch"]) {
		for name, v := range tbl(src) {
			add(name, v)
		}
	}
	for name, v := range tbl(m.doc["replace"]) {
		add(name, v)
	}
}

// rootOf finds the workspace root of a package: its own [workspace], an explicit package.workspace, or the nearest ancestor
// workspace that does not exclude it.
func (w *workspaces) rootOf(p *pkg) string {
	if _, ok := w.roots[p.dir]; ok {
		return p.dir
	}
	if pt := tbl(p.m.doc["package"]); pt != nil {
		if ref := str(pt["workspace"]); ref != "" {
			if d := path.Join(p.dir, strings.ReplaceAll(ref, "\\", "/")); w.roots[d] != nil {
				return d
			}
		}
	}
	for d := path.Dir(p.dir); ; d = path.Dir(d) {
		if info, ok := w.roots[d]; ok {
			for _, ex := range info.exclude {
				if ex.Match(p.dir) {
					return ""
				}
			}
			return d
		}
		if d == "." || d == "/" || d == "" {
			break
		}
	}
	return ""
}

// depDirs resolves the package's dependencies on other packages of the repository.
func (w *workspaces) depDirs(p *pkg) []string {
	var tables []map[string]any
	collect := func(doc map[string]any) {
		for _, k := range []string{"dependencies", "dev-dependencies", "build-dependencies", "dev_dependencies", "build_dependencies"} {
			if t := tbl(doc[k]); t != nil {
				tables = append(tables, t)
			}
		}
	}
	collect(p.m.doc)
	for _, cfg := range tbl(p.m.doc["target"]) {
		if ct := tbl(cfg); ct != nil {
			collect(ct)
		}
	}
	var wsDeps map[string]any
	if info := w.roots[p.ws]; info != nil && p.ws != "" {
		wsDeps = info.deps
	}
	set := map[string]bool{}
	for _, t := range tables {
		for key, val := range t {
			base, rel, real := p.dir, "", key
			if v := tbl(val); v != nil {
				rel = str(v["path"])
				if s := str(v["package"]); s != "" {
					real = s
				}
				if inh, _ := v["workspace"].(bool); inh && wsDeps != nil {
					if wd := tbl(wsDeps[key]); wd != nil {
						if rel == "" {
							rel, base = str(wd["path"]), p.ws
						}
						if s := str(wd["package"]); s != "" && str(v["package"]) == "" {
							real = s
						}
					}
				}
			}
			dir := ""
			if rel != "" {
				dir = path.Join(base, strings.ReplaceAll(rel, "\\", "/"))
			} else if d, ok := w.patches[real]; ok {
				dir = d
			}
			if dir != "" && dir != p.dir && w.pkgs[dir] != nil {
				set[dir] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// discoverTargets lists the Cargo targets of the package the way Cargo does. files are the files of the package (repo paths).
func discoverTargets(t tree.Tree, p *pkg, files []string) []*target {
	doc := p.m.doc
	pt := tbl(doc["package"])
	if pt == nil {
		pt = tbl(doc["project"])
	}
	join := func(rel string) string { return path.Join(p.dir, rel) }
	auto := func(key string) bool {
		if v, ok := pt[key].(bool); ok {
			return v
		}
		return true
	}
	var out []*target
	add := func(kind, name, root string, unit bool) *target {
		tg := &target{kind: kind, name: name, root: root, unit: unit, node: Node(UnitID(p.dir, kind, name))}
		out = append(out, tg)
		return tg
	}
	testOK := func(t map[string]any) bool {
		v, ok := t["test"].(bool)
		return !ok || v
	}

	// Library.
	libTbl := tbl(doc["lib"])
	libRoot := join("src/lib.rs")
	if libTbl != nil {
		if s := str(libTbl["path"]); s != "" {
			libRoot = join(strings.ReplaceAll(s, "\\", "/"))
		}
	}
	if libTbl != nil || t.Has(libRoot) {
		add(KindLib, "", libRoot, libTbl == nil || testOK(libTbl))
		if dt, ok := libTbl["doctest"].(bool); !ok || dt {
			add(KindDoc, "", "", true)
		}
	}

	// Binaries, tests, benches, examples.
	type kindSpec struct {
		kind, key, dir, autoKey string
	}
	for _, ks := range []kindSpec{
		{KindBin, "bin", "src/bin", "autobins"},
		{KindTest, "test", "tests", "autotests"},
		{KindBench, "bench", "benches", "autobenches"},
		{KindExample, "example", "examples", "autoexamples"},
	} {
		taken := map[string]bool{}
		takenRoot := map[string]bool{}
		unit := func(tg map[string]any) bool {
			if ks.kind == KindBin || ks.kind == KindTest {
				return testOK(tg)
			}
			return true
		}
		arr, _ := doc[ks.key].([]any)
		for _, e := range arr {
			et := tbl(e)
			if et == nil {
				continue
			}
			name := str(et["name"])
			root := ""
			if s := str(et["path"]); s != "" {
				root = join(strings.ReplaceAll(s, "\\", "/"))
			}
			if name == "" && root != "" {
				name = strings.TrimSuffix(path.Base(root), ".rs")
				if name == "main" {
					name = path.Base(path.Dir(root))
				}
			}
			if name == "" {
				continue
			}
			if root == "" {
				root = inferRoot(t, p, ks.kind, ks.dir, name)
			}
			taken[name], takenRoot[root] = true, true
			add(ks.kind, name, root, unit(et))
		}
		if !auto(ks.autoKey) {
			continue
		}
		if ks.kind == KindBin {
			if r := join("src/main.rs"); t.Has(r) && !taken[p.name] && !takenRoot[r] {
				taken[p.name], takenRoot[r] = true, true
				add(KindBin, p.name, r, true)
			}
		}
		prefix := join(ks.dir) + "/"
		var found []*target
		for _, f := range files {
			rest, ok := strings.CutPrefix(f, prefix)
			if !ok {
				continue
			}
			var name string
			switch parts := strings.Split(rest, "/"); {
			case len(parts) == 1 && strings.HasSuffix(parts[0], ".rs"):
				name = strings.TrimSuffix(parts[0], ".rs")
			case len(parts) == 2 && parts[1] == "main.rs":
				name = parts[0]
			default:
				continue
			}
			if name == "" || taken[name] || takenRoot[f] {
				continue
			}
			found = append(found, &target{kind: ks.kind, name: name, root: f, unit: true, node: Node(UnitID(p.dir, ks.kind, name))})
		}
		sort.Slice(found, func(i, j int) bool { return found[i].name < found[j].name })
		for _, tg := range found {
			if taken[tg.name] {
				continue
			}
			taken[tg.name] = true
			out = append(out, tg)
		}
	}
	return out
}

// inferRoot: the root file of an explicit target without a path (Cargo's defaults).
func inferRoot(t tree.Tree, p *pkg, kind, dir, name string) string {
	a := path.Join(p.dir, dir, name+".rs")
	b := path.Join(p.dir, dir, name, "main.rs")
	switch {
	case t.Has(a):
		return a
	case t.Has(b):
		return b
	case kind == KindBin && name == p.name && t.Has(path.Join(p.dir, "src/main.rs")):
		return path.Join(p.dir, "src/main.rs")
	}
	return a
}

// buildScript returns the build script of the package ("" when none).
func (p *pkg) buildScript(t tree.Tree) string {
	pt := tbl(p.m.doc["package"])
	if pt == nil {
		pt = tbl(p.m.doc["project"])
	}
	switch v := pt["build"].(type) {
	case string:
		return path.Join(p.dir, strings.ReplaceAll(v, "\\", "/"))
	case bool:
		if !v {
			return ""
		}
	}
	if r := path.Join(p.dir, "build.rs"); t.Has(r) {
		return r
	}
	return ""
}

func tbl(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strs(v any) []string {
	arr, _ := v.([]any)
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

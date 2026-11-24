package dart

import (
	"maps"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/model"
)

// assetNode is the virtual node standing for the Flutter assets a package declares.
func assetNode(dir string) string { return "<dartassets:" + dir + ">" }

// flutterAssets returns the paths a package's `flutter:` section names (assets, fonts, shaders, in any form: a string, or
// a map with path/asset entries), repo-relative. Every string that may be a path counts: an over-approximation.
func flutterAssets(p *pkg) []string {
	if p.raw == nil {
		return nil
	}
	f, ok := p.raw["flutter"]
	if !ok {
		return nil
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if strings.ContainsAny(x, "/.") {
				if d, ok := resolveRel(p.dir, x); ok && d != p.dir {
					out = append(out, d)
				}
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(x)) {
				walk(x[k])
			}
		}
	}
	walk(f)
	slices.Sort(out)
	return slices.Compact(out)
}

// assets links the Flutter assets of every package to the test files of that package and of every in-repository package
// that depends on it (transitively): Flutter bundles the declared assets of every dependency, and a test loads them by key
// (`AssetImage('x.png', package: 'other')`, `rootBundle.load('packages/other/x.png')`), never by a path a literal names.
func (b *builder) assets() {
	for _, d := range slices.Sorted(maps.Keys(b.r.pkgs)) {
		q := b.r.pkgs[d]
		paths := flutterAssets(q)
		if len(paths) == 0 {
			continue
		}
		node := assetNode(d)
		for _, a := range paths {
			if b.t.Has(a) {
				b.edge(node, a, model.EdgeFixtureData, "flutter asset", 0)
			} else {
				// A directory entry (direct files only for Flutter; the whole tree here) or a file added later.
				b.edge(node, escapeGlob(strings.TrimSuffix(a, "/"))+"/**", model.EdgePattern, "flutter asset directory", 0)
				b.edge(node, escapeGlob(strings.TrimSuffix(a, "/")), model.EdgePattern, "flutter asset", 0)
			}
		}
		for _, u := range b.dependentsOf(d) {
			for _, f := range b.units[u] {
				b.edge(f, node, model.EdgeImplicit, "flutter assets of "+q.name, 0)
			}
		}
	}
}

// dependentsOf returns dir and every package of the tree that depends on it, transitively (by path or by name).
func (b *builder) dependentsOf(dir string) []string {
	set := map[string]bool{dir: true}
	for changed := true; changed; {
		changed = false
		names := map[string]bool{}
		for d := range set {
			if p := b.r.pkgs[d]; p != nil && p.name != "" {
				names[p.name] = true
			}
		}
		for d, q := range b.r.pkgs {
			if !set[d] && dependsOn(q, set, names) {
				set[d] = true
				changed = true
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// dependenciesOf returns dir and every package of the tree it depends on, transitively (path dependencies, and every
// directory of a dependency's name). An unreadable pubspec may depend on anything: every package.
func (b *builder) dependenciesOf(dir string) []string {
	set := map[string]bool{}
	todo := []string{dir}
	for len(todo) > 0 {
		d := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		p := b.r.pkgs[d]
		if set[d] || p == nil {
			continue
		}
		set[d] = true
		if p.raw == nil {
			return slices.Sorted(maps.Keys(b.r.pkgs))
		}
		todo = append(todo, p.pathDeps...)
		for _, n := range p.depNames {
			todo = append(todo, b.r.byName[n]...)
		}
	}
	return slices.Sorted(maps.Keys(set))
}

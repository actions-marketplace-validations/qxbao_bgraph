package python

import (
	"path"
	"slices"
	"sort"
	"strings"
)

// rootSpec is an import root: a directory (repo-relative) that is on sys.path, or a `package-dir` mapping where the package named
// by prefix lives in dir.
type rootSpec struct {
	prefix []string
	dir    string
}

func (r rootSpec) key() string { return strings.Join(r.prefix, ".") + "@" + r.dir }

type resolved struct {
	files []string
	found bool
}

func escapeGlob(s string) string {
	var sb strings.Builder
	for _, c := range s {
		if strings.ContainsRune(`*?[]{}\`, c) {
			sb.WriteByte('\\')
		}
		sb.WriteRune(c)
	}
	return sb.String()
}

func joinPath(dir string, rel ...string) string {
	if len(rel) == 0 {
		return dir
	}
	return path.Join(dir, path.Join(rel...))
}

// addRoot registers an import root if its directory holds Python files (or is the repo root).
func (b *builder) addRoot(r rootSpec) {
	r.dir = path.Clean(r.dir)
	if r.dir == ".." || strings.HasPrefix(r.dir, "../") || strings.HasPrefix(r.dir, "/") {
		return
	}
	if r.dir != "." && !b.pyDirs[r.dir] {
		return
	}
	k := r.key()
	if b.rootSeen[k] {
		return
	}
	b.rootSeen[k] = true
	b.roots = append(b.roots, r)
}

// resolveAt looks for the module rel (relative to dir) and appends the files an import of it executes to *out. fromRoot: dir is an
// import root, so the ancestors start at rel[:1] (a root's own __init__.py is not an ancestor of anything).
func (b *builder) resolveAt(dir string, rel []string, fromRoot bool, names []string, out *[]string) bool {
	d := joinPath(dir, rel...)
	found := false
	var local []string
	if len(rel) > 0 {
		for _, ext := range []string{".py", ".pyi"} {
			if f := d + ext; b.fileSet[f] {
				local = append(local, f)
				found = true
			}
		}
	}
	for _, init := range []string{"__init__.py", "__init__.pyi"} {
		if f := path.Join(d, init); b.fileSet[f] {
			local = append(local, f)
			found = true
		}
	}
	isDir := b.pyDirs[d]
	if !found && isDir {
		found = true // PEP 420 namespace package
	}
	if !found {
		return false
	}
	start := 0
	if fromRoot {
		start = 1
	}
	for j := start; j < len(rel); j++ {
		anc := joinPath(dir, rel[:j]...)
		for _, init := range []string{"__init__.py", "__init__.pyi"} {
			if f := path.Join(anc, init); b.fileSet[f] {
				*out = append(*out, f)
			}
		}
	}
	*out = append(*out, local...)
	if isDir {
		for _, n := range names {
			if n == "*" || n == "" {
				continue
			}
			for _, f := range []string{path.Join(d, n+".py"), path.Join(d, n+".pyi"), path.Join(d, n, "__init__.py"), path.Join(d, n, "__init__.pyi")} {
				if b.fileSet[f] {
					*out = append(*out, f)
				}
			}
		}
	}
	return true
}

// resolveAbs resolves an absolute import against every candidate root and unions the matches.
func (b *builder) resolveAbs(comps, names []string) resolved {
	key := strings.Join(comps, ".") + "|" + strings.Join(names, ",")
	if r, ok := b.cache[key]; ok {
		return r
	}
	var files []string
	found := false
	for _, r := range b.roots {
		if len(comps) < len(r.prefix) || !slices.Equal(comps[:len(r.prefix)], r.prefix) {
			continue
		}
		if b.resolveAt(r.dir, comps[len(r.prefix):], len(r.prefix) == 0, names, &files) {
			found = true
		}
	}
	res := resolved{found: found}
	if found {
		sort.Strings(files)
		res.files = slices.Compact(files)
	}
	b.cache[key] = res
	return res
}

// suffixMatches reports whether some module of the repo has a path ending in comps (a/b/c.py, a/b/c/__init__.py ...).
func (b *builder) suffixMatches(comps []string) bool {
	want := strings.Join(comps, "/")
	for _, stem := range b.byLast[comps[len(comps)-1]] {
		if stem == want || strings.HasSuffix(stem, "/"+want) {
			return true
		}
	}
	return false
}

func (b *builder) isExternal(top string) bool {
	return stdlib[top] || b.declared.isDeclared(top)
}

// moduleNode is the virtual graph node of an unresolved absolute module.
func moduleNode(spec string) string { return "<pymod:" + spec + ">" }

// patterns of a module that may live under a root we do not know about: **/a/b/c.py, **/a/b/c/__init__.py and the __init__.py of
// every ancestor package.
func modulePatterns(comps []string) []string {
	var out []string
	esc := make([]string, len(comps))
	for i, c := range comps {
		esc[i] = escapeGlob(c)
	}
	for j := 1; j < len(comps); j++ {
		p := "**/" + strings.Join(esc[:j], "/") + "/__init__"
		out = append(out, p+".py", p+".pyi")
	}
	full := "**/" + strings.Join(esc, "/")
	return append(out, full+".py", full+".pyi", full+"/__init__.py", full+"/__init__.pyi")
}

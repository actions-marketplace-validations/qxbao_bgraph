package tree

import (
	"path"
	"slices"
	"sort"
	"strings"
)

// ModeSymlink is git's mode of a symbolic link (its blob holds the target path).
const ModeSymlink = "120000"

// LinkSemantics names how trees present symbolic links. It is part of the key of stored analysis snapshots: change it whenever the
// files a tree lists (or what they read) change for the same commit.
const LinkSemantics = "links-1"

// Problems of a link that cannot be followed (Link.Problem).
const (
	LinkAbsolute = "absolute target"
	LinkOutside  = "target outside the repository"
	LinkDangling = "dangling target"
	LinkCycle    = "link cycle"
	LinkTooLarge = "directory expansion too large"
)

// maxAliases bounds the number of paths that directory links add to a tree (pathological nesting of links).
const maxAliases = 1 << 20

// Link is a symbolic link of the tree (git mode 120000), whatever the checkout made of it (a real link, or a plain text file
// holding the target when core.symlinks=false).
type Link struct {
	Path   string // the link's own path
	Target string // the target as stored in git
	// Real is the path the link resolves to in the tree (a file, or a directory when Dir); "" when the target is not in the
	// tree. For a LinkCycle it is the directory the cycle re-enters.
	Real string
	Dir  bool
	// Problem says why the link cannot be followed completely; "" when it can. A link with a problem must not be silently
	// ignored by a caller: the selector widens with symlink_outside when it is involved in a change.
	Problem string
}

// Linker is implemented by trees that follow their symbolic links: a file link reads its target file, and a directory link
// behaves as if the target directory's files were present under the link path. Files, Has, IsDir, Read and Key of such a tree
// already resolve links; Linker exposes what was resolved, so that the graph can tie every alias to the file it reads.
type Linker interface {
	// Links returns the tree's symbolic links, sorted by path.
	Links() []Link
	// Aliases maps every path that reads another file through a link (a file link, a file under a directory link) to that
	// regular file.
	Aliases() map[string]string
}

// LinksOf returns t's symbolic links (none when t does not follow links).
func LinksOf(t Tree) []Link {
	if l, ok := t.(Linker); ok {
		return l.Links()
	}
	return nil
}

// AliasesOf returns t's aliases (see Linker.Aliases); nil when t does not follow links.
func AliasesOf(t Tree) map[string]string {
	if l, ok := t.(Linker); ok {
		return l.Aliases()
	}
	return nil
}

// LinkView is the result of following the links of a set of entries; tree implementations embed it.
type LinkView struct {
	files   []string          // regular files ∪ aliases, sorted
	alias   map[string]string // alias → regular file
	links   []Link
	regular func(string) bool
}

// Files returns the regular files and every alias, sorted.
func (v *LinkView) Files() []string { return v.files }

// Links: see Linker.
func (v *LinkView) Links() []Link { return v.links }

// Aliases: see Linker.
func (v *LinkView) Aliases() map[string]string { return v.alias }

// Real returns the regular file p reads: p itself when it is a regular file, the link's target file when p is an alias.
func (v *LinkView) Real(p string) (string, bool) {
	if v.regular(p) {
		return p, true
	}
	r, ok := v.alias[p]
	return r, ok
}

// FollowLinks resolves links (link path → target as stored) against the regular files (sorted, distinct; no link among them);
// isRegular reports membership in regular. Paths follow the model convention (root-relative, forward slashes, clean).
func FollowLinks(regular []string, isRegular func(string) bool, links map[string]string) *LinkView {
	v := &LinkView{alias: map[string]string{}, regular: isRegular}
	if len(links) == 0 {
		v.files = regular
		return v
	}
	f := &follower{regular: regular, reg: isRegular, links: links, dirs: map[string]bool{".": true}, res: map[string]resolved{}}
	for _, p := range regular {
		f.addDirs(p)
	}
	for p := range links {
		f.linkPaths = append(f.linkPaths, p)
		f.addDirs(p)
	}
	slices.Sort(f.linkPaths)

	byPath := map[string]*Link{}
	for _, p := range f.linkPaths {
		r := f.resolve(p, 0)
		l := &Link{Path: p, Target: links[p], Real: r.real, Dir: r.dir, Problem: r.problem}
		byPath[p] = l
		if r.problem == "" && !r.dir {
			v.alias[p] = r.real
		}
	}
	// A directory link whose expansion re-enters itself (directly or through other directory links) is a cycle. It is still
	// followed one level (every file under its target gets an alias), but not into the re-entry.
	for _, p := range f.linkPaths {
		l := byPath[p]
		if r := f.resolve(p, 0); r.problem != "" || !r.dir {
			continue // (l.Problem may already be LinkCycle, found while expanding another link: still followed one level)
		}
		added := map[string]string{}
		ok := f.expand(p, l.Real, []string{p}, added, byPath, len(v.alias))
		if !ok {
			l.Problem = LinkTooLarge
			continue
		}
		for a, r := range added {
			if !isRegular(a) {
				v.alias[a] = r
			}
		}
	}
	for _, p := range f.linkPaths {
		v.links = append(v.links, *byPath[p])
	}
	v.files = make([]string, 0, len(regular)+len(v.alias))
	v.files = append(v.files, regular...)
	for a := range v.alias {
		v.files = append(v.files, a)
	}
	slices.Sort(v.files)
	return v
}

type resolved struct {
	real    string
	dir     bool
	problem string
}

type follower struct {
	regular   []string // sorted
	reg       func(string) bool
	links     map[string]string
	linkPaths []string // sorted
	dirs      map[string]bool
	res       map[string]resolved
}

func (f *follower) addDirs(p string) {
	for d := path.Dir(p); d != "." && !f.dirs[d]; d = path.Dir(d) {
		f.dirs[d] = true
	}
}

// maxHops bounds link-to-link hops while resolving one path (like the OS limit; beyond it the chain is a cycle).
const maxHops = 40

// cleanTarget turns a stored link target into a path relative to the repo root, or reports why it cannot.
func cleanTarget(linkPath, target string) (string, string) {
	t := strings.ReplaceAll(strings.TrimRight(target, "\r\n"), "\\", "/")
	if t == "" {
		return "", LinkDangling
	}
	if strings.HasPrefix(t, "/") || len(t) >= 2 && t[1] == ':' {
		return "", LinkAbsolute
	}
	j := path.Join(path.Dir(linkPath), t)
	if j == ".." || strings.HasPrefix(j, "../") {
		return "", LinkOutside
	}
	return j, ""
}

// resolve resolves p component by component, following every link on the way.
func (f *follower) resolve(p string, hops int) resolved {
	if r, ok := f.res[p]; ok && hops == 0 {
		return r
	}
	r := f.resolveUncached(p, hops)
	if hops == 0 {
		f.res[p] = r
	}
	return r
}

func (f *follower) resolveUncached(p string, hops int) resolved {
	if p == "." {
		return resolved{real: ".", dir: true}
	}
	parts := strings.Split(p, "/")
	cur := ""
	for i, c := range parts {
		next := c
		if cur != "" && cur != "." {
			next = cur + "/" + c
		}
		if tgt, ok := f.links[next]; ok {
			if hops >= maxHops {
				return resolved{problem: LinkCycle}
			}
			j, prob := cleanTarget(next, tgt)
			if prob != "" {
				return resolved{problem: prob}
			}
			r := f.resolveUncached(j, hops+1)
			if r.problem != "" {
				return r
			}
			if i < len(parts)-1 && !r.dir {
				return resolved{problem: LinkDangling}
			}
			cur = r.real
			continue
		}
		cur = next
	}
	switch {
	case f.reg(cur):
		return resolved{real: cur}
	case f.dirs[cur]:
		return resolved{real: cur, dir: true}
	}
	return resolved{problem: LinkDangling}
}

// under returns the elements of sorted that lie strictly under directory d ("." is the root).
func under(sorted []string, d string) []string {
	if d == "." || d == "" {
		return sorted
	}
	pre := d + "/"
	i := sort.SearchStrings(sorted, pre)
	j := i
	for j < len(sorted) && strings.HasPrefix(sorted[j], pre) {
		j++
	}
	return sorted[i:j]
}

func rel(d, p string) string {
	if d == "." || d == "" {
		return p
	}
	return p[len(d)+1:]
}

// expand adds to out an alias vprefix/<rel> for every file under the real directory d, following nested links. stack holds the
// directory links being expanded: re-entering one is a cycle (marked on it, and not followed further). Returns false when the
// expansion grows beyond maxAliases.
func (f *follower) expand(vprefix, d string, stack []string, out map[string]string, byPath map[string]*Link, base int) bool {
	for _, r := range under(f.regular, d) {
		out[vprefix+"/"+rel(d, r)] = r
	}
	if base+len(out) > maxAliases {
		return false
	}
	for _, lp := range under(f.linkPaths, d) {
		vp := vprefix + "/" + rel(d, lp)
		l := byPath[lp]
		if slices.Contains(stack, lp) {
			l.Problem = LinkCycle
			continue
		}
		r := f.resolve(lp, 0)
		if r.problem != "" {
			continue
		}
		if !r.dir {
			out[vp] = r.real
			continue
		}
		if !f.expand(vp, r.real, append(stack, lp), out, byPath, base) {
			return false
		}
	}
	return true
}

package evalkit

import (
	"io/fs"
	"path"
	"strings"

	"github.com/qxbao/bgraph/internal/tree"
)

// linkedGitTree is a gitTree with its symbolic links followed (tree.FollowLinks), as bgraph's own commit tree presents them: an
// alias path under a directory link reads its target file. Used by the Dart locator and oracle, where test directories are
// sometimes links into another package (cbl-dart: test/cbl_e2e_tests -> ../../cbl_e2e_tests/lib/src).
type linkedGitTree struct {
	*gitTree
	view *tree.LinkView
	has  map[string]bool
	dirs map[string]bool
}

var (
	_ tree.Tree   = (*linkedGitTree)(nil)
	_ tree.Linker = (*linkedGitTree)(nil)
)

// followGitLinks returns gt itself when it has no symbolic link, else a view that follows them. Link targets are read from gt.
func followGitLinks(gt *gitTree) tree.Tree {
	if len(gt.links) == 0 {
		return gt
	}
	var regular []string
	isReg := map[string]bool{}
	for _, f := range gt.files {
		if !gt.links[f] {
			regular = append(regular, f)
			isReg[f] = true
		}
	}
	targets := map[string]string{}
	for p := range gt.links {
		if b, err := gt.Read(p); err == nil {
			targets[p] = strings.TrimSpace(string(b))
		}
	}
	lt := &linkedGitTree{gitTree: gt, view: tree.FollowLinks(regular, func(p string) bool { return isReg[p] }, targets),
		has: map[string]bool{}, dirs: map[string]bool{}}
	for _, f := range lt.view.Files() {
		lt.has[f] = true
		for d := path.Dir(f); d != "." && !lt.dirs[d]; d = path.Dir(d) {
			lt.dirs[d] = true
		}
	}
	return lt
}

func (t *linkedGitTree) Files() []string              { return t.view.Files() }
func (t *linkedGitTree) Has(p string) bool            { return t.has[p] }
func (t *linkedGitTree) IsDir(p string) bool          { return t.dirs[p] }
func (t *linkedGitTree) Links() []tree.Link           { return t.view.Links() }
func (t *linkedGitTree) Aliases() map[string]string   { return t.view.Aliases() }
func (t *linkedGitTree) real(p string) (string, bool) { return t.view.Real(p) }

func (t *linkedGitTree) Read(p string) ([]byte, error) {
	r, ok := t.real(p)
	if !ok {
		return nil, fs.ErrNotExist
	}
	return t.gitTree.Read(r)
}

func (t *linkedGitTree) Key(p string) string {
	if r, ok := t.real(p); ok {
		return t.gitTree.Key(r)
	}
	return ""
}

func (t *linkedGitTree) FoldLookup(p string) []string {
	var out []string
	for _, f := range t.view.Files() {
		if strings.EqualFold(f, p) {
			out = append(out, f)
		}
	}
	return out
}

// dartRealPath maps a path of t (possibly an alias) to the file holding its blob.
func dartRealPath(t tree.Tree, p string) string {
	if lt, ok := t.(*linkedGitTree); ok {
		if r, ok := lt.real(p); ok {
			return r
		}
	}
	return p
}

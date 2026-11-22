// Package tree defines the file tree that the resolver and graph builder read: the working tree or a commit's tree.
// The git-backed implementation lives in internal/gitx; the in-memory implementation (MapTree) is used for tests.
package tree

import (
	"io/fs"
	"path"
	"slices"
	"strings"
)

// Tree is a read-only snapshot of the repo's file set (tracked + untracked non-ignored). Paths follow the
// model convention: root-relative, forward slashes, case-sensitive per git's index.
//
// Symbolic links (git mode 120000) are followed when the target is inside the tree (see Linker): a file link is a file that reads
// its target, a directory link behaves as if the target directory's files were present under the link path. A directory link
// and a link that cannot be followed are not files.
type Tree interface {
	// Files returns every file, sorted. Contains no directories.
	Files() []string
	// Has: p is a file in the tree (exact match, case-sensitive).
	Has(p string) bool
	// IsDir: there is at least one file under p/.
	IsDir(p string) bool
	// Read reads the content (commit tree: blob; working tree: disk). fs.ErrNotExist error if absent.
	Read(p string) ([]byte, error)
	// Key is a stable content key (git blob OID or blob-style hash) for caching scan results.
	Key(p string) string
	// FoldLookup returns files matching p ignoring case (for reporting case_mismatch).
	FoldLookup(p string) []string
}

// MapTree is an in-memory Tree, used for tests and fixtures. It follows its symbolic links (see Linker).
type MapTree struct {
	*LinkView
	files map[string]string
	dirs  map[string]bool
	fold  map[string][]string
}

var _ Linker = (*MapTree)(nil)

// NewMapTree builds a tree from a map of path → content.
func NewMapTree(files map[string]string) *MapTree { return NewMapTreeLinks(files, nil) }

// NewMapTreeLinks builds a tree from a map of path → content and a map of symbolic link path → target (as git stores it).
func NewMapTreeLinks(files, links map[string]string) *MapTree {
	t := &MapTree{files: map[string]string{}, dirs: map[string]bool{}, fold: map[string][]string{}}
	var list []string
	for p, c := range files {
		p = path.Clean(strings.ReplaceAll(p, "\\", "/"))
		t.files[p] = c
		list = append(list, p)
	}
	slices.Sort(list)
	ls := map[string]string{}
	for p, tg := range links {
		ls[path.Clean(strings.ReplaceAll(p, "\\", "/"))] = tg
	}
	t.LinkView = FollowLinks(list, func(p string) bool { _, ok := t.files[p]; return ok }, ls)
	for _, p := range t.LinkView.Files() {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			t.dirs[d] = true
		}
		t.dirs["."] = true
		k := strings.ToLower(p)
		t.fold[k] = append(t.fold[k], p)
	}
	for _, v := range t.fold {
		slices.Sort(v)
	}
	return t
}

func (t *MapTree) Has(p string) bool {
	_, ok := t.Real(p)
	return ok
}
func (t *MapTree) IsDir(p string) bool { return t.dirs[p] }
func (t *MapTree) Read(p string) ([]byte, error) {
	r, ok := t.Real(p)
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(t.files[r]), nil
}
func (t *MapTree) Key(p string) string {
	r, ok := t.Real(p)
	if !ok {
		return ""
	}
	return BlobKey([]byte(t.files[r]))
}
func (t *MapTree) FoldLookup(p string) []string { return t.fold[strings.ToLower(p)] }

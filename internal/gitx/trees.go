package gitx

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/qxbao/bgraph/internal/tree"
)

var (
	_ tree.Tree = (*WorkTree)(nil)
	_ tree.Tree = (*CommitTree)(nil)
)

func joinRoot(root, p string) string { return filepath.Join(root, filepath.FromSlash(p)) }

// pathSet holds the shared part: the sorted file list, the set of ancestor directories, and a case-insensitive lookup table.
type pathSet struct {
	files []string
	dirs  map[string]bool
	fold  map[string][]string
}

// newPathSet takes a list (sorted in place) of distinct files.
func newPathSet(files []string) pathSet {
	slices.Sort(files)
	ps := pathSet{files: files, dirs: map[string]bool{}, fold: make(map[string][]string, len(files))}
	if len(files) > 0 {
		ps.dirs["."] = true
	}
	for _, p := range files {
		for d := path.Dir(p); d != "." && !ps.dirs[d]; d = path.Dir(d) {
			ps.dirs[d] = true
		}
		k := strings.ToLower(p)
		ps.fold[k] = append(ps.fold[k], p) // files is sorted, so each list is sorted too
	}
	return ps
}

func (ps *pathSet) Files() []string              { return ps.files }
func (ps *pathSet) IsDir(p string) bool          { return ps.dirs[p] }
func (ps *pathSet) FoldLookup(p string) []string { return ps.fold[strings.ToLower(p)] }

func notExist(op, p string) error { return &fs.PathError{Op: op, Path: p, Err: fs.ErrNotExist} }

// ---------------------------------------------------------------------------------------------------------------------
// WorkTree

type wtEntry struct {
	mode, oid string
	dirty     bool // key must come from disk, not the index oid
	once      sync.Once
	key       string
}

// WorkTree is the working tree: index entries (stage 0; a conflicted path counts once) ∪ untracked non-ignored files, minus files
// deleted from disk. Gitlinks are not in Files but in Submodules().
type WorkTree struct {
	pathSet
	root string
	ents map[string]*wtEntry
	subs []string
}

func buildWorkTree(root string, idx []indexEntry, ls lsFiles, diff []ChangedFile) *WorkTree {
	ents := make(map[string]*wtEntry, len(idx)+len(ls.untracked))
	var subs []string
	conflicted := map[string]bool{}
	for _, e := range idx {
		if e.stage != 0 {
			conflicted[e.path] = true
		}
		if cur, ok := ents[e.path]; ok {
			// Stage 0 wins; otherwise keep the first entry (lowest stage).
			if e.stage == 0 {
				cur.mode, cur.oid = e.mode, e.oid
			}
			continue
		}
		ents[e.path] = &wtEntry{mode: e.mode, oid: e.oid}
	}
	for p := range conflicted {
		ents[p].dirty = true
		if lstatMissing(root, p) {
			delete(ents, p)
		}
	}
	for p := range ls.modified {
		if e, ok := ents[p]; ok {
			e.dirty = true
		}
	}
	for p := range ls.removed {
		delete(ents, p)
	}
	// Defensive: diff reports 'D' but ls-files does not report 'R' → check the disk.
	for _, f := range diff {
		if f.Status == 'D' {
			if _, ok := ents[f.Path]; ok && lstatMissing(root, f.Path) {
				delete(ents, f.Path)
			}
		}
	}
	for _, p := range ls.untracked {
		if strings.HasSuffix(p, "/") {
			subs = append(subs, strings.TrimRight(p, "/"))
			continue
		}
		if _, ok := ents[p]; !ok {
			ents[p] = &wtEntry{mode: "100644", dirty: true}
		}
	}
	files := make([]string, 0, len(ents))
	for p, e := range ents {
		if e.mode == "160000" {
			subs = append(subs, p)
			delete(ents, p)
			continue
		}
		files = append(files, p)
	}
	slices.Sort(subs)
	subs = slices.Compact(subs)
	return &WorkTree{pathSet: newPathSet(files), root: root, ents: ents, subs: subs}
}

func (t *WorkTree) Has(p string) bool { _, ok := t.ents[p]; return ok }

// Mode returns the file's git mode ("100644", "100755", "120000"); untracked is always "100644"; "" if absent.
func (t *WorkTree) Mode(p string) string {
	if e, ok := t.ents[p]; ok {
		return e.mode
	}
	return ""
}

// Submodules returns gitlink paths (mode 160000) and untracked directories that are nested git repos; sorted.
func (t *WorkTree) Submodules() []string { return t.subs }

// Read reads a file from disk. For a "dirty" file it also computes and caches the key.
func (t *WorkTree) Read(p string) ([]byte, error) {
	e, ok := t.ents[p]
	if !ok {
		return nil, notExist("read", p)
	}
	b, err := os.ReadFile(joinRoot(t.root, p))
	if err != nil {
		return nil, err
	}
	if e.dirty {
		e.once.Do(func() { e.key = tree.BlobKey(b) })
	}
	return b, nil
}

// Key: a tracked file unchanged versus the index → the index blob OID; a modified file (including staged-then-reverted), unmerged or
// untracked → the BlobKey of the on-disk bytes (computed lazily, cached). "" if the file is absent or the disk cannot be read.
func (t *WorkTree) Key(p string) string {
	e, ok := t.ents[p]
	if !ok {
		return ""
	}
	if !e.dirty {
		return e.oid
	}
	e.once.Do(func() {
		if b, err := os.ReadFile(joinRoot(t.root, p)); err == nil {
			e.key = tree.BlobKey(b)
		}
	})
	return e.key
}

// ---------------------------------------------------------------------------------------------------------------------
// CommitTree

type ctEntry struct{ mode, oid string }

// CommitTree is the tree of a commit, reading blobs via the Repo's cat-file --batch.
type CommitTree struct {
	pathSet
	r    *Repo
	ents map[string]ctEntry
	subs []string
}

// CommitTree builds the tree of rev with `git ls-tree -r -z --full-tree`.
func (r *Repo) CommitTree(ctx context.Context, rev string) (*CommitTree, error) {
	out, err := r.run(ctx, "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", rev)
	if err != nil {
		return nil, err
	}
	t := &CommitTree{r: r, ents: map[string]ctEntry{}}
	var files []string
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		tab := bytes.IndexByte(rec, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("gitx: ls-tree: malformed record %q", rec)
		}
		f := strings.Fields(string(rec[:tab]))
		if len(f) != 3 {
			return nil, fmt.Errorf("gitx: ls-tree: malformed metadata %q", rec[:tab])
		}
		p := string(rec[tab+1:])
		if f[0] == "160000" || f[1] == "commit" {
			t.subs = append(t.subs, p)
			continue
		}
		t.ents[p] = ctEntry{mode: f[0], oid: f[2]}
		files = append(files, p)
	}
	slices.Sort(t.subs)
	t.pathSet = newPathSet(files)
	return t, nil
}

func (t *CommitTree) Has(p string) bool { _, ok := t.ents[p]; return ok }

// Mode returns the file's git mode in the commit; "" if absent.
func (t *CommitTree) Mode(p string) string { return t.ents[p].mode }

// Submodules returns the commit's gitlink paths; sorted.
func (t *CommitTree) Submodules() []string { return t.subs }

// Key is the blob OID.
func (t *CommitTree) Key(p string) string { return t.ents[p].oid }

// Read reads a blob (symlink: the link's target text). tree.Tree has no ctx; Repo.Close can interrupt a hung request.
func (t *CommitTree) Read(p string) ([]byte, error) {
	e, ok := t.ents[p]
	if !ok {
		return nil, notExist("read", p)
	}
	return t.r.cat.get(context.Background(), e.oid)
}

// ReadContext is like Read but honors ctx (cancelling ctx kills the cat-file process; the next read restarts it).
func (t *CommitTree) ReadContext(ctx context.Context, p string) ([]byte, error) {
	e, ok := t.ents[p]
	if !ok {
		return nil, notExist("read", p)
	}
	return t.r.cat.get(ctx, e.oid)
}

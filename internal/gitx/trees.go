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

// pathSet holds the shared part: the sorted file list (symbolic links followed, see tree.Linker), the set of ancestor directories,
// a case-insensitive lookup table, and the raw entries (links as entries, not followed).
type pathSet struct {
	*tree.LinkView
	entries []string // every entry except gitlinks, links included, sorted
	dirs    map[string]bool
	fold    map[string][]string
}

// newPathSet takes a list (sorted in place) of distinct entries, the targets of those that are symbolic links, and the entry
// membership test.
func newPathSet(entries []string, links map[string]string, isEntry func(string) bool) pathSet {
	slices.Sort(entries)
	regular := entries
	if len(links) > 0 {
		regular = make([]string, 0, len(entries))
		for _, p := range entries {
			if _, ok := links[p]; !ok {
				regular = append(regular, p)
			}
		}
	}
	isRegular := isEntry
	if len(links) > 0 {
		isRegular = func(p string) bool { _, link := links[p]; return !link && isEntry(p) }
	}
	lv := tree.FollowLinks(regular, isRegular, links)
	files := lv.Files()
	ps := pathSet{LinkView: lv, entries: entries, dirs: map[string]bool{}, fold: make(map[string][]string, len(files))}
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

func (ps *pathSet) Has(p string) bool   { _, ok := ps.Real(p); return ok }
func (ps *pathSet) IsDir(p string) bool { return ps.dirs[p] }

// Entries returns every entry (symbolic links as entries, not followed; no gitlinks), sorted.
func (ps *pathSet) Entries() []string            { return ps.entries }
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
	// The working tree's mode wins over the index's (an unstaged file ↔ symlink type change): the diff against the base reports it.
	for _, f := range diff {
		if e, ok := ents[f.Path]; ok && (f.NewMode == "100644" || f.NewMode == "100755" || f.NewMode == tree.ModeSymlink) {
			e.mode = f.NewMode
		}
	}
	for _, p := range ls.untracked {
		if strings.HasSuffix(p, "/") {
			subs = append(subs, strings.TrimRight(p, "/"))
			continue
		}
		if _, ok := ents[p]; !ok {
			mode := "100644"
			if fi, err := os.Lstat(joinRoot(root, p)); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
				mode = tree.ModeSymlink
			}
			ents[p] = &wtEntry{mode: mode, dirty: true}
		}
	}
	files := make([]string, 0, len(ents))
	var links map[string]string
	for p, e := range ents {
		if e.mode == "160000" {
			subs = append(subs, p)
			delete(ents, p)
			continue
		}
		if e.mode == tree.ModeSymlink {
			tgt, err := readLinkTarget(root, p)
			if err != nil {
				tgt = "" // unreadable: a dangling link (never followed; widened when involved in a change)
			}
			if links == nil {
				links = map[string]string{}
			}
			links[p] = tgt
		}
		files = append(files, p)
	}
	slices.Sort(subs)
	subs = slices.Compact(subs)
	return &WorkTree{pathSet: newPathSet(files, links, func(p string) bool { _, ok := ents[p]; return ok }), root: root, ents: ents, subs: subs}
}

// readLinkTarget reads the target of the symbolic link p from disk: the link itself when the checkout made one, otherwise the
// plain file holding the target text (core.symlinks=false, the usual Windows checkout).
func readLinkTarget(root, p string) (string, error) {
	full := joinRoot(root, p)
	if fi, err := os.Lstat(full); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		t, err := os.Readlink(full)
		return filepath.ToSlash(t), err
	}
	b, err := os.ReadFile(full)
	return string(b), err
}

// Mode returns the entry's git mode ("100644", "100755", "120000"); untracked is "100644" ("120000" for a symbolic link); ""
// if absent. It is not resolved through links.
func (t *WorkTree) Mode(p string) string {
	if e, ok := t.ents[p]; ok {
		return e.mode
	}
	return ""
}

// Submodules returns gitlink paths (mode 160000) and untracked directories that are nested git repos; sorted.
func (t *WorkTree) Submodules() []string { return t.subs }

// Read reads a file from disk (through a symbolic link: its target file). For a "dirty" file it also computes and caches the key.
func (t *WorkTree) Read(p string) ([]byte, error) {
	r, ok := t.Real(p)
	if !ok {
		return nil, notExist("read", p)
	}
	p = r
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
// Through a symbolic link: the key of its target file.
func (t *WorkTree) Key(p string) string {
	r, ok := t.Real(p)
	if !ok {
		return ""
	}
	p = r
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
	r       *Repo
	ents    map[string]ctEntry
	subs    []string
	subOIDs map[string]string // gitlink path → commit OID
}

// CommitTree builds the tree of rev with `git ls-tree -r -z --full-tree`.
func (r *Repo) CommitTree(ctx context.Context, rev string) (*CommitTree, error) {
	out, err := r.run(ctx, "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", rev)
	if err != nil {
		return nil, err
	}
	t := &CommitTree{r: r, ents: map[string]ctEntry{}}
	var files []string
	var links map[string]string
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
			if t.subOIDs == nil {
				t.subOIDs = map[string]string{}
			}
			t.subOIDs[p] = f[2]
			continue
		}
		t.ents[p] = ctEntry{mode: f[0], oid: f[2]}
		files = append(files, p)
		if f[0] == tree.ModeSymlink {
			if links == nil {
				links = map[string]string{}
			}
			links[p] = ""
		}
	}
	for p := range links {
		b, err := r.cat.get(ctx, t.ents[p].oid)
		if err != nil {
			return nil, err
		}
		links[p] = string(b)
	}
	slices.Sort(t.subs)
	t.pathSet = newPathSet(files, links, func(p string) bool { _, ok := t.ents[p]; return ok })
	return t, nil
}

// Mode returns the entry's git mode in the commit ("120000" for a symbolic link: not resolved); "" if absent.
func (t *CommitTree) Mode(p string) string { return t.ents[p].mode }

// Submodules returns the commit's gitlink paths; sorted.
func (t *CommitTree) Submodules() []string { return t.subs }

// SubmoduleOID returns the commit a gitlink points to; "" if p is not a gitlink of this tree.
func (t *CommitTree) SubmoduleOID(p string) string { return t.subOIDs[p] }

// EntryOID returns the blob OID of entry p, not resolved through links (a symbolic link: the blob holding its target); "" if absent.
func (t *CommitTree) EntryOID(p string) string { return t.ents[p].oid }

// Key is the blob OID of the file p reads (through a symbolic link: of its target file); "" if absent.
func (t *CommitTree) Key(p string) string {
	r, ok := t.Real(p)
	if !ok {
		return ""
	}
	return t.ents[r].oid
}

// Read reads a blob (through a symbolic link: its target file's). tree.Tree has no ctx; Repo.Close can interrupt a hung request.
func (t *CommitTree) Read(p string) ([]byte, error) { return t.ReadContext(context.Background(), p) }

// ReadContext is like Read but honors ctx (cancelling ctx kills the cat-file process; the next read restarts it).
func (t *CommitTree) ReadContext(ctx context.Context, p string) ([]byte, error) {
	r, ok := t.Real(p)
	if !ok {
		return nil, notExist("read", p)
	}
	e, ok := t.ents[r]
	if !ok {
		return nil, notExist("read", p)
	}
	return t.r.cat.get(ctx, e.oid)
}

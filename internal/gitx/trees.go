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

	"bgraph/internal/tree"
)

var (
	_ tree.Tree = (*WorkTree)(nil)
	_ tree.Tree = (*CommitTree)(nil)
)

func joinRoot(root, p string) string { return filepath.Join(root, filepath.FromSlash(p)) }

// pathSet giữ phần dùng chung: danh sách tệp đã sắp xếp, tập thư mục tổ tiên, bảng tra không phân biệt hoa thường.
type pathSet struct {
	files []string
	dirs  map[string]bool
	fold  map[string][]string
}

// newPathSet nhận danh sách (sẽ bị sắp xếp tại chỗ) các tệp không trùng.
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
		ps.fold[k] = append(ps.fold[k], p) // files đã sắp xếp nên mỗi danh sách cũng sắp xếp
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
	dirty     bool // khoá phải lấy từ đĩa, không dùng oid của index
	once      sync.Once
	key       string
}

// WorkTree là cây làm việc: mục index (stage 0; đường dẫn xung đột gồm một lần) ∪ untracked không bị ignore, trừ tệp đã
// xoá khỏi đĩa. Gitlink không nằm trong Files mà ở Submodules().
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
			// Stage 0 thắng; nếu không thì giữ mục đầu tiên (stage thấp nhất).
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
	// Phòng thủ: diff báo 'D' nhưng ls-files không báo 'R' → kiểm tra đĩa.
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

// Mode trả mode git của tệp ("100644", "100755", "120000"); untracked luôn "100644"; "" nếu không có.
func (t *WorkTree) Mode(p string) string {
	if e, ok := t.ents[p]; ok {
		return e.mode
	}
	return ""
}

// Submodules trả đường dẫn gitlink (mode 160000) và thư mục untracked là repo git lồng; đã sắp xếp.
func (t *WorkTree) Submodules() []string { return t.subs }

// Read đọc tệp từ đĩa. Với tệp "dirty" cũng tính và nhớ khoá luôn.
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

// Key: tệp tracked không đổi so với index → blob OID của index; tệp bị sửa (kể cả staged-rồi-hoàn-nguyên), chưa merge hay
// untracked → BlobKey của byte trên đĩa (tính lười, nhớ lại). "" nếu không có tệp hoặc không đọc được đĩa.
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

// CommitTree là cây của một commit, đọc blob qua cat-file --batch của Repo.
type CommitTree struct {
	pathSet
	r    *Repo
	ents map[string]ctEntry
	subs []string
}

// CommitTree dựng cây của rev bằng `git ls-tree -r -z --full-tree`.
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
			return nil, fmt.Errorf("gitx: ls-tree: bản ghi sai dạng %q", rec)
		}
		f := strings.Fields(string(rec[:tab]))
		if len(f) != 3 {
			return nil, fmt.Errorf("gitx: ls-tree: siêu dữ liệu sai dạng %q", rec[:tab])
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

// Mode trả mode git của tệp trong commit; "" nếu không có.
func (t *CommitTree) Mode(p string) string { return t.ents[p].mode }

// Submodules trả đường dẫn gitlink của commit; đã sắp xếp.
func (t *CommitTree) Submodules() []string { return t.subs }

// Key là blob OID.
func (t *CommitTree) Key(p string) string { return t.ents[p].oid }

// Read đọc blob (symlink: văn bản đích của liên kết). Không có ctx trong tree.Tree; Repo.Close ngắt được yêu cầu treo.
func (t *CommitTree) Read(p string) ([]byte, error) {
	e, ok := t.ents[p]
	if !ok {
		return nil, notExist("read", p)
	}
	return t.r.cat.get(context.Background(), e.oid)
}

// ReadContext như Read nhưng tôn trọng ctx (huỷ ctx giết tiến trình cat-file; lần đọc sau khởi động lại).
func (t *CommitTree) ReadContext(ctx context.Context, p string) ([]byte, error) {
	e, ok := t.ents[p]
	if !ok {
		return nil, notExist("read", p)
	}
	return t.r.cat.get(ctx, e.oid)
}

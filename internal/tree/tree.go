// Package tree định nghĩa cây tệp mà bộ phân giải và bộ dựng đồ thị đọc: cây làm việc hoặc cây của một commit.
// Cài đặt dựa trên git nằm ở internal/gitx; cài đặt trong bộ nhớ (MapTree) dùng cho test.
package tree

import (
	"io/fs"
	"path"
	"slices"
	"strings"
)

// Tree là ảnh chụp chỉ đọc của tập tệp của repo (tracked + untracked không bị ignore). Đường dẫn theo quy ước của
// model: tương đối gốc, gạch xuôi, phân biệt hoa thường theo index của git.
type Tree interface {
	// Files trả mọi tệp, đã sắp xếp. Không chứa thư mục.
	Files() []string
	// Has: p là một tệp trong cây (so chính xác, phân biệt hoa thường).
	Has(p string) bool
	// IsDir: có ít nhất một tệp dưới p/.
	IsDir(p string) bool
	// Read đọc nội dung (cây commit: blob; cây làm việc: đĩa). Lỗi fs.ErrNotExist nếu không có.
	Read(p string) ([]byte, error)
	// Key là khoá nội dung ổn định (blob OID của git hoặc băm kiểu blob) để cache kết quả quét.
	Key(p string) string
	// FoldLookup trả các tệp khớp p khi bỏ qua hoa thường (để báo case_mismatch).
	FoldLookup(p string) []string
}

// MapTree là Tree trong bộ nhớ, dùng cho test và fixture.
type MapTree struct {
	files map[string]string
	list  []string
	dirs  map[string]bool
	fold  map[string][]string
}

// NewMapTree dựng cây từ map đường dẫn → nội dung.
func NewMapTree(files map[string]string) *MapTree {
	t := &MapTree{files: map[string]string{}, dirs: map[string]bool{}, fold: map[string][]string{}}
	for p, c := range files {
		p = path.Clean(strings.ReplaceAll(p, "\\", "/"))
		t.files[p] = c
		t.list = append(t.list, p)
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			t.dirs[d] = true
		}
		t.dirs["."] = true
		k := strings.ToLower(p)
		t.fold[k] = append(t.fold[k], p)
	}
	slices.Sort(t.list)
	for _, v := range t.fold {
		slices.Sort(v)
	}
	return t
}

func (t *MapTree) Files() []string { return t.list }
func (t *MapTree) Has(p string) bool {
	_, ok := t.files[p]
	return ok
}
func (t *MapTree) IsDir(p string) bool { return t.dirs[p] }
func (t *MapTree) Read(p string) ([]byte, error) {
	c, ok := t.files[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(c), nil
}
func (t *MapTree) Key(p string) string {
	c, ok := t.files[p]
	if !ok {
		return ""
	}
	return BlobKey([]byte(c))
}
func (t *MapTree) FoldLookup(p string) []string { return t.fold[strings.ToLower(p)] }

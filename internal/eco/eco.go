// Package eco định nghĩa giao diện của một hệ sinh thái (node, python, rust...) đối với bộ chọn (mục 4.1, 4.3).
//
// Một hệ sinh thái phân tích MỘT cây (mốc hoặc hiện tại) thành Analysis; bộ chọn lấy hợp Analysis của hai cây.
// Nó cũng phân loại tệp đổi là manifest/cấu hình của nó (nới theo luật riêng) dựa trên cả hai cây.
package eco

import (
	"context"
	"path"
	"strings"

	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// Project là một dự án của hệ sinh thái (package.json, crate, module...). Dir là thư mục gốc dự án ("." là gốc repo).
type Project struct {
	Eco  string `json:"eco"`
	Dir  string `json:"dir"`
	Name string `json:"name,omitempty"`
}

// Analysis là kết quả phân tích một cây.
type Analysis struct {
	Projects   []Project
	Roles      map[string]model.Role // tệp do hệ sinh thái này nhận (source, test, config, manifest, lock, data)
	Edges      []model.Edge          // From phụ thuộc To; Kind=pattern thì To là glob đánh giá trên cả hai cây
	Unresolved []model.Unresolved
	Tests      []model.TestUnit
	// Warnings không đổi phạm vi (case_mismatch...); Path là tệp chứa vấn đề.
	Warnings []model.Widening
}

// Scanner trả kết quả quét import của một tệp, có cache theo khoá nội dung.
type Scanner interface {
	Scan(t tree.Tree, p string) (model.ScanResult, error)
}

// Ecosystem là một hệ sinh thái.
type Ecosystem interface {
	Name() string
	// Version đổi khi luật phân tích đổi (vô hiệu snapshot phân tích đã lưu).
	Version() string
	// Analyze phân tích cây t. Lỗi trả về làm bộ chọn nới full (internal_error); lỗi cục bộ nên thành Unresolved.
	Analyze(ctx context.Context, t tree.Tree, sc Scanner) (*Analysis, error)
	// ClassifyChange: p (đã đổi giữa base và head) là manifest/cấu hình/lockfile của hệ sinh thái này? Trả các lần nới và
	// true nếu đã nhận tệp. base có thể nil (không có mốc).
	ClassifyChange(p string, base, head tree.Tree, a *Analysis) ([]model.Widening, bool)
}

// Within: p nằm trong thư mục dir (dir "." hoặc "" chứa mọi thứ).
func Within(p, dir string) bool {
	if dir == "" || dir == "." {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// NearestProject trả dự án sâu nhất chứa p.
func NearestProject(projects []Project, p string) (Project, bool) {
	best, ok := Project{}, false
	for _, pr := range projects {
		if Within(p, pr.Dir) && (!ok || depth(pr.Dir) > depth(best.Dir)) {
			best, ok = pr, true
		}
	}
	return best, ok
}

func depth(dir string) int {
	if dir == "." || dir == "" {
		return 0
	}
	return strings.Count(path.Clean(dir), "/") + 1
}

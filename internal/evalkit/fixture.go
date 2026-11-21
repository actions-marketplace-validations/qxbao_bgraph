package evalkit

import (
	"path"
	"strings"
)

// FixtureReq ghi một yêu cầu dạng fixture (dữ liệu do runner test đọc, không phải đơn vị test) để kiểm toán oracle.
type FixtureReq struct {
	File        string `json:"file"`
	Root        string `json:"test_root"`
	SatisfiedBy string `json:"satisfied_by,omitempty"` // đơn vị test thoả; rỗng = không thoả
}

// SelUnit là một đơn vị test được chọn (tách khỏi model để oracle độc lập).
type SelUnit struct {
	Unit  string
	Files []string
}

var (
	fixtureDirs  = map[string]bool{"fixtures": true, "__fixtures__": true, "__testfixtures__": true, "fixture": true}
	testRootDirs = map[string]bool{"test": true, "tests": true, "__tests__": true, "spec": true}
)

// fixtureRoot: nếu f nằm dưới thư mục fixture thì trả (thư mục fixtures ngoài cùng, test root, true).
// Test root = tổ tiên gần nhất của thư mục fixtures tên test/tests/__tests__/spec; không có thì cha của thư mục fixtures.
func fixtureRoot(f string) (fixDir, root string, ok bool) {
	segs := strings.Split(path.Clean(strings.ReplaceAll(f, "\\", "/")), "/")
	for i := 0; i < len(segs)-1; i++ { // đoạn cuối là tên tệp, không tính
		if !fixtureDirs[segs[i]] {
			continue
		}
		fixDir = strings.Join(segs[:i+1], "/")
		for j := i - 1; j >= 0; j-- {
			if testRootDirs[segs[j]] {
				return fixDir, strings.Join(segs[:j+1], "/"), true
			}
		}
		return fixDir, strings.Join(segs[:i], "/"), true
	}
	return "", "", false
}

func under(p, dir string) bool {
	if dir == "" {
		return true
	}
	return strings.HasPrefix(p, dir+"/")
}

// CheckRequired trả các tệp bắt buộc còn thiếu và danh sách yêu cầu fixture.
// Yêu cầu thường: phải nằm trong Files (hoặc là ID) của một đơn vị được chọn.
// Yêu cầu fixture: thoả khi có đơn vị được chọn có tệp trong cây test root (mọi độ sâu, trừ chính thư mục fixtures).
func CheckRequired(req []string, sel []SelUnit) (missing []string, fixtures []FixtureReq) {
	have := map[string]bool{}
	for _, u := range sel {
		have[u.Unit] = true
		for _, f := range u.Files {
			have[f] = true
		}
	}
	for _, f := range req {
		fixDir, root, ok := fixtureRoot(f)
		if !ok {
			if !have[f] {
				missing = append(missing, f)
			}
			continue
		}
		fr := FixtureReq{File: f, Root: root}
	search:
		for _, u := range sel {
			for _, uf := range u.Files {
				uf = strings.ReplaceAll(uf, "\\", "/")
				if under(uf, root) && !under(uf, fixDir) {
					fr.SatisfiedBy = u.Unit
					break search
				}
			}
		}
		fixtures = append(fixtures, fr)
		if fr.SatisfiedBy == "" {
			missing = append(missing, f)
		}
	}
	return
}

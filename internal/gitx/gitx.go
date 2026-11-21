// Package gitx là lớp vỏ mỏng, nhanh quanh git CLI. Mỗi lần spawn git.exe trên Windows tốn ~20–25 ms nên đường nóng
// (Snapshot) chỉ dùng 3 tiến trình chạy song song, cộng một `git cat-file --batch` sống lâu để đọc blob của commit mốc.
// Không dùng shell, không dùng go-git, không cgo.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Repo là một kho git đã mở. An toàn khi dùng đồng thời từ nhiều goroutine.
type Repo struct {
	// Root là thư mục gốc làm việc, tuyệt đối, dạng của hệ điều hành.
	Root string

	spawns atomic.Int64
	cat    catFile
}

// Open tìm gốc làm việc của repo chứa dir (git rev-parse --show-toplevel).
func Open(ctx context.Context, dir string) (*Repo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	r := &Repo{Root: abs}
	r.cat.r = r
	out, err := r.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	top := strings.TrimRight(string(out), "\r\n")
	if top == "" {
		return nil, fmt.Errorf("gitx: %s không có thư mục làm việc (repo bare?)", dir)
	}
	r.Root = filepath.Clean(filepath.FromSlash(top))
	return r, nil
}

// Close dừng tiến trình cat-file nếu có. Gọi nhiều lần được.
func (r *Repo) Close() error { return r.cat.close() }

// Spawns trả số tiến trình git đã khởi chạy qua Repo này (dùng cho test hiệu năng).
func (r *Repo) Spawns() int64 { return r.spawns.Load() }

// gitEnv: kế thừa môi trường, tắt khoá tuỳ chọn (không ghi lại index khi chỉ đọc) và không hỏi mật khẩu.
// Không đổi locale: việc parse không phụ thuộc thông điệp.
func gitEnv() []string {
	return append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
}

func baseArgs(args []string) []string {
	return append([]string{"-c", "core.quotepath=off"}, args...)
}

func describe(args []string) string {
	s := "git " + strings.Join(args, " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// run chạy một lệnh git và trả stdout. Lỗi kèm lệnh và stderr đã cắt khoảng trắng.
func (r *Repo) run(ctx context.Context, args ...string) ([]byte, error) {
	out, _, err := r.runRaw(ctx, args...)
	return out, err
}

// runRaw như run nhưng trả thêm stderr (để người gọi phân biệt mã thoát 1 "không có kết quả").
func (r *Repo) runRaw(ctx context.Context, args ...string) (stdout, stderr []byte, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, "git", baseArgs(args)...)
	cmd.Dir = r.Root
	cmd.Env = gitEnv()
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	r.spawns.Add(1)
	if err := cmd.Run(); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			err = cerr
		}
		return nil, se.Bytes(), fmt.Errorf("gitx: %s: %w: %s", describe(args), err, strings.TrimSpace(se.String()))
	}
	return so.Bytes(), se.Bytes(), nil
}

// RevParse phân giải rev thành OID commit đầy đủ.
func (r *Repo) RevParse(ctx context.Context, rev string) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// MergeBase trả merge-base của a và b; lỗi nếu không có.
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	out, se, err := r.runRaw(ctx, "merge-base", "--end-of-options", a, b)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 && len(bytes.TrimSpace(se)) == 0 {
			return "", fmt.Errorf("gitx: không có merge-base giữa %q và %q", a, b)
		}
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("gitx: không có merge-base giữa %q và %q", a, b)
	}
	return s, nil
}

// IsShallow cho biết repo là clone nông (lịch sử bị cắt).
func (r *Repo) IsShallow(ctx context.Context) (bool, error) {
	out, err := r.run(ctx, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(string(out)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("gitx: đầu ra --is-shallow-repository không hiểu được: %q", out)
}

// isHexOID: chuỗi là OID đầy đủ (sha1 40 hoặc sha256 64 ký tự hex thường).
func isHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

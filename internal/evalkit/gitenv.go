// Package evalkit là bộ đánh giá ngoài của bgraph (mục 10.2 của docs/plan.md): oracle cứng T1 (FAIL_TO_PASS) và phát lại
// lịch sử commit kèm bất biến RTSCheck. Oracle KHÔNG phụ thuộc bgraph: nó chỉ dùng git và nội dung tệp.
package evalkit

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SetupHermeticEnv đặt môi trường git kín cho cả tiến trình này lẫn mọi git mà bgraph khởi chạy: không đọc cấu hình hệ thống/
// người dùng, tác giả cố định, core.autocrlf=false, core.longpaths=true, không hỏi mật khẩu. emptyCfgDir chứa tệp cấu hình rỗng.
func SetupHermeticEnv(emptyCfgDir string) error {
	if err := os.MkdirAll(emptyCfgDir, 0o755); err != nil {
		return err
	}
	cfg := filepath.Join(emptyCfgDir, "empty.gitconfig")
	if err := os.WriteFile(cfg, nil, 0o644); err != nil {
		return err
	}
	kv := map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   cfg,
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_AUTHOR_NAME":     "bgraph-eval",
		"GIT_AUTHOR_EMAIL":    "eval@example.invalid",
		"GIT_AUTHOR_DATE":     "2020-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME":  "bgraph-eval",
		"GIT_COMMITTER_EMAIL": "eval@example.invalid",
		"GIT_COMMITTER_DATE":  "2020-01-01T00:00:00Z",
		"GIT_CONFIG_COUNT":    "3",
		"GIT_CONFIG_KEY_0":    "core.autocrlf",
		"GIT_CONFIG_VALUE_0":  "false",
		"GIT_CONFIG_KEY_1":    "core.longpaths",
		"GIT_CONFIG_VALUE_1":  "true",
		"GIT_CONFIG_KEY_2":    "gc.auto",
		"GIT_CONFIG_VALUE_2":  "0",
	}
	for k, v := range kv {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

// git chạy git trong dir và trả stdout. Lỗi chứa stderr.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 600 {
			msg = msg[:600]
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out.String(), nil
}

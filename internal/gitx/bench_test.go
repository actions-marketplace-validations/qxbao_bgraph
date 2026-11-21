package gitx

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

var (
	benchOnce sync.Once
	benchDir  string
	benchBase string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if benchDir != "" {
		os.RemoveAll(benchDir)
	}
	os.Exit(code)
}

// benchRepo sinh repo ~10.000 tệp (100 thư mục x 100 tệp) đúng một lần cho cả lần chạy benchmark (testing gọi lại hàm
// benchmark nhiều lần khi hiệu chỉnh N), commit, rồi sửa 5 tệp và thêm 1 tệp untracked.
func benchRepo(b *testing.B) (*Repo, string) {
	b.Helper()
	benchOnce.Do(func() {
		hermetic(b) // t.Setenv sẽ bị gỡ khi benchmark đầu kết thúc; đặt lại bằng os.Setenv bên dưới
		for _, k := range []string{"GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "HOME", "USERPROFILE", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
			benchEnv = append(benchEnv, k+"="+os.Getenv(k))
		}
		dir, err := os.MkdirTemp("", "gitx-bench-")
		if err != nil {
			b.Fatal(err)
		}
		benchDir = dir
		git(b, dir, "init", "-q", "-b", "main")
		git(b, dir, "config", "core.autocrlf", "false")
		for d := 0; d < 100; d++ {
			for f := 0; f < 100; f++ {
				write(b, dir, fmt.Sprintf("pkg%03d/file%03d.ts", d, f), fmt.Sprintf("export const v = %d_%d;\n", d, f))
			}
		}
		benchBase = commitAll(b, dir, "base")
		for i := 0; i < 5; i++ {
			write(b, dir, fmt.Sprintf("pkg000/file%03d.ts", i), "changed\n")
		}
		write(b, dir, "pkg001/new.ts", "new\n")
	})
	for _, kv := range benchEnv {
		k, v, _ := strings.Cut(kv, "=")
		b.Setenv(k, v)
	}
	r, err := Open(context.Background(), benchDir)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { r.Close() })
	return r, benchBase
}

var benchEnv []string

func BenchmarkSnapshot(b *testing.B) {
	r, base := benchRepo(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ch, wt, err := r.Snapshot(ctx, base)
		if err != nil {
			b.Fatal(err)
		}
		if len(ch.Files) != 6 || len(wt.Files()) != 10001 {
			b.Fatalf("changes=%d files=%d", len(ch.Files), len(wt.Files()))
		}
	}
}

func BenchmarkCommitTreeRead1000(b *testing.B) {
	r, _ := benchRepo(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ct, err := r.CommitTree(ctx, "HEAD")
		if err != nil {
			b.Fatal(err)
		}
		for _, p := range ct.Files()[:1000] {
			if _, err := ct.Read(p); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkCommitTreeOnly(b *testing.B) {
	r, _ := benchRepo(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.CommitTree(ctx, "HEAD"); err != nil {
			b.Fatal(err)
		}
	}
}

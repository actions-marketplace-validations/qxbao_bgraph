package evalkit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func tgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func writeF(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestIndexBuilder: P và C dựng bằng index tạm phải có đúng cây của commit tham chiếu (tệp mới có mode thực thi, đổi tên, nhị phân),
// P có cha là base và cùng OID với `git commit` cũ; cây làm việc không bị đụng tới.
func TestIndexBuilder(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("không có git")
	}
	if err := SetupHermeticEnv(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	tgit(t, repo, "init", "-q")
	bin1 := []byte{0, 1, 2, 3, 255, 254, 0, 7}
	writeF(t, repo, "a.txt", []byte("alpha\nbeta\ngamma\ndelta\nepsilon\nzeta\neta\ntheta\n"))
	writeF(t, repo, "src/b.js", []byte("module.exports = 1\n"))
	writeF(t, repo, "img.bin", bin1)
	tgit(t, repo, "add", "-A")
	tgit(t, repo, "commit", "-q", "-m", "base")
	base := tgit(t, repo, "rev-parse", "HEAD")

	// T = base + test patch: thêm tệp test (mode 755) và một tệp nhị phân mới.
	writeF(t, repo, "test/new.test.js", []byte("it('x', () => {})\n"))
	if err := os.Chmod(filepath.Join(repo, "test/new.test.js"), 0o755); err != nil {
		t.Fatal(err)
	}
	tgit(t, repo, "update-index", "--add", "--chmod=+x", "test/new.test.js")
	writeF(t, repo, "test/snap.bin", []byte{9, 0, 9, 0, 1})
	tgit(t, repo, "add", "-A")
	tgit(t, repo, "commit", "-q", "-m", "T")
	tc := tgit(t, repo, "rev-parse", "HEAD")
	// G = T + patch vàng: sửa b.js, đổi tên a.txt (có sửa nhẹ), sửa tệp nhị phân.
	writeF(t, repo, "src/b.js", []byte("module.exports = 2\n"))
	os.MkdirAll(filepath.Join(repo, "renamed"), 0o755)
	tgit(t, repo, "mv", "a.txt", "renamed/c.txt")
	writeF(t, repo, "renamed/c.txt", []byte("alpha\nbeta\ngamma\ndelta\nepsilon\nzeta\neta\nTHETA\n"))
	writeF(t, repo, "img.bin", []byte{0, 1, 2, 3, 4, 5, 6, 7, 8})
	tgit(t, repo, "add", "-A")
	tgit(t, repo, "commit", "-q", "-m", "G")
	gc := tgit(t, repo, "rev-parse", "HEAD")

	testPatch, err := git(context.Background(), repo, "diff", "--binary", "-M", base, tc)
	if err != nil {
		t.Fatal(err)
	}
	gold, err := git(context.Background(), repo, "diff", "--binary", "-M", tc, gc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(testPatch, "new file mode 100755") || !strings.Contains(testPatch, "GIT binary patch") ||
		!strings.Contains(gold, "rename from a.txt") || !strings.Contains(gold, "GIT binary patch") {
		t.Fatalf("patch thử nghiệm thiếu thành phần:\n%s\n%s", testPatch, gold)
	}
	// Đưa cây làm việc về base để chắc chắn dựng P/C không phụ thuộc cây làm việc.
	tgit(t, repo, "checkout", "-q", base)
	before := tgit(t, repo, "status", "--porcelain")

	ib := newIndexBuilder(repo, filepath.Join(t.TempDir(), "x.index"), base)
	defer ib.Close()
	ctx := context.Background()
	p, err := ib.Apply(ctx, testPatch, "eval: test patch")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tgit(t, repo, "rev-parse", p+"^{tree}"), tgit(t, repo, "rev-parse", tc+"^{tree}"); got != want {
		t.Fatalf("cây P %s != cây T %s", got, want)
	}
	if par := tgit(t, repo, "rev-parse", p+"^"); par != base {
		t.Fatalf("cha của P = %s, muốn %s", par, base)
	}
	if mode := tgit(t, repo, "ls-tree", p, "test/new.test.js"); !strings.HasPrefix(mode, "100755") {
		t.Fatalf("mất mode thực thi: %s", mode)
	}
	c, err := ib.Apply(ctx, gold, "eval: gold patch")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tgit(t, repo, "rev-parse", c+"^{tree}"), tgit(t, repo, "rev-parse", gc+"^{tree}"); got != want {
		t.Fatalf("cây C %s != cây G %s", got, want)
	}
	if par := tgit(t, repo, "rev-parse", c+"^"); par != p {
		t.Fatalf("cha của C = %s, muốn P %s", par, p)
	}
	if after := tgit(t, repo, "status", "--porcelain"); after != before {
		t.Fatalf("cây làm việc bị đổi: %q -> %q", before, after)
	}
	if _, err := os.Stat(filepath.Join(repo, "test")); err == nil {
		t.Fatal("tệp test xuất hiện trong cây làm việc")
	}

	// Lối cũ (áp lên cây làm việc rồi `git commit`) cho cùng nội dung; riêng mode thực thi của tệp mới có thể mất trên Windows
	// (core.fileMode=false) nên so theo (đường dẫn, blob) chứ không so OID cây.
	tgit(t, repo, "apply", "--binary", "--whitespace=nowarn", writePatch(t, testPatch))
	tgit(t, repo, "add", "-A")
	tgit(t, repo, "commit", "-q", "--no-verify", "--allow-empty", "-m", "eval: test patch")
	strip := func(c string) string {
		var out []string
		for _, l := range strings.Split(tgit(t, repo, "ls-tree", "-r", c), "\n") {
			if i := strings.Index(l, " "); i >= 0 {
				out = append(out, l[i+1:])
			}
		}
		return strings.Join(out, "\n")
	}
	if strip("HEAD") != strip(p) {
		t.Fatalf("nội dung P index khác lối worktree:\n%s\n---\n%s", strip(p), strip("HEAD"))
	}

	// Patch không áp được → lỗi (người gọi rơi về worktree).
	ib2 := newIndexBuilder(repo, filepath.Join(t.TempDir(), "y.index"), base)
	defer ib2.Close()
	if _, err := ib2.Apply(ctx, "--- a/nothere.txt\n+++ b/nothere.txt\n@@ -1 +1 @@\n-x\n+y\n", "m"); err == nil {
		t.Fatal("patch không áp được phải lỗi")
	}
	if _, err := ib2.Apply(ctx, "", "m"); err == nil {
		t.Fatal("patch rỗng phải lỗi")
	}
}

func writePatch(t *testing.T, s string) string {
	p := filepath.Join(t.TempDir(), "p.patch")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

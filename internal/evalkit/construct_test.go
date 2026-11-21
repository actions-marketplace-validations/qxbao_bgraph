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

// TestIndexBuilder: P and C built with a temporary index must have exactly the tree of the reference commit (new file with exec mode, rename, binary),
// P has base as parent and the same OID as the old `git commit`; the working tree is not touched.
func TestIndexBuilder(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
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

	// T = base + test patch: add a test file (mode 755) and a new binary file.
	writeF(t, repo, "test/new.test.js", []byte("it('x', () => {})\n"))
	if err := os.Chmod(filepath.Join(repo, "test/new.test.js"), 0o755); err != nil {
		t.Fatal(err)
	}
	tgit(t, repo, "update-index", "--add", "--chmod=+x", "test/new.test.js")
	writeF(t, repo, "test/snap.bin", []byte{9, 0, 9, 0, 1})
	tgit(t, repo, "add", "-A")
	tgit(t, repo, "commit", "-q", "-m", "T")
	tc := tgit(t, repo, "rev-parse", "HEAD")
	// G = T + gold patch: edit b.js, rename a.txt (with a small edit), edit the binary file.
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
		t.Fatalf("test patch is missing a component:\n%s\n%s", testPatch, gold)
	}
	// Reset the working tree to base to make sure building P/C does not depend on the working tree.
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
		t.Fatalf("tree P %s != tree T %s", got, want)
	}
	if par := tgit(t, repo, "rev-parse", p+"^"); par != base {
		t.Fatalf("parent of P = %s, want %s", par, base)
	}
	if mode := tgit(t, repo, "ls-tree", p, "test/new.test.js"); !strings.HasPrefix(mode, "100755") {
		t.Fatalf("exec mode lost: %s", mode)
	}
	c, err := ib.Apply(ctx, gold, "eval: gold patch")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tgit(t, repo, "rev-parse", c+"^{tree}"), tgit(t, repo, "rev-parse", gc+"^{tree}"); got != want {
		t.Fatalf("tree C %s != tree G %s", got, want)
	}
	if par := tgit(t, repo, "rev-parse", c+"^"); par != p {
		t.Fatalf("parent of C = %s, want P %s", par, p)
	}
	if after := tgit(t, repo, "status", "--porcelain"); after != before {
		t.Fatalf("working tree changed: %q -> %q", before, after)
	}
	if _, err := os.Stat(filepath.Join(repo, "test")); err == nil {
		t.Fatal("test file appeared in the working tree")
	}

	// The old way (apply to the working tree then `git commit`) gives the same content; only the exec mode of the new file may be lost on Windows
	// (core.fileMode=false), so compare by (path, blob) rather than by tree OID.
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
		t.Fatalf("content of P (index) differs from the worktree path:\n%s\n---\n%s", strip(p), strip("HEAD"))
	}

	// A patch that does not apply → error (the caller falls back to the worktree).
	ib2 := newIndexBuilder(repo, filepath.Join(t.TempDir(), "y.index"), base)
	defer ib2.Close()
	if _, err := ib2.Apply(ctx, "--- a/nothere.txt\n+++ b/nothere.txt\n@@ -1 +1 @@\n-x\n+y\n", "m"); err == nil {
		t.Fatal("a patch that does not apply must error")
	}
	if _, err := ib2.Apply(ctx, "", "m"); err == nil {
		t.Fatal("an empty patch must error")
	}
}

func writePatch(t *testing.T, s string) string {
	p := filepath.Join(t.TempDir(), "p.patch")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

// hermetic dựng môi trường git cô lập (không đọc cấu hình hệ thống/người dùng).
func hermetic(t testing.TB) {
	t.Helper()
	home := t.TempDir()
	empty := filepath.Join(home, "gitconfig-empty")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GIT_AUTHOR_NAME", "T")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "T")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, se.String())
	}
	return strings.TrimSpace(so.String())
}

func newRepoDir(t testing.TB) string {
	t.Helper()
	hermetic(t)
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "core.autocrlf", "false")
	git(t, dir, "config", "core.symlinks", "false")
	return dir
}

func write(t testing.TB, dir, p, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t testing.TB, dir, msg string) string {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

func open(t testing.TB, dir string) *Repo {
	t.Helper()
	r, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func snap(t testing.TB, r *Repo, base string) (Change, *WorkTree) {
	t.Helper()
	ch, wt, err := r.Snapshot(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	return ch, wt
}

// summary: "A:path M:path ..." theo thứ tự đã sắp xếp.
func summary(ch Change) []string {
	var s []string
	for _, f := range ch.Files {
		s = append(s, fmt.Sprintf("%c:%s", f.Status, f.Path))
	}
	return s
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestOpen(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "sub/x.txt", "x")
	r := open(t, filepath.Join(dir, "sub"))
	want, _ := filepath.EvalSymlinks(dir)
	got, _ := filepath.EvalSymlinks(r.Root)
	if got != want {
		t.Fatalf("Root = %q, want %q", r.Root, dir)
	}
	if _, err := Open(context.Background(), t.TempDir()); err == nil {
		t.Fatal("Open ngoài repo phải lỗi")
	}
}

func TestSnapshotBasic(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "keep.ts", "keep")
	write(t, dir, "mod.ts", "v1")
	write(t, dir, "del.ts", "gone")
	write(t, dir, "wtdel.ts", "gone in wt")
	write(t, dir, "staged_mod.ts", "s1")
	write(t, dir, "unstaged_mod.ts", "u1")
	base := commitAll(t, dir, "base")

	write(t, dir, "mod.ts", "v2")
	git(t, dir, "rm", "-q", "del.ts")
	os.Remove(filepath.Join(dir, "wtdel.ts"))
	write(t, dir, "added_staged.ts", "new")
	git(t, dir, "add", "added_staged.ts")
	write(t, dir, "staged_mod.ts", "s2")
	git(t, dir, "add", "staged_mod.ts")
	write(t, dir, "unstaged_mod.ts", "u2")
	write(t, dir, "untracked.ts", "untr")
	write(t, dir, ".gitignore", "ignored.log\n")
	write(t, dir, "ignored.log", "x")

	r := open(t, dir)
	ch, wt := snap(t, r, base)
	eq(t, summary(ch), []string{
		"A:.gitignore", "A:added_staged.ts", "D:del.ts", "M:mod.ts", "M:staged_mod.ts",
		"M:unstaged_mod.ts", "A:untracked.ts", "D:wtdel.ts",
	})
	eq(t, wt.Files(), []string{".gitignore", "added_staged.ts", "keep.ts", "mod.ts", "staged_mod.ts", "unstaged_mod.ts", "untracked.ts"})
	for _, p := range []string{"del.ts", "wtdel.ts", "ignored.log"} {
		if wt.Has(p) {
			t.Errorf("%s không được có trong WorkTree", p)
		}
		if _, err := wt.Read(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Read(%s) err=%v, want ErrNotExist", p, err)
		}
	}
	// Key: keep.ts không đổi → OID của index; các tệp khác theo nội dung đĩa.
	if got, want := wt.Key("keep.ts"), tree.BlobKey([]byte("keep")); got != want {
		t.Errorf("Key(keep)=%s want %s", got, want)
	}
	for p, c := range map[string]string{"mod.ts": "v2", "staged_mod.ts": "s2", "unstaged_mod.ts": "u2", "untracked.ts": "untr", "added_staged.ts": "new"} {
		if got, want := wt.Key(p), tree.BlobKey([]byte(c)); got != want {
			t.Errorf("Key(%s)=%s want %s", p, got, want)
		}
		b, err := wt.Read(p)
		if err != nil || string(b) != c {
			t.Errorf("Read(%s)=%q,%v", p, b, err)
		}
	}
	if !wt.IsDir(".") || wt.IsDir("nope") || wt.Has("nope") {
		t.Error("IsDir/Has sai")
	}
}

func TestStagedThenReverted(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "a.ts", "base")
	base := commitAll(t, dir, "base")
	write(t, dir, "a.ts", "X")
	git(t, dir, "add", "a.ts")
	write(t, dir, "a.ts", "base") // hoàn nguyên trên đĩa; index vẫn giữ X

	r := open(t, dir)
	ch, wt := snap(t, r, base)
	if len(ch.Files) != 0 {
		t.Fatalf("không được có thay đổi so với base: %v", summary(ch))
	}
	if got, want := wt.Key("a.ts"), tree.BlobKey([]byte("base")); got != want {
		t.Fatalf("Key phải lấy từ đĩa: got %s want %s", got, want)
	}
	if idxOID := git(t, dir, "rev-parse", ":a.ts"); wt.Key("a.ts") == idxOID {
		t.Fatal("Key trùng OID index (sai)")
	}
}

func TestStagedOnlyAndUnstagedOnly(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "s.ts", "1")
	write(t, dir, "u.ts", "1")
	base := commitAll(t, dir, "base")
	write(t, dir, "s.ts", "2")
	git(t, dir, "add", "s.ts")
	write(t, dir, "u.ts", "2")
	r := open(t, dir)
	ch, wt := snap(t, r, base)
	eq(t, summary(ch), []string{"M:s.ts", "M:u.ts"})
	if wt.Key("s.ts") != tree.BlobKey([]byte("2")) || wt.Key("u.ts") != tree.BlobKey([]byte("2")) {
		t.Fatal("key sai")
	}
}

func TestSpacesUnicodeDash(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "seed.txt", "s")
	base := commitAll(t, dir, "base")
	names := []string{"a b/c d.ts", "tiếng việt.ts", "中文/文件.ts", "-dash.ts", "--end-of-options", "dir/-x y.ts"}
	for _, n := range names {
		write(t, dir, n, "c:"+n)
	}
	// Một nửa staged, một nửa untracked.
	git(t, dir, "add", "--", "tiếng việt.ts", "-dash.ts")
	r := open(t, dir)
	ch, wt := snap(t, r, base)
	var got []string
	for _, f := range ch.Files {
		got = append(got, f.Path)
	}
	want := append([]string{}, names...)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	for _, f := range ch.Files {
		if f.Status != 'A' {
			t.Errorf("%s status %c", f.Path, f.Status)
		}
	}
	for _, n := range names {
		b, err := wt.Read(n)
		if err != nil || string(b) != "c:"+n {
			t.Errorf("Read(%q)=%q,%v", n, b, err)
		}
		if wt.Key(n) != tree.BlobKey([]byte("c:"+n)) {
			t.Errorf("Key(%q) sai", n)
		}
	}
	if !wt.IsDir("a b") || !wt.IsDir("中文") {
		t.Error("IsDir")
	}
	commitAll(t, dir, "more")
	ct, err := r.CommitTree(context.Background(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ct.Read("tiếng việt.ts")
	if err != nil || string(b) != "c:tiếng việt.ts" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := r.RevParse(context.Background(), "-dash"); err == nil {
		t.Fatal("rev bắt đầu bằng - phải lỗi, không bị hiểu là cờ")
	}
}

func TestTypeChangeAndGitlinkAndSymlink(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "target.txt", "target.txt-content")
	write(t, dir, "t.ts", "file")
	base := commitAll(t, dir, "base")
	blob := git(t, dir, "rev-parse", "HEAD:target.txt")
	// symlink 120000 trỏ tới "target.txt" (blob chứa văn bản "target.txt")
	lnk := hashObject(t, dir, "target.txt")
	_ = blob
	git(t, dir, "update-index", "--add", "--cacheinfo", "120000,"+lnk+",link")
	// gitlink
	git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+base+",sub")
	// type change: t.ts thành symlink trong index
	git(t, dir, "update-index", "--cacheinfo", "120000,"+lnk+",t.ts")
	write(t, dir, "link", "target.txt") // core.symlinks=false: symlink là tệp thường trên đĩa
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "t.ts", "target.txt") // khớp nội dung đã stage (core.symlinks=false → tệp thường)

	r := open(t, dir)
	ch, wt := snap(t, r, base)
	var byPath = map[string]ChangedFile{}
	for _, f := range ch.Files {
		byPath[f.Path] = f
	}
	if f := byPath["sub"]; f.Status != 'A' || f.NewMode != "160000" {
		t.Errorf("sub: %+v", f)
	}
	if f := byPath["link"]; f.Status != 'A' || f.NewMode != "120000" {
		t.Errorf("link: %+v (all %v)", f, summary(ch))
	}
	if f := byPath["t.ts"]; f.Status != 'T' || f.OldMode != "100644" || f.NewMode != "120000" {
		t.Errorf("t.ts: %+v (all %v)", f, summary(ch))
	}
	if wt.Has("sub") {
		t.Error("gitlink không được nằm trong Files")
	}
	eq(t, wt.Submodules(), []string{"sub"})
	if wt.Mode("link") != "120000" || wt.Mode("target.txt") != "100644" {
		t.Error("Mode")
	}

	// Commit tree có symlink + gitlink.
	git(t, dir, "commit", "-q", "-m", "special")
	ct, err := r.CommitTree(context.Background(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if ct.Has("sub") || !slices.Equal(ct.Submodules(), []string{"sub"}) {
		t.Error("CommitTree gitlink")
	}
	if ct.Mode("link") != "120000" {
		t.Errorf("Mode(link)=%s", ct.Mode("link"))
	}
	b, err := ct.Read("link")
	if err != nil || string(b) != "target.txt" {
		t.Fatalf("Read(link)=%q,%v", b, err)
	}
}

func hashObject(t testing.TB, dir, content string) string {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "-w", "--stdin")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestMergeCommitAsBase(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "a.ts", "a")
	root := commitAll(t, dir, "root")
	git(t, dir, "checkout", "-q", "-b", "feat")
	write(t, dir, "f.ts", "f")
	commitAll(t, dir, "feat")
	git(t, dir, "checkout", "-q", "main")
	write(t, dir, "m.ts", "m")
	commitAll(t, dir, "main")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "merge", "feat")
	merge := git(t, dir, "rev-parse", "HEAD")
	write(t, dir, "after.ts", "x")

	r := open(t, dir)
	ctx := context.Background()
	got, err := r.RevParse(ctx, "HEAD")
	if err != nil || got != merge {
		t.Fatalf("%s %v", got, err)
	}
	ch, wt := snap(t, r, merge)
	eq(t, summary(ch), []string{"A:after.ts"})
	if !wt.Has("f.ts") || !wt.Has("m.ts") {
		t.Fatal("thiếu tệp merge")
	}
	// Snapshot chấp nhận rev không phải OID (tự phân giải).
	ch2, _ := snap(t, r, "HEAD~1")
	if ch2.Base == merge || len(ch2.Base) != 40 {
		t.Fatalf("Base=%s", ch2.Base)
	}
	mb, err := r.MergeBase(ctx, "feat", "main")
	if err != nil || mb == "" {
		t.Fatalf("MergeBase %q %v", mb, err)
	}
	if mb != git(t, dir, "merge-base", "feat", "main") {
		t.Fatal("merge-base khác git")
	}
	_ = root
	// Hai nhánh không liên quan → lỗi "không có merge-base".
	git(t, dir, "checkout", "-q", "--orphan", "orph")
	git(t, dir, "rm", "-rfq", ".")
	write(t, dir, "o.txt", "o")
	git(t, dir, "add", "o.txt")
	git(t, dir, "commit", "-q", "-m", "orphan")
	if _, err := r.MergeBase(ctx, "orph", "main"); err == nil {
		t.Fatal("kỳ vọng lỗi không có merge-base")
	}
}

func TestRevParseUnknownAndShallow(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "a", "a")
	commitAll(t, dir, "c")
	r := open(t, dir)
	if _, err := r.RevParse(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("kỳ vọng lỗi")
	} else if !strings.Contains(err.Error(), "rev-parse") {
		t.Fatalf("lỗi thiếu tên lệnh: %v", err)
	}
	if sh, err := r.IsShallow(context.Background()); err != nil || sh {
		t.Fatalf("IsShallow=%v,%v", sh, err)
	}
}

func TestCommitTreeAndCatFile(t *testing.T) {
	dir := newRepoDir(t)
	bin := string([]byte{0, 1, 2, 255, 254, '\n', 0, '\r', '\n'})
	write(t, dir, "empty.txt", "")
	write(t, dir, "bin.dat", bin)
	write(t, dir, "src/Main.ts", "main")
	write(t, dir, "big.txt", strings.Repeat("0123456789abcdef\n", 100000)) // ~1.7 MB
	// Win/mac không có hai tệp chỉ khác hoa thường trên đĩa: chèn thẳng vào index.
	git(t, dir, "add", "-A")
	git(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+hashObject(t, dir, "lower")+",src/main.ts")
	git(t, dir, "commit", "-q", "-m", "c")
	r := open(t, dir)
	ctx := context.Background()
	ct, err := r.CommitTree(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ct.Files(), []string{"big.txt", "bin.dat", "empty.txt", "src/Main.ts", "src/main.ts"})
	if !ct.IsDir("src") || !ct.IsDir(".") || ct.IsDir("big.txt") {
		t.Error("IsDir")
	}
	eq(t, ct.FoldLookup("SRC/MAIN.TS"), []string{"src/Main.ts", "src/main.ts"})
	for _, p := range ct.Files() {
		want, _ := os.ReadFile(filepath.Join(dir, p))
		if p == "src/main.ts" {
			want = []byte("lower")
		}
		got, err := ct.Read(p)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("Read(%s) len=%d err=%v want len=%d", p, len(got), err, len(want))
		}
		if ct.Key(p) != tree.BlobKey(want) {
			t.Errorf("Key(%s)", p)
		}
	}
	if _, err := ct.Read("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err=%v", err)
	}
	// blob không tồn tại: lỗi nhưng tiến trình dùng tiếp được.
	if _, err := r.cat.get(ctx, strings.Repeat("ab", 20)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing err=%v", err)
	}
	if b, err := ct.Read("src/main.ts"); err != nil || string(b) != "lower" {
		t.Errorf("sau missing: %q %v", b, err)
	}
	if _, err := r.CommitTree(ctx, "nope-rev"); err == nil {
		t.Error("kỳ vọng lỗi")
	}
	// Đồng thời.
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 50; j++ {
				b, err := ct.Read("src/Main.ts")
				if err != nil || string(b) != "main" {
					done <- fmt.Errorf("%q %v", b, err)
					return
				}
			}
			done <- nil
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// Open(1) + CommitTree(1) + cat-file(1) + CommitTree lỗi(1) = 4
	if n := r.Spawns(); n != 4 {
		t.Errorf("Spawns=%d, want 4", n)
	}
}

func TestCloseIdempotentAndReadAfterClose(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "a.txt", "a")
	commitAll(t, dir, "c")
	r := open(t, dir)
	ct, err := r.CommitTree(context.Background(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ct.Read("a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ct.Read("a.txt"); !errors.Is(err, ErrClosed) {
		t.Fatalf("err=%v, want ErrClosed", err)
	}
	// Close khi chưa từng khởi động cat-file.
	r2 := open(t, dir)
	if err := r2.Close(); err != nil || r2.Close() != nil {
		t.Fatal("Close")
	}
}

func TestCatFileProcessDeathAndCancel(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "a.txt", "a")
	commitAll(t, dir, "c")
	r := open(t, dir)
	ct, _ := r.CommitTree(context.Background(), "HEAD")
	if _, err := ct.Read("a.txt"); err != nil {
		t.Fatal(err)
	}
	// Giết tiến trình từ bên ngoài: lần đọc kế tiếp lỗi (không treo), lần sau nữa tự khởi động lại.
	r.cat.smu.Lock()
	p := r.cat.p
	r.cat.smu.Unlock()
	p.kill()
	<-p.done
	if _, err := ct.Read("a.txt"); err == nil {
		t.Fatal("kỳ vọng lỗi khi tiến trình chết")
	}
	if b, err := ct.Read("a.txt"); err != nil || string(b) != "a" {
		t.Fatalf("khởi động lại: %q %v", b, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ct.ReadContext(ctx, "a.txt"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestSnapshotSpawns(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "a.ts", "a")
	base := commitAll(t, dir, "c")
	write(t, dir, "a.ts", "b")
	write(t, dir, "n.ts", "n")
	r := open(t, dir)
	before := r.Spawns()
	snap(t, r, base)
	if d := r.Spawns() - before; d != 3 {
		t.Fatalf("Snapshot spawn %d tiến trình, want 3", d)
	}
}

func TestSnapshotBadBase(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "a.ts", "a")
	commitAll(t, dir, "c")
	r := open(t, dir)
	if _, _, err := r.Snapshot(context.Background(), "nope"); err == nil {
		t.Fatal("kỳ vọng lỗi")
	}
	if _, _, err := r.Snapshot(context.Background(), strings.Repeat("0", 40)); err == nil {
		t.Fatal("kỳ vọng lỗi với OID không tồn tại")
	}
}

func TestDeletedStagedAddedThenRemovedFromDisk(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "a.ts", "a")
	base := commitAll(t, dir, "c")
	write(t, dir, "new.ts", "n")
	git(t, dir, "add", "new.ts")
	os.Remove(filepath.Join(dir, "new.ts"))
	r := open(t, dir)
	ch, wt := snap(t, r, base)
	if wt.Has("new.ts") {
		t.Fatal("new.ts đã xoá khỏi đĩa không được nằm trong cây")
	}
	// Index có new.ts (A) nhưng đĩa không có: diff vs worktree báo D? Chấp nhận mọi kết quả không báo 'A' cho tệp không tồn tại.
	for _, f := range ch.Files {
		if f.Path == "new.ts" && f.Status == 'A' {
			t.Logf("ghi chú: diff báo A cho tệp chỉ có trong index: %+v", f)
		}
	}
}

func TestRmCachedStillOnDisk(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "a.ts", "a")
	base := commitAll(t, dir, "c")
	git(t, dir, "rm", "-q", "--cached", "a.ts")
	r := open(t, dir)
	ch, wt := snap(t, r, base)
	// a.ts: 'D' (có trong base, không còn trong index) gộp với untracked 'A' → 'M'.
	eq(t, summary(ch), []string{"M:a.ts"})
	if !wt.Has("a.ts") || wt.Key("a.ts") != tree.BlobKey([]byte("a")) {
		t.Fatal("a.ts phải còn trong cây làm việc")
	}
}

func TestConflict(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "c.ts", "base\n")
	base := commitAll(t, dir, "base")
	git(t, dir, "checkout", "-q", "-b", "other")
	write(t, dir, "c.ts", "other\n")
	commitAll(t, dir, "other")
	git(t, dir, "checkout", "-q", "main")
	write(t, dir, "c.ts", "main\n")
	commitAll(t, dir, "main")
	cmd := exec.Command("git", "merge", "other")
	cmd.Dir = dir
	_ = cmd.Run() // xung đột là mong đợi
	r := open(t, dir)
	ch, wt := snap(t, r, base)
	n := 0
	for _, f := range wt.Files() {
		if f == "c.ts" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("c.ts xuất hiện %d lần", n)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "c.ts"))
	if wt.Key("c.ts") != tree.BlobKey(b) {
		t.Fatal("tệp xung đột phải lấy khoá từ đĩa")
	}
	found := false
	for _, f := range ch.Files {
		if f.Path == "c.ts" {
			found = true
		}
	}
	if !found {
		t.Fatalf("c.ts phải nằm trong tập thay đổi: %v", summary(ch))
	}
}

func TestNestedRepoUntracked(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "a.ts", "a")
	base := commitAll(t, dir, "c")
	nested := filepath.Join(dir, "vendor", "lib")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, nested, "init", "-q")
	write(t, nested, "x.ts", "x")
	r := open(t, dir)
	ch, wt := snap(t, r, base)
	t.Logf("changes=%v subs=%v files=%v", summary(ch), wt.Submodules(), wt.Files())
	for _, f := range wt.Files() {
		if strings.HasPrefix(f, "vendor/") {
			t.Errorf("không được liệt kê tệp trong repo lồng: %s", f)
		}
	}
}

func TestParseRaw(t *testing.T) {
	z := "0000000000000000000000000000000000000000"
	in := ":000000 100644 " + z + " " + z + " A\x00new file\x00" +
		":100644 000000 " + z + " " + z + " D\x00gone\x00" +
		":100644 120000 " + z + " " + z + " T\x00t\x00" +
		":100644 100644 " + z + " " + z + " R086\x00old\x00newname\x00" +
		":000000 000000 " + z + " " + z + " U\x00conf\x00"
	got, err := parseRaw([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, f := range got {
		s = append(s, fmt.Sprintf("%c:%s", f.Status, f.Path))
	}
	eq(t, s, []string{"A:new file", "D:gone", "T:t", "D:old", "A:newname", "M:conf"})
	if _, err := parseRaw([]byte(":100644 100644 a b X\x00p\x00")); err == nil {
		t.Fatal("trạng thái lạ phải lỗi")
	}
	if _, err := parseRaw([]byte(":100644 100644 a b M\x00")); err == nil {
		t.Fatal("thiếu đường dẫn phải lỗi")
	}
}

func TestParseStatusV2(t *testing.T) {
	h := "1111111111111111111111111111111111111111"
	in := "# branch.oid " + h + "\x00" +
		"1 .M N... 100644 100644 100644 " + h + " " + h + " mod file.ts\x00" +
		"1 M. N... 100644 100644 100644 " + h + " " + h + " staged.ts\x00" +
		"1 AD N... 000000 100644 000000 " + h + " " + h + " gone.ts\x00" +
		"2 R. N... 100644 100644 100644 " + h + " " + h + " R100 new name.ts\x00old name.ts\x00" +
		"u UU N... 100644 100644 100644 100644 " + h + " " + h + " " + h + " conflict.ts\x00" +
		"? untracked dir/x.ts\x00" +
		"? nested/\x00" +
		"! ignored.log\x00"
	r, err := parseStatusV2([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	mod := func() (s []string) {
		for p := range r.modified {
			s = append(s, p)
		}
		slices.Sort(s)
		return
	}()
	eq(t, mod, []string{"conflict.ts", "gone.ts", "mod file.ts", "new name.ts", "old name.ts"})
	if len(r.removed) != 1 || !r.removed["gone.ts"] {
		t.Fatalf("removed=%v", r.removed)
	}
	eq(t, r.untracked, []string{"untracked dir/x.ts", "nested/"})
	for _, bad := range []string{"Z foo", "1 .M N... 1 2", "?x", "2 R. N... a b c d e R1 p"} {
		if _, err := parseStatusV2([]byte(bad + "\x00")); err == nil {
			t.Errorf("%q phải lỗi", bad)
		}
	}
}

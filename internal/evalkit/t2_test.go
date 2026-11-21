package evalkit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func TestParseRunJSON(t *testing.T) {
	// vitest: mục typecheck bị bỏ; tệp lặp ở nhiều project đỏ nếu có lượt đỏ; jest dùng đường dẫn Windows.
	js := `{"numTotalTests":3,"testResults":[
	 {"name":"D:/r/src/a.test.ts","status":"passed","assertionResults":[{"meta":{}}]},
	 {"name":"D:/r/src/a.test.ts","status":"failed","assertionResults":[{"meta":{}}]},
	 {"name":"D:/r/src/b.test.ts","status":"failed","assertionResults":[{"meta":{"typecheck":true}}]},
	 {"name":"D:\\r\\test\\c.test.js","status":"pending","assertionResults":[]},
	 {"name":"D:\\r\\test\\d.test.js","status":"passed","assertionResults":[]}]}`
	m, n, err := ParseRunJSON("D:/r", []byte(js))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"src/a.test.ts": "failed", "test/c.test.js": "skipped", "test/d.test.js": "passed"}
	if !reflect.DeepEqual(m, want) || n != 3 {
		t.Fatalf("got %v %d", m, n)
	}
	if _, _, err := ParseRunJSON("D:/r", []byte(`{"testResults":[]}`)); err == nil {
		t.Fatal("báo cáo rỗng phải là lỗi")
	}
}

func TestApplyEdit(t *testing.T) {
	blob := []byte("a === b\r\nxé + y\n")
	m := T2Mutant{ID: "x", Start: 2, End: 5, Repl: "!=="}
	out, err := ApplyEdit(blob, m)
	if err != nil || string(out) != "a !== b\r\nxé + y\n" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := ApplyEdit(blob, T2Mutant{Start: 3, End: 99}); err == nil {
		t.Fatal("ngoài phạm vi phải lỗi")
	}
}

func TestSelCovers(t *testing.T) {
	sel := model.Selection{Tests: []model.SelectedTest{
		{Unit: "a/x.test.js", Files: []string{"a/x.test.js"}},
		{Unit: "pkg:whole", Project: "pkg", Whole: true},
	}}
	for f, want := range map[string]bool{"a/x.test.js": true, "a/y.test.js": false, "pkg/t/z.test.js": true, "pkgx/z.test.js": false} {
		if got := selCovers(sel, f); got != want {
			t.Errorf("%s: %v", f, got)
		}
	}
	if !selCovers(model.Selection{Tests: []model.SelectedTest{{Unit: "w", Project: ".", Whole: true}}}, "any/t.js") {
		t.Error("Whole ở gốc phải bao mọi tệp")
	}
}

// TestMutantCommit: M = R + đột biến, dựng bằng index tạm; cây làm việc không bị đụng; mode giữ nguyên; tất định.
func TestMutantCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("không có git")
	}
	if err := SetupHermeticEnv(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	tgit(t, repo, "init", "-q")
	writeF(t, repo, "src/a.js", []byte("if (a === b) go()\n"))
	writeF(t, repo, "src/b.js", []byte("keep\n"))
	tgit(t, repo, "add", "-A")
	tgit(t, repo, "commit", "-q", "-m", "R")
	r := tgit(t, repo, "rev-parse", "HEAD")
	oid := tgit(t, repo, "rev-parse", "HEAD:src/a.js")
	rep := T2Repo{Name: "x/y", Dir: "y", R: r}
	m := T2Mutant{ID: "y-01", File: "src/a.js", Start: 6, End: 9, Repl: "!==", Blob: oid}
	idx := filepath.Join(t.TempDir(), "idx")
	c1, mut, err := rep.MutantCommit(context.Background(), repo, idx, m)
	if err != nil {
		t.Fatal(err)
	}
	if string(mut) != "if (a !== b) go()\n" {
		t.Fatalf("%q", mut)
	}
	if got := tgit(t, repo, "show", c1+":src/a.js"); got != "if (a !== b) go()" {
		t.Fatalf("blob trong M: %q", got)
	}
	if tgit(t, repo, "show", c1+":src/b.js") != "keep" || tgit(t, repo, "rev-parse", c1+"^") != r {
		t.Fatal("M phải giữ tệp khác và có cha R")
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "src/a.js")); string(b) != "if (a === b) go()\n" {
		t.Fatal("cây làm việc bị đụng")
	}
	if tgit(t, repo, "status", "--porcelain") != "" {
		t.Fatal("cây làm việc không sạch")
	}
	c2, _, err := rep.MutantCommit(context.Background(), repo, idx, m)
	if err != nil || c2 != c1 {
		t.Fatalf("không tất định: %s %s %v", c1, c2, err)
	}
	m.Blob = "deadbeef"
	if _, _, err := rep.MutantCommit(context.Background(), repo, idx, m); err == nil {
		t.Fatal("blob lệch phải lỗi (cache cũ)")
	}
}

func TestOracleStore(t *testing.T) {
	s := &OracleStore{Path: filepath.Join(t.TempDir(), "o.jsonl")}
	a := T2Record{Repo: "r", R: "1", Mutant: T2Mutant{ID: "m1"}, Status: "error"}
	if err := s.Append(a); err != nil {
		t.Fatal(err)
	}
	b := a
	b.Status = "killed"
	s.Append(b)
	s.Append(T2Record{Repo: "r", R: "1", Mutant: T2Mutant{ID: "m2"}, Status: "survived"})
	got, err := s.Load()
	if err != nil || len(got) != 2 || got[0].Status != "killed" || got[1].Mutant.ID != "m2" {
		t.Fatalf("%v %v", got, err)
	}
}

// TestQuickMarkdownT2: vi phạm T2 hiện trong mục Vi phạm và được tính vào quick.
func TestQuickMarkdownT2(t *testing.T) {
	c := T2Check{Rec: T2Record{Mutant: T2Mutant{ID: "zod-05", File: "src/a.ts", Line: 3, Op: "swap-op"}}, Scope: "graph", Missed: []string{"t/x.test.ts"}}
	q := QuickResult{Violations: 1, T2: &T2Result{Violations: 1, Repos: []T2RepoResult{{Repo: T2Repo{Name: "colinhacks/zod"}, Violations: []T2Check{c}, Checks: []T2Check{c}}}}}
	md := QuickMarkdown(q)
	for _, want := range []string{"FAIL: 1 violations", "T2 colinhacks/zod zod-05", "## T2 (re-check on cached oracle"} {
		if !strings.Contains(md, want) {
			t.Errorf("thiếu %q trong:\n%s", want, md)
		}
	}
}

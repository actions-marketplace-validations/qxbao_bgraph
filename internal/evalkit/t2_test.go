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
	// vitest: typecheck entries are dropped; a file repeated across projects is red if any run is red; jest uses Windows paths.
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
		t.Fatal("an empty report must be an error")
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
		t.Fatal("out of range must error")
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
		t.Error("Whole at the root must cover every file")
	}
}

// TestMutantCommit: M = R + mutation, built with a temporary index; working tree untouched; mode preserved; deterministic.
func TestMutantCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
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
		t.Fatal("M must keep the other files and have parent R")
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "src/a.js")); string(b) != "if (a === b) go()\n" {
		t.Fatal("working tree was touched")
	}
	if tgit(t, repo, "status", "--porcelain") != "" {
		t.Fatal("working tree is not clean")
	}
	c2, _, err := rep.MutantCommit(context.Background(), repo, idx, m)
	if err != nil || c2 != c1 {
		t.Fatalf("not deterministic: %s %s %v", c1, c2, err)
	}
	m.Blob = "deadbeef"
	if _, _, err := rep.MutantCommit(context.Background(), repo, idx, m); err == nil {
		t.Fatal("a mismatched blob must error (stale cache)")
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

// TestQuickMarkdownT2: a T2 violation shows up in the Violations section and is counted in quick.
func TestQuickMarkdownT2(t *testing.T) {
	c := T2Check{Rec: T2Record{Mutant: T2Mutant{ID: "zod-05", File: "src/a.ts", Line: 3, Op: "swap-op"}}, Scope: "graph", Missed: []string{"t/x.test.ts"}}
	q := QuickResult{Violations: 1, T2: &T2Result{Violations: 1, Repos: []T2RepoResult{{Repo: T2Repo{Name: "colinhacks/zod"}, Violations: []T2Check{c}, Checks: []T2Check{c}}}}}
	md := QuickMarkdown(q)
	for _, want := range []string{"FAIL: 1 violations", "T2 colinhacks/zod zod-05", "## T2 (re-check on cached oracle"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
}

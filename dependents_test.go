package bgraph

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func testRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfg, nil, 0o644)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	for _, k := range []string{"AUTHOR", "COMMITTER"} {
		t.Setenv("GIT_"+k+"_NAME", "t")
		t.Setenv("GIT_"+k+"_EMAIL", "t@example.com")
	}
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "core.autocrlf=false"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for rel, c := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return root
}

func TestDependents(t *testing.T) {
	root := testRepo(t, map[string]string{
		"package.json":     `{"name":"p","devDependencies":{"vitest":"^1.0.0"}}`,
		"src/a.ts":         "export const a = 1\n",
		"src/b.ts":         "import { a } from './a'\nexport const b = a\n",
		"src/b.test.ts":    "import { b } from './b'\nconsole.log(b)\n",
		"src/c.test.ts":    "console.log('c')\n",
		"src/dyn.ts":       "export const d = (n: string) => import(n)\n",
		"src/locales/x.ts": "export default 1\n",
	})
	ix, err := Open(context.Background(), root, Options{MemoryOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	res, err := ix.Dependents(context.Background(), DependentsQuery{Files: []string{"./src/a.ts", `src\a.ts`, filepath.Join(root, "src", "a.ts"), "src/nope.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Files, []string{"src/a.ts", "src/nope.ts"}) {
		t.Errorf("files = %v", res.Files)
	}
	if !slices.Equal(res.Missing, []string{"src/nope.ts"}) {
		t.Errorf("missing = %v", res.Missing)
	}
	var got []string
	for _, d := range res.Dependents {
		got = append(got, d.File)
	}
	if !slices.Equal(got, []string{"src/b.test.ts", "src/b.ts"}) {
		t.Fatalf("dependents = %v (tests first, then by path)", got)
	}
	d := res.Dependents[0]
	if !d.Test || len(d.Chain) != 3 || !strings.HasPrefix(d.Chain[0], "src/a.ts") || !strings.HasPrefix(d.Chain[2], "src/b.test.ts") {
		t.Errorf("chain = %v", d.Chain)
	}
	if len(res.Tests) != 1 || res.Tests[0].Unit != "src/b.test.ts" || len(res.Tests[0].Reasons) == 0 {
		t.Errorf("tests = %+v", res.Tests)
	}
	if res.Unresolved == nil || res.Dependents == nil {
		t.Error("slices must be non-nil")
	}

	// A fully dynamic import (scope: the whole repo) contains the queried file: reported as a fail-closed caveat.
	res, err = ix.Dependents(context.Background(), DependentsQuery{Files: []string{"src/locales/x.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unresolved) == 0 {
		t.Errorf("expected the dynamic import in unresolved, got %+v", res)
	}

	// A past revision.
	res, err = ix.Dependents(context.Background(), DependentsQuery{Files: []string{"src/a.ts"}, Rev: "HEAD"})
	if err != nil || len(res.Dependents) != 2 {
		t.Errorf("rev HEAD: %v %+v", err, res)
	}
	if _, err := ix.Dependents(context.Background(), DependentsQuery{Files: []string{"src/a.ts"}, Rev: "no-such-rev"}); err == nil {
		t.Error("bad rev should be an error")
	}
}

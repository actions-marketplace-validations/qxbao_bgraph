package bgraph

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// A gitlink present in both commits is a change only when it points to another commit.
func TestDiffTreesSubmodulePointer(t *testing.T) {
	root := testRepo(t, map[string]string{"a.txt": "a\n"})
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	first := git("rev-parse", "HEAD")
	git("update-index", "--add", "--cacheinfo", "160000,"+first+",sub")
	git("commit", "-q", "-m", "add gitlink")
	c1 := git("rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "same pointer")
	c2 := git("rev-parse", "HEAD")
	git("update-index", "--cacheinfo", "160000,"+c1+",sub")
	git("commit", "-q", "-m", "move pointer")
	c3 := git("rev-parse", "HEAD")

	ix, err := Open(context.Background(), root, Options{MemoryOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	diff := func(a, b string) []string {
		ta, err := ix.repo.CommitTree(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		tb, err := ix.repo.CommitTree(context.Background(), b)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range diffTrees(ta, tb) {
			out = append(out, c.Path)
		}
		return out
	}
	if got := diff(c1, c2); len(got) != 0 {
		t.Errorf("same pointer: changes %v, want none", got)
	}
	if got := diff(c2, c3); len(got) != 1 || got[0] != "sub" {
		t.Errorf("moved pointer: changes %v, want [sub]", got)
	}
}

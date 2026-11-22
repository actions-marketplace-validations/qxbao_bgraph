package gitx

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

// addLink stages a symbolic link (mode 120000) whatever the checkout can make of it, and writes the target text on disk as a
// core.symlinks=false checkout does.
func addLink(t *testing.T, dir, p, target string) {
	t.Helper()
	oid := hashObject(t, dir, target)
	git(t, dir, "update-index", "--add", "--cacheinfo", "120000,"+oid+","+p)
	write(t, dir, p, target)
}

func TestLinksFollowed(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "shared/a.js", "A")
	write(t, dir, "shared/sub/b.js", "B")
	write(t, dir, "lib/x.js", "X")
	commitAll(t, dir, "base")
	addLink(t, dir, "app/shared", "../shared")     // directory link
	addLink(t, dir, "app/x.js", "../lib/x.js")     // file link
	addLink(t, dir, "app/out", "../../elsewhere")  // outside the repo
	addLink(t, dir, "app/abs", "/etc/passwd")      // absolute
	addLink(t, dir, "app/gone", "missing/file.js") // dangling
	addLink(t, dir, "loop/self", "..")             // cycle (re-enters itself)
	git(t, dir, "commit", "-q", "-m", "links")

	r := open(t, dir)
	ct, err := r.CommitTree(context.Background(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	_, wt := snap(t, r, "HEAD")
	for name, tr := range map[string]tree.Tree{"commit": ct, "work": wt} {
		t.Run(name, func(t *testing.T) {
			for _, p := range []string{"app/shared/a.js", "app/shared/sub/b.js", "app/x.js", "loop/self/shared/a.js"} {
				if !tr.Has(p) {
					t.Errorf("Has(%s) = false", p)
				}
			}
			for _, p := range []string{"app/shared", "app/out", "app/abs", "app/gone"} {
				if tr.Has(p) {
					t.Errorf("Has(%s) = true", p)
				}
			}
			if !tr.IsDir("app/shared") || !tr.IsDir("app/shared/sub") {
				t.Error("IsDir of a directory link")
			}
			if b, err := tr.Read("app/shared/sub/b.js"); err != nil || string(b) != "B" {
				t.Errorf("Read through a directory link = %q, %v", b, err)
			}
			if tr.Key("app/x.js") != tr.Key("lib/x.js") || tr.Key("app/x.js") == "" {
				t.Error("Key of a file link")
			}
			if !slices.Contains(tr.Files(), "app/shared/sub/b.js") || slices.Contains(tr.Files(), "app/shared") {
				t.Error("Files")
			}
			al := tree.AliasesOf(tr)
			if al["app/shared/a.js"] != "shared/a.js" || al["app/x.js"] != "lib/x.js" {
				t.Errorf("Aliases = %v", al)
			}
			problems := map[string]string{}
			for _, l := range tree.LinksOf(tr) {
				problems[l.Path] = l.Problem
			}
			want := map[string]string{"app/shared": "", "app/x.js": "", "app/out": tree.LinkOutside, "app/abs": tree.LinkAbsolute,
				"app/gone": tree.LinkDangling, "loop/self": tree.LinkCycle}
			for p, w := range want {
				if got, ok := problems[p]; !ok || got != w {
					t.Errorf("link %s: problem %q, want %q", p, got, w)
				}
			}
		})
	}
}

// A real symbolic link on disk (when the OS lets the test make one): untracked, it is still a link, read with Readlink.
func TestLinksOnDisk(t *testing.T) {
	dir := newRepoDir(t)
	write(t, dir, "shared/a.js", "A")
	write(t, dir, "other/c.js", "C")
	commitAll(t, dir, "base")
	if err := os.Symlink("shared", filepath.Join(dir, "app")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	r := open(t, dir)
	_, wt := snap(t, r, "HEAD")
	if wt.Mode("app") != tree.ModeSymlink || !wt.Has("app/a.js") {
		t.Fatalf("untracked link: mode %q, Has(app/a.js)=%v", wt.Mode("app"), wt.Has("app/a.js"))
	}
	if b, err := wt.Read("app/a.js"); err != nil || string(b) != "A" {
		t.Fatalf("Read = %q, %v", b, err)
	}
}

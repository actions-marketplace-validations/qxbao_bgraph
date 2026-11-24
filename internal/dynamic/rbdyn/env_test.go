package rbdyn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestFingerprint(t *testing.T) {
	for _, k := range envVars {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	root := t.TempDir()
	tr := tree.NewMapTree(map[string]string{"Gemfile": "gem \"x\"\n", "lib/x.rb": "", "spec/fixtures/app/Gemfile": ""})
	if _, err := Fingerprint("ruby 3.4", root, tr); err == nil || !strings.Contains(err.Error(), "no Gemfile.lock") {
		t.Fatalf("no lockfile: %v", err)
	}
	write := func(rel, c string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Gemfile.lock", "GEM\n  specs:\n    x (1.0)\n")
	a, err := Fingerprint("ruby 3.4", root, tr)
	if err != nil {
		t.Fatal(err) // the Gemfile below spec/fixtures is test data: no lockfile needed
	}
	write("Gemfile.lock", "GEM\n  specs:\n    x (1.1)\n")
	b, _ := Fingerprint("ruby 3.4", root, tr)
	c, _ := Fingerprint("ruby 3.5", root, tr)
	t.Setenv("BUNDLE_WITHOUT", "doc")
	d, _ := Fingerprint("ruby 3.5", root, tr)
	write(".bundle/config", "BUNDLE_PATH: x\n")
	e, _ := Fingerprint("ruby 3.5", root, tr)
	if a == b || b == c || c == d || d == e {
		t.Fatalf("fingerprints do not change: %s %s %s %s %s", a, b, c, d, e)
	}
	if _, err := Fingerprint("ruby 3.4", root, tree.NewMapTree(map[string]string{"x.rb": ""})); err == nil {
		t.Fatal("a repository without a Gemfile has a fingerprint")
	}
}

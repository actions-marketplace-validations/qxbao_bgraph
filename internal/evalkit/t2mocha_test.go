package evalkit

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseMochaJSON(t *testing.T) {
	b := []byte(`{"stats":{"tests":4},"passes":[{"file":"/r/test/a.js"},{"file":"/r/test/b.js"}],
"failures":[{"file":"/r/test/b.js","fullTitle":"b x"}],"pending":[{"file":"/r/test/c.js"}],"tests":[]}`)
	m, n, err := ParseMochaJSON("/r", b)
	if err != nil || n != 4 || m["test/a.js"] != "passed" || m["test/b.js"] != "failed" || m["test/c.js"] != "skipped" || len(m) != 3 {
		t.Fatalf("%v %d %v", m, n, err)
	}
	if _, _, err := ParseMochaJSON("/r", []byte(`{"stats":{"tests":1},"failures":[{"fullTitle":"\"before all\" hook"}]}`)); err == nil {
		t.Error("unattributed failure accepted")
	}
	if _, _, err := ParseMochaJSON("/r", []byte(`{"stats":{"tests":0}}`)); err == nil {
		t.Error("empty report accepted")
	}
}

func TestDynEco(t *testing.T) {
	for eco, want := range map[string]string{"python": "python", "": "node", "rust": "", "jvm": ""} {
		if got := dynEco(T2Repo{Eco: eco}); got != want {
			t.Errorf("%q: %q", eco, got)
		}
	}
}

func TestNodeModulesDirs(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"node_modules/a/node_modules", "packages/x/node_modules/y", "packages/x/src", ".git/node_modules", "a/b/c/d/node_modules"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := nodeModulesDirs(root)
	want := []string{"node_modules", filepath.FromSlash("packages/x/node_modules")}
	if !slices.Equal(got, want) {
		t.Errorf("%v, want %v", got, want)
	}
}

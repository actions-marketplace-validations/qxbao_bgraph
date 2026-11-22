package pydyn

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestEnvFiles(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{
		"pyproject.toml": "a", "sub/setup.cfg": "b", "requirements-dev.txt": "c", "uv.lock": "d", "tox.ini": "e", ".python-version": "f",
		"pkg/a.py": "", "conftest.py": "", "node_modules/x/pyproject.toml": "", "README.md": "", "docs/requirements/base.txt": "g",
	})
	got := EnvFiles(tr)
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{".python-version", "docs/requirements/base.txt", "pyproject.toml", "requirements-dev.txt", "sub/setup.cfg", "tox.ini", "uv.lock"}
	if !slices.Equal(keys, want) {
		t.Fatalf("%v", keys)
	}
	if got["uv.lock"] != tr.Key("uv.lock") {
		t.Error("key")
	}
	a := Fingerprint(Collector(false), []string{"x"}, tr)
	if a == Fingerprint(Collector(true), []string{"x"}, tr) {
		t.Error("trust setting not in the fingerprint")
	}
	tr2 := tree.NewMapTree(map[string]string{"pyproject.toml": "a2"})
	if a == Fingerprint(Collector(false), []string{"x"}, tr2) {
		t.Error("manifest content not in the fingerprint")
	}
}

func TestResolvePython(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "venv", "bin"), 0o755)
	os.WriteFile(filepath.Join(root, "venv", "bin", "python"), nil, 0o755)
	if p, err := ResolvePython(root, "venv/bin/python"); err != nil || p != filepath.Join(root, "venv", "bin", "python") {
		t.Errorf("%q %v", p, err)
	}
	if _, err := ResolvePython(root, "venv/bin/nope"); err == nil {
		t.Error("missing interpreter accepted")
	}
	if _, err := ResolvePython(root, "no-such-python-bgraph"); err == nil {
		t.Error("missing PATH interpreter accepted")
	}
}

func TestProbe(t *testing.T) {
	py := os.Getenv("BGRAPH_TEST_PYTHON")
	if py == "" {
		var err error
		if py, err = exec.LookPath("python"); err != nil {
			t.Skip("no python")
		}
	}
	var c ProbeCache
	in, err := c.Get(context.Background(), py, t.TempDir(), t.TempDir())
	if err != nil {
		t.Skip("python does not run: ", err)
	}
	has := func(prefix string) bool {
		return slices.ContainsFunc(in.Lines, func(l string) bool { return strings.HasPrefix(l, prefix) })
	}
	for _, p := range []string{"version=", "implementation=", "platform=", "os=", "machine="} {
		if !has(p) {
			t.Errorf("no %s line: %q", p, in.Lines)
		}
	}
	if has("site=") {
		t.Error("site lines in the fingerprint")
	}
	again, err := c.Get(context.Background(), py, t.TempDir(), t.TempDir())
	if err != nil || !slices.Equal(again.Lines, in.Lines) {
		t.Errorf("cached probe differs: %v", err)
	}
}

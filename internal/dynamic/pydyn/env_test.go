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
	pyproject := `[project]
name = "x"
version = "1"
dependencies = ["a"]

[tool.pytest.ini_options]
addopts = "-q"
`
	setupCfg := `[metadata]
name = x

[tool:pytest]
testpaths = tests
`
	base := map[string]string{
		"pyproject.toml": pyproject, "sub/setup.cfg": setupCfg, "requirements-dev.txt": "c", "uv.lock": "d", "tox.ini": "e",
		".python-version": "f", "pytest.ini": "[pytest]\n", "pkg/a.py": "", "conftest.py": "", "node_modules/x/pyproject.toml": "",
		"README.md": "", "setup.py": "s",
	}
	tr := tree.NewMapTree(base)
	got := EnvFiles(tr)
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"pyproject.toml", "pytest.ini", "sub/setup.cfg", "tox.ini"}; !slices.Equal(keys, want) {
		t.Fatalf("%v", keys)
	}
	fp := func(over map[string]string) string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		return Fingerprint(Collector(false), []string{"x"}, tree.NewMapTree(m))
	}
	a := fp(nil)
	if a == Fingerprint(Collector(true), []string{"x"}, tr) {
		t.Error("trust setting not in the fingerprint")
	}
	// Lockfiles, requirements, setup.py and the non-pytest parts of pyproject.toml/setup.cfg do not change it.
	for name, over := range map[string]map[string]string{
		"lockfile":          {"uv.lock": "d2"},
		"requirements":      {"requirements-dev.txt": "c2"},
		"setup.py":          {"setup.py": "s2"},
		"pyproject project": {"pyproject.toml": strings.Replace(pyproject, `["a"]`, `["a", "b"]`, 1)},
		"setup.cfg meta":    {"sub/setup.cfg": strings.Replace(setupCfg, "name = x", "name = y", 1)},
	} {
		if fp(over) != a {
			t.Errorf("%s changes the fingerprint", name)
		}
	}
	// The pytest configuration does.
	for name, over := range map[string]map[string]string{
		"pyproject pytest":  {"pyproject.toml": strings.Replace(pyproject, `"-q"`, `"-x"`, 1)},
		"setup.cfg pytest":  {"sub/setup.cfg": strings.Replace(setupCfg, "testpaths = tests", "testpaths = t", 1)},
		"tox.ini":           {"tox.ini": "e2"},
		"pytest.ini":        {"pytest.ini": "[pytest]\naddopts = -x\n"},
		"broken pyproject":  {"pyproject.toml": pyproject + "[[[\n"},
		"new pytest config": {"tests/pytest.ini": "[pytest]\n"},
	} {
		if fp(over) == a {
			t.Errorf("%s does not change the fingerprint", name)
		}
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

package bgraph

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
)

// pytestPython returns an interpreter with pytest (BGRAPH_TEST_PYTHON, else python on PATH) or skips.
func pytestPython(t *testing.T) string {
	t.Helper()
	py := os.Getenv("BGRAPH_TEST_PYTHON")
	if py == "" {
		p, err := exec.LookPath("python")
		if err != nil {
			t.Skip("no python (set BGRAPH_TEST_PYTHON to an interpreter with pytest)")
		}
		py = p
	}
	if out, err := exec.Command(py, "-c", "import pytest").CombinedOutput(); err != nil {
		t.Skipf("%s has no pytest (set BGRAPH_TEST_PYTHON): %s", py, strings.TrimSpace(string(out)))
	}
	return py
}

// TestRecordIntegration runs the real collector with pytest on a tiny repository: a dynamic-import widening is narrowed for
// the test that never loads the changed module and kept for the one that does; a test that spawns a subprocess gets an
// incomplete record and is kept.
func TestRecordIntegration(t *testing.T) {
	py := pytestPython(t)
	root := testRepo(t, map[string]string{
		"pyproject.toml":        "[project]\nname = \"x\"\nversion = \"1\"\n",
		"pkg/__init__.py":       "",
		"pkg/loader.py":         "import importlib\ndef load(n):\n    return importlib.import_module(n)\n",
		"pkg/a.py":              "X = 1\n",
		"pkg/b.py":              "Y = 1\n",
		"pkg/data.txt":          "hello\n",
		"tests/test_loader.py":  "from pkg.loader import load\ndef test_x():\n    assert load('pkg.a').X == 1\n",
		"tests/test_other.py":   "from pkg.loader import load\ndef test_y():\n    assert load('pkg.b').Y == 1\n",
		"tests/test_reads.py":   "import os\nfrom pkg.loader import load\nHERE = os.path.dirname(__file__)\ndef test_r():\n    load('pkg.b')\n    with open(os.path.join(HERE, '..', 'pkg', 'data.txt')) as f:\n        assert f.read() == 'hello\\n'\n",
		"tests/test_sub.py":     "import subprocess, sys\nfrom pkg.loader import load\ndef test_s():\n    load('pkg.b')\n    subprocess.run([sys.executable, '-c', 'pass'], check=True)\n",
		"tests/test_failing.py": "from pkg.loader import load\ndef test_f():\n    load('pkg.b')\n    assert False\n",
	})
	state := t.TempDir()
	ctx := context.Background()
	// trust_native_env: on Windows pytest's own terminal setup (colorama) calls ctypes, which the strict default counts as
	// native code; the integration test is about the repository's own behavior.
	ix, err := Open(ctx, root, Options{MemoryOnly: true, StateDir: state, Python: py, TrustNativeEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	sum, err := ix.Record(ctx, RecordOptions{Jobs: 2, Timeout: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Units != 5 || sum.Recorded != 5 || sum.Failed != 1 {
		t.Fatalf("summary %+v", sum)
	}
	recs, warn := dynamic.Load(dynamic.StorePath(state, "python"))
	if len(warn) > 0 || len(recs) != 5 {
		t.Fatalf("store: %d records, %v", len(recs), warn)
	}
	by := map[string]dynamic.Record{}
	for _, r := range recs {
		by[r.Unit] = r
	}
	for _, u := range []string{"tests/test_loader.py", "tests/test_other.py", "tests/test_reads.py"} {
		if r := by[u]; !r.Complete || !r.Pass {
			t.Errorf("%s: complete=%v pass=%v (%s)", u, r.Complete, r.Pass, r.Incomplete)
		}
	}
	if r := by["tests/test_loader.py"]; r.Deps["pkg/a.py"] == "" || r.Deps["pkg/loader.py"] == "" || r.Deps["tests/test_loader.py"] == "" {
		t.Errorf("test_loader deps %v", r.Deps)
	}
	if r := by["tests/test_other.py"]; r.Deps["pkg/a.py"] != "" || r.Deps["pkg/b.py"] == "" {
		t.Errorf("test_other deps %v", r.Deps)
	}
	if r := by["tests/test_reads.py"]; r.Deps["pkg/data.txt"] == "" {
		t.Errorf("test_reads deps %v", r.Deps)
	}
	if r := by["tests/test_sub.py"]; r.Complete || !strings.Contains(r.Incomplete, "subprocess") || !r.Pass {
		t.Errorf("test_sub: complete=%v pass=%v (%s)", r.Complete, r.Pass, r.Incomplete)
	}
	if r := by["tests/test_failing.py"]; r.Pass {
		t.Error("failing test recorded as passing")
	}
	if out, err := exec.Command("git", "-C", root, "status", "--porcelain").CombinedOutput(); err != nil || len(out) > 0 {
		t.Errorf("recording dirtied the tree: %v %s", err, out)
	}

	write := func(rel, c string) {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sel := func() Selection {
		t.Helper()
		s, err := ix.Select(ctx, Change{Base: "HEAD", Worktree: true, Dynamic: DynamicOn})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	units := func(s Selection) (kept, narrowed []string) {
		for _, x := range s.Tests {
			kept = append(kept, x.Unit)
		}
		for _, x := range s.Narrowed {
			narrowed = append(narrowed, x.Unit)
		}
		return kept, narrowed
	}

	// pkg/a.py changes: every test reaches it only through the dynamic import (and the unreferenced-file widening).
	write("pkg/a.py", "X = 2\n")
	kept, narrowed := units(sel())
	if !slices.Equal(kept, []string{"tests/test_failing.py", "tests/test_loader.py", "tests/test_sub.py"}) ||
		!slices.Equal(narrowed, []string{"tests/test_other.py", "tests/test_reads.py"}) {
		t.Fatalf("pkg/a.py: kept %v narrowed %v", kept, narrowed)
	}
	write("pkg/a.py", "X = 1\n")

	// pkg/b.py changes: only test_loader never loaded it.
	write("pkg/b.py", "Y = 1\n# edit\n")
	kept, narrowed = units(sel())
	if !slices.Equal(kept, []string{"tests/test_failing.py", "tests/test_other.py", "tests/test_reads.py", "tests/test_sub.py"}) ||
		!slices.Equal(narrowed, []string{"tests/test_loader.py"}) {
		t.Fatalf("pkg/b.py: kept %v narrowed %v", kept, narrowed)
	}
	write("pkg/b.py", "Y = 1\n")

	// A data file is not a source file the guards accept (docs/dynamic.md §2): no narrowing at all, with a note.
	write("pkg/data.txt", "bye\n")
	s := sel()
	if len(s.Narrowed) != 0 || len(s.Warnings) == 0 || !strings.Contains(s.Warnings[len(s.Warnings)-1].Detail, "pkg/data.txt") {
		t.Fatalf("pkg/data.txt: narrowed %v warnings %+v", s.Narrowed, s.Warnings)
	}
}

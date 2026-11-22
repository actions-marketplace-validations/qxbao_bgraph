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

	"github.com/qxbao/bgraph/internal/audit"
	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/model"
)

// testPython returns an interpreter for the tests (BGRAPH_TEST_PYTHON, else python on PATH) or skips.
func testPython(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("BGRAPH_TEST_PYTHON"); p != "" {
		return p
	}
	p, err := exec.LookPath("python")
	if err != nil {
		t.Skip("no python")
	}
	if exec.Command(p, "-c", "import sys").Run() != nil {
		t.Skip("python does not run")
	}
	return p
}

var dynFiles = map[string]string{
	"pyproject.toml":        "[project]\nname = \"x\"\nversion = \"1\"\n",
	"pkg/__init__.py":       "",
	"pkg/loader.py":         "import importlib\ndef load(n):\n    return importlib.import_module(n)\n",
	"pkg/a.py":              "X = 1\n",
	"pkg/b.py":              "Y = 1\n",
	"tests/test_loader.py":  "from pkg.loader import load\ndef test_x():\n    load('pkg.a')\n",
	"tests/test_loader2.py": "from pkg.loader import load\nimport pkg.b\ndef test_y():\n    pass\n",
	"tests/test_b.py":       "import pkg.b\n",
}

// TestDynamicWiring: Select narrows a widened unit with a usable record only when the dynamic layer is on; guards and
// missing records leave a dynamic_skipped warning; audit treats the removed unit as unselected.
func TestDynamicWiring(t *testing.T) {
	py := testPython(t)
	root := testRepo(t, dynFiles)
	state := t.TempDir()
	ctx := context.Background()
	ix, err := Open(ctx, root, Options{MemoryOnly: true, StateDir: state, Python: py})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	head, err := ix.repo.RevParse(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ht, err := ix.repo.CommitTree(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	env, coll, err := ix.pythonEnv(ctx, nil, ht)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(unit string, deps ...string) dynamic.Record {
		m := map[string]string{unit: ht.Key(unit)}
		for _, d := range deps {
			m[d] = ht.Key(d)
		}
		return dynamic.Record{Eco: "python", Unit: unit, Collector: coll, Commit: head, Deps: m, Env: env, Complete: true, Pass: true,
			Recorded: time.Now().UTC()}
	}
	// Edit pkg/a.py: test_loader (loads pkg.a dynamically) and test_loader2 are widened through the dynamic import, test_b
	// through the project widening of an unreferenced file.
	write := func(rel, c string) {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/a.py", "X = 2\n")
	sel := func(mode DynamicMode) Selection {
		t.Helper()
		s, err := ix.Select(ctx, Change{Base: "HEAD", Worktree: true, Dynamic: mode})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	units := func(s Selection) []string {
		var out []string
		for _, x := range s.Tests {
			out = append(out, x.Unit)
		}
		return out
	}
	hasSkip := func(s Selection, sub string) bool {
		return slices.ContainsFunc(s.Warnings, func(w model.Widening) bool {
			return w.Reason == model.ReasonDynamicSkipped && strings.Contains(w.Detail, sub)
		})
	}
	static := sel(DynamicDefault)
	if got := units(static); !slices.Equal(got, []string{"tests/test_b.py", "tests/test_loader.py", "tests/test_loader2.py"}) {
		t.Fatalf("static selection %v", got)
	}
	if s := sel(DynamicOn); !hasSkip(s, "no records") || len(s.Narrowed) != 0 {
		t.Fatalf("no records: %+v", s.Warnings)
	}
	if _, err := dynamic.Add(dynamic.StorePath(state, "python"), []dynamic.Record{
		mk("tests/test_loader.py", "pkg/loader.py", "pkg/a.py", "pkg/__init__.py"),
		mk("tests/test_loader2.py", "pkg/loader.py", "pkg/b.py", "pkg/__init__.py"),
		mk("tests/test_b.py", "pkg/b.py", "pkg/__init__.py"),
	}); err != nil {
		t.Fatal(err)
	}
	if s := sel(DynamicDefault); len(s.Narrowed) != 0 || len(s.Tests) != 3 {
		t.Fatal("narrowed without being enabled")
	}
	if s := sel(DynamicOff); len(s.Narrowed) != 0 || hasSkip(s, "") {
		t.Fatal("narrowed while off")
	}
	on := sel(DynamicOn)
	if got := units(on); !slices.Equal(got, []string{"tests/test_loader.py"}) || len(on.Narrowed) != 2 || on.Narrowed[1].Unit != "tests/test_loader2.py" {
		t.Fatalf("dynamic selection %v narrowed %+v warnings %+v", got, on.Narrowed, on.Warnings)
	}
	if !strings.Contains(on.Tests[0].Dynamic, "pkg/a.py") {
		t.Errorf("kept note %q", on.Tests[0].Dynamic)
	}

	// bgraph.toml enables the layer; --dynamic=false still disables it. (bgraph.toml is committed so it is not a change.)
	write("bgraph.toml", "[dynamic_layer]\necosystems = [\"python\"]\n")
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "core.autocrlf=false"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("add", "bgraph.toml")
	git("commit", "-q", "-m", "toml")
	if s := sel(DynamicDefault); len(s.Narrowed) != 2 {
		t.Fatalf("bgraph.toml did not enable the layer: %+v", s.Warnings)
	}
	if s := sel(DynamicOff); len(s.Narrowed) != 0 {
		t.Fatal("off did not win over bgraph.toml")
	}

	// Audit: a failure in the narrowed unit is a miss.
	rep, err := ix.Audit(ctx, Change{Base: "HEAD", Worktree: true, Dynamic: DynamicOn}, []AuditResult{{Format: audit.FormatJUnit,
		Failures: []audit.Failure{{Test: "test_y", Candidates: []string{"tests/test_loader2.py"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Clean || len(rep.Missed) != 1 || !strings.Contains(rep.Missed[0].Reason, "dynamic layer") {
		t.Fatalf("audit: %+v", rep)
	}

	// A stale dependency (pkg/b.py differs at the base) makes the record unusable.
	write("pkg/b.py", "Y = 3\n")
	git("add", "pkg/b.py")
	git("commit", "-q", "-m", "b")
	if s := sel(DynamicOn); len(s.Narrowed) != 0 || !strings.Contains(strings.Join(func() []string {
		var n []string
		for _, x := range s.Tests {
			n = append(n, x.Dynamic)
		}
		return n
	}(), "|"), "stale") {
		t.Fatalf("stale record used: %+v", s.Tests)
	}

	// Guard: an added Python file forbids narrowing for the whole selection.
	write("pkg/new.py", "")
	if s := sel(DynamicOn); len(s.Narrowed) != 0 || !hasSkip(s, "pkg/new.py") {
		t.Fatalf("added file: %+v", s.Warnings)
	}
	os.Remove(filepath.Join(root, "pkg", "new.py"))

	// An interpreter that cannot run: no narrowing, with a note.
	ix2, err := Open(ctx, root, Options{MemoryOnly: true, StateDir: state, Python: "no-such-python-bgraph"})
	if err != nil {
		t.Fatal(err)
	}
	defer ix2.Close()
	s, err := ix2.Select(ctx, Change{Base: "HEAD", Worktree: true, Dynamic: DynamicOn})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Narrowed) != 0 || !hasSkip(s, "environment fingerprint") {
		t.Fatalf("bad interpreter: %+v", s.Warnings)
	}
}

package pydyn

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestRecordOf(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "build"), 0o755)
	os.WriteFile(filepath.Join(root, "build", "gen.txt"), []byte("x"), 0o644)
	tr := tree.NewMapTree(map[string]string{
		"pkg/__init__.py": "", "pkg/a.py": "a", "pkg/b.py": "b", "data/x/1.json": "1", "data/x/2.json": "2", "data/y.json": "y",
		"tests/test_a.py": "t",
	})
	u := Unit{ID: "tests/test_a.py", Runner: "pytest", Project: ".", Files: []string{"tests/test_a.py"}}
	o := Outcome{Unit: u.ID, Exit: 0,
		Files: []string{"pkg/a.py", "PKG/__init__.py", "pkg/__pycache__/b.cpython-313.pyc", "pkg/__pycache__/gone.cpython-313.pyc",
			".pytest_cache/v/x", "x.egg-info/PKG-INFO", "missing.py", "pkg"},
		Dirs: []string{"data/x"}}
	r := RecordOf(o, u, tr, root, "env", "c", "h", time.Unix(0, 0))
	var deps []string
	for d := range r.Deps {
		deps = append(deps, d)
	}
	slices.Sort(deps)
	want := []string{"data/x/1.json", "data/x/2.json", "pkg/__init__.py", "pkg/a.py", "pkg/b.py", "tests/test_a.py"}
	if !slices.Equal(deps, want) {
		t.Fatalf("deps %v, want %v", deps, want)
	}
	if !r.Complete || !r.Pass || r.Deps["pkg/a.py"] != tr.Key("pkg/a.py") || r.Env != "env" || r.Collector != "c" || r.Commit != "h" {
		t.Errorf("%+v", r)
	}

	// An untracked file that exists makes the record incomplete.
	o.Files = append(o.Files, "build/gen.txt")
	if r := RecordOf(o, u, tr, root, "env", "c", "h", time.Now()); r.Complete || !strings.Contains(r.Incomplete, "untracked file build/gen.txt") {
		t.Errorf("untracked: %+v", r)
	}
	for name, oc := range map[string]Outcome{
		"timeout":   {Unit: u.ID, TimedOut: true, Exit: -1},
		"no output": {Unit: u.ID, Exit: 0, Err: "the collector wrote no output"},
		"exit 5":    {Unit: u.ID, Exit: 5},
		"failed":    {Unit: u.ID, Exit: 1},
		"incomp":    {Unit: u.ID, Exit: 0, Incomplete: []string{"subprocess: subprocess.Popen"}},
	} {
		r := RecordOf(oc, u, tr, root, "env", "c", "h", time.Now())
		switch name {
		case "failed":
			if r.Pass || !r.Complete {
				t.Errorf("%s: %+v", name, r)
			}
		case "incomp":
			if !r.Pass || r.Complete || r.Incomplete != "subprocess: subprocess.Popen" {
				t.Errorf("%s: %+v", name, r)
			}
		default:
			if r.Pass || r.Complete || r.Incomplete == "" {
				t.Errorf("%s: %+v", name, r)
			}
		}
		if r.Deps["tests/test_a.py"] == "" {
			t.Errorf("%s: own file missing", name)
		}
	}
}

func TestArgv(t *testing.T) {
	plug := "P"
	cases := []struct {
		u    Unit
		want string
	}{
		{Unit{ID: "tests/test_a.py", Project: ".", Runner: "pytest"}, "-m pytest -p bgraph_collect -p no:cacheprovider -q --rootdir . tests/test_a.py"},
		{Unit{ID: "sub/tests/test_a.py", Project: "sub", Runner: "pytest", Config: "sub/pytest.ini"},
			"-m pytest -p bgraph_collect -p no:cacheprovider -q --rootdir . -c pytest.ini tests/test_a.py"},
		{Unit{ID: "-x.py", Project: ".", Runner: "pytest"}, "-m pytest -p bgraph_collect -p no:cacheprovider -q --rootdir . ./-x.py"},
		{Unit{ID: "tests/admin/test_x.py", Project: ".", Runner: "django", Config: "tests/runtests.py"},
			filepath.Join("P", "bgraph_boot.py") + " tests/runtests.py admin.test_x"},
		{Unit{ID: "app/tests/test_x.py", Project: ".", Runner: "django", Config: "manage.py"},
			filepath.Join("P", "bgraph_boot.py") + " manage.py test app.tests.test_x"},
	}
	for _, c := range cases {
		if got := strings.Join(Argv(c.u, plug), " "); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.u.ID, got, c.want)
		}
	}
}

func TestPycSource(t *testing.T) {
	for in, want := range map[string]string{
		"pkg/__pycache__/a.cpython-313.pyc":                 "pkg/a.py",
		"__pycache__/conftest.cpython-313-pytest-9.2.0.pyc": "conftest.py",
		"pkg/a.pyc": "",
	} {
		got, ok := pycSource(in)
		if (want == "") == ok || got != want {
			t.Errorf("%s: %q %v", in, got, ok)
		}
	}
}

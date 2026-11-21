package python

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/tree"
)

type failScanner struct{ calls int }

func (f *failScanner) Scan(tree.Tree, string) (model.ScanResult, error) {
	f.calls++
	return model.ScanResult{}, errors.New("must not be called")
}

// A repo without Python files costs nothing: an empty analysis and no scanning.
func TestNoPythonIsFree(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{"src/a.rs": "mod b;", "package.json": "{}", "a.ts": "", "pyproject.toml": "[project]\nname='x'"})
	sc := &failScanner{}
	a, err := New(nil).Analyze(context.Background(), tr, sc)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Projects)+len(a.Edges)+len(a.Tests)+len(a.Unresolved)+len(a.Roles) != 0 || sc.calls != 0 {
		t.Fatalf("expected an empty analysis, got %+v", a)
	}
	// and it claims no file
	if _, ok := New(nil).ClassifyChange("requirements.txt", tr, tr, a); ok {
		t.Fatal("claimed a file in a repo without Python")
	}
}

func analyze(t *testing.T, files map[string]string) *eco.Analysis {
	t.Helper()
	a, err := New(nil).Analyze(context.Background(), tree.NewMapTree(files), scancache.New())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAnalysisIsDeterministicAndGobSafe(t *testing.T) {
	files := map[string]string{"pyproject.toml": "[project]\nname='x'\n", "pkg/__init__.py": "", "pkg/a.py": "from . import b\nimport ghost\n", "pkg/b.py": "",
		"tests/conftest.py": "pytest_plugins = ['pkg.a']\n", "tests/test_a.py": "from pkg import a\nimport importlib\nimportlib.import_module(x)\n"}
	a1, a2 := analyze(t, files), analyze(t, files)
	if !reflect.DeepEqual(a1, a2) {
		t.Fatal("non-deterministic analysis")
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(a1); err != nil {
		t.Fatal(err)
	}
	var back eco.Analysis
	if err := gob.NewDecoder(&buf).Decode(&back); err != nil || len(back.Edges) != len(a1.Edges) {
		t.Fatalf("gob: %v", err)
	}
}

func TestUnitsAndProjects(t *testing.T) {
	a := analyze(t, map[string]string{
		"pytest.ini": "[pytest]\ntestpaths = tests\n", "pkg/__init__.py": "",
		"tests/test_a.py": "", "tests/a_test.py": "", "tests/helper.py": "", "scripts/test_x.py": "", "tests/conftest.py": ""})
	var ids []string
	for _, u := range a.Tests {
		if u.Eco != Name || u.Runner != RunnerPytest || u.Project != "." || u.Config != "pytest.ini" {
			t.Errorf("bad unit %+v", u)
		}
		ids = append(ids, u.ID)
	}
	if !reflect.DeepEqual(ids, []string{"tests/a_test.py", "tests/test_a.py"}) {
		t.Errorf("units: %v", ids)
	}
	if a.Roles["tests/helper.py"] != model.RoleSource || a.Roles["tests/test_a.py"] != model.RoleTest || a.Roles["pytest.ini"] != model.RoleConfig {
		t.Errorf("roles: %v", a.Roles)
	}
}

func TestFnmatchAndINI(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"test_*.py", "test_a.py", true}, {"test_*.py", "atest_a.py", false}, {"*_test.py", "x_test.py", true}, {"t?st.py", "test.py", true},
		{"[ab]*.py", "b1.py", true}, {"[!ab]*.py", "b1.py", false}, {"*.egg", "x.egg", true}, {".*", ".git", true}, {"*", "a/b", true},
	}
	for _, c := range cases {
		if fnmatch(c.pat, c.s) != c.want {
			t.Errorf("fnmatch(%q,%q) != %v", c.pat, c.s, c.want)
		}
	}
	if !fnmatchEx("tests/test_*.py", "src/tests/test_a.py") || fnmatchEx("tests/test_*.py", "src/other/test_a.py") || !fnmatchEx("test_*.py", "a/b/test_a.py") {
		t.Error("fnmatchEx")
	}
	ini := parseINI("# c\n[pytest]\naddopts = -ra\n  -q\ntestpaths =\n    a\n    b\n[other]\nk: v\n")
	if ini.sections["pytest"]["addopts"] != "-ra\n-q" || ini.sections["pytest"]["testpaths"] != "\na\nb" || ini.sections["other"]["k"] != "v" {
		t.Errorf("%+v", ini.sections)
	}
	if got := shlexSplit(`-p "a b" --x='c d' e\ f`); !reflect.DeepEqual(got, []string{"-p", "a b", "--x=c d", "e f"}) {
		t.Errorf("%q", got)
	}
}

func TestConfigOrderFirstWins(t *testing.T) {
	// pytest.ini beats pyproject.toml; a pyproject without a pytest table is skipped; setup.cfg is the last resort.
	tr := tree.NewMapTree(map[string]string{"a/pytest.ini": "[pytest]\n", "a/pyproject.toml": "[tool.pytest.ini_options]\naddopts='-x'\n",
		"b/pyproject.toml": "[project]\nname='b'\n", "b/setup.cfg": "[tool:pytest]\npython_files = c_*.py\n"})
	if c := loadPytestAt(tr, "a"); c == nil || c.file != "a/pytest.ini" || len(c.addopts) != 0 {
		t.Errorf("a: %+v", c)
	}
	if c := loadPytestAt(tr, "b"); c == nil || c.file != "b/setup.cfg" || !reflect.DeepEqual(c.pythonFiles, []string{"c_*.py"}) {
		t.Errorf("b: %+v", c)
	}
	if c := loadPytestAt(tr, "."); c != nil {
		t.Errorf("root: %+v", c)
	}
}

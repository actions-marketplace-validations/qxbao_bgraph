package evalkit

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestLocatePythonNodeIDs(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{
		"lib/pkg/tests/test_a.py": "def test_x(): pass\n",
		"test_requests.py":        "class RequestsTestCase: pass\n",
	})
	got := LocatePythonTests(tr, "x/y", []string{
		"lib/pkg/tests/test_a.py::TestC::test_y[a::b c-1]",
		"lib/pkg/tests/test_a.py::test_x[weird :: param]",
		"test_requests.py::RequestsTestCase::test_no_content_length",
		"lib/pkg/tests/missing.py::test_z",
		"lib/pkg/tests/test_a.py",
	})
	want := [][]string{{"lib/pkg/tests/test_a.py"}, {"lib/pkg/tests/test_a.py"}, {"test_requests.py"}, nil, {"lib/pkg/tests/test_a.py"}}
	for i, w := range want {
		if !reflect.DeepEqual(got[i].Files, w) || got[i].NotFound != (w == nil) || got[i].Ambiguous || got[i].Form != "nodeid" {
			t.Errorf("%d %s: %+v, want %v", i, got[i].Name, got[i], w)
		}
	}
}

func TestLocatePythonDjango(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{
		"tests/auth_tests/test_validators.py": "class UsernameValidatorsTests: pass\n",
		"tests/admin_views/tests.py":          "class AdminViewBasicTest: pass\n",
		"tests/pkg/__init__.py":               "class InInit: pass\n",
		"tests/forms_tests/tests/tests.py":    "",
		"tests/forms_tests/tests/__init__.py": "",
		"tests/mod/tests.py": `
class T:
    def test_doc(self):
        """
        Named URLs should be reversible.

        More text.
        """

    def test_one_line(self):
        '''Data for a ManyToManyField is a list rather than a lazy QuerySet.'''

    def test_no_doc(self):
        pass

    def test_single(self):
        "An exception report can be generated without request"
        pass
`,
		"tests/mod2/tests.py": `
class U:
    def test_doc(self) -> None:
        """Named URLs should be reversible."""
`,
	})
	got := LocatePythonTests(tr, "django/django", []string{
		"test_ascii_validator (auth_tests.test_validators.UsernameValidatorsTests)",
		"test_x (admin_views.tests.AdminViewBasicTest.test_x)",
		"test_y (pkg.InInit)",
		"test_z (forms_tests.tests.tests.Foo)",
		"test_q (nope.Foo)",
		"Data for a ManyToManyField is a list rather than a lazy QuerySet.",
		"Named URLs should be reversible.",
		"Never documented anywhere",
		"An exception report can be generated without request",
	})
	type exp struct {
		files   []string
		amb, nf bool
		form    string
	}
	want := []exp{
		{[]string{"tests/auth_tests/test_validators.py"}, false, false, "django"},
		{[]string{"tests/admin_views/tests.py"}, false, false, "django"},
		{[]string{"tests/pkg/__init__.py"}, false, false, "django"},
		{[]string{"tests/forms_tests/tests/tests.py"}, false, false, "django"},
		{nil, false, true, "django"},
		{[]string{"tests/mod/tests.py"}, false, false, "docstring"},
		{[]string{"tests/mod/tests.py", "tests/mod2/tests.py"}, true, false, "docstring"},
		{nil, false, true, "other"},
		{[]string{"tests/mod/tests.py"}, false, false, "docstring"},
	}
	for i, w := range want {
		g := got[i]
		if !reflect.DeepEqual(g.Files, w.files) || g.Ambiguous != w.amb || g.NotFound != w.nf || g.Form != w.form {
			t.Errorf("%d %q: %+v, want %+v", i, g.Name, g, w)
		}
	}
}

func TestLocatePythonBare(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{
		"sympy/core/tests/test_a.py":    "def test_issue_1():\n    pass\n\n# def test_commented():\n",
		"sympy/core/tests/test_b.py":    "def test_dup():\n    pass\n",
		"sympy/solvers/tests/test_c.py": "def test_dup():\n    pass\n",
		"sympy/core/a.py":               "def test_not_a_test_file():\n    pass\n",
	})
	got := LocatePythonTests(tr, "sympy/sympy", []string{"test_issue_1", "test_dup", "test_commented", "test_not_a_test_file"})
	if !reflect.DeepEqual(got[0].Files, []string{"sympy/core/tests/test_a.py"}) || got[0].Ambiguous || got[0].Form != "bare" {
		t.Errorf("unique: %+v", got[0])
	}
	if !got[1].Ambiguous || len(got[1].Files) != 2 {
		t.Errorf("ambiguous: %+v", got[1])
	}
	if !got[2].NotFound || !got[3].NotFound {
		t.Errorf("notfound: %+v %+v", got[2], got[3])
	}
}

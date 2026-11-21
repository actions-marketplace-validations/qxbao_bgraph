package evalkit

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func pyOps(src string) []string {
	var out []string
	for _, c := range pyScan([]byte(src)) {
		out = append(out, c.Op+":"+src[c.Start:c.End]+">"+c.Repl)
	}
	sort.Strings(out)
	return out
}

func TestPyScanOperators(t *testing.T) {
	src := `import os
from a import b
def f(a, b=True):
    # a == b in a comment
    s = "x == y and z"
    t = f"{a == b} {s!r}"
    u = '''a != b
    and c'''
    if a == b and (a < b or a is not None):
        return a
    for x in a:
        pass
    y = [i for i in a if i not in b]
    z = a + 1 - b
    w = -1
    return a.b
@dec(x == 1)
class C:
    pass
`
	got := pyOps(src)
	has := func(s string) bool {
		for _, g := range got {
			if g == s {
				return true
			}
		}
		return false
	}
	for _, w := range []string{"and-or:and>or", "eq-ne:==>!=", "lt-ge:<>>=", "or-and:or>and", "isnot-is:is not>is", "notin-in:not in>in", "add-sub:+>-", "sub-add:->+", "true-false:True>False", "return-none:a>None", "return-none:a.b>None"} {
		if !has(w) {
			t.Errorf("missing %s in %v", w, got)
		}
	}
	for _, g := range got {
		if strings.HasPrefix(g, "in-notin") {
			t.Errorf("for-in / comprehension `in` must not be mutated: %v", got)
		}
	}
	// 1 eq-ne (the if line; the decorator, the f-string and strings are skipped), one return-none for each of the two simple returns
	n := map[string]int{}
	for _, g := range got {
		n[strings.SplitN(g, ":", 2)[0]]++
	}
	if n["eq-ne"] != 1 || n["return-none"] != 2 || n["sub-add"] != 1 || n["add-sub"] != 1 || n["and-or"] != 1 {
		t.Errorf("counts %v in %v", n, got)
	}
}

func TestPyScanNoStrings(t *testing.T) {
	src := "x = 'a == b'\ny = \"\"\"\nTrue and False\n\"\"\"\nz = f\"{'in' in q}\"\n# if a < b\n"
	if got := pyOps(src); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if got := pyOps("x = a if b in c else d\n"); !reflect.DeepEqual(got, []string{"in-notin:in>not in"}) {
		t.Fatalf("%v", got)
	}
}

func TestGenPyMutantsDeterministic(t *testing.T) {
	files := []RustMutantSource{
		{Path: "src/a.py", Blob: "1", Data: []byte("def f(x):\n    return x == 1 or x == 2\n")},
		{Path: "src/b.py", Blob: "2", Data: []byte("def g(x):\n    if x > 1:\n        return x + 1\n    return x - 1\n")},
	}
	m1 := GenPyMutants(files, "s", "p", 10)
	if !reflect.DeepEqual(m1, GenPyMutants(files, "s", "p", 10)) {
		t.Fatal("not deterministic")
	}
	if len(m1) != 4 || m1[0].ID != "p-01" { // one per line: a.py line 2; b.py lines 2, 3, 4
		t.Fatalf("%+v", m1)
	}
	for _, m := range m1 {
		out, err := ApplyEdit(files[map[string]int{"src/a.py": 0, "src/b.py": 1}[m.File]].Data, m)
		if err != nil || !strings.Contains(string(out), m.Repl) {
			t.Fatalf("%+v %v", m, err)
		}
	}
}

func TestPyCandidateFile(t *testing.T) {
	for f, want := range map[string]bool{"src/click/core.py": true, "tests/test_x.py": false, "src/pkg/test_helpers.py": false, "docs/conf.py": false,
		"conftest.py": false, "setup.py": false, "examples/a/b.py": false, "src/pkg/data.txt": false, "src/attr/_make.py": true} {
		if pyCandidateFile(f) != want {
			t.Errorf("%s", f)
		}
	}
}

func TestParsePyJUnit(t *testing.T) {
	known := map[string]bool{"tests/test_a.py": true, "tests/sub/test_b.py": true, "tests/test_c.py": true}
	xml := `<testsuites><testsuite>
<testcase classname="tests.test_a.TestX" name="t1" file="tests\test_a.py"/>
<testcase classname="tests.test_a.TestX" name="t2" file="tests\test_a.py"><failure message="boom"/></testcase>
<testcase classname="tests.sub.test_b" name="t1"/>
<testcase classname="" name="tests.test_c" file="tests/test_c.py"><error message="collection failure"/></testcase>
<testcase classname="tests.test_c" name="t" file="tests/test_c.py"><skipped/></testcase>
</testsuite></testsuites>`
	run, err := parsePyJUnit("D:/w/repo", []byte(xml), known)
	want := map[string]string{"tests/test_a.py": "failed", "tests/sub/test_b.py": "passed", "tests/test_c.py": "failed"}
	if err != nil || !reflect.DeepEqual(run.Files, want) {
		t.Fatalf("%v %v", run.Files, err)
	}
	if _, err := parsePyJUnit("D:/w/repo", []byte(`<testsuite><testcase classname="x" name="y"><failure/></testcase></testsuite>`), known); err == nil {
		t.Fatal("an unmappable failure must be an error")
	}
}

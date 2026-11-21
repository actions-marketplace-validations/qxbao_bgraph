package python

import (
	"reflect"
	"strings"
	"testing"
)

var fuzzSeeds = []string{
	"import a.b.c as x, d\nfrom . import y\nfrom ..p import (q,\n r as s)\nfrom m import *\n",
	"def f():\n    import inner\n    try:\n        from x import y\n    except ImportError:\n        pass\nif TYPE_CHECKING:\n    import t\n",
	"importlib.import_module('a'); __import__('b'); import_module(f'p.{x}'); import_module('a' + n); pkgutil.walk_packages(p)",
	"mock.patch('a.b'); patch.object('c.D', 'x'); monkeypatch.setattr('e.f', 1)\npytest_plugins = ['a', 'b']\nINSTALLED_APPS = ['x']",
	"sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'src'))\nsys.path.append(str(Path(__file__).parent))\nsys.path = []",
	"a = f'{x!r:>{w}}' f\"{d[\"k\"]}\" f'{{}}' rf'\\d{x}' fr\"{y}\" b'x' rb'y' u'z'\nimport real",
	"x = '''a\n''' + \"\"\"b\"\"\" + 'c\\\nd'\nimport a",
	"s = f'''{\n  f\"{g(\"x\")}\"\n}'''\nimport a",
	"def f():\n    \"\"\"\n    >>> import a\n    >>> from b import (c, d)\n    \"\"\"\n",
	"\xef\xbb\xbfimport a\r\nimport b\r\n",
	"x = 1_000 + 0xFF + 1e-5 + .5j + 1.\n",
	"if x: import a\nelse: from b import c\nx = 1; import d",
	"lazy import a\nlazy from b import c",
	"'unterminated", "f'{", "f'{x:{", "\"\"\"unterminated", "((((", "))))", "{[(}])", "\\", "\\\n", "from", "from .", "from . import", "import", "import a.", "import a as",
	"from a import (", "from a import b,", "importlib.import_module(", "sys.path.insert(", "pytest_plugins =", "\x00", "\xff\xfe", "é", "rb", "f", "r'",
	"sys.path.insert(0, os.path.join(", "Path(__file__).parents[", "x.parents[99999999999999999999]",
}

func FuzzScan(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		a := Scan("a.py", []byte(src))
		b := Scan("a.py", []byte(src))
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("non-deterministic: %+v vs %+v", a, b)
		}
		if a.Uncertain != (a.UncertainReason != "") {
			t.Fatalf("inconsistent uncertain flag: %+v", a)
		}
		if a.Imports == nil {
			t.Fatal("nil Imports")
		}
		maxLine := strings.Count(src, "\n") + 1
		for _, r := range a.Imports {
			if r.Kind == "" || r.Line < 1 || r.Line > maxLine {
				t.Fatalf("bad ref %+v (max line %d)", r, maxLine)
			}
			if r.Dynamic && r.Suffix != "" {
				t.Fatalf("suffix on a dynamic ref: %+v", r)
			}
		}
	})
}

// Every seed must run without panicking (this also runs when -fuzz is not enabled).
func TestFuzzSeeds(t *testing.T) {
	for _, s := range fuzzSeeds {
		Scan("a.py", []byte(s))
	}
}

// Property: inserting a comment, a blank line or spaces between tokens of a valid source (outside strings) must not change what is found.
func TestCommentInsertionInvariant(t *testing.T) {
	base := "import a.b as c, d\nfrom ..p import (q, r)\nx = f'{y!r}' + 'z'\nif t:\n    import e\nimportlib.import_module('m')\nmock.patch('a.b')\nsys.path.insert(0, os.path.dirname(__file__))\n"
	want := Scan("a.py", []byte(base))
	if want.Uncertain || len(want.Imports) == 0 {
		t.Fatalf("%+v", want)
	}
	lx, _ := lex([]byte(base))
	for _, tk := range lx.toks {
		if tk.kind == tNewline {
			continue
		}
		for _, ins := range []string{" ", "  ", "# c\n"} {
			if ins == "# c\n" && tk.sstart == false && tk.kind != tName {
				// a comment line break inside a statement is only valid inside brackets: insert only at statement starts
				continue
			}
			if ins == "# c\n" && !tk.sstart {
				continue
			}
			s := base[:tk.start] + ins + base[tk.start:]
			got := Scan("a.py", []byte(s))
			if got.Uncertain || len(got.Imports) != len(want.Imports) {
				t.Fatalf("insert %q at %d changed the result: %+v vs %+v", ins, tk.start, got, want)
			}
			for i := range got.Imports {
				g, w := got.Imports[i], want.Imports[i]
				g.Line, w.Line = 0, 0
				if g != w {
					t.Fatalf("insert %q at %d changed ref %d: %+v vs %+v", ins, tk.start, i, g, w)
				}
			}
		}
	}
}

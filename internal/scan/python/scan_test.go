package python

import (
	"fmt"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func refs(t *testing.T, src string) []string {
	t.Helper()
	r := Scan("a.py", []byte(src))
	if r.Uncertain {
		t.Fatalf("uncertain: %s\n%s", r.UncertainReason, src)
	}
	var out []string
	for _, im := range r.Imports {
		s := fmt.Sprintf("%s:%s", im.Kind, im.Spec)
		if im.Suffix != "" {
			s += "{" + im.Suffix + "}"
		}
		if im.Dynamic {
			s += "!dyn"
		}
		out = append(out, s)
	}
	return out
}

func TestImports(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{"import a.b.c", "import:a.b.c"},
		{"import a.b as x, c, d.e as y", "import:a.b import:c import:d.e"},
		{"from a.b import c, d", "import:a.b{c,d}"},
		{"from . import x", "import:.{x}"},
		{"from ..a import b as c, d", "import:..a{b,d}"},
		{"from ... import x", "import:...{x}"},
		{"from x import *", "import:x{*}"},
		{"from x import (a,\n    b as c,\n)", "import:x{a,b}"},
		{"def f():\n    import inner\n    from p import q", "import:inner import:p{q}"},
		{"try:\n    import ujson\nexcept ImportError:\n    ujson = None", "import:ujson"},
		{"if TYPE_CHECKING:\n    from a import B", "import:a{B}"},
		{"if x: import y", "import:y"},
		{"import a; import b", "import:a import:b"},
		{"x = 1; from a import b", "import:a{b}"},
		{"def g():\n    yield from other\n    raise E from e", ""},
		{"lazy import a.b\nlazy from c import d", "import:a.b import:c{d}"},
		{"# import nope\ns = 'import nope'\nt = \"\"\"\nimport nope\nfrom x import y\n\"\"\"", ""},
		{"import a \\\n  , b", "import:a import:b"},
		{"from a import b \\\n  , c", "import:a{b,c}"},
		{"import é.ü", "import:é.ü"},
		{"x.import_thing(1)\nobj.from_x(2)", ""},
	}
	for _, c := range cases {
		if got := strings.Join(refs(t, c.src), " "); got != c.want {
			t.Errorf("%q:\n got  %q\n want %q", c.src, got, c.want)
		}
	}
}

func TestDynamicAndStringRefs(t *testing.T) {
	cases := []struct{ src, want string }{
		{`importlib.import_module("a.b")`, "dynamic:a.b"},
		{`import_module('a')`, "dynamic:a"},
		{`importlib.import_module("a." + name)`, "dynamic:a.!dyn"},
		{`importlib.import_module(f"pkg.plugins.{name}")`, "dynamic:pkg.plugins.!dyn"},
		{`importlib.import_module("pkg.%s" % name)`, "dynamic:pkg.!dyn"},
		{`importlib.import_module("pkg.{}".format(name))`, "dynamic:pkg.!dyn"},
		{`importlib.import_module(name)`, "dynamic:!dyn"},
		{`importlib.import_module(".x", package="p.q")`, "dynamic:p.q.x"},
		{`importlib.import_module("..x", "p.q.r")`, "dynamic:p.q.x"},
		{`importlib.import_module(".x", __package__)`, "dynamic:.x"},
		{`importlib.import_module(".x", pkg)`, "dynamic:!dyn"},
		{`__import__("a.b")`, "dynamic:a.b"},
		{`__import__(n)`, "dynamic:!dyn"},
		{`pytest.importorskip("numpy")`, "require:numpy"},
		{`pkgutil.walk_packages(path)`, "dynamic:!dyn"},
		{`pkgutil.iter_modules()`, "dynamic:!dyn"},
		{`mock.patch("a.b.c")`, "mock:a.b.c"},
		{`@patch('a.b.c', new=1)`, "mock:a.b.c"},
		{`patch.object("a.B", "x")`, "mock:a.B"},
		{`mock.patch.dict("a.b.SETTINGS", {})`, "mock:a.b.SETTINGS"},
		{`monkeypatch.setattr("a.b.c", 1)`, "mock:a.b.c"},
		{`monkeypatch.setattr(obj, "attr", 1)`, ""},
		{`requests.patch("https://x.io/a")`, ""},
		{`foo.object("a.b")`, ""},
		{"pytest_plugins = [\"a.b\", 'c']", "plugin:a.b plugin:c"},
		{"pytest_plugins = 'x'", "plugin:x"},
		{"pytest_plugins: list[str] = ['x']", "plugin:x"},
		{"pytest_plugins = [\"a\"] + more", "plugin:a plugin:!dyn"},
		{"INSTALLED_APPS = [\n  'django.contrib.auth',\n  'myapp.apps.Cfg',\n]", "mock:django.contrib.auth mock:myapp.apps.Cfg"},
		{"INSTALLED_APPS += ['x']\nROOT_URLCONF = 'proj.urls'", "mock:x mock:proj.urls"},
		{`def f():` + "\n    \"\"\"\n    >>> import a.b, c as d\n    >>> from x import (y, z)\n    \"\"\"", "mock:a.b mock:c mock:x{y,z}"},
	}
	for _, c := range cases {
		if got := strings.Join(refs(t, c.src), " "); got != c.want {
			t.Errorf("%q:\n got  %q\n want %q", c.src, got, c.want)
		}
	}
}

func TestSysPath(t *testing.T) {
	cases := []struct{ src, want string }{
		{`sys.path.insert(0, os.path.dirname(__file__))`, "sys_path:."},
		{`sys.path.insert(0, os.path.abspath(os.path.dirname(__file__)))`, "sys_path:."},
		{`sys.path.append(os.path.join(os.path.dirname(__file__), "..", "src"))`, "sys_path:../src"},
		{`sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), '../..'))`, "sys_path:../.."},
		{`sys.path.insert(0, str(Path(__file__).parent))`, "sys_path:."},
		{`sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "lib"))`, "sys_path:../lib"},
		{`sys.path.append(str(Path(__file__).parents[2]))`, "sys_path:../.."},
		{`sys.path.insert(0, os.path.dirname(__file__) + "/../x")`, "sys_path:../x"},
		{`sys.path.insert(0, "src")`, "sys_path:cwd:src"},
		{`sys.path.insert(0, "./a/../b")`, "sys_path:cwd:b"},
		{`sys.path.insert(0, "/abs/path")`, "sys_path:!dyn"},
		{`sys.path.insert(0, os.getcwd())`, "sys_path:!dyn"},
		{`sys.path.insert(0, compute())`, "sys_path:!dyn"},
		{`sys.path.insert(0, os.environ["X"])`, "sys_path:!dyn"},
		{`sys.path.extend([os.path.dirname(__file__), "lib"])`, "sys_path:. sys_path:cwd:lib"},
		{`site.addsitedir(os.path.dirname(__file__))`, "sys_path:."},
		{"sys.path = [x] + sys.path", "sys_path:!dyn"},
		{"sys.path += [x]", "sys_path:!dyn"},
		{"sys.path[0:0] = [x]", "sys_path:!dyn"},
		{`other.path.insert(0, ".")`, ""},
		{`x = sys.path[0]`, ""},
	}
	for _, c := range cases {
		if got := strings.Join(refs(t, c.src), " "); got != c.want {
			t.Errorf("%q:\n got  %q\n want %q", c.src, got, c.want)
		}
	}
}

func TestStrings(t *testing.T) {
	cases := []struct{ src, want string }{
		// Strings of every form hide their content.
		{`a = r"import x" + b"import y" + u'import z' + rb'import w' + Rb"q" + F"{x}" + '''import v''' + """from q import r"""`, ""},
		{"a = 'it\\'s'\nimport real", "import:real"},
		{"a = \"\\\\\"\nimport real", "import:real"},
		{"a = r'\\d' + r'\\''\nimport real\nb = 'x'", "import:real"},
		{"a = 'multi\\\nline'\nimport real", "import:real"},
		// f-strings: nesting, same quotes (3.12), format specs, escapes of braces.
		{"a = f'{x}' f\"{y!r:>{w}}\" f'{{import no}}'\nimport real", "import:real"},
		{"a = f\"{d[\"k\"]}\"\nimport real", "import:real"},
		{"a = f'{f\"{g('x')}\"}'\nimport real", "import:real"},
		{"a = f'''{\n  x\n  }'''\nimport real", "import:real"},
		{"a = f'{x:{'a'}}' \nimport real", "import:real"},
		{"a = f'{x!r}{y=}{a != b}'\nimport real", "import:real"},
		{"a = f'{lambda_: 1}'\nimport real", "import:real"},
		{"a = f'\\{x}'\nimport real", "import:real"},
		{"a = rf'\\d{x}'\nimport real", "import:real"},
		// brackets and continuation
		{"x = [\n  1,\n  2]\nimport real", "import:real"},
		{"x = (1 +\n  2) \\\n + 3\nimport real", "import:real"},
		{"if True:\n\timport a\nelse:\n\timport b", "import:a import:b"},
		{"\ufeffimport a", "import:a"},
		{"import a\r\nimport b\r\n", "import:a import:b"},
		{"print(1.5e-3, 0xFFj, 1_000, .5)\nimport a", "import:a"},
	}
	for _, c := range cases {
		if got := strings.Join(refs(t, c.src), " "); got != c.want {
			t.Errorf("%q:\n got  %q\n want %q", c.src, got, c.want)
		}
	}
}

func TestLines(t *testing.T) {
	r := Scan("a.py", []byte("\"\"\"doc\nmore\n\"\"\"\nimport a\n\nx = f'''\n{y}\n'''\nfrom b import c\n"))
	if len(r.Imports) != 2 || r.Imports[0].Line != 4 || r.Imports[1].Line != 9 {
		t.Fatalf("%+v", r.Imports)
	}
}

func TestUncertain(t *testing.T) {
	for _, src := range []string{"x = 'abc\nimport a", "x = \"\"\"abc", "x = f'{a", "foo(1, 2", "x = [1,\n 2", "x = f'abc\n'"} {
		r := Scan("a.py", []byte(src))
		if !r.Uncertain || r.UncertainReason == "" || r.Imports == nil {
			t.Errorf("%q: want uncertain, got %+v", src, r)
		}
	}
	// Stray closing brackets (a Python 2 print >>f, or a syntax error) do not make a file uncertain.
	if r := Scan("a.py", []byte("x = 1)\nimport a")); r.Uncertain || len(r.Imports) != 1 {
		t.Errorf("%+v", r)
	}
}

func TestDeterministicAndDeep(t *testing.T) {
	deep := strings.Repeat("(", 10000) + strings.Repeat(")", 10000) + "\nimport a"
	if r := Scan("a.py", []byte(deep)); r.Uncertain || len(r.Imports) != 1 {
		t.Fatalf("deep brackets: %+v", r)
	}
	nested := "x = " + strings.Repeat("f'{", 200) + "1" + strings.Repeat("}'", 200)
	r := Scan("a.py", []byte(nested))
	if !r.Uncertain {
		t.Fatalf("200-deep f-string nesting should be uncertain, got %+v", r)
	}
	var _ model.ScanResult = r
}

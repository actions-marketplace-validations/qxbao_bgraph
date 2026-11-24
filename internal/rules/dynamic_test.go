package rules

import (
	"strings"
	"testing"
)

func TestDynamicParses(t *testing.T) {
	f, err := Parse("x", []byte(`
[[dynamic]]
file = "pkg/loader.py"
lines = [3, 5]
loads = ["plugins/**/*.py"]
reason = "r"

[[dynamic]]
file = "a/b.py"
none = true
reason = "r"

[[dynamic]]
file = "c.py"
sys_path_only = true
reason = "r"
`))
	if err != nil || len(f.Dynamics) != 3 {
		t.Fatalf("%v %+v", err, f)
	}
	d := f.Dynamics[0]
	if d.File != "pkg/loader.py" || len(d.Lines) != 2 || d.Lines[1] != 5 || len(d.Loads) != 1 || d.RawLoads[0] != "plugins/**/*.py" {
		t.Errorf("%+v", d)
	}
	if !f.Dynamics[1].None || !f.Dynamics[2].SysPathOnly {
		t.Errorf("%+v", f.Dynamics)
	}
}

func TestDynamicErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"no loads", "file = \"a.py\"\nreason = \"r\"", "loads"},
		{"no reason", "file = \"a.py\"\nnone = true", "reason"},
		{"no file", "none = true\nreason = \"r\"", "file"},
		{"none and loads", "file = \"a.py\"\nnone = true\nloads = [\"x\"]\nreason = \"r\"", "exclusive"},
		{"bad line", "file = \"a.py\"\nlines = [0]\nnone = true\nreason = \"r\"", "lines"},
		{"outside the repo", "file = \"../a.py\"\nnone = true\nreason = \"r\"", "file"},
		{"unknown key", "file = \"a.py\"\nnone = true\nreason = \"r\"\nbogus = 1", "bogus"},
		{"sys_path_only with lines", "file = \"a.py\"\nsys_path_only = true\nlines = [1]\nreason = \"r\"", "sys_path_only"},
		{"bad glob", "file = \"a.py\"\nloads = [\"[\"]\nreason = \"r\"", "loads"},
	}
	for _, c := range cases {
		_, err := Parse("x", []byte("[[dynamic]]\n"+c.src))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want it to mention %q", c.name, err, c.want)
		}
	}
}

func TestDynamicLayer(t *testing.T) {
	f, err := Parse("x", []byte(`
[[dynamic]]
file = "c.py"
sys_path_only = true
reason = "r"

[dynamic_layer]
ecosystems = ["python", "python", "node", "ruby", "php"]
python = ".venv/Scripts/python.exe"
node = "tools/node.exe"
ruby = "C:/Ruby34/bin/ruby.exe"
php = "/usr/bin/php8.3"
trust_native_env = true
`))
	if err != nil {
		t.Fatal(err)
	}
	if d := f.Dynamic; len(d.Ecosystems) != 4 || d.Ecosystems[0] != "python" || d.Ecosystems[1] != "node" || d.Ecosystems[2] != "ruby" ||
		d.Ecosystems[3] != "php" || d.PHP != "/usr/bin/php8.3" ||
		d.Python != ".venv/Scripts/python.exe" || d.Node != "tools/node.exe" || d.Ruby != "C:/Ruby34/bin/ruby.exe" || !d.TrustNativeEnv ||
		len(f.Dynamics) != 1 {
		t.Fatalf("%+v", f)
	}
	f, err = Parse("x", nil)
	if err != nil || len(f.Dynamic.Ecosystems) != 0 || f.Dynamic.Python != "" || f.Dynamic.TrustNativeEnv {
		t.Fatalf("default: %v %+v", err, f.Dynamic)
	}
	for src, want := range map[string]string{
		"[dynamic_layer]\necosystems = [\"rust\"]": "rust",
		"[dynamic_layer]\nnode = \"node \"":        "node",
		"[dynamic_layer]\nbogus = 1":               "bogus",
		"[dynamic_layer]\nruby = \"ruby \"":        "ruby",
		"[dynamic_layer]\nphp = \"php \"":          "php",
		"[dynamic_layer]\npython = \" python\"":    "python",
	} {
		if _, err := Parse("x", []byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", src, err)
		}
	}
}

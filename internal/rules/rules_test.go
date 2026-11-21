package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const valid = `
[[rule]]
paths = ["proto/**/*.proto"]
affects = ["services/api", "web/src/gen/**"]
reason = "proto"

[[rule]]
paths = ["docs/**", "*.md"]
affects = "none"

[[rule]]
paths = ["proto/**"]
affects = "all"

[[generated]]
outputs = ["packages/ui/dist/**"]
sources = ["packages/ui/src/**"]
owner = "packages/ui"

[options]
max_changed = 100
`

func TestParseValid(t *testing.T) {
	f, err := Parse("bgraph.toml", []byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Rules) != 3 || len(f.Generated) != 1 || f.Options.MaxChanged != 100 {
		t.Fatalf("%+v", f)
	}
	r := f.Rules[0]
	if r.Index != 0 || r.Reason != "proto" || len(r.Affects) != 2 || r.Affects[1] != "web/src/gen/**" || len(r.Paths) != 1 {
		t.Errorf("rule0 %+v", r)
	}
	if !f.Rules[1].AffectsNone || f.Rules[1].AffectsAll || len(f.Rules[1].RawPaths) != 2 {
		t.Errorf("rule1 %+v", f.Rules[1])
	}
	if !f.Rules[2].AffectsAll || f.Rules[2].Index != 2 {
		t.Errorf("rule2 %+v", f.Rules[2])
	}
	g := f.Generated[0]
	if g.Owner != "packages/ui" || len(g.Sources) != 1 || g.RawOutputs[0] != "packages/ui/dist/**" {
		t.Errorf("gen %+v", g)
	}
}

func TestDefaults(t *testing.T) {
	f, err := Parse("x", []byte(""))
	if err != nil || f.Options.MaxChanged != 5000 || len(f.Rules) != 0 {
		t.Fatalf("%v %+v", err, f)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct{ name, src, want string }{
		{"unknown top key", "foo = 1", "foo"},
		{"unknown rule key", "[[rule]]\npaths=[\"a\"]\naffects=\"all\"\nbogus=1", "bogus"},
		{"unknown option", "[options]\nmax = 1", "max"},
		{"syntax", "[[rule", "bgraph.toml"},
		{"no paths", "[[rule]]\naffects=\"all\"", "rule[0]: 'paths'"},
		{"empty paths", "[[rule]]\npaths=[]\naffects=\"all\"", "rule[0]: 'paths'"},
		{"no affects", "[[rule]]\npaths=[\"a\"]", "rule[0]: 'affects'"},
		{"bad affects string", "[[rule]]\npaths=[\"a\"]\naffects=\"services\"", "rule[0].affects"},
		{"empty affects array", "[[rule]]\npaths=[\"a\"]\naffects=[]", "rule[0].affects"},
		{"affects wrong type", "[[rule]]\npaths=[\"a\"]\naffects=3", "rule[0].affects"},
		{"affects mixed", "[[rule]]\npaths=[\"a\"]\naffects=[\"x\", 3]", "rule[0].affects"},
		{"bad glob paths", "[[rule]]\npaths=[\"a\"]\naffects=\"all\"\n[[rule]]\npaths=[\"[a\"]\naffects=\"all\"", "rule[1].paths"},
		{"bad glob affects", "[[rule]]\npaths=[\"a\"]\naffects=[\"{x\"]", "rule[0].affects"},
		{"gen no outputs", "[[generated]]\nowner=\"a\"", "generated[0]: 'outputs'"},
		{"gen no owner", "[[generated]]\noutputs=[\"a/**\"]", "generated[0]: 'owner'"},
		{"gen bad glob", "[[generated]]\noutputs=[\"[\"]\nowner=\"a\"", "generated[0].outputs"},
		{"gen bad src", "[[generated]]\noutputs=[\"a\"]\nsources=[\"[\"]\nowner=\"a\"", "generated[0].sources"},
		{"max zero", "[options]\nmax_changed = 0", "max_changed"},
		{"max neg", "[options]\nmax_changed = -3", "max_changed"},
	}
	for _, tc := range tests {
		_, err := Parse("bgraph.toml", []byte(tc.src))
		if err == nil {
			t.Errorf("%s: want error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q lacks %q", tc.name, err, tc.want)
		}
	}
}

func TestMatchOrdering(t *testing.T) {
	f, _ := Parse("x", []byte(valid))
	got := f.Match("proto/a/b.proto")
	if len(got) != 2 || got[0].Index != 0 || got[1].Index != 2 {
		t.Errorf("got %+v", got)
	}
	got = f.Match("README.md")
	if len(got) != 1 || !got[0].AffectsNone {
		t.Errorf("got %+v", got)
	}
	if len(f.Match("src/a.ts")) != 0 {
		t.Error("want none")
	}
	// *.md only at the root
	if len(f.Match("sub/README.md")) != 0 {
		t.Error("*.md must not match in subdirectories")
	}
}

func TestGeneratedFor(t *testing.T) {
	f, _ := Parse("x", []byte(valid))
	if g := f.GeneratedFor("packages/ui/dist/index.js"); g == nil || g.Owner != "packages/ui" {
		t.Errorf("%v", g)
	}
	if f.GeneratedFor("packages/ui/src/index.ts") != nil {
		t.Error("want nil")
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	f, err := Load(dir)
	if err != nil || f.Path != "" || f.Options.MaxChanged != 5000 {
		t.Fatalf("missing: %v %+v", err, f)
	}
	p := filepath.Join(dir, "bgraph.toml")
	if err := os.WriteFile(p, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err = Load(dir)
	if err != nil || len(f.Rules) != 3 || f.Path != p {
		t.Fatalf("%v %+v", err, f)
	}
	if err := os.WriteFile(p, []byte("[[rule]]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("broken file must error")
	}
}

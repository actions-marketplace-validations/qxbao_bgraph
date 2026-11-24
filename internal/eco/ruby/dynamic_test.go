package ruby

import (
	"context"
	"slices"
	"testing"

	"github.com/qxbao/bgraph/internal/eco/python"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/tree"
)

// [[dynamic]] declarations: stale ones of Ruby files are reported by Ruby; Python stays silent about Ruby files and keeps its
// own reports; sys_path_only is inert for Ruby.
func TestDynamicDeclarationReports(t *testing.T) {
	rf, err := rules.Parse(rules.FileName, []byte(`
[[dynamic]]
file = "lib/a.rb"
none = true
reason = "used"
[[dynamic]]
file = "lib/b.rb"
none = true
reason = "stale: no computed require there"
[[dynamic]]
file = "lib/a.rb"
sys_path_only = true
reason = "python-only option"
[[dynamic]]
file = "pkg/x.py"
none = true
reason = "python's"
`))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"Gemfile": "", "lib/a.rb": "require name", "lib/b.rb": "require 'x'", "spec/a_spec.rb": "",
		"pyproject.toml": "[project]\nname='x'\nversion='1'", "pkg/__init__.py": "", "pkg/x.py": "import os"}
	tr := tree.NewMapTree(files)
	a, err := New(rf).Analyze(context.Background(), tr, scancache.New())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range a.Unresolved {
		if u.File == rules.FileName {
			got = append(got, u.Spec)
		}
		if u.File == "lib/a.rb" && u.Class == model.UnresolvedDynamic {
			t.Fatalf("declared site still fires: %+v", u)
		}
	}
	slices.Sort(got)
	if want := []string{`[[dynamic]] #2 file = "lib/b.rb"`, `[[dynamic]] #3 file = "lib/a.rb"`}; !slices.Equal(got, want) {
		t.Fatalf("ruby reports %q, want %q", got, want)
	}
	pa, err := python.New(rf).Analyze(context.Background(), tr, scancache.New())
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	for _, u := range pa.Unresolved {
		if u.File == rules.FileName {
			got = append(got, u.Spec)
		}
	}
	if want := []string{`[[dynamic]] #4 file = "pkg/x.py"`}; !slices.Equal(got, want) {
		t.Fatalf("python reports %q, want %q (Ruby files are Ruby's)", got, want)
	}
}

package selector_test

import (
	"context"
	"testing"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/python"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/selector"
	"github.com/qxbao/bgraph/internal/tree"
)

// TestWidenedTag checks the precise/widened classification of docs/dynamic.md §1 and the classification details of Run.
func TestWidenedTag(t *testing.T) {
	base := map[string]string{
		"pyproject.toml":          "[project]\nname = \"x\"\nversion = \"1\"\n",
		"pkg/__init__.py":         "",
		"pkg/loader.py":           "import importlib\ndef load(n):\n    return importlib.import_module(n)\n",
		"pkg/a.py":                "X = 1\n",
		"tests/test_loader.py":    "from pkg.loader import load\n",
		"tests/test_a.py":         "import pkg.a\n",
		"tests/test_failed.py":    "import json\n",
		"tests/test_loader2.py":   "import pkg.loader\n",
		"tests/test_unrelated.py": "import json\n",
	}
	head := map[string]string{}
	for k, v := range base {
		head[k] = v
	}
	head["pkg/a.py"] = "X = 2\n"
	res := selector.Run(context.Background(), selector.Input{
		Base: "b", Head: "h", BaseTree: tree.NewMapTree(base), HeadTree: tree.NewMapTree(head),
		Changed:          []selector.Changed{{Path: "pkg/a.py", Status: 'M', OldMode: "100644", NewMode: "100644"}},
		Ecos:             []eco.Ecosystem{python.New(nil)},
		Scanner:          scancache.New(),
		PreviouslyFailed: []string{"tests/test_failed.py"},
	})
	want := map[string]bool{ // unit → widened
		"tests/test_a.py":       false,
		"tests/test_failed.py":  false,
		"tests/test_loader.py":  true,
		"tests/test_loader2.py": true,
	}
	got := map[string]bool{}
	for _, st := range res.Selection.Tests {
		got[st.Unit] = st.Widened
	}
	if len(got) != len(want) {
		t.Fatalf("selected %v, want %v", got, want)
	}
	for u, w := range want {
		if g, ok := got[u]; !ok || g != w {
			t.Errorf("%s: widened=%v (selected %v), want %v", u, g, ok, w)
		}
		if res.UnitEco[u] != python.Name {
			t.Errorf("%s: eco %q", u, res.UnitEco[u])
		}
	}
	if len(res.Changes) != 1 {
		t.Fatalf("changes: %+v", res.Changes)
	}
	c := res.Changes[0]
	if c.Path != "pkg/a.py" || c.Roles[python.Name] != [2]model.Role{model.RoleSource, model.RoleSource} || c.Global || c.Rule || len(c.Claimed) > 0 {
		t.Errorf("change info: %+v", c)
	}
}

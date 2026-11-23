package explain

import (
	"slices"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func TestLines(t *testing.T) {
	sel := model.Selection{
		Changed: []string{"src/a.ts", "src/lonely.ts"},
		Tests:   []model.SelectedTest{{Unit: "src/a.test.ts", Files: []string{"src/a.test.ts"}, Reasons: []string{"src/a.test.ts --import-> src/a.ts ; src/a.ts (changed)"}}},
	}
	cases := map[string][]string{
		`.\src\a.test.ts`: {"test src/a.test.ts is selected because:", "  - src/a.test.ts --import-> src/a.ts ; src/a.ts (changed)"},
		"src/a.ts":        {"file src/a.ts appears in the reasons of:", "  src/a.test.ts: src/a.test.ts --import-> src/a.ts ; src/a.ts (changed)", "src/a.ts is a changed file"},
		"src/lonely.ts":   {"src/lonely.ts is a changed file but no test was selected because of it"},
		"nothing.ts":      {"no selected test is related to nothing.ts"},
	}
	for target, want := range cases {
		if got := Lines(sel, target); !slices.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", target, got, want)
		}
	}
}

func TestLinesDynamic(t *testing.T) {
	sel := model.Selection{
		Changed: []string{"pkg/a.py"},
		Tests: []model.SelectedTest{
			{Unit: "tests/test_l.py", Files: []string{"tests/test_l.py"}, Reasons: []string{"r"}, Widened: true,
				Dynamic: "kept by the dynamic layer: recorded dependencies include the changed file pkg/a.py"},
			{Unit: "tests/test_w.py", Files: []string{"tests/test_w.py"}, Reasons: []string{"w"}, Widened: true},
		},
		Narrowed: []model.NarrowedTest{{Unit: "tests/test_b.py", Commit: "0123456789abcdef", Reason: "record by c: none"}},
	}
	if got := Lines(sel, "tests/test_l.py"); len(got) != 3 || got[2] != "  "+sel.Tests[0].Dynamic {
		t.Errorf("kept: %q", got)
	}
	if got := Lines(sel, "tests/test_b.py"); len(got) != 2 || got[0] != "test tests/test_b.py was selected but removed by the dynamic layer:" {
		t.Errorf("narrowed: %q", got)
	}
	sel.Warnings = []model.Widening{{Reason: model.ReasonDynamicSkipped, Detail: "python: pkg/n.py: status A"}}
	if got := Lines(sel, "tests/test_w.py"); len(got) != 3 || got[2] != "  the dynamic layer did not narrow this selection: python: pkg/n.py: status A" {
		t.Errorf("skipped: %q", got)
	}
}

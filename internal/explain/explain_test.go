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

package dotnetdyn

import (
	"slices"
	"strings"
	"testing"
)

// A class that failed in the previous run (its sub-unit ID in always_run, from --failed) is kept, and a failed project ID keeps
// every class.
func TestNarrowKeepsPreviouslyFailed(t *testing.T) {
	base := treeOf(baseFiles)
	head := with(baseFiles, map[string]string{"src/Lib/Foo.cs": strings.Replace(baseFiles["src/Lib/Foo.cs"], "return 1", "return 3", 1)})
	p := Params{Env: "env", Records: recordsAt(base, "env"), Base: base, Head: head, UnitEco: map[string]string{unitT: Eco}}
	s := sel("src/Lib/Foo.cs")
	s.AlwaysRun = []string{unitT + "#T.BarTests"}
	out := Narrow(s, p)
	if tt := out.Tests[0]; tt.ClassFilter && !slices.Contains(tt.Classes, "T.BarTests") {
		t.Errorf("failed class removed: %+v", tt)
	}
	if len(out.Narrowed) != 0 {
		t.Errorf("narrowed %+v", out.Narrowed)
	}
	s.AlwaysRun = []string{unitT}
	if tt := Narrow(s, p).Tests[0]; tt.ClassFilter {
		t.Errorf("failed project narrowed: %+v", tt)
	}
}

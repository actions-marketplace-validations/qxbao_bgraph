package jvmdyn

import (
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/tree"
)

// A class that failed in the previous run (its sub-unit ID in always_run, from --failed) is kept, and a failed module ID keeps
// every class.
func TestNarrowKeepsPreviouslyFailed(t *testing.T) {
	base := tree.NewMapTree(baseFiles)
	deps := map[string][]string{
		"a.FooTest": {"core/src/test/java/a/FooTest.java", "core/src/main/java/a/Foo.java"},
		"a.BarTest": {"core/src/test/java/a/BarTest.java", "core/src/main/java/a/Bar.java"},
	}
	p := Params{Collector: "jvm-agent-2", Env: "env", Records: classRecords(base, "env", time.Now(), deps), Base: base, Head: base,
		UnitEco: map[string]string{"maven:core": Eco}}
	s := sel("core/src/main/java/a/Foo.java")
	s.AlwaysRun = []string{"maven:core#a.BarTest"}
	out := Narrow(s, p)
	if got := out.Tests[0]; got.ClassFilter && !strings.Contains(strings.Join(got.Classes, ","), "a.BarTest") || len(out.Narrowed) != 0 {
		t.Errorf("failed class removed: %+v %+v", got, out.Narrowed)
	}
	s.AlwaysRun = []string{"maven:core"}
	if got := Narrow(s, p).Tests[0]; got.ClassFilter {
		t.Errorf("failed module narrowed: %+v", got)
	}
}

package evalkit

import (
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func TestGoCountTests(t *testing.T) {
	src := `package x

import "testing"

func TestMain(m *testing.M) {}
func TestA(t *testing.T)   {}
func Testlower(t *testing.T) {}
func Test(t *testing.T)    {}
func FuzzB(f *testing.F)   {}
func BenchmarkC(b *testing.B) {}
func helper()             {}
func (s S) TestMethod(t *testing.T) {}

func ExampleOut() {
	// Output: x
}

func ExampleEmpty() {
	// Output:
}

func ExampleNone() {}
`
	if got := goCountTests("x_test.go", []byte(src)); got != 5 { // TestA, Test, FuzzB, ExampleOut, ExampleEmpty
		t.Fatalf("got %d, want 5", got)
	}
	if got := goCountTests("x_plan9_test.go", []byte(src)); got != 0 {
		t.Fatalf("plan9 file: got %d, want 0", got)
	}
	if got := goCountTests("y_test.go", []byte("//go:build ignore\n\n"+src)); got != 0 {
		t.Fatalf("ignored file: got %d, want 0", got)
	}
}

func TestGoSummarize(t *testing.T) {
	sel := model.Selection{
		Tests: []model.SelectedTest{
			{Unit: "go:a", ClassFilter: true, Classes: []string{"TestA"}, Dynamic: "Go layer: 1 of 3 test functions run"},
			{Unit: "go:b", ClassFilter: true, Dynamic: "Go layer: none of 2 test functions runs; the package is still compiled and vetted (go test -run '^$')"},
			{Unit: "go:c"},
		},
		Narrowed: []model.NarrowedTest{{Unit: "go:a#TestB"}, {Unit: "go:a#TestC"}, {Unit: "go:b#TestX"}, {Unit: "go:b#TestY"}},
	}
	counts := map[string]int{"go:a": 3, "go:b": 2, "go:c": 4}
	d := goSummarize(sel, counts, []string{"go:a", "go:c", "go:d"}, map[string][]string{"go:a": {"TestA", "TestB"}, "go:c": {"TestZ"}})
	if d.Packages != 3 || d.RunPackages != 2 || d.CompileOnly != 1 || d.Tests != 5 || len(d.Removed) != 4 {
		t.Fatalf("summary %+v", d)
	}
	want := []string{"go:a#TestB", "go:d"}
	if len(d.Missed) != 2 || d.Missed[0] != want[0] || d.Missed[1] != want[1] {
		t.Fatalf("missed %v, want %v", d.Missed, want)
	}
	if d.Kept["narrowed"] != 1 || d.Kept["compile only"] != 1 {
		t.Fatalf("kept %v", d.Kept)
	}
}

func TestGoGuardBucket(t *testing.T) {
	for _, c := range []struct {
		in   GoGuardCommit
		want string
	}{
		{GoGuardCommit{Stage: "accepted", Files: []string{"a/b_test.go"}}, "accepted: test files only"},
		{GoGuardCommit{Stage: "accepted", Files: []string{"a/b.go", "README.md"}}, "accepted: Go sources"},
		{GoGuardCommit{Stage: "refused", Reason: "go.mod: claimed as a manifest, lockfile or other build input"}, "refused: go.mod / go.sum / go.work"},
		{GoGuardCommit{Stage: "refused", Reason: "a/x.go: status A (only modifications of existing files: ...)"}, "refused: not a modification (status A)"},
		{GoGuardCommit{Stage: "refused", Reason: "a/x.go: a change outside function bodies near base line 3 (declaration, signature, import or directive)"},
			"refused: declaration (change outside function bodies)"},
		{GoGuardCommit{Stage: "refused", Reason: "the selection is full (never narrowed)"}, "refused: full selection"},
	} {
		if got := GoGuardBucket(c.in); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.in, got, c.want)
		}
	}
}

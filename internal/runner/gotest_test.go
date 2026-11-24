package runner

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func goUnit(mod, dir string) model.SelectedTest {
	return model.SelectedTest{Unit: "go:" + dir, Project: mod, Runner: "go", Files: []string{"<go:" + dir + ">"}}
}

func TestGoCommands(t *testing.T) {
	s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{
		goUnit(".", "b"), goUnit(".", "."), goUnit(".", "a/x"), goUnit(".", "b"),
		goUnit("tools", "tools"), goUnit("tools", "tools/gen"),
	}}
	p, err := Commands(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	type cmd struct {
		project string
		argv    []string
	}
	var got []cmd
	for _, c := range p.Commands {
		if c.Runner != "go" {
			t.Errorf("runner %q", c.Runner)
		}
		got = append(got, cmd{c.Project, c.Argv})
	}
	want := []cmd{
		{".", []string{"go", "test", ".", "./a/x", "./b"}},
		{"tools", []string{"go", "test", ".", "./gen"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

// A unit that cannot be placed in its module runs the whole module instead of being dropped.
func TestGoCommandsUnplaceable(t *testing.T) {
	s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{goUnit("mod", "other/x"), goUnit("mod", "mod/y"),
		{Unit: "weird", Project: "w", Runner: "go"}}}
	p, err := Commands(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Commands) != 2 || !reflect.DeepEqual(p.Commands[0].Argv, []string{"go", "test", "./..."}) || p.Commands[0].Project != "mod" ||
		!reflect.DeepEqual(p.Commands[1].Argv, []string{"go", "test", "./..."}) || p.Commands[1].Project != "w" {
		t.Errorf("got %+v", p.Commands)
	}
}

// Long lists are split under the command-line limit and nothing is lost.
func TestGoCommandsBatched(t *testing.T) {
	var tests []model.SelectedTest
	for i := 0; i < 600; i++ {
		tests = append(tests, goUnit(".", fmt.Sprintf("pkg/some/rather/long/directory/name%03d", i)))
	}
	p, err := Commands(model.Selection{Scope: model.TierGraph, Tests: tests}, Options{MaxArgChars: 8000})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Commands) < 2 {
		t.Fatalf("expected batches, got %d commands", len(p.Commands))
	}
	n := 0
	for _, c := range p.Commands {
		if l := len(strings.Join(c.Argv, " ")); l > 8000 {
			t.Errorf("command of %d chars", l)
		}
		n += len(c.Argv) - 2
	}
	if n != 600 {
		t.Errorf("%d packages in the batches, want 600", n)
	}
}

// A unit narrowed by the Go layer runs its kept functions with -run (none: compile and vet only); one whose filter does not fit
// on a command line runs whole with the others.
func TestGoCommandsFiltered(t *testing.T) {
	kept := goUnit(".", "a")
	kept.ClassFilter, kept.Classes = true, []string{"TestA", "ExampleB"}
	none := goUnit(".", "b")
	none.ClassFilter = true
	var long []string
	for i := 0; i < 2000; i++ {
		long = append(long, fmt.Sprintf("TestLongName%04d", i))
	}
	huge := goUnit(".", "c")
	huge.ClassFilter, huge.Classes = true, long
	s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{kept, none, huge, goUnit(".", "d"), goUnit("tools", "tools/x")}}
	p, err := Commands(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got [][]string
	for _, c := range p.Commands {
		got = append(got, append([]string{c.Project}, c.Argv...))
	}
	want := [][]string{
		{".", "go", "test", "./c", "./d"},
		{".", "go", "test", "-run", "^(TestA|ExampleB)$", "./a"},
		{".", "go", "test", "-run", "^$", "./b"},
		{"tools", "go", "test", "./x"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if !strings.Contains(strings.Join(p.Notes, "\n"), "do not fit on one command line") {
		t.Errorf("notes %v", p.Notes)
	}
	if !strings.Contains(p.Commands[2].Note, "compiled and vetted") {
		t.Errorf("note %q", p.Commands[2].Note)
	}
}

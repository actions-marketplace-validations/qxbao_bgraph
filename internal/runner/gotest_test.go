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

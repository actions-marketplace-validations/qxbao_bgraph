package runner

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func dotnetUnit(file, config string) model.SelectedTest {
	return model.SelectedTest{Unit: "dotnet:" + file, Project: cleanProject(file[:len(file)-len("/x.csproj")]), Runner: "dotnet", Package: file,
		Config: config, Files: []string{"<dotnet:" + file + ">"}}
}

func TestDotnetCommands(t *testing.T) {
	s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{
		dotnetUnit("tests/b/x.csproj", ""),
		dotnetUnit("tests/a/x.csproj", ""),
		dotnetUnit("tests/a/x.csproj", ""),
	}}
	p, err := Commands(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got [][]string
	for _, c := range p.Commands {
		got = append(got, c.Argv)
		if c.Runner != "dotnet" || c.Project != "." {
			t.Errorf("command %+v", c)
		}
	}
	want := [][]string{{"dotnet", "test", "tests/a/x.csproj"}, {"dotnet", "test", "tests/b/x.csproj"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestDotnetCommandsMTP(t *testing.T) {
	s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{dotnetUnit("t/x.csproj", "global.json")}}
	p, err := Commands(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"dotnet", "test", "--project", "t/x.csproj"}
	if len(p.Commands) != 1 || !reflect.DeepEqual(p.Commands[0].Argv, want) {
		t.Errorf("got %+v, want %v", p.Commands, want)
	}
}

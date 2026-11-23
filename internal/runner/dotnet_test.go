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

// TestDotnetClassFilter: units narrowed by the class layer (docs/dynamic-layer.md §9.11).
func TestDotnetClassFilter(t *testing.T) {
	a := dotnetUnit("tests/a/x.csproj", "")
	a.ClassFilter, a.Classes = true, []string{"A.FooTests", "A.Bar(Tests)"}
	b := dotnetUnit("tests/b/x.csproj", "")
	b.ClassFilter = true
	c := dotnetUnit("tests/c/x.csproj", "")
	c.ClassFilter, c.Classes = true, []string{"C.LongNameTests"}
	p, err := Commands(model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{a, b, c}}, Options{MaxArgChars: 40})
	if err != nil {
		t.Fatal(err)
	}
	var got [][]string
	for _, c := range p.Commands {
		got = append(got, c.Argv)
	}
	want := [][]string{
		{"dotnet", "test", "tests/a/x.csproj"}, // the filter is longer than MaxArgChars: the whole project runs
		{"dotnet", "build", "tests/b/x.csproj"},
		{"dotnet", "test", "tests/c/x.csproj", "--filter", "FullyQualifiedName~C.LongNameTests"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	p, _ = Commands(model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{a}}, Options{})
	if f := p.Commands[0].Argv; len(f) != 5 || f[4] != `FullyQualifiedName~A.FooTests|FullyQualifiedName~A.Bar\(Tests\)` {
		t.Errorf("filter %v", f)
	}
}

func TestDotnetClassFilterMTP(t *testing.T) {
	a := dotnetUnit("tests/a/x.csproj", "global.json")
	a.ClassFilter, a.Classes = true, []string{"A.FooTests"}
	p, err := Commands(model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{a}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"dotnet", "test", "--project", "tests/a/x.csproj", "--filter", "FullyQualifiedName~A.FooTests"}
	if !reflect.DeepEqual(p.Commands[0].Argv, want) {
		t.Errorf("got %v", p.Commands[0].Argv)
	}
}

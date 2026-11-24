package runner

import (
	"reflect"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/eco/dart"
	"github.com/qxbao/bgraph/internal/model"
)

func dartUnit(project, file, runner string) model.SelectedTest {
	return model.SelectedTest{Unit: file, Project: project, Runner: runner, Files: []string{file}}
}

func TestDartCommands(t *testing.T) {
	s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{
		dartUnit("packages/core", "packages/core/test/b_test.dart", "dart"),
		dartUnit("packages/core", "packages/core/test/a_test.dart", "dart"),
		dartUnit(".", "test/widget_test.dart", "flutter"),
		dartUnit(".", "integration_test/app_test.dart", "flutter"),
		{Unit: dart.WholeUnitID("packages/opaque"), Project: "packages/opaque", Runner: "dart", Whole: true},
		dartUnit("packages/opaque", "packages/opaque/test/x_test.dart", "dart"),
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
		got = append(got, cmd{c.Project, c.Argv})
	}
	want := []cmd{
		{".", []string{"flutter", "test", "test/widget_test.dart"}},
		{".", []string{"flutter", "test", "integration_test/app_test.dart"}},
		{"packages/core", []string{"dart", "test", "test/a_test.dart", "test/b_test.dart"}},
		{"packages/opaque", []string{"dart", "test"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if !strings.Contains(p.Commands[1].Note, "device") {
		t.Fatalf("integration command needs a device note: %+v", p.Commands[1])
	}
}

func TestDartCommandsTooLong(t *testing.T) {
	var tests []model.SelectedTest
	for i := 0; i < 400; i++ {
		f := "pkg/test/some/rather/long/directory/name/file_" + strings.Repeat("x", 10) + string(rune('a'+i%26)) + "_" + string(rune('a'+i/26)) + "_test.dart"
		tests = append(tests, dartUnit("pkg", f, "dart"))
		tests = append(tests, dartUnit("pkg", strings.Replace(f, "pkg/test/", "pkg/integration_test/", 1), "flutter"))
	}
	p, err := Commands(model.Selection{Scope: model.TierGraph, Tests: tests}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Commands) != 2 {
		t.Fatalf("got %+v", p.Commands)
	}
	if !reflect.DeepEqual(p.Commands[0].Argv, []string{"flutter", "test"}) || !reflect.DeepEqual(p.Commands[1].Argv, []string{"flutter", "test", "integration_test"}) {
		t.Fatalf("too long lists must fall back to whole commands: %+v", p.Commands)
	}
}

func TestDartCommandsOutsidePackage(t *testing.T) {
	p, err := Commands(model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{dartUnit("a", "b/test/x_test.dart", "dart")}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Commands) != 1 || !reflect.DeepEqual(p.Commands[0].Argv, []string{"dart", "test"}) {
		t.Fatalf("got %+v", p.Commands)
	}
}

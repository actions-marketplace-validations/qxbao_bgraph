package runner

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func TestCppCommands(t *testing.T) {
	sel := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{
		{Unit: "cpp:meson:tools::test:quick", Runner: "meson", Project: "tools"},
		{Unit: "cpp:cmake:.::test:slow", Runner: "ctest", Project: "."},
		{Unit: "cpp:cmake:.::test:core.unit", Runner: "ctest", Project: "."},
		{Unit: "cpp:cmake:.::test:slow", Runner: "ctest", Project: "."},
	}}
	got, err := Commands(sel, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []Command{
		{Runner: CMakeBuildRunner, Project: ".", Argv: []string{"cmake", "--build", "build"}, Note: "builds the tests of the next ctest command: stop if it fails"},
		{Runner: "ctest", Project: ".", Argv: []string{"ctest", "--test-dir", "build", "--output-on-failure", "--no-tests=error", "-R", `^(core\.unit|slow)$`}},
		{Runner: "meson", Project: "tools", Argv: []string{"meson", "test", "-C", "build", "--print-errorlogs", "quick"}},
	}
	if got.Scope != model.TierGraph || !reflect.DeepEqual(got.Commands, want) {
		t.Fatalf("plan = %+v, want %+v", got, want)
	}
}

func TestCppWholeProjectFallback(t *testing.T) {
	tests := []struct {
		test   model.SelectedTest
		limit  int
		prefix []string
	}{
		{model.SelectedTest{Unit: "cpp:cmake:.::all", Runner: "ctest", Whole: true}, 0,
			[]string{"ctest", "--test-dir", "build", "--output-on-failure", "--no-tests=error"}},
		{model.SelectedTest{Unit: "cpp:meson:.::test:unsafe*", Runner: "meson"}, 0,
			[]string{"meson", "test", "-C", "build", "--print-errorlogs"}},
		{model.SelectedTest{Unit: "cpp:meson:other::test:wrong", Runner: "meson"}, 0,
			[]string{"meson", "test", "-C", "build", "--print-errorlogs"}},
		{model.SelectedTest{Unit: "cpp:meson:.::test:this_test_name_is_too_long_for_the_argument_limit", Runner: "meson"}, 75,
			[]string{"meson", "test", "-C", "build", "--print-errorlogs"}},
	}
	for _, tt := range tests {
		t.Run(tt.test.Unit, func(t *testing.T) {
			p, err := Commands(model.Selection{Scope: model.TierModule, Tests: []model.SelectedTest{tt.test}}, Options{MaxArgChars: tt.limit})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Commands) == 0 {
				t.Fatalf("no command: %+v", p)
			}
			last := p.Commands[len(p.Commands)-1]
			if !reflect.DeepEqual(last.Argv, tt.prefix) || last.Note == "" {
				t.Fatalf("plan = %+v, want whole project with note", p)
			}
		})
	}
}

func TestCppBuildOptions(t *testing.T) {
	tests := []model.SelectedTest{{Unit: "cpp:cmake:.::test:one", Runner: "ctest"}}
	sel := model.Selection{Scope: model.TierGraph, Tests: tests}
	p, err := Commands(sel, Options{BuildDir: "out/debug build", BuildConfig: "Debug"})
	if err != nil || len(p.Commands) != 2 {
		t.Fatalf("plan = %+v, error = %v", p, err)
	}
	if want := []string{"cmake", "--build", "out/debug build", "--config", "Debug"}; !reflect.DeepEqual(p.Commands[0].Argv, want) {
		t.Errorf("build = %q, want %q", p.Commands[0].Argv, want)
	}
	if want := []string{"ctest", "--test-dir", "out/debug build", "--output-on-failure", "--no-tests=error", "--build-config", "Debug", "-R", "^(one)$"}; !reflect.DeepEqual(p.Commands[1].Argv, want) {
		t.Errorf("runner = %q, want %q", p.Commands[1].Argv, want)
	}

	sel.Tests = append(sel.Tests, model.SelectedTest{Unit: "cpp:meson:.::test:two", Runner: "meson"})
	p, err = Commands(sel, Options{})
	if err != nil || len(p.Commands) != 3 || p.Commands[0].Argv[2] != "build-cmake" || p.Commands[2].Argv[3] != "build-meson" {
		t.Fatalf("plan for both runners = %+v, error = %v", p, err)
	}
	if _, err := Commands(sel, Options{BuildDir: "shared"}); err == nil {
		t.Fatal("expected error for two backends sharing a build directory")
	}
}

func TestCppBuildDirectorySpelling(t *testing.T) {
	sel := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{
		{Unit: "cpp:cmake:Lib::test:one", Runner: "ctest", Project: "Lib"},
		{Unit: "cpp:cmake:lib::test:two", Runner: "ctest", Project: "lib"},
	}}
	_, err := Commands(sel, Options{})
	if caseInsensitiveFS != (err != nil) {
		t.Fatalf("Lib/build and lib/build: error = %v, case-insensitive filesystem = %v", err, caseInsensitiveFS)
	}

	abs := "/tmp/build"
	if runtime.GOOS == "windows" {
		abs = `C:\build`
	}
	one := model.Selection{Scope: model.TierGraph, Tests: sel.Tests[:1]}
	if p, err := Commands(one, Options{BuildDir: abs}); err != nil || len(p.Commands) != 2 {
		t.Fatalf("one project with an absolute build directory: plan = %+v, error = %v", p, err)
	}
	sel.Tests[1] = model.SelectedTest{Unit: "cpp:cmake:other::test:two", Runner: "ctest", Project: "other"}
	if _, err := Commands(sel, Options{BuildDir: abs}); err == nil || !strings.Contains(err.Error(), "relative to each project") {
		t.Fatalf("two projects with an absolute build directory: error = %v", err)
	}
}

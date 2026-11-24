package runner

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func TestPHPClassFilter(t *testing.T) {
	withGOOS(t, "linux")
	s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{
		{Unit: ".::php", Project: ".", Runner: "phpunit", Config: "phpunit.xml.dist", BinDir: "vendor/bin", ClassFilter: true,
			Classes: []string{"tests/ATest.php", "-odd/BTest.php"}},
		{Unit: "none::php", Project: "none", Runner: "pest", Config: "none/phpunit.xml", BinDir: "none/vendor/bin", ClassFilter: true},
		{Unit: "pest::php", Project: "pest", Runner: "pest", Config: "pest/phpunit.xml", BinDir: "pest/vendor/bin", ClassFilter: true,
			RunnerPaths: []string{"pest/test"}, Classes: []string{"pest/test/ATest.php"}},
	}}
	p, err := Commands(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []Command{
		{Runner: "phpunit", Project: ".", Argv: []string{"vendor/bin/phpunit", "--configuration", "phpunit.xml.dist", "tests/ATest.php", "./-odd/BTest.php"},
			Note: "PHP layer: 2 test files"},
		{Runner: "pest", Project: "pest", Argv: []string{"vendor/bin/pest", "--configuration", "phpunit.xml", "--test-directory=test", "test/ATest.php"},
			Note: "PHP layer: 1 test files"},
	}
	if !reflect.DeepEqual(p.Commands, want) {
		t.Fatalf("got %+v", p.Commands)
	}
	if !slices.ContainsFunc(p.Notes, func(n string) bool { return strings.Contains(n, "no test file of none::php") }) {
		t.Fatalf("notes %v", p.Notes)
	}
	// Too long for one command line: the unit runs whole, with a note.
	p, err = Commands(s, Options{MaxArgChars: 70})
	if err != nil || len(p.Commands) != 2 || len(p.Commands[0].Argv) != 3 ||
		!slices.ContainsFunc(p.Notes, func(n string) bool { return strings.Contains(n, "do not fit") }) {
		t.Fatalf("long: %+v %v", p, err)
	}
}

func TestPHPCommands(t *testing.T) {
	withGOOS(t, "linux")
	s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{
		{Unit: ".::php", Project: ".", Runner: "phpunit", Config: "phpunit.dist.xml", BinDir: "vendor/bin", Files: []string{"<php:.::php>"}},
		{Unit: "apps/pest::php", Project: "apps/pest", Runner: "pest", Config: "apps/pest/phpunit.xml", BinDir: "tools/bin"},
		{Unit: "apps/unit::php", Project: "apps/unit", Runner: "phpunit", BinDir: "apps/unit/vendor/bin", RunnerPaths: []string{"apps/unit/test", "apps/unit/tests", "apps/unit/-[Standalone]Test.php"}},
		{Unit: "apps/no-xml::php", Project: "apps/no-xml", Runner: "pest", BinDir: "apps/no-xml/vendor/bin", RunnerPaths: []string{"apps/no-xml/test", "apps/no-xml/tests"}},
		{Unit: "xml-only::php", Project: "xml-only", Runner: "phpunit", Config: "xml-only/phpunit.xml"},
	}}
	want := []Command{
		{Runner: "phpunit", Project: ".", Argv: []string{"vendor/bin/phpunit", "--configuration", "phpunit.dist.xml"}},
		{Runner: "pest", Project: "apps/pest", Argv: []string{"../../tools/bin/pest", "--configuration", "phpunit.xml"}},
		{Runner: "phpunit", Project: "apps/unit", Argv: []string{"vendor/bin/phpunit", "test"}},
		{Runner: "phpunit", Project: "apps/unit", Argv: []string{"vendor/bin/phpunit", "tests"}},
		{Runner: "phpunit", Project: "apps/unit", Argv: []string{"vendor/bin/phpunit", "./-[Standalone]Test.php"}},
		{Runner: "pest", Project: "apps/no-xml", Argv: []string{"vendor/bin/pest", "--test-directory=test", "test"}},
		{Runner: "pest", Project: "apps/no-xml", Argv: []string{"vendor/bin/pest", "tests"}},
		{Runner: "phpunit", Project: "xml-only", Argv: []string{"phpunit", "--configuration", "phpunit.xml"}},
	}
	p, err := Commands(s, Options{})
	if err != nil || !reflect.DeepEqual(p.Commands, want) {
		t.Fatalf("got %+v, %v; want %+v", p, err, want)
	}
	s.Tests[0].BinDir = "/"
	if p, err = Commands(s, Options{}); err != nil || p.Scope != model.TierFull || len(p.Commands) != 0 {
		t.Fatalf("unrepresentable launcher returned %+v, %v", p, err)
	}
}

func TestPHPWindowsLauncher(t *testing.T) {
	withGOOS(t, "windows")
	s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{{Unit: ".::php", Runner: "pest", BinDir: "vendor/bin", RunnerPaths: []string{"tests"}}}}
	p, err := Commands(s, Options{})
	if err != nil || len(p.Commands) != 1 || !reflect.DeepEqual(p.Commands[0].Argv, []string{"php", `vendor\bin\pest`, "tests"}) {
		t.Fatalf("got %+v, %v", p, err)
	}
}

func TestPHPCustomSuiteFallsBack(t *testing.T) {
	s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{
		{Unit: ".::php", Project: ".", Runner: "phpunit", Config: "phpunit.xml", BinDir: "vendor/bin", Whole: true},
	}}
	p, err := Commands(s, Options{})
	if err != nil || p.Scope != model.TierFull || len(p.Commands) != 0 {
		t.Fatalf("custom suite returned %+v, %v", p, err)
	}
}

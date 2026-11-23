package runner

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

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

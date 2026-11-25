package runner

import (
	"reflect"
	"regexp"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func swiftTestUnit(project, suffix string, whole bool) model.SelectedTest {
	return model.SelectedTest{Unit: "swift:" + project + "::" + suffix, Runner: "swift", Project: project, Whole: whole}
}

func TestSwiftCommandsTargets(t *testing.T) {
	s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{
		swiftTestUnit("packages/tools", "test:ToolsTests", false),
		swiftTestUnit(".", "test:BetaTests", false),
		swiftTestUnit(".", "test:AlphaTests", false),
		swiftTestUnit(".", "test:AlphaTests", false),
	}}
	for i := range s.Tests {
		if s.Tests[i].Project == "." {
			s.Tests[i].TestModules = []string{"AlphaTests", "BetaTests", "OtherTests", "SecondTests"}
		} else {
			s.Tests[i].TestModules = []string{"ToolsTests", "PluginTests"}
		}
	}
	p, err := Commands(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Scope != model.TierGraph || len(p.Commands) != 2 || p.Commands[0].Project != "." || p.Commands[1].Project != "packages/tools" {
		t.Fatalf("unexpected plan %+v", p)
	}
	for _, command := range p.Commands {
		if command.Runner != "swift" || len(command.Argv) != 4 || !reflect.DeepEqual(command.Argv[:3], []string{"swift", "test", "--skip"}) {
			t.Fatalf("unexpected command %+v", command)
		}
	}
	skip := regexp.MustCompile(p.Commands[0].Argv[3])
	for _, item := range []struct {
		id      string
		skipped bool
	}{
		{"AlphaTests.Probe/testFirst", false},
		{"BetaTests.topLevel()", false},
		{"Shared.inLibrary()", false},
		{"OtherTests/testMethod", false}, // An unqualified Objective-C XCTest name.
		{"OtherTests.Probe/testFirst", true},
		{"SecondTests.topLevel()/Probe.swift:8:2", true},
	} {
		if got := skip.MatchString(item.id); got != item.skipped {
			t.Errorf("skip matched %q = %t, want %t", item.id, got, item.skipped)
		}
	}
}

func TestSwiftCommandsWholePackage(t *testing.T) {
	for _, tt := range []struct {
		name string
		unit model.SelectedTest
		opt  Options
	}{
		{"opaque", swiftTestUnit("kit", "all", true), Options{}},
		{"whole", swiftTestUnit("kit", "test:BetaTests", true), Options{}},
		{"bad ID", model.SelectedTest{Unit: "swift:other::test:AlphaTests", Runner: "swift", Project: "kit"}, Options{}},
		{"no modules", swiftTestUnit("kit", "test:AlphaTests", false), Options{}},
		{"inconsistent modules", model.SelectedTest{Unit: "swift:kit::test:AlphaTests", Runner: "swift", Project: "kit", TestModules: []string{"AlphaTests", "BetaTests"}}, Options{}},
		{"limit", model.SelectedTest{Unit: "swift:kit::test:AlphaTests", Runner: "swift", Project: "kit", TestModules: []string{"AlphaTests", "LongUnselectedTestTargetName"}}, Options{MaxArgChars: 20}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			peer := swiftTestUnit("kit", "test:AlphaTests", false)
			peer.TestModules = []string{"AlphaTests", "LongUnselectedTestTargetName"}
			s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{tt.unit, peer}}
			p, err := Commands(s, tt.opt)
			if err != nil {
				t.Fatal(err)
			}
			if p.Scope != model.TierGraph || len(p.Commands) != 1 || p.Commands[0].Project != "kit" ||
				!reflect.DeepEqual(p.Commands[0].Argv, []string{"swift", "test"}) {
				t.Fatalf("unexpected plan %+v", p)
			}
		})
	}
}

func TestSwiftCommandsKeepsUnrepresentableSibling(t *testing.T) {
	unit := swiftTestUnit(".", "test:AlphaTests", false)
	unit.TestModules = []string{"AlphaTests", "Dash-Tests"}
	s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{unit}}
	p, err := Commands(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Commands) != 1 || !reflect.DeepEqual(p.Commands[0].Argv, []string{"swift", "test"}) {
		t.Fatalf("unexpected plan %+v", p)
	}
}

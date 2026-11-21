package runner

import (
	"reflect"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func cargoUnit(id, dir, pkg string) model.SelectedTest {
	return model.SelectedTest{Unit: id, Project: dir, Runner: "cargo", Package: pkg, Files: []string{"<rust:" + id + ">"}}
}

func TestCargoCommands(t *testing.T) {
	s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{
		cargoUnit("crates/foo::test:b", "crates/foo", "foo"),
		cargoUnit("crates/foo::lib", "crates/foo", "foo"),
		cargoUnit("crates/foo::test:a", "crates/foo", "foo"),
		cargoUnit("crates/foo::doc", "crates/foo", "foo"),
		cargoUnit("crates/foo::bin:cli", "crates/foo", "foo"),
		cargoUnit("crates/foo::bench:fast", "crates/foo", "foo"),
		cargoUnit("crates/foo::example:demo", "crates/foo", "foo"),
		cargoUnit(".::lib", ".", "root-pkg"),
		cargoUnit("crates/bar::doc", "crates/bar", "bar"),
	}}
	p, err := Commands(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got [][]string
	for _, c := range p.Commands {
		got = append(got, c.Argv)
		if c.Runner != "cargo" {
			t.Errorf("runner %q", c.Runner)
		}
	}
	want := [][]string{
		{"cargo", "test", "-p", "root-pkg", "--lib"},
		{"cargo", "test", "-p", "bar", "--doc"},
		{"cargo", "test", "-p", "foo", "--lib", "--bin", "cli", "--test", "a", "--test", "b", "--bench", "fast", "--example", "demo"},
		{"cargo", "test", "-p", "foo", "--doc"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%v\nwant\n%v", got, want)
	}
	found := false
	for _, n := range p.Notes {
		if strings.Contains(n, "feature unification") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing feature-unification note: %v", p.Notes)
	}
}

func TestCargoFallbacks(t *testing.T) {
	// No package name: address the package by manifest path. Unknown ID: the whole package, never dropped.
	s := model.Selection{Scope: model.TierModule, Tests: []model.SelectedTest{
		{Unit: "x/y::lib", Project: "x/y", Runner: "cargo"},
		{Unit: "weird", Project: "w", Runner: "cargo", Package: "w"},
	}}
	p, _ := Commands(s, Options{})
	var got [][]string
	for _, c := range p.Commands {
		got = append(got, c.Argv)
	}
	want := [][]string{
		{"cargo", "test", "-p", "w", "--all-targets"},
		{"cargo", "test", "-p", "w", "--doc"},
		{"cargo", "test", "--manifest-path", "x/y/Cargo.toml", "--lib"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%v\nwant\n%v", got, want)
	}
}

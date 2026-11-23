package ruby

import (
	"context"
	"slices"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/tree"
)

func analyze(t *testing.T, files map[string]string) (*testAnalysis, *tree.MapTree) {
	t.Helper()
	tr := tree.NewMapTree(files)
	a, err := New(nil).Analyze(context.Background(), tr, scancache.New())
	if err != nil {
		t.Fatal(err)
	}
	ta := &testAnalysis{units: map[string]model.TestUnit{}, edges: map[string][]string{}}
	for _, p := range a.Projects {
		ta.projects = append(ta.projects, p.Dir)
	}
	for _, u := range a.Tests {
		ta.units[u.ID] = u
	}
	for _, e := range a.Edges {
		ta.edges[e.From] = append(ta.edges[e.From], e.To)
	}
	ta.roles = a.Roles
	ta.unres = a.Unresolved
	return ta, tr
}

type testAnalysis struct {
	projects []string
	units    map[string]model.TestUnit
	edges    map[string][]string
	roles    map[string]model.Role
	unres    []model.Unresolved
}

func TestNoRubyIsFree(t *testing.T) {
	a, _ := analyze(t, map[string]string{"package.json": "{}", "src/a.ts": "x"})
	if len(a.projects) != 0 || len(a.roles) != 0 {
		t.Fatalf("%+v", a)
	}
	// A stray .rb without any Ruby project is not analyzed.
	a, _ = analyze(t, map[string]string{"package.json": "{}", "tools/gen.rb": "require 'x'", "spec/x_spec.rb": ""})
	if len(a.projects) != 0 || len(a.units) != 0 {
		t.Fatalf("%+v", a)
	}
}

func TestProjectsUnitsRunners(t *testing.T) {
	a, _ := analyze(t, map[string]string{
		"Gemfile":                                  "gem 'rails'",
		"bin/rails":                                "#!/usr/bin/env ruby\nrequire_relative '../config/boot'\n",
		"config/application.rb":                    "class App < Rails::Application; end",
		"config/boot.rb":                           "",
		"test/models/user_test.rb":                 "",
		"spec/models/user_spec.rb":                 "",
		"spec/support/helpers.rb":                  "",
		"spec/fixtures/sample/sample_spec.rb":      "",
		"spec/fixtures/sample/sample.gemspec":      "",
		"engines/billing/billing.gemspec":          "",
		"engines/billing/test/test_helper.rb":      "",
		"engines/billing/test/invoice_test.rb":     "",
		"engines/billing/test/dummy/Rakefile":      "",
		"engines/billing/test/dummy/app/models.rb": "",
		"scripts/x.rb":                             "",
		"vendor/gems/old/test/old_test.rb":         "",
	})
	if want := []string{".", "engines/billing"}; !slices.Equal(a.projects, want) {
		t.Fatalf("projects %v, want %v", a.projects, want)
	}
	runners := map[string]string{}
	for id, u := range a.units {
		runners[id] = u.Runner + " " + u.Project + " " + u.Config
	}
	want := map[string]string{
		"test/models/user_test.rb":             "rails . Gemfile",
		"spec/models/user_spec.rb":             "rspec . Gemfile",
		"engines/billing/test/invoice_test.rb": "minitest engines/billing Gemfile|lib,test",
	}
	if len(runners) != len(want) {
		t.Fatalf("units %v", runners)
	}
	for id, w := range want {
		if runners[id] != w {
			t.Fatalf("%s: %q, want %q (all: %v)", id, runners[id], w, runners)
		}
	}
	if a.roles["bin/rails"] != model.RoleSource || a.roles["Gemfile"] != model.RoleManifest {
		t.Fatalf("roles %v", a.roles)
	}
	if !slices.Contains(a.edges["config/application.rb"], "app/**") || !slices.Contains(a.edges["config/application.rb"], "scripts/**") ||
		slices.Contains(a.edges["config/application.rb"], "spec/**") {
		t.Fatalf("rails edges %v", a.edges["config/application.rb"])
	}
}

func TestUnitIDs(t *testing.T) {
	if d, ok := ParseWholeUnitID(WholeUnitID("gems/x")); !ok || d != "gems/x" {
		t.Fatal(d)
	}
	if _, ok := ParseWholeUnitID("spec/a_spec.rb"); ok {
		t.Fatal("a file is not a whole unit")
	}
}

func TestFeatureSuffixUnion(t *testing.T) {
	a, _ := analyze(t, map[string]string{
		"Gemfile":                       "",
		"lib/a/b.rb":                    "",
		"gems/x/lib/a/b.rb":             "",
		"gems/x/lib/xa/b.rb":            "",
		"spec/b_spec.rb":                "require 'a/b'\nrequire './lib/a/b'\nrequire 'json'\n",
		"spec/d_spec.rb":                "require name",
		"spec/e_spec.rb":                "x = <<~A\n",
		"spec/f_spec.rb":                "require_relative 'missing'",
		"spec/g_spec.rb":                "require \"a/#{x}\"",
		"spec/fixtures/data/c.json":     "",
		"spec/h_spec.rb":                "File.read('spec/fixtures/data/c.json'); File.read('data/c.json')",
		"spec/i_spec.rb":                "Dir[File.join(__dir__, 'support/*.rb')].each { |f| require f }",
		"spec/j_spec.rb":                "load 'tasks/t.rake'",
		"lib/tasks/t.rake":              "",
		"spec/k_spec.rb":                "Dir.glob(x).each { |f| require f }",
		"spec/l_spec.rb":                "require File.expand_path('../lib/a/b', __dir__)",
		"spec/m_spec.rb":                "Gem.bin_path('x', 'tool')",
		"exe/tool":                      "#!/usr/bin/env ruby\nrequire 'a/b'\n",
		"exe/notruby":                   "#!/bin/sh\n",
		"spec/fixtures/ignored_spec.rb": "",
	})
	node := a.edges["<rbreq:a/b>"]
	slices.Sort(node)
	if want := []string{"**/a/b.rb", "gems/x/lib/a/b.rb", "lib/a/b.rb"}; !slices.Equal(node, want) {
		t.Fatalf("<rbreq:a/b> → %v, want %v", node, want)
	}
	if !slices.Contains(a.edges["spec/b_spec.rb"], "<rbreq:lib/a/b>") || !slices.Contains(a.edges["<rbreq:lib/a/b>"], "lib/a/b.rb") {
		t.Fatalf("working-directory require: %v", a.edges["spec/b_spec.rb"])
	}
	if !slices.Contains(a.edges["<rbreq:json>"], "**/json.rb") {
		t.Fatal("external feature keeps its pattern")
	}
	classes := map[string]model.UnresolvedClass{}
	for _, u := range a.unres {
		classes[u.File] = u.Class
	}
	if classes["spec/d_spec.rb"] != model.UnresolvedDynamic || classes["spec/e_spec.rb"] != model.UnresolvedUncertainScan ||
		classes["spec/f_spec.rb"] != model.UnresolvedUnknown || classes["spec/k_spec.rb"] != model.UnresolvedDynamic {
		t.Fatalf("unresolved %+v", a.unres)
	}
	for _, u := range a.unres {
		if u.Class == model.UnresolvedDynamic && (u.ScopeDir != "." || len(u.TriggerGlobs) == 0) {
			t.Fatalf("dynamic scope %+v", u)
		}
	}
	if !slices.Contains(a.edges["spec/f_spec.rb"], "spec/missing.rb") {
		t.Fatal("missing require_relative keeps an exact edge")
	}
	if !slices.Contains(a.edges["spec/g_spec.rb"], "**/a/*.rb") || !slices.Contains(a.edges["spec/g_spec.rb"], "**/a/*/**/*.rb") {
		t.Fatalf("prefix: %v", a.edges["spec/g_spec.rb"])
	}
	if !slices.Contains(a.edges["spec/h_spec.rb"], "spec/fixtures/data/c.json") {
		t.Fatalf("data: %v", a.edges["spec/h_spec.rb"])
	}
	if !slices.Contains(a.edges["spec/i_spec.rb"], "spec/support/*.rb") {
		t.Fatalf("glob: %v", a.edges["spec/i_spec.rb"])
	}
	if !slices.Contains(a.edges["<rbload:tasks/t.rake>"], "lib/tasks/t.rake") {
		t.Fatalf("load: %v", a.edges)
	}
	if !slices.Contains(a.edges["spec/l_spec.rb"], "lib/a/b.rb") {
		t.Fatalf("expand_path: %v", a.edges["spec/l_spec.rb"])
	}
	if !slices.Contains(a.edges["spec/m_spec.rb"], "exe/tool") || !slices.Contains(a.edges["exe/tool"], "<rbreq:a/b>") {
		t.Fatalf("executables: %v / %v", a.edges["spec/m_spec.rb"], a.edges["exe/tool"])
	}
	if _, ok := a.roles["exe/notruby"]; ok {
		t.Fatal("a shell script is not Ruby")
	}
	if _, ok := a.units["spec/fixtures/ignored_spec.rb"]; ok {
		t.Fatal("files below fixtures are never units")
	}
}

package scancache

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

func TestRubyScanRoundTrip(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{
		"lib/a.rb":    "require 'x'\nrequire_relative '../y'\nrequire \"p/#{q}\"\nDir[File.join(__dir__, '*.rb')].each { |f| require f }\n",
		"Gemfile":     "gem 'rails', require: false\ngemspec\n",
		"Rakefile":    "Rake::TestTask.new { |t| t.pattern = 'test/**/*_test.rb' }\n",
		"a.gemspec":   "Gem::Specification.new { |s| s.name = 'a' }\n",
		"x.rake":      "load 'y.rake'\n",
		"config.ru":   "run App\n",
		"bad.rb":      "x = <<~EOS\n",
		"bin/console": "#!/usr/bin/env ruby\n",
	})
	c := New()
	for _, p := range []string{"lib/a.rb", "Gemfile", "Rakefile", "a.gemspec", "x.rake", "config.ru", "bad.rb"} {
		r, err := c.Scan(tr, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		got, err := decode(encode(r))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if len(r.Imports) == 0 {
			r.Imports = nil // the codec does not keep an empty slice
		}
		if !reflect.DeepEqual(got, r) {
			t.Fatalf("%s: round trip mismatch\n got %+v\nwant %+v", p, got, r)
		}
	}
	if r, _ := c.Scan(tr, "lib/a.rb"); len(r.Imports) < 4 || r.Imports[0].Kind != model.ImportRbRequire {
		t.Fatalf("unexpected scan: %+v", r.Imports)
	}
	if r, _ := c.Scan(tr, "bad.rb"); !r.Uncertain {
		t.Fatal("bad.rb must be uncertain")
	}
	// Extensionless executables are scanned by the Ruby ecosystem itself, not through the cache.
	if _, err := c.Scan(tr, "bin/console"); err == nil {
		t.Fatal("bin/console has no scanner by extension")
	}
	// Every Ruby kind has a compact index (rows written before this phase keep decoding: the table only grew).
	for _, k := range []model.ImportKind{model.ImportRbRequire, model.ImportRbLoadPath, model.ImportSysPath, model.ImportPlugin} {
		if _, ok := kindIdx[k]; !ok {
			t.Fatalf("kind %s has no index", k)
		}
	}
	if kindIdx[model.ImportPlugin] != 11 || kindIdx[model.ImportRead] != 12 || kindIdx[model.ImportRbRequire] != 13 {
		t.Fatal("existing kind indexes must not move")
	}
}

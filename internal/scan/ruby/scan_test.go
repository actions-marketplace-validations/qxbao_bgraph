package ruby

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

// refs renders the non-string refs of a scan as "kind spec [suffix] [dyn]".
func refs(t *testing.T, src string, withStrings bool) []string {
	t.Helper()
	r := Scan("a.rb", []byte(src))
	if r.Uncertain {
		t.Fatalf("uncertain: %s\n%s", r.UncertainReason, src)
	}
	var out []string
	for _, im := range r.Imports {
		if !withStrings && im.Kind == model.ImportRbString {
			continue
		}
		s := string(im.Kind) + " " + im.Spec
		if im.Suffix != "" {
			s += " [" + im.Suffix + "]"
		}
		if im.Dynamic {
			s += " dyn"
		}
		out = append(out, s)
	}
	return out
}

func expect(t *testing.T, src string, want ...string) {
	t.Helper()
	got := refs(t, src, false)
	slices.Sort(got)
	want = slices.Sorted(slices.Values(want))
	if !slices.Equal(got, want) {
		t.Fatalf("source:\n%s\ngot  %q\nwant %q", src, got, want)
	}
}

func TestRequireForms(t *testing.T) {
	expect(t, `require "a/b"`, "rb_require a/b")
	expect(t, `require 'a/b'`, "rb_require a/b")
	expect(t, `require("a/b")`, "rb_require a/b")
	expect(t, `require"a/b"`, "rb_require a/b")
	expect(t, `Kernel.require "x"`, "rb_require x")
	expect(t, `::Kernel.require "x"`, "rb_require x")
	expect(t, `require_relative "../x"`, "rb_require ../x [rel]")
	expect(t, `require_relative("sub/y")`, "rb_require sub/y [rel]")
	expect(t, `load "tasks/x.rake"`, "rb_load tasks/x.rake")
	expect(t, `load File.expand_path("x.rb", __dir__)`, "rb_load x.rb [rel]")
	expect(t, `require_dependency "a/b"`, "rb_require a/b")
	expect(t, `autoload :Foo, "foo/bar"`, "rb_require foo/bar")
	expect(t, `autoload(:Foo, "foo/bar")`, "rb_require foo/bar")
	expect(t, `Mod.autoload :Foo, "foo/bar"`, "rb_require foo/bar")
	expect(t, `send(:require, "x")`, "rb_require x")
	expect(t, `Kernel.send(:load, "y.rb")`, "rb_load y.rb")
	expect(t, `require "x" if defined?(Y)`, "rb_require x")
	expect(t, "require 'a'; require 'b'", "rb_require a", "rb_require b")
	expect(t, "begin\n  require 'x'\nrescue LoadError\nend", "rb_require x")
}

func TestNotRequires(t *testing.T) {
	expect(t, `YAML.load(x)`)
	expect(t, `Marshal.load "x"`)
	expect(t, `obj.require "x"`)
	expect(t, `def require(x); end`)
	expect(t, `gem "x", require: false`, "rb_gem x [-]")
	expect(t, `load = 1`)
	expect(t, `x = { require: "a" }`)
	expect(t, `config.load_defaults 7.0`)
	expect(t, `# require "commented"`)
	expect(t, "=begin\nrequire 'x'\n=end\nrequire 'y'", "rb_require y")
	expect(t, "require 'y'\n__END__\nrequire 'x'\n", "rb_require y")
	expect(t, `s = "require 'x'"`)
	expect(t, `s = 'require "x"'`)
	expect(t, `s = %q(require "x")`)
	expect(t, "s = <<~EOS\n  require 'x'\nEOS\nrequire 'y'", "rb_require y")
	expect(t, `alias_method :old_require, :require`)
}

func TestPathExpressions(t *testing.T) {
	expect(t, `require File.expand_path("../lib/x", __FILE__)`, "rb_require lib/x [rel]")
	expect(t, `require File.expand_path("../../lib/x", __FILE__)`, "rb_require ../lib/x [rel]")
	expect(t, `require File.expand_path("lib/x", __dir__)`, "rb_require lib/x [rel]")
	expect(t, `require File.join(__dir__, "a", "b")`, "rb_require a/b [rel]")
	expect(t, `require File.join(File.dirname(__FILE__), "..", "lib", "x")`, "rb_require ../lib/x [rel]")
	expect(t, `require File.dirname(__FILE__) + "/x"`, "rb_require x [rel]")
	expect(t, `require File.dirname(__FILE__) + '/../lib/y'`, "rb_require ../lib/y [rel]")
	expect(t, `require "#{__dir__}/x"`, "rb_require x [rel]")
	expect(t, `require "#{File.dirname(__FILE__)}/sub/x"`, "rb_require sub/x [rel]")
	expect(t, `require Pathname(__dir__).join("x").to_s`, "rb_require x [rel]")
	expect(t, `require Pathname.new(__FILE__).dirname.join("y")`, "rb_require y [rel]")
	expect(t, `require Rails.root.join("lib", "x")`, "rb_require lib/x [root]")
	expect(t, `require ::File.expand_path('../config/environment', __dir__)`, "rb_require ../config/environment [rel]")
	expect(t, `require File.expand_path("x")`, "rb_require x")
	expect(t, `require "./lib/x"`, "rb_require ./lib/x")
	expect(t, `require_relative File.join("support", "x")`, "rb_require support/x [rel]")
}

func TestDynamicRequires(t *testing.T) {
	expect(t, `require name`, "rb_require  dyn")
	expect(t, `require "rubocop/cop/#{name}"`, "rb_require rubocop/cop/ dyn")
	expect(t, `require "#{x}/y"`, "rb_require #{}/y dyn")
	expect(t, `require "#{x}"`, "rb_require  dyn")
	expect(t, `require "rubocop/#{feature}/version"`, "rb_require rubocop/#{}/version dyn")
	expect(t, `require "a/#{b}/#{c}.rb"`, "rb_require a/#{}.rb dyn")
	expect(t, `require "a/#{b}/#{"c"}"`, "rb_require a/#{}/c dyn")
	expect(t, `require "a/" + x + "/b"`, "rb_require a/#{}/b dyn")
	expect(t, `require "a/" + x + y`, "rb_require a/ dyn")
	expect(t, `require File.join(__dir__, "cops", name, "x")`, "rb_require cops/#{}/x [rel] dyn")
	expect(t, `require File.join(x, "a")`, "rb_require #{}/a dyn")
	expect(t, `require "foo_" + kind`, "rb_require foo_ dyn")
	expect(t, `require File.join(__dir__, name)`, "rb_require  [rel] dyn")
	expect(t, `require File.join(__dir__, "plugins", name)`, "rb_require plugins/ [rel] dyn")
	expect(t, `require "#{__dir__}/support/#{f}"`, "rb_require support/ [rel] dyn")
	expect(t, `load path`, "rb_load  dyn")
	expect(t, `Kernel.require x`, "rb_require  dyn")
	expect(t, `send(:require, lib)`, "rb_require  dyn")
	expect(t, `instance_eval(File.read(path))`, "rb_load  dyn")
	expect(t, `eval(File.read(File.expand_path("x.rb", __dir__)))`, "rb_load x.rb [rel]")
	expect(t, `class_eval <<~RUBY
  def x; end
RUBY
`)
}

func TestDirGlobs(t *testing.T) {
	expect(t, `Dir[File.join(__dir__, "support/**/*.rb")].each { |f| require f }`, "rb_glob support/**/*.rb [rel]")
	expect(t, `Dir["#{__dir__}/support/**/*.rb"].sort.each do |f|
  require f
end`, "rb_glob support/**/*.rb [rel]")
	expect(t, `Dir[File.expand_path("support/*.rb", __dir__)].each(&method(:require))`, "rb_glob support/*.rb [rel]")
	expect(t, `Dir.glob(File.expand_path("support/*.rb", __dir__), &method(:require))`, "rb_glob support/*.rb [rel]")
	expect(t, `Dir["./spec/support/**/*.rb"].each { |f| require f }`, "rb_glob ./spec/support/**/*.rb")
	expect(t, `Dir[Rails.root.join("spec/support/**/*.rb")].sort.each { |f| require f }`, "rb_glob spec/support/**/*.rb [root]")
	expect(t, `Dir.glob("lib/**/*.rb").each { |f| require_relative f }`, "rb_glob lib/**/*.rb", "rb_glob lib/**/*.rb [rel]")
	expect(t, `Dir[File.dirname(__FILE__) + "/x/*.rb"].each { |file| require File.expand_path(file) }`, "rb_glob x/*.rb [rel]")
	expect(t, `Dir.glob("*.rb", base: File.join(__dir__, "cops")).each { require_relative "cops/#{_1}" }`,
		"rb_data_glob cops/*.rb [rel]", "rb_require cops/ [rel] dyn")
	expect(t, `Dir[pattern].each { |f| require f }`, "rb_require  dyn")
	expect(t, `Dir["x/*.rb"].each { |f| require "y/#{File.basename(f)}" }`, "rb_require y/ dyn", "rb_data_glob x/*.rb")
	expect(t, `Dir["x/*.rb"].each { |f| require File.basename(f, ".rb") }`, "rb_require  dyn", "rb_data_glob x/*.rb")
	expect(t, `files = Dir[File.join(__dir__, "fixtures", "*.json")]`, "rb_data_glob fixtures/*.json [rel]")
	expect(t, `Dir.each_child(File.join(__dir__, "plugins")) { |f| require File.join(__dir__, "plugins", f) }`,
		"rb_require plugins/ [rel] dyn", "rb_data_glob plugins/* [rel]")
	expect(t, `Dir.glob(%w[a/*.rb b/*.rb]).each { |f| load f }`, "rb_glob a/*.rb", "rb_glob b/*.rb")
	expect(t, `def require_all(path)
  glob = File.join(__dir__, path, "*.rb")
  Dir[glob].sort.each do |f|
    require f
  end
end`, "rb_glob #{}/*.rb [rel] dyn")
}

func TestVariables(t *testing.T) {
	expect(t, "paths = Dir[File.expand_path(\"**/ui/*.rb\", File.dirname(__FILE__))]\nraise 'x' unless paths.count > 0\npaths.each do |file|\n  require file\nend",
		"rb_glob **/ui/*.rb [rel]")
	expect(t, "paths = Dir['a/*.rb']\npaths << 'b.rb'\npaths.each { |f| require f }", "rb_data_glob a/*.rb", "rb_require  dyn")
	expect(t, "TOOLS.each do |tool|\n  path = File.join(tool.to_s, \"spec\", \"spec_helper.rb\")\n  require_relative \"../#{path}\" if File.exist?(path)\nend",
		"rb_require ../#{}/spec/spec_helper.rb [rel] dyn")
	expect(t, "def a\n  path = \"lib/x\"\n  require path\nend", "rb_require lib/x")
	expect(t, "path = \"x\"\ndef a\n  require path\nend", "rb_require  dyn")
	expect(t, "def a\n  path = \"x\"\nend\nrequire path", "rb_require  dyn")
	expect(t, "require path\npath = \"x\"", "rb_require  dyn")
	expect(t, "x = \"a\"\nx << \"b\"\nrequire x", "rb_require  dyn")
	expect(t, "x = \"a\"\nx = \"b\"\nrequire x", "rb_require  dyn")
	expect(t, "def a(x)\n  x = \"a\"\n  require x\nend", "rb_require  dyn")
	expect(t, "[1].each { |x| x = 'a'; require x }", "rb_require  dyn")
	expect(t, "a, x = 'a', 'b'\nrequire x", "rb_require  dyn")
	expect(t, "LIB = File.expand_path('../lib', __dir__)\ndef t\n  require File.join(LIB, 'x')\nend", "rb_require ../lib/x [rel]")
	expect(t, "LIB = File.expand_path('../lib', __dir__)\nLIB = 'other'\nrequire File.join(LIB, 'x')", "rb_require #{}/x dyn")
	expect(t, "A = B\nB = A\nrequire A", "rb_require  dyn")
	expect(t, "def load(p) = Marshal.load(File.binread(p))\nx = load(path)")
	got := refs(t, "FIX = File.expand_path(\"fixtures\", __dir__)\ndef t = File.read(File.join(FIX, \"a.json\"))", true)
	if !slices.Contains(got, "rb_string fixtures/a.json [rel]") {
		t.Fatalf("constant path: %q", got)
	}
}

func TestZeitwerkRailsRake(t *testing.T) {
	expect(t, "loader = Zeitwerk::Loader.for_gem\nloader.setup", "rb_zeitwerk ")
	expect(t, `loader.push_dir(File.join(__dir__, "app"))`, "rb_push_dir app [rel]")
	expect(t, `loader.push_dir("#{__dir__}/lib", namespace: Foo)`, "rb_push_dir lib [rel]")
	expect(t, `loader.push_dir(dir)`, "rb_push_dir  dyn")
	expect(t, "module App\n  class Application < Rails::Application\n  end\nend", "rb_rails app")
	expect(t, "class Engine < ::Rails::Engine\nend", "rb_rails engine")
	expect(t, `Rake::TestTask.new(:test) do |t|
  t.libs << "test"
  t.libs << "lib"
  t.pattern = "test/**/*_test.rb"
  t.ruby_opts << "-rhelper"
end`, "rb_test_task rake", "rb_test_lib test", "rb_test_lib lib", "rb_test_pattern test/**/*_test.rb [rake]", "rb_ruby_opt helper")
	expect(t, `Rake::TestTask.new do |t|
  t.test_files = FileList["test/a_test.rb", "test/b/*_test.rb"].exclude("test/fixtures/**/*")
end`, "rb_test_task rake", "rb_test_pattern test/a_test.rb [rake]", "rb_test_pattern test/b/*_test.rb [rake]")
	expect(t, `Rake::TestTask.new { |t| t.pattern = ENV["TEST"] || "test/**/test_*.rb" }`,
		"rb_test_task rake", "rb_test_pattern test/**/test_*.rb [rake]")
	expect(t, `Rake::TestTask.new { |t| t.test_files = TEST_FILES }`, "rb_test_task rake", "rb_test_pattern  [rake] dyn")
	expect(t, `Rake::TestTask.new { |t| t.ruby_opts << "-r#{HELPER}" }`, "rb_test_task rake", "rb_test_pattern  [rake] dyn")
	expect(t, `RSpec::Core::RakeTask.new(:spec) { |t| t.pattern = "spec/**/*_spec.rb" }`,
		"rb_test_task rspec", "rb_test_pattern spec/**/*_spec.rb [rspec]")
	expect(t, `Rake::TestTask.new { |t| t.test_files = Dir.glob("t/**/*_test.rb") - Dir["t/slow/*"] }`,
		"rb_test_task rake", "rb_test_pattern t/**/*_test.rb [rake]")
}

func TestGemfile(t *testing.T) {
	expect(t, `source "https://rubygems.org"
gemspec
gemspec path: "sub"
gem "rails", "~> 7.0"
gem "foo-bar", require: "foo/bar"
gem "rspec", require: false
group :test do
  gem "minitest"
end
Bundler.require(*Rails.groups)
`, "rb_gemspec ", "rb_gemspec sub", "rb_gem rails", "rb_gem foo-bar [foo/bar]", "rb_gem rspec [-]", "rb_gem minitest", "rb_bundler ")
	expect(t, `Gem.bin_path("rubocop", "rubocop")`, "rb_bin_path rubocop")
	expect(t, `fixtures :all`, "rb_fixtures_all ")
	expect(t, `$LOAD_PATH.unshift File.expand_path("../lib", __dir__)`, "rb_load_path ")
}

func TestStrings(t *testing.T) {
	got := refs(t, `x = File.read(File.join(__dir__, "fixtures", "a.json"))
system("exe/rubocop --only Style")
y = "spec/fixtures/b.yml"
z = "Use x instead."
w = %w[config/default.yml plain]
v = `+"`bin/x --help`"+`
u = "#{__dir__}/data/c.txt"
`, true)
	want := []string{"rb_string fixtures/a.json [rel]", "rb_string exe/rubocop", "rb_string spec/fixtures/b.yml",
		"rb_string config/default.yml", "rb_string bin/x", "rb_string data/c.txt [rel]"}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestLexerAmbiguities(t *testing.T) {
	cases := []string{
		"x = a / b; require 'r'",
		"x = 4 / 2 # /\nrequire 'r'",
		"puts /x'y/\nrequire 'r'",
		"a = foo(/'/)\nrequire 'r'",
		"a = b ? 'c' : 'd'\nrequire 'r'",
		"c == ?' ? 1 : 2\nrequire 'r'",
		"x = [?a, ?\\n, ?\"]\nrequire 'r'",
		"h = { :\"a'b\" => 1, 'c': 2 }\nrequire 'r'",
		"s = %w(a'b c) + %i[d\"]\nrequire 'r'",
		"s = %q{a {nested} 'b}\nrequire 'r'",
		"s = %Q<a <b> #{\"}\"}>\nrequire 'r'",
		"s = %|a'b|\nrequire 'r'",
		"x = a % 2; y = 'z'\nrequire 'r'",
		"foo(<<~A, <<-B)\n  a'\nA\n  b\"\n  B\nrequire 'r'",
		"x = <<~'EOS'.strip\n  #{not interp} '\nEOS\nrequire 'r'",
		"x = <<EOS\na \"#{y + \"}\"}\" '\nEOS\nrequire 'r'",
		"class << self\n  def x; end\nend\nrequire 'r'",
		"a << b << 'c'\nrequire 'r'",
		"def /(other); end\ndef %(x); end\ndef `(cmd); end\nrequire 'r'",
		"x = :/ ; y = :% ; z = :<<\nrequire 'r'",
		"s = \"a #{\"b #{'c'} }\" } d\"\nrequire 'r'",
		"r = %r{a/b#{x}}i\nrequire 'r'",
		"$\" << 'x'; $' ; $`\nrequire 'r'",
		"x = y.empty? ? 'a' : 'b'\nrequire 'r'",
		"x = 1.0e-5 + 0x1F + 1_000 + 2r + 3i\nrequire 'r'",
		"a = 1\nb = a /2\nc = 'd'\nrequire 'r'",
		"foo.bar /x/\nrequire 'r'",
		"x = a&.b || c\nrequire 'r'",
		"-> (x) { x / 2 }\nrequire 'r'",
		"x = \"a\\\"b'\"\nrequire 'r'",
		"x = 'a\\'b\"'\nrequire 'r'",
		"=begin\n'\n=end\nrequire 'r'",
		"x = 1 \\\n  + 2\nrequire 'r'",
		"if x then y end; puts %(a'b)\nrequire 'r'",
		"x = `ls '`\nrequire 'r'",
		"ary.map { |p| p / 2 }\nrequire 'r'",
		"case x\nwhen :$&, :$MATCH then 1\nwhen :$`, :$' then 2\nend\nrequire 'r'",
		"assert_includes %w(\n  a'b\n  c\n), x\nrequire 'r'",
	}
	for _, c := range cases {
		got := refs(t, c, false)
		if !slices.Contains(got, "rb_require r") {
			t.Errorf("source %q: require 'r' not found: %q", c, got)
		}
	}
}

func TestUncertain(t *testing.T) {
	for _, c := range []string{
		"x = 'unterminated", "x = \"a #{b\"", "x = <<~EOS\nno end", "=begin\nnever ends", "x = %w(a b", "r = /abc",
		"x = foo /a\nrequire 'r' # b/", "x = y /2 if z\nrequire 'r'",
	} {
		r := Scan("a.rb", []byte(c))
		if !r.Uncertain || r.UncertainReason == "" {
			t.Errorf("%q: want uncertain, got %+v", c, r)
		}
	}
}

func TestGemspecFingerprint(t *testing.T) {
	base := `Gem::Specification.new do |spec|
  spec.name = "x"
  spec.version = X::VERSION
  spec.summary = "old"
  spec.description = <<~DESC
    long text
  DESC
  spec.metadata["changelog_uri"] = "https://a"
  spec.metadata = {
    "a" => "b",
  }
  spec.authors = ["A"]
  spec.add_dependency "y", "~> 1.0"
end
`
	fp := func(s string) string {
		f, ok := GemspecFingerprint([]byte(s))
		if !ok {
			t.Fatalf("not ok: %s", s)
		}
		return f
	}
	b := fp(base)
	harmless := strings.NewReplacer(`"old"`, `"new"`, "long text", "other text", `"https://a"`, `"https://b"`, `["A"]`, `["A", "B"]`,
		`"a" => "b"`, `"c" => "d"`).Replace(base)
	if fp(harmless) != b {
		t.Fatal("harmless change changed the fingerprint")
	}
	if fp("# comment\n"+base) != b {
		t.Fatal("comment changed the fingerprint")
	}
	for _, c := range []string{
		strings.Replace(base, `"~> 1.0"`, `"~> 2.0"`, 1),
		strings.Replace(base, `spec.name = "x"`, `spec.name = "z"`, 1),
		strings.Replace(base, `spec.summary = "old"`, `spec.summary = "old"; spec.files = []`, 1),
		strings.Replace(base, "end\n", "  spec.require_paths = [\"lib\"]\nend\n", 1),
	} {
		if fp(c) == b {
			t.Fatalf("relevant change not detected:\n%s", c)
		}
	}
}

func ExampleScan() {
	r := Scan("spec/spec_helper.rb", []byte(`require "bundler/setup"
require_relative "../lib/foo"
Dir[File.join(__dir__, "support", "**", "*.rb")].each { |f| require f }
`))
	for _, im := range r.Imports {
		fmt.Println(strings.TrimSpace(fmt.Sprint(im.Kind, " ", im.Spec, " ", im.Suffix)))
	}
	// Output:
	// rb_require bundler/setup
	// rb_require ../lib/foo rel
	// rb_glob support/**/*.rb rel
}

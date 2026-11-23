package rbdyn

import (
	"strings"
	"testing"
)

const libBase = `# frozen_string_literal: true

module Shop
  # A cart.
  class Cart
    TAX = 0.2
    attr_reader :items

    def initialize(items = [])
      @items = items
    end

    def total(discount: 0,
              rounding: 2)
      sum = items.sum(&:price)
      (sum * (1 + TAX) - discount).round(rounding)
    end

    def describe
      text = <<~TXT
        Cart with #{items.size} items
      TXT
      text.strip
    end

    def safe
      Integer(items.first)
    rescue ArgumentError
      0
    ensure
      @checked = true
    end

    class << self
      def empty
        new([])
      end
    end

    def self.build(*items)
      new(items)
    end

    private def secret
      42
    end

    def one; 1; end

    def endless = 3
  end

  module Helpers
    def self.fmt(x)
      x.to_s
    end
  end
end
`

func mustGuard(t *testing.T, base, head string) []Span {
	t.Helper()
	spans, why := BodyGuard("lib/shop.rb", []byte(base), []byte(head))
	if why != "" {
		t.Fatalf("refused: %s", why)
	}
	return spans
}

func mustRefuse(t *testing.T, base, head, want string) {
	t.Helper()
	_, why := BodyGuard("lib/shop.rb", []byte(base), []byte(head))
	if why == "" {
		t.Fatalf("accepted, want a refusal containing %q", want)
	}
	if !strings.Contains(why, want) {
		t.Fatalf("refusal %q does not contain %q", why, want)
	}
}

func repl(t *testing.T, s, old, new string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("fixture lacks %q", old)
	}
	return strings.Replace(s, old, new, 1)
}

func TestBodyGuardAccepts(t *testing.T) {
	cases := []struct {
		name, old, new string
		from, to       int
		what           string
	}{
		{"instance method", "@items = items", "@items = items.dup", 9, 11, "Shop::Cart#initialize"},
		{"multi-line signature", "(sum * (1 + TAX) - discount)", "(sum * (1 + TAX) - discount * 2)", 13, 17, "Shop::Cart#total"},
		{"heredoc inside a body", "Cart with #{items.size} items", "Cart holding #{items.size} items", 19, 24, "Shop::Cart#describe"},
		{"rescue clause", "      0\n    ensure", "      -1\n    ensure", 26, 32, "Shop::Cart#safe"},
		{"ensure clause", "@checked = true", "@checked = :yes", 26, 32, "Shop::Cart#safe"},
		{"class << self", "new([])", "new([].freeze)", 35, 37, "Shop::Cart.empty"},
		{"def self.x", "new(items)\n    end\n\n    private", "new(items.compact)\n    end\n\n    private", 40, 42, "Shop::Cart.build"},
		{"private def one-liner header", "      42", "      43", 44, 46, "Shop::Cart#secret"},
		{"method in a module", "x.to_s", "x.inspect", 54, 56, "Shop::Helpers.fmt"},
		{"added line in a body", "      text.strip\n", "      text = text.upcase\n      text.strip\n", 19, 24, "Shop::Cart#describe"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spans := mustGuard(t, libBase, repl(t, libBase, c.old, c.new))
			if len(spans) != 1 || spans[0].From != c.from || spans[0].To != c.to || !strings.HasSuffix(spans[0].What, c.what) {
				t.Fatalf("spans = %+v, want one %d-%d ending in %q", spans, c.from, c.to, c.what)
			}
		})
	}
}

func TestBodyGuardCommentsAndLayoutAreFree(t *testing.T) {
	head := repl(t, libBase, "  # A cart.\n", "  # A shopping cart.\n  # Second comment line.\n\n")
	head = repl(t, head, "      sum = items.sum(&:price)", "      sum = items.sum(&:price) # the sum")
	head = repl(t, head, "    TAX = 0.2", "    TAX = 0.2 # rate")
	if spans := mustGuard(t, libBase, head); len(spans) != 0 {
		t.Fatalf("comment-only change gave members %+v", spans)
	}
	// =begin/=end blocks are comments too.
	head2 := repl(t, libBase, "  module Helpers", "=begin\nclass Fake; end\n=end\n  module Helpers")
	if spans := mustGuard(t, libBase, head2); len(spans) != 0 {
		t.Fatalf("=begin comment gave members %+v", spans)
	}
	// Two methods changed: both reported, base spans.
	head3 := repl(t, repl(t, libBase, "@items = items", "@items = items.to_a"), "x.to_s", "x.to_str")
	if spans := mustGuard(t, libBase, head3); len(spans) != 2 || spans[0].From != 9 || spans[1].From != 54 {
		t.Fatalf("spans = %+v", spans)
	}
}

func TestBodyGuardRefuses(t *testing.T) {
	cases := []struct{ name, old, new, want string }{
		{"constant", "TAX = 0.2", "TAX = 0.3", "outside method bodies"},
		{"attr_reader", "attr_reader :items", "attr_reader :items, :tax", "outside method bodies"},
		{"include", "attr_reader :items", "attr_reader :items\n    include Comparable", "outside method bodies"},
		{"private", "private def secret", "def secret", "outside method bodies"},
		{"alias", "attr_reader :items", "attr_reader :items\n    alias_method :all, :items", "outside method bodies"},
		{"define_method", "attr_reader :items", "attr_reader :items\n    define_method(:x) { 1 }", "outside method bodies"},
		{"require", "module Shop", "require \"json\"\nmodule Shop", "outside method bodies"},
		{"signature", "def initialize(items = [])", "def initialize(items = nil)", "outside method bodies"},
		{"multi-line signature default", "rounding: 2)", "rounding: 3)", "outside method bodies"},
		{"def line", "def describe", "def describe2", "outside method bodies"},
		{"end line", "      text.strip\n    end", "      text.strip\n    end; FOO = 1", "outside method bodies"},
		{"one-liner", "def one; 1; end", "def one; 2; end", "outside method bodies"},
		{"endless def", "def endless = 3", "def endless = 4", "outside method bodies"},
		{"class body DSL", "class Cart\n", "class Cart\n    validates :x\n", "outside method bodies"},
		{"magic comment", "# frozen_string_literal: true", "# frozen_string_literal: false", "magic comment"},
		{"nested def", "@items = items", "@items = items\n      def inner; end", "contains a def"},
		{"BEGIN block", "module Shop", "BEGIN { $x = 1 }\nmodule Shop", "outside method bodies"},
		{"class-level heredoc", "TAX = 0.2", "TAX = 0.2\n    DOC = <<~D\n      text\n    D", "outside method bodies"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mustRefuse(t, libBase, repl(t, libBase, c.old, c.new), c.want)
		})
	}
}

func TestBodyGuardHeredocOutsideDef(t *testing.T) {
	base := "class A\n  DOC = <<~D\n    one\n  D\n\n  def x\n    1\n  end\nend\n"
	mustRefuse(t, base, strings.Replace(base, "    one", "    two", 1), "outside method bodies")
	// The same heredoc text inside a method body is fine.
	base2 := "class A\n  def doc\n    <<~D\n      one\n    D\n  end\nend\n"
	if s := mustGuard(t, base2, strings.Replace(base2, "      one", "      two", 1)); len(s) != 1 || s[0].From != 2 || s[0].To != 6 {
		t.Fatalf("spans %+v", s)
	}
}

func TestBodyGuardDataSection(t *testing.T) {
	base := "def x\n  DATA.read\nend\n__END__\nhello\n"
	mustRefuse(t, base, strings.Replace(base, "hello", "bye", 1), "__END__")
}

func TestBodyGuardUncertainLexer(t *testing.T) {
	base := "def x\n  1\nend\n"
	mustRefuse(t, base, "def x\n  s = <<~A\n  1\nend\n", "lexer")
}

func TestBodyGuardModifiersAndBlocks(t *testing.T) {
	base := `class A
  def x(list)
    return if list.empty?
    y = 1 unless list.size > 3
    list.each do |i|
      puts i if i
    end
    while y < 3 do y += 1 end
    z = if y then 1 else 2 end
    begin
      foo
    end while false
    case y
    when 1 then :a
    end
    for k in list do k end
    h = { if: 1, end: 2, class: 3 }
    self.class.name
    y
  end

  def after
    :after
  end
end
`
	head := strings.Replace(base, "    y\n  end", "    y + 1\n  end", 1)
	s := mustGuard(t, base, head)
	if len(s) != 1 || s[0].From != 2 || s[0].To != 20 {
		t.Fatalf("spans %+v", s)
	}
	head2 := strings.Replace(base, ":after", ":later", 1)
	s = mustGuard(t, base, head2)
	if len(s) != 1 || s[0].From != 22 || s[0].To != 24 {
		t.Fatalf("spans %+v", s)
	}
}

func TestBodyGuardDefsInBlocksAndSetters(t *testing.T) {
	base := `B = Class.new do
  def w
    1
  end
end

class C
  def x=(v)
    @x = v
  end

  def ==(o)
    o.is_a?(C)
  end

  def call(...)
    run(...)
  end
end
`
	for _, c := range []struct {
		old, new string
		from     int
	}{{"    1\n", "    2\n", 2}, {"@x = v", "@x = v.to_s", 8}, {"o.is_a?(C)", "o.instance_of?(C)", 12}, {"run(...)", "go(...)", 16}} {
		s := mustGuard(t, base, strings.Replace(base, c.old, c.new, 1))
		if len(s) != 1 || s[0].From != c.from {
			t.Fatalf("%q: spans %+v", c.new, s)
		}
	}
}

func TestBodyGuardUnbalancedRefuses(t *testing.T) {
	base := "class A\n  def x\n    1\n  end\nend\n"
	mustRefuse(t, base, "class A\n  def x\n    if y\n  end\nend\n", "unbalanced")
}

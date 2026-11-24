package evalkit

import (
	"sort"
	"strings"
	"testing"
)

func rbMutated(src string, c rustCand) string { return src[:c.Start] + c.Repl + src[c.End:] }

func TestRbScanCandidates(t *testing.T) {
	src := `# frozen_string_literal: true
require "a/b" if x == 1
# a == b in a comment
=begin
c == d
=end
module Foo
  class Bar < Base
    def baz(a, b = 2)
      return a.size if a == b
      s = "str == #{a > 1}" + 'lit'
      t = <<~EOS
        x == y
      EOS
      u = %w[a == b]
      v = s =~ /a<b/
      w = a.empty? && b > 3 || !c
      return false unless w
      x = { if: true, "k": 1 }
      a - b
    end
  end
end
`
	var got []string
	for _, c := range rbScan([]byte(src)) {
		l, _ := lineOf([]byte(src), c.Start)
		_, after := lineOf([]byte(rbMutated(src, c)), c.Start)
		got = append(got, strings.Join([]string{itoa(l), c.Op, after}, " "))
	}
	sort.Strings(got)
	want := []string{
		"10 eq-ne return a.size if a != b",
		"10 if-unless return a.size unless a == b",
		"10 return-nil return nil if a == b",
		"11 add-sub s = \"str == #{a > 1}\" - 'lit'",
		"11 str-change s = \"str == #{a > 1}\" + 'lit_mut'",
		"17 and-or w = a.empty? || b > 3 || !c",
		"17 gt-le w = a.empty? && b <= 3 || !c",
		"17 int-inc w = a.empty? && b > 4 || !c",
		"17 or-and w = a.empty? && b > 3 && !c",
		"18 false-true return true unless w",
		"18 return-nil return nil unless w",
		"18 unless-if return false if w",
		"19 int-dec x = { if: true, \"k\": 0 }",
		"19 true-false x = { if: false, \"k\": 1 }",
		"20 sub-add a + b",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("candidates:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func itoa(i int) string { return pad2(i) }

func TestRbLexerSymbolsAndHeredocs(t *testing.T) {
	src := "a = :== \nb = x <<~A\n  y == z\nA\nc = arr << 1\nd = h[:<=]\ne = cond ? 1 : 2\n"
	var ops []string
	for _, c := range rbScan([]byte(src)) {
		ops = append(ops, c.Op)
	}
	sort.Strings(ops)
	// :== and :<= are symbols, the heredoc body is skipped; `arr << 1` and the ternary only give integer candidates (1, 1, 2)
	if got := strings.Join(ops, ","); got != "int-dec,int-dec,int-inc" {
		t.Fatalf("ops = %s", got)
	}
}

func TestRbCandidateFile(t *testing.T) {
	for f, want := range map[string]bool{
		"lib/rack/utils.rb":           true,
		"lib/dry/types/spec/types.rb": false,
		"test/spec_utils.rb":          false,
		"lib/rake/test_task.rb":       true,
		"lib/foo_test.rb":             false,
		"exe/rake":                    false,
		"Rakefile":                    false,
	} {
		if got := rbCandidateFile(f, nil); got != want {
			t.Errorf("%s: %v, want %v", f, got, want)
		}
	}
}

func TestRbFailedTests(t *testing.T) {
	out := "Run options: --seed 1\n\n  1) Failure:\nRack::Utils#test_0031_figure out which encodings are acceptable [test/spec_utils.rb:534]:\nExpected: \"foo\"\n" +
		"\n  2) Error:\nRack::Files#test_0002_serves files:\nNameError: x\n" +
		"===\nFailure: test_load(TestRakeRequire)\nx.rb:3\n===\nError: test_run(TestRakeFunctional): RuntimeError: boom\n" +
		"Failed examples:\n\nrspec ./spec/dry/types/sum_spec.rb:12 # Dry::Types::Sum works\r\n"
	got := strings.Join(rbFailedTests(out), "\n")
	want := strings.Join([]string{
		"minitest:Rack::Files#test_0002_serves files",
		"minitest:Rack::Utils#test_0031_figure out which encodings are acceptable",
		"rspec:Dry::Types::Sum works",
		"test-unit:test_load(TestRakeRequire)",
		"test-unit:test_run(TestRakeFunctional)",
	}, "\n")
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	bl := rbBaseline{T2Baseline: T2Baseline{Failing: []string{"a", "b"}, Flaky: []string{"c"}, FailingTests: map[string][]string{"a": {"minitest:X#t1"}}}}
	for _, c := range []struct {
		f    string
		x    rbFileResult
		want string
	}{
		{"a", rbFileResult{State: "passed"}, "green"},
		{"a", rbFileResult{State: "failed", Failed: []string{"minitest:X#t1"}}, "excluded"},
		{"a", rbFileResult{State: "failed", Failed: []string{"minitest:X#t1", "minitest:X#t2"}}, "red"},
		{"a", rbFileResult{State: "failed"}, "red"},
		{"a", rbFileResult{State: "timeout"}, "red"},
		{"b", rbFileResult{State: "failed"}, "excluded"},
		{"c", rbFileResult{State: "failed"}, "excluded"},
		{"d", rbFileResult{State: "failed"}, "red"},
	} {
		if got := bl.verdict(c.f, c.x); got != c.want {
			t.Errorf("%s %+v: %s, want %s", c.f, c.x, got, c.want)
		}
	}
}

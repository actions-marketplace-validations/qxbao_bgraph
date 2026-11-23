package rbdyn

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// testRuby returns a Ruby executable (BGRAPH_TEST_RUBY, else ruby on PATH) or skips: CI has no Ruby.
func testRuby(t *testing.T) string {
	t.Helper()
	if r := os.Getenv("BGRAPH_TEST_RUBY"); r != "" {
		return r
	}
	r, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("no ruby (set BGRAPH_TEST_RUBY)")
	}
	return r
}

func TestParseCheck(t *testing.T) {
	ruby := testRuby(t)
	ctx := context.Background()
	base := "def x(a)\n  if a.respond_to?(:to_str)\n    1\n  elsif a\n    2\n  end\nend\n"
	cases := []struct{ name, head, want string }{
		{"compiles", strings.Replace(base, "    1\n", "    3\n", 1), ""},
		{"unless with elsif", strings.Replace(base, "  if a.", "  unless a.", 1), "does not compile at the head"},
		{"new warning", strings.Replace(base, "    1\n", "    unused = 1\n    1\n", 1), "warnings"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			why := ParseCheck(ctx, ruby, []ParseFile{{Path: "lib/x.rb", Base: []byte(base), Head: []byte(c.head)}})
			if c.want == "" && why != "" || c.want != "" && !strings.Contains(why, c.want) {
				t.Fatalf("ParseCheck = %q, want %q", why, c.want)
			}
		})
	}
	if why := ParseCheck(ctx, ruby+"-missing", []ParseFile{{Path: "lib/x.rb", Base: []byte(base), Head: []byte(base)}}); why == "" {
		t.Fatal("a Ruby that cannot run passed the check")
	}
}

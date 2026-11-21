package glob

import (
	"strings"
	"testing"
)

// TestStaticPrefixSound: Match(p) ⇒ StaticPrefix rỗng hoặc là thư mục tổ tiên của p. graph dựa vào bất biến này để
// chỉ xét pattern theo tiền tố; vi phạm là sót im lặng.
func TestStaticPrefixSound(t *testing.T) {
	pats := []string{
		"a/b/c.ts", "a/**", "a/b/**/x.ts", "**/x.ts", "*.md", "a/*/c", `src/\[id\]/**`, `src/\{x\}/a.ts`,
		"{a,b}/c/**", "{a/b,a/c}/d", "a/{b,c}/d/**", "a/b{1,2}/c", "[ab]/c", "a/?/c", `pkg/my*dir/**`,
		"x/y/z", "./a/b/**", "a/b/", "a//b/c",
	}
	paths := []string{
		"a/b/c.ts", "a/b/x.ts", "a/b/q/x.ts", "x.ts", "r.md", "src/[id]/p.ts", "src/{x}/a.ts", "a/c/z", "b/c/z",
		"a/b/d", "a/c/d", "a/b/d/e", "a/c/d/e", "a/b1/c", "a/b2/c", "a/c", "b/c", "a/q/c", "pkg/my*dir/f", "x/y/z",
	}
	for _, ps := range pats {
		p, err := Compile(ps)
		if err != nil {
			continue
		}
		pre := p.StaticPrefix()
		for _, path := range paths {
			if !p.Match(path) {
				continue
			}
			if pre != "" && !strings.HasPrefix(path, pre+"/") {
				t.Errorf("pattern %q khớp %q nhưng tiền tố tĩnh %q không phải tổ tiên", ps, path, pre)
			}
		}
	}
}

func FuzzStaticPrefixSound(f *testing.F) {
	f.Add("a/{b,c}/**", "a/b/x")
	f.Add(`s/\[i\]/*`, "s/[i]/x")
	f.Fuzz(func(t *testing.T, ps, path string) {
		p, err := Compile(ps)
		if err != nil || !p.Match(path) {
			return
		}
		if pre := p.StaticPrefix(); pre != "" && !strings.HasPrefix(path, pre+"/") {
			t.Fatalf("pattern %q khớp %q nhưng tiền tố %q", ps, path, pre)
		}
	})
}

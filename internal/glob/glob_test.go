package glob

import (
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		pat, path string
		want      bool
	}{
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false},
		{"**/*.md", "README.md", true},
		{"**/*.md", "a/b/README.md", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/y/c", false},
		{"a/**", "a/b", true},
		{"a/**", "a/b/c/d", true},
		{"a/**", "a", false},
		{"a/**", "ab/c", false},
		{"**", "x", true},
		{"**", "x/y", true},
		{"./src/*.ts", "src/a.ts", true},
		{"src/*.ts", "src/a/b.ts", false},
		{"a?c", "abc", true},
		{"a?c", "a/c", false},
		{"a?c", "ac", false},
		{"[abc].go", "b.go", true},
		{"[abc].go", "d.go", false},
		{"[a-z]x", "qx", true},
		{"[a-z]x", "Qx", false},
		{"[!a]x", "bx", true},
		{"[!a]x", "ax", false},
		{"[^a]x", "ax", false},
		{"[^a]x", "zx", true},
		{"[]]x", "]x", true},
		{"{a,b,c}.go", "b.go", true},
		{"{a,b,c}.go", "d.go", false},
		{"*.{ts,tsx}", "a.tsx", true},
		{"*.{ts,tsx}", "a.js", false},
		{"{src,lib}/**/*.ts", "lib/x/y.ts", true},
		{"{src,lib}/**/*.ts", "app/x/y.ts", false},
		{"{a/b,c}/d", "a/b/d", true},
		{"{a/b,c}/d", "c/d", true},
		{"{a,{b,c}d}/x", "cd/x", true},
		{"{a,{b,c}d}/x", "a/x", true},
		{"{a,{b,c}d}/x", "b/x", false},
		{"{src/**,docs}/x", "src/q/x", true},
		{`a\*b`, "a*b", true},
		{`a\*b`, "axb", false},
		{`\[x\]`, "[x]", true},
		{`\**`, "*abc", true},
		{"*", "a", true},
		{"*", "a/b", false},
		{"*a*", "bab", true},
		{"a*b", "ab", true},
		{"a*b", "a/b", false},
		{"A.go", "a.go", false},
		{"é?", "éx", true},
		{"?", "é", true},
		{"a//b", "a/b", true},
		{"packages/*/src/**", "packages/ui/src/x/y.ts", true},
		{"packages/*/src/**", "packages/ui/lib/y.ts", false},
		{"**/a/**/b/**", "x/a/y/b/z", true},
		{"**/a/**/b/**", "a/b", false},
		{"**/a/**/b/**", "a/b/c", true},
		{"**/*", "a", true},
		{"a/**/**/b", "a/b", true},
		{"**.ts", "x.ts", true},
		{"**.ts", "a/x.ts", false},
		{"", "a", false},
		{"a", "", false},
		{"[a", "a", false},
		{"{a", "a", false},
	}
	for _, tc := range tests {
		if got := Match(tc.pat, tc.path); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pat, tc.path, got, tc.want)
		}
	}
}

func TestCompileErrors(t *testing.T) {
	for _, pat := range []string{"", "[a", "{a,b", `a\`, "[z-a]", "./"} {
		if _, err := Compile(pat); err == nil {
			t.Errorf("Compile(%q): want error", pat)
		}
	}
	if !Match("a}b", "a}b") {
		t.Error("a lone '}' must be an ordinary character")
	}
}

func TestStaticPrefix(t *testing.T) {
	tests := map[string]string{
		"src/a/**/*.ts":      "src/a",
		"**/*.ts":            "",
		"src/*.ts":           "src",
		"a/b/c.ts":           "a/b",
		"*.md":               "",
		"./pkg/x/**":         "pkg/x",
		"{src,lib}/**":       "",
		"{src/a,src/b}/**":   "src",
		"src/{a,b}/x/**/*.c": "src",
		"a/b*/c":             "a",
		`a/\*/c`:             "a/*",
	}
	for pat, want := range tests {
		if got := MustCompile(pat).StaticPrefix(); got != want {
			t.Errorf("StaticPrefix(%q) = %q, want %q", pat, got, want)
		}
	}
}

func TestNoCatastrophicBacktracking(t *testing.T) {
	long := strings.Repeat("a", 5000)
	if MustCompile("a*a*a*a*a*a*a*a*b").Match(long) {
		t.Error("must not match")
	}
	segs := strings.Repeat("aa/", 500) + "c"
	if MustCompile("**/a*/**/a*/**/a*/**/b").Match(segs) {
		t.Error("must not match")
	}
}

func FuzzCompileMatch(f *testing.F) {
	for _, s := range []string{"a/**/b", "{a,b}/*.ts", "[a-z]*", `\`, "[", "{{{", "**/**", "a/{b,c/**}/d", "[!]]"} {
		f.Add(s, "a/b/c.ts")
	}
	f.Fuzz(func(t *testing.T, pat, path string) {
		p, err := Compile(pat)
		if err != nil {
			return
		}
		_ = p.Match(path)
		_ = p.StaticPrefix()
	})
}

func BenchmarkMatch(b *testing.B) {
	p := MustCompile("src/**/*.{ts,tsx}")
	for i := 0; i < b.N; i++ {
		p.Match("src/components/ui/button/index.tsx")
	}
}

package php

import (
	"strings"
	"testing"
)

func kinds(ts []Token) string {
	var b strings.Builder
	for _, t := range ts {
		b.WriteByte(t.Kind)
	}
	return b.String()
}

func TestTokensBasics(t *testing.T) {
	src := "<html>\n<?php\n// c\n# c2\n/* b */\n/** doc */\n#[Attr]\nfunction f() { return \"a{$x['}']}b\" . 'q\\'' . `ls`; }\n?>\ntail\n"
	ts, why := Tokens([]byte(src))
	if why != "" {
		t.Fatalf("problem %q", why)
	}
	var docLine, fnLine, tailKind int
	for _, tk := range ts {
		switch {
		case tk.Kind == TokDocComment:
			docLine = tk.Line
		case tk.Kind == TokIdent && tk.Text == "function":
			fnLine = tk.Line
		case tk.Kind == TokHTML && strings.Contains(tk.Text, "tail"):
			tailKind = tk.Line
		}
	}
	if docLine != 6 || fnLine != 8 || tailKind != 10 {
		t.Fatalf("lines: doc %d fn %d tail %d (%s)", docLine, fnLine, tailKind, kinds(ts))
	}
	// The interpolation with a quoted brace stays inside one string token.
	n := 0
	for _, tk := range ts {
		if tk.Kind == TokString {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("want 3 strings, got %d: %s", n, kinds(ts))
	}
}

func TestTokensHeredoc(t *testing.T) {
	src := "<?php\n$a = <<<EOT\n  x {$y} }\n  EOT;\n$b = <<<'N'\nraw $z\nN;\nfoo();\n"
	ts, why := Tokens([]byte(src))
	if why != "" {
		t.Fatalf("problem %q", why)
	}
	var strs []Token
	for _, tk := range ts {
		if tk.Kind == TokString {
			strs = append(strs, tk)
		}
	}
	if len(strs) != 2 || strs[0].Line != 2 || strs[0].EndLine != 4 || strs[1].Line != 5 || strs[1].EndLine != 7 {
		t.Fatalf("heredocs: %+v", strs)
	}
	last := ts[len(ts)-1]
	if last.Text != ";" || last.Line != 8 {
		t.Fatalf("last token %+v", last)
	}
}

func TestTokensHaltCompiler(t *testing.T) {
	ts, why := Tokens([]byte("<?php\nfoo();\n__halt_compiler();\n{{{ not php \"\n"))
	if why != "" {
		t.Fatalf("problem %q", why)
	}
	if last := ts[len(ts)-1]; last.Kind != TokHalt || !strings.Contains(last.Text, "not php") {
		t.Fatalf("halt data: %+v", last)
	}
}

func TestTokensProblems(t *testing.T) {
	for _, src := range []string{
		"<?php /* open",
		"<?php $a = 'open",
		"<?php $a = \"open {$x",
		"<?php $a = <<<EOT\nno end\n",
		"<? echo 1;",
		"<?php __halt_compiler(",
	} {
		if _, why := Tokens([]byte(src)); why == "" {
			t.Errorf("%q: want a problem", src)
		}
	}
}

func TestTokensCommentEndsAtCloseTag(t *testing.T) {
	ts, why := Tokens([]byte("<?php // a ?>html<?php echo 1;"))
	if why != "" {
		t.Fatal(why)
	}
	if k := kinds(ts); k != "ocxhoinp" {
		t.Fatalf("kinds %s", k)
	}
}

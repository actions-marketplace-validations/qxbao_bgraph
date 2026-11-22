package golang

import (
	"go/scanner"
	"go/token"
	"reflect"
	"strings"
	"testing"
)

var fuzzSeeds = []string{
	"package a\nimport \"fmt\"\nimport (\n\tx \"example.com/m/b\"\n\t_ \"embed\"\n)\n//go:embed a.txt `b c` \"d\"\nvar s string\n",
	"//go:build ignore\n\npackage main\n\nimport \"C\"\n",
	"package a_test\nimport \"testing\"\nfunc TestX(t *testing.T) { os.ReadFile(filepath.Join(\"..\", \"testdata\", n)); _ = `raw/x.txt`; _ = 'c' }\n",
	"\xef\xbb\xbfpackage a\r\nimport \"b\"\r\n//go:embed x\r\nvar v string\r\n",
	"package a\nvar s = \"\\u00e9/x.y\" + \"\\x00\" + \"a\\\"b.c\"\n",
	"package", "package a\nimport (", "package a\nimport \"", "package a\nimport `x", "/* unterminated", "\"unterminated", "`", "'",
	"package a\n//go:embed", "package a\n//go:embed \"", "package a\n//go:embed `", "package a\n//go:embed a\"b", "\x00", "\xff\xfe", "é",
	"package a\nfunc f() { x.Join(\"a\", \"b\"", "package a\nfunc f() { x.Join(", "package a\nfunc f() { .Join(\"a\",\"b\") }",
}

func FuzzScan(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s, false)
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, src string, test bool) {
		name := "a.go"
		if test {
			name = "a_test.go"
		}
		a := Scan(name, []byte(src))
		b := Scan(name, []byte(src))
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("non-deterministic: %+v vs %+v", a, b)
		}
		if a.Uncertain != (a.UncertainReason != "") {
			t.Fatalf("inconsistent uncertain flag: %+v", a)
		}
		if a.Uncertain {
			if a.Go != nil || len(a.Imports) != 0 {
				t.Fatalf("uncertain result carries data: %+v", a)
			}
			return
		}
		if a.Go == nil {
			t.Fatal("nil Go section on a certain result")
		}
		maxLine := strings.Count(src, "\n") + 1
		for _, im := range a.Imports {
			if im.Line < 1 || im.Line > maxLine {
				t.Fatalf("bad import %+v", im)
			}
		}
		for _, e := range a.Go.Embeds {
			if e.Line < 1 || e.Line > maxLine || (e.Pattern == "") == (e.Unknown == "") {
				t.Fatalf("bad embed %+v", e)
			}
		}
		for _, s := range a.Go.Strings {
			if !test && !strings.HasPrefix(s, "./") && !strings.HasPrefix(s, "../") {
				t.Fatalf("non-relative string in a non-test file: %q", s)
			}
		}
		for _, s := range a.Go.Strings {
			if s == "" || strings.ContainsAny(s, "\n\r\x00") || !strings.ContainsAny(s, "/\\.") {
				t.Fatalf("bad string %q", s)
			}
		}
	})
}

// Every seed must run without panicking (this also runs when -fuzz is not enabled).
func TestFuzzSeeds(t *testing.T) {
	for _, s := range fuzzSeeds {
		Scan("a.go", []byte(s))
		Scan("a_test.go", []byte(s))
	}
}

// Property: inserting a comment or a space between tokens of a valid source must not change what is found. (A newline is not
// inserted: Go's automatic semicolons make it significant.)
func TestCommentInsertionInvariant(t *testing.T) {
	base := "package a_test\n\nimport (\n\t\"os\"\n\tfp \"path/filepath\"\n\t_ \"embed\"\n)\n\n//go:embed testdata/*.json all:static\nvar fs embed.FS\n\n" +
		"func TestX(t *testing.T) {\n\tos.ReadFile(fp.Join(\"..\", \"testdata\", \"x.json\"))\n\t_ = 'x'\n\t_ = `golden/out.txt`\n}\n"
	want := Scan("a_test.go", []byte(base))
	if want.Uncertain {
		t.Fatal(want.UncertainReason)
	}
	if len(want.Imports) != 3 || len(want.Go.Embeds) != 2 || len(want.Go.Strings) < 3 {
		t.Fatalf("unexpected base scan: %+v %+v", want.Imports, want.Go)
	}
	fset := token.NewFileSet()
	file := fset.AddFile("a.go", -1, len(base))
	var s scanner.Scanner
	s.Init(file, []byte(base), nil, scanner.ScanComments)
	for {
		pos, tk, _ := s.Scan()
		if tk == token.EOF {
			break
		}
		if tk == token.COMMENT || (tk == token.SEMICOLON && base[file.Offset(pos)] == '\n') {
			continue // a directive comment must stay one line comment; automatic semicolons have no position of their own
		}
		off := file.Offset(pos)
		for _, ins := range []string{" ", "/* c */", "\t"} {
			src := base[:off] + ins + base[off:]
			got := Scan("a_test.go", []byte(src))
			if got.Uncertain {
				t.Fatalf("insert %q at %d made the source uncertain: %s", ins, off, got.UncertainReason)
			}
			if !reflect.DeepEqual(specs(got), specs(want)) || !reflect.DeepEqual(got.Go.Strings, want.Go.Strings) ||
				len(got.Go.Embeds) != len(want.Go.Embeds) {
				t.Fatalf("insert %q at %d changed results: %+v %+v", ins, off, got.Imports, got.Go)
			}
			for i, e := range got.Go.Embeds {
				if e.Pattern != want.Go.Embeds[i].Pattern {
					t.Fatalf("insert %q at %d changed embed %d: %+v", ins, off, i, e)
				}
			}
		}
	}
}

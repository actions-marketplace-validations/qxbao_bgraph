package rust

import (
	"reflect"
	"strings"
	"testing"
)

var fuzzSeeds = []string{
	"mod a;\npub mod b;\n#[path = \"x.rs\"] mod c;\n#[cfg_attr(unix, path = \"u.rs\")] mod d;",
	"mod a { mod b { mod c; } }",
	"macro_rules! m { ($n:ident) => { mod $n; }; }\nm!(a);\ncfg_if! { if #[cfg(unix)] { mod u; } }",
	"include!(\"a.rs\"); include_str!(concat!(env!(\"OUT_DIR\"), \"/x\")); include_bytes!(concat!(env!(\"CARGO_MANIFEST_DIR\"), \"/b\"))",
	"/* a /* nested */ still */ // mod x;\nlet s = r##\"a \"# b\"##; let b = br#\"x\"#; let c = c\"y\";",
	"fn f<'a, 'b: 'a>(x: &'a str) -> char { 'outer: loop { break 'outer; } let c = '\\u{1F600}'; let d = b'\\''; '\"' }",
	"let x = 1_000u8 + 0xFFi32 + 1.5e10 + 1..2 + x.0.1;",
	"#!/usr/bin/env run\nmod a;",
	"#![doc = include_str!(\"../README.md\")]\n#[doc = include_str!(\"d.md\")] pub mod z;",
	"\xef\xbb\xbfmod a;\r\nmod b;\r\n",
	"/* unterminated", "\"unterminated", "r#\"unterminated", "'", "'\\", "b'", "r#", "r#a", "mod", "mod $", "mod a", "#[path", "#[path =",
	"include!(", "include!(concat!(", "concat!(env!(", "((((", "))))", "{[(}])", "\x00", "\xff\xfe", "é",
}

func FuzzScan(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		a := Scan("a.rs", []byte(src))
		b := Scan("a.rs", []byte(src))
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("non-deterministic: %+v vs %+v", a, b)
		}
		if a.Uncertain != (a.UncertainReason != "") {
			t.Fatalf("inconsistent uncertain flag: %+v", a)
		}
		if a.Uncertain {
			if a.Rust != nil {
				t.Fatalf("uncertain result carries data: %+v", a)
			}
			return
		}
		if a.Rust == nil {
			t.Fatal("nil Rust section on a certain result")
		}
		maxLine := strings.Count(src, "\n") + 1
		for _, m := range a.Rust.Mods {
			if m.Name == "" || m.Line < 1 || m.Line > maxLine {
				t.Fatalf("bad mod %+v (max line %d)", m, maxLine)
			}
		}
		for _, inc := range a.Rust.Includes {
			if inc.Macro == "" || inc.Kind == "" || inc.Line < 1 || inc.Line > maxLine {
				t.Fatalf("bad include %+v", inc)
			}
			if (inc.Kind == "outdir" || inc.Kind == "dynamic") && inc.Path != "" {
				t.Fatalf("path on %s include: %+v", inc.Kind, inc)
			}
		}
		// The string extractor agrees with the scanner about well-formedness.
		if _, ok := Strings([]byte(src)); !ok {
			t.Fatalf("Strings rejects a source Scan accepted: %q", src)
		}
	})
}

// Every seed must run without panicking (this also runs when -fuzz is not enabled).
func TestFuzzSeeds(t *testing.T) {
	for _, s := range fuzzSeeds {
		Scan("a.rs", []byte(s))
		Strings([]byte(s))
	}
}

// Property: inserting a comment or whitespace between tokens of a valid source must not change what is found.
func TestCommentInsertionInvariant(t *testing.T) {
	base := "mod a;\n#[path = \"p.rs\"] pub mod b;\nmod c { mod d; }\ninclude_str!(\"x.txt\");\nfn f<'a>(x: &'a u8) -> char { 'z' }"
	want := Scan("a.rs", []byte(base))
	if want.Uncertain {
		t.Fatal(want.UncertainReason)
	}
	toks, _ := lex([]byte(base))
	for _, tk := range toks {
		for _, ins := range []string{" ", "/* c */", "/* /* n */ */", "\n"} {
			s := base[:tk.start] + ins + base[tk.start:]
			got := Scan("a.rs", []byte(s))
			if got.Uncertain {
				t.Fatalf("insert %q at %d made the source uncertain: %s", ins, tk.start, got.UncertainReason)
			}
			if len(got.Rust.Mods) != len(want.Rust.Mods) || len(got.Rust.Includes) != len(want.Rust.Includes) {
				t.Fatalf("insert %q at %d changed results: %+v", ins, tk.start, got.Rust)
			}
			for i, m := range got.Rust.Mods {
				w := want.Rust.Mods[i]
				if m.Name != w.Name || m.Path != w.Path || m.Unknown != w.Unknown || !reflect.DeepEqual(m.Nest, w.Nest) {
					t.Fatalf("insert %q at %d changed mod %d: %+v vs %+v", ins, tk.start, i, m, w)
				}
			}
		}
	}
}

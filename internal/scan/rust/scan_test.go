package rust

import (
	"reflect"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func mods(src string) []model.RustMod {
	r := Scan("a.rs", []byte(src))
	if r.Rust == nil {
		return nil
	}
	return r.Rust.Mods
}

func TestMods(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []model.RustMod
	}{
		{"plain", "mod a;\npub mod b;\npub(crate) mod c;\npub(in crate::x) mod d;",
			[]model.RustMod{{Name: "a", Line: 1}, {Name: "b", Line: 2}, {Name: "c", Line: 3}, {Name: "d", Line: 4}}},
		{"inline keeps nesting", "mod a { mod b; mod c { mod d; } }\nmod e;",
			[]model.RustMod{{Name: "b", Line: 1, Nest: []string{"a"}}, {Name: "d", Line: 1, Nest: []string{"a", "c"}}, {Name: "e", Line: 2}}},
		{"cfg test inline has no file", "#[cfg(test)]\nmod tests { fn f() {} }", nil},
		{"path attribute", "#[path = \"x/y.rs\"]\nmod a;\n#[cfg(unix)] #[path=\"u.rs\"] pub mod b;",
			[]model.RustMod{{Name: "a", Line: 2, Path: "x/y.rs"}, {Name: "b", Line: 3, Path: "u.rs"}}},
		{"raw path attribute", `#[path = r"a\b.rs"] mod a;`, []model.RustMod{{Name: "a", Line: 1, Path: `a\b.rs`}}},
		{"cfg_attr path is unknown", "#[cfg_attr(windows, path = \"w.rs\")]\nmod a;",
			[]model.RustMod{{Name: "a", Line: 2, Unknown: "cfg_attr with a path attribute"}}},
		{"non literal path", "#[path = concat!(\"a\", \".rs\")]\nmod a;",
			[]model.RustMod{{Name: "a", Line: 2, Unknown: "path attribute is not a string literal"}}},
		{"inside macro invocation", "cfg_if::cfg_if! { if #[cfg(unix)] { mod unix; } else { mod win; } }",
			[]model.RustMod{{Name: "unix", Line: 1, Unknown: "module declared inside a macro"}, {Name: "win", Line: 1, Unknown: "module declared inside a macro"}}},
		{"inside macro_rules body", "macro_rules! m { ($n:ident) => { mod $n; } }\nm!(a);",
			[]model.RustMod{{Name: "$n", Line: 1, Unknown: "module declared inside a macro"}}},
		{"inside function", "fn f() { mod a; }", []model.RustMod{{Name: "a", Line: 1, Unknown: "module declared inside a block"}}},
		{"inline module with path attribute", "#[path = \"d\"]\nmod a { mod b; }",
			[]model.RustMod{{Name: "b", Line: 2, Nest: []string{"a"}, Unknown: "enclosing inline module has a path attribute"}}},
		{"mod in comments and strings", "// mod a;\n/* mod b; /* mod c; */ mod d; */\nlet s = \"mod e;\"; let r = r#\"mod \"f\";\"#; let b = b\"mod g;\";\nmod real;",
			[]model.RustMod{{Name: "real", Line: 4}}},
		{"lifetimes and chars", "fn f<'a>(x: &'a str) -> char { let c = 'm'; let q = '\\''; let d = '\"'; c }\nmod after;",
			[]model.RustMod{{Name: "after", Line: 2}}},
		{"raw identifier is not a keyword", "const r#mod: u8 = 1;\nmod ok;", []model.RustMod{{Name: "ok", Line: 2}}},
		{"attribute with doc comment between", "#[path = \"p.rs\"]\n/// doc\nmod a;", []model.RustMod{{Name: "a", Line: 3, Path: "p.rs"}}},
		{"not an attribute of another item", "#[path = \"p.rs\"]\nfn f() {}\nmod a;", []model.RustMod{{Name: "a", Line: 3}}},
		{"unicode and escapes in strings", "let s = \"\\\" mod x; \\\\\"; let t = \"é\"; mod a;", []model.RustMod{{Name: "a", Line: 1}}},
		{"multiline string counts lines", "let s = \"a\nb\nc\";\nmod a;", []model.RustMod{{Name: "a", Line: 4}}},
		{"shebang", "#!/usr/bin/env -S cargo +nightly -Zscript\nmod a;", []model.RustMod{{Name: "a", Line: 2}}},
		{"inner attribute is not shebang", "#![allow(dead_code)]\nmod a;", []model.RustMod{{Name: "a", Line: 2}}},
		{"bom", "\xef\xbb\xbfmod a;", []model.RustMod{{Name: "a", Line: 1}}},
		{"field named mod is skipped", "let x = s.mod; mod a;", []model.RustMod{{Name: "a", Line: 1}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Scan("a.rs", []byte(c.src))
			if r.Uncertain {
				t.Fatalf("uncertain: %s", r.UncertainReason)
			}
			if got := mods(c.src); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestIncludes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []model.RustInclude
	}{
		{"str", `const A: &str = include_str!("../data/a.txt");`, []model.RustInclude{{Macro: "include_str", Kind: "literal", Path: "../data/a.txt", Line: 1}}},
		{"bytes and include", "include_bytes!(\"b.bin\");\ninclude!(\"gen.rs\");",
			[]model.RustInclude{{Macro: "include_bytes", Kind: "literal", Path: "b.bin", Line: 1}, {Macro: "include", Kind: "literal", Path: "gen.rs", Line: 2}}},
		{"doc attribute", "#![doc = include_str!(\"../README.md\")]", []model.RustInclude{{Macro: "include_str", Kind: "literal", Path: "../README.md", Line: 1}}},
		{"std path", `std::include_str!("a.txt")`, []model.RustInclude{{Macro: "include_str", Kind: "literal", Path: "a.txt", Line: 1}}},
		{"out dir", `include!(concat!(env!("OUT_DIR"), "/gen.rs"));`, []model.RustInclude{{Macro: "include", Kind: "outdir", Line: 1}}},
		{"manifest dir", `include_str!(concat!(env!("CARGO_MANIFEST_DIR"), "/data/x.txt"))`, []model.RustInclude{{Macro: "include_str", Kind: "manifest", Path: "data/x.txt", Line: 1}}},
		{"concat of literals", `include_str!(concat!("data/", "x.txt"))`, []model.RustInclude{{Macro: "include_str", Kind: "literal", Path: "data/x.txt", Line: 1}}},
		{"other env", `include_str!(concat!(env!("FOO"), "/x"))`, []model.RustInclude{{Macro: "include_str", Kind: "dynamic", Line: 1}}},
		{"dynamic", "include_str!(PATH);\ninclude!($p);", []model.RustInclude{{Macro: "include_str", Kind: "dynamic", Line: 1}, {Macro: "include", Kind: "dynamic", Line: 2}}},
		{"trailing comma", `include_str!("a.txt",)`, []model.RustInclude{{Macro: "include_str", Kind: "literal", Path: "a.txt", Line: 1}}},
		{"identifier named include is not a call", "let include = 1; if include != 2 {}", nil},
		{"in string", `let s = "include_str!(\"a\")";`, nil},
		{"automod", `automod::dir!("tests/builder");`, []model.RustInclude{{Macro: "automod::dir", Kind: "dir", Path: "tests/builder", Line: 1}}},
		{"automod pub", "automod::dir!(pub \"a\"); automod::dir!(pub(crate) \"b\"); automod::dir!(P);",
			[]model.RustInclude{{Macro: "automod::dir", Kind: "dir", Path: "a", Line: 1}, {Macro: "automod::dir", Kind: "dir", Path: "b", Line: 1}, {Macro: "automod::dir", Kind: "dynamic", Line: 1}}},
		{"escapes", `include_str!("a\\b\x41\u{42}.txt")`, []model.RustInclude{{Macro: "include_str", Kind: "literal", Path: `a\bAB.txt`, Line: 1}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Scan("a.rs", []byte(c.src))
			if r.Uncertain {
				t.Fatalf("uncertain: %s", r.UncertainReason)
			}
			var got []model.RustInclude
			if r.Rust != nil {
				got = r.Rust.Includes
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestUncertain(t *testing.T) {
	for _, src := range []string{"/* open", "\"open", "r#\"open\"", "fn f() {", "fn f() }", "fn f(] {", "'\\"} {
		if r := Scan("a.rs", []byte(src)); !r.Uncertain || r.UncertainReason == "" {
			t.Errorf("%q: want uncertain, got %+v", src, r)
		}
	}
	if r := Scan("a.rs", make([]byte, MaxFileSize+1)); !r.Uncertain || !strings.HasPrefix(r.UncertainReason, "file_too_large") {
		t.Errorf("too large: %+v", r)
	}
}

func TestStrings(t *testing.T) {
	got, ok := Strings([]byte("fn main() {\n println!(\"cargo:rerun-if-changed=../shared/x.txt\");\n let p = \"a\\nb\"; let r = r#\"raw \"q\"\"#; let b = b\"by\"; }"))
	want := []string{"cargo:rerun-if-changed=../shared/x.txt", "a\nb", `raw "q"`, "by"}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	if _, ok := Strings([]byte("\"open")); ok {
		t.Fatal("malformed source must not be ok")
	}
}

func TestDeterministicAndDeep(t *testing.T) {
	src := "mod a; mod b { mod c; }\ninclude!(\"x.rs\");"
	if !reflect.DeepEqual(Scan("a.rs", []byte(src)), Scan("a.rs", []byte(src))) {
		t.Fatal("non-deterministic")
	}
	deep := strings.Repeat("mod a {", 5000) + strings.Repeat("}", 5000) + "\nmod z;"
	if r := Scan("a.rs", []byte(deep)); r.Uncertain || len(r.Rust.Mods) != 1 {
		t.Fatalf("deep nesting: %+v", r)
	}
}

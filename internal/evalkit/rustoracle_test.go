package evalkit

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestLocateRustTests(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{
		"src/lib.rs": `
pub fn parse_flag() {}

#[cfg(test)]
mod tests {
    #[test]
    fn parse_flag() {}

    // #[test]
    // fn commented_out() {}

    #[test]
    fn strings() { let _ = "
#[test]
fn in_string() {}
"; }
}
`,
		"tests/cli/help.rs": `
#[tokio::test(flavor = "multi_thread")]
async fn works() {}

#[test]
#[should_panic]
pub fn helper_panics() {}
`,
		"tests/cli/version.rs": `
#[test]
fn works() {}
`,
		"tests/other.rs": `
#[test]
fn works() {}
fn not_a_test() {}
#[rstest]
#[case(1)]
fn parse_let_signature(#[case] x: i32) {}
macro_rules! gen { ($n:ident) => { #[test] fn $n() {} } }
gen!(from_macro);
rgtest!(r1319, |dir, cmd| {});
`,
		"tests/ui/bad.rs": "fn main() {}\n",
		"src/doc.rs":      "/// ```\n/// fn doc_fn() {}\n/// ```\npub fn x() {}\n",
	})
	got := LocateRustTests(tr, []string{
		"tests::parse_flag",                    // inline mod tests + same fn name in non-test code: only the test counts
		"cli::help::works",                     // path-consistent candidate
		"works",                                // unqualified: ambiguous
		"cli::works",                           // two consistent files: stay ambiguous
		"unknown::works",                       // no consistent candidate: keep all
		"helper_panics",                        // attribute stack
		"commented_out",                        // comment
		"in_string",                            // string literal
		"not_a_test",                           // no test attribute
		"from_macro",                           // macro-generated
		"parsing::parse_let_signature::case_5", // rstest case
		"src/lib.rs - parse (line 42)",         // doc-test
		"nope.rs - x (line 1)",                 // doc-test whose file is missing
		"regression::r1319",                    // test-like macro invocation: heuristic MacroFiles only
		"tests/ui/bad.rs",                      // trybuild UI test: the name is a file
		"doc_fn",                               // only in a doc comment
	})
	want := []RustTestLoc{
		{Name: "tests::parse_flag", Files: []string{"src/lib.rs"}},
		{Name: "cli::help::works", Files: []string{"tests/cli/help.rs"}},
		{Name: "works", Files: []string{"tests/cli/help.rs", "tests/cli/version.rs", "tests/other.rs"}, Ambiguous: true},
		{Name: "cli::works", Files: []string{"tests/cli/help.rs", "tests/cli/version.rs"}, Ambiguous: true},
		{Name: "unknown::works", Files: []string{"tests/cli/help.rs", "tests/cli/version.rs", "tests/other.rs"}, Ambiguous: true},
		{Name: "helper_panics", Files: []string{"tests/cli/help.rs"}},
		{Name: "commented_out", NotFound: true},
		{Name: "in_string", NotFound: true},
		{Name: "not_a_test", NotFound: true},
		{Name: "from_macro", NotFound: true},
		{Name: "parsing::parse_let_signature::case_5", Files: []string{"tests/other.rs"}},
		{Name: "src/lib.rs - parse (line 42)", Doc: true, DocFile: "src/lib.rs", Files: []string{"src/lib.rs"}},
		{Name: "nope.rs - x (line 1)", Doc: true, DocFile: "nope.rs", NotFound: true},
		{Name: "regression::r1319", NotFound: true, MacroFiles: []string{"tests/other.rs"}},
		{Name: "tests/ui/bad.rs", Files: []string{"tests/ui/bad.rs"}},
		{Name: "doc_fn", NotFound: true},
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("%s:\n got  %+v\n want %+v", want[i].Name, got[i], want[i])
		}
	}
}

func TestMaskRust(t *testing.T) {
	src := "let a = 'x'; let l: &'a str = r#\"fn in_raw\"#; /* /* fn nested */ */ fn real() {} // fn tail\n"
	m := maskRust(src)
	if len(m) != len(src) {
		t.Fatalf("length changed")
	}
	got := rustFnDef.FindAllStringSubmatch(m, -1)
	if len(got) != 1 || got[0][1] != "real" {
		t.Fatalf("%q -> %v", m, got)
	}
}

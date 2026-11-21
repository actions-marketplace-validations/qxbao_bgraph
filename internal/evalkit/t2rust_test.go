package evalkit

import (
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func scanOps(t *testing.T, src string) []string {
	t.Helper()
	cs, _ := rustScan([]byte(src))
	var out []string
	for _, c := range cs {
		out = append(out, c.Op+":"+src[c.Start:c.End])
	}
	sort.Strings(out)
	return out
}

func TestRustScanOperators(t *testing.T) {
	src := `
fn f(a: u32, b: u32) -> bool {
    // a == b in a comment
    let s = "x == y && z";
    let r = r#"a != b"#;
    let c = '<';
    let v: Vec<Vec<u8>> = Vec::new();
    let g = |x| x;
    let h = || 1;
    if a < b && b + 1 > a || a == b { return true; }
    let n = -1 - a;
    a >= b
}
#[cfg(test)]
mod tests {
    fn t() { assert!(1 == 1); }
}
#[test]
fn also_test() { let _ = 1 + 2; }
macro_rules! m { ($a:expr) => { $a == 1 } }
`
	got := scanOps(t, src)
	want := []string{"add-sub:+", "and-or:&&", "eq-ne:==", "ge-lt:>=", "gt-le:>", "lt-ge:<", "or-and:||", "sub-add:-", "true-false:true"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestRustScanTestModFile(t *testing.T) {
	cs, mods := rustScan([]byte("pub fn a() -> bool { true }\n#[cfg(test)]\nmod tests;\n#[cfg(not(test))]\nfn b() -> bool { false }\n"))
	if len(cs) != 2 || !reflect.DeepEqual(mods, []string{"tests"}) {
		t.Fatalf("%v %v", cs, mods)
	}
}

func TestGenRustMutants(t *testing.T) {
	files := []RustMutantSource{
		{Path: "src/lib.rs", Blob: "1", Data: []byte("fn a(x: u32) -> bool { x == 1 || x == 2 }\n#[cfg(test)]\nmod tests;\n")},
		{Path: "src/tests.rs", Blob: "2", Data: []byte("fn t() { assert!(1 == 1); }\n")},
		{Path: "src/b.rs", Blob: "3", Data: []byte("fn b(x: u32) -> u32 {\n    if x > 1 { x + 1 } else { x - 1 }\n}\n")},
	}
	m1 := GenRustMutants(files, "s", "r", 10)
	m2 := GenRustMutants(files, "s", "r", 10)
	if !reflect.DeepEqual(m1, m2) {
		t.Fatal("not deterministic")
	}
	if len(m1) != 2 { // one per line: lib.rs line 1, b.rs line 2
		t.Fatalf("got %d: %+v", len(m1), m1)
	}
	for _, m := range m1 {
		if m.File == "src/tests.rs" {
			t.Fatal("test module file was mutated")
		}
		out, err := ApplyEdit(files[map[string]int{"src/lib.rs": 0, "src/b.rs": 2}[m.File]].Data, m)
		if err != nil || strings.Count(string(out), "\n") != strings.Count(string(files[map[string]int{"src/lib.rs": 0, "src/b.rs": 2}[m.File]].Data), "\n") {
			t.Fatalf("bad edit %+v %v", m, err)
		}
	}
	if m1[0].ID != "r-01" {
		t.Fatalf("id %s", m1[0].ID)
	}
}

func TestParseCargoTest(t *testing.T) {
	root := filepath.FromSlash("D:/w/repo")
	rm := &rustMap{root: root, bySrc: map[string][]rustTarget{}, docUnit: map[string]string{"grep_matcher": "crates/matcher::doc"},
		pkgName: map[string]string{"crates/matcher": "grep-matcher", ".": "ripgrep"}, pkgDir: map[string]string{"grep-matcher": "crates/matcher", "ripgrep": "."}, pkgDirs: []string{".", "crates/matcher"}}
	add := func(src, unit, kind, name string) {
		k := normPath(filepath.Join(root, filepath.FromSlash(src)))
		rm.bySrc[k] = append(rm.bySrc[k], rustTarget{Unit: unit, Kind: kind, Name: name, Src: k})
	}
	add("crates/matcher/src/lib.rs", "crates/matcher::lib", "lib", "grep-matcher")
	add("crates/core/main.rs", ".::bin:rg", "bin", "rg")
	add("tests/tests.rs", ".::test:integration", "test", "integration")
	out := "   Compiling x\n" +
		"     Running unittests crates/matcher/src/lib.rs (target/debug/deps/grep_matcher-0123456789abcdef.exe)\n" +
		"running 1 test\ntest a ... ok\n\ntest result: ok. 1 passed; 0 failed\n\n" +
		"     Running unittests crates/core/main.rs (target/debug/deps/rg-0123456789abcdef.exe)\n" +
		"running 2 tests\ntest x ... FAILED\ntest y ... ok\n\ntest result: FAILED. 1 passed; 1 failed\n\n" +
		"error: test failed, to rerun pass `-p ripgrep --bin rg`\n" +
		"     Running tests/tests.rs (target/debug/deps/integration-0123456789abcdef.exe)\n" +
		"running 1 test\nerror: process didn't exit successfully\n" +
		"   Doc-tests grep_matcher\nrunning 0 tests\n\ntest result: ok. 0 passed\n" +
		"error: 2 targets failed:\n    `-p ripgrep --bin rg`\n    `-p ripgrep --test integration`\n"
	run := rm.parseCargoTest(out)
	want := map[string]string{"crates/matcher::lib": "passed", ".::bin:rg": "failed", ".::test:integration": "failed", "crates/matcher::doc": "passed"}
	if !reflect.DeepEqual(run.Units, want) || run.CompileErr || len(run.Unmapped) != 0 {
		t.Fatalf("%+v", run)
	}
	if !reflect.DeepEqual(run.Names[".::bin:rg"], []string{"x"}) {
		t.Fatalf("names %v", run.Names)
	}
	ce := rm.parseCargoTest("error[E0308]: mismatched types\nerror: could not compile `x` (lib) due to 1 previous error\n")
	if !ce.CompileErr || !strings.Contains(ce.Detail, "E0308") {
		t.Fatalf("%+v", ce)
	}
	if sel, ok := rm.rustSelector(".::test:integration"); !ok || !reflect.DeepEqual(sel, []string{"-p", "ripgrep", "--test", "integration"}) {
		t.Fatalf("%v", sel)
	}
	if got := newFailures([]string{"a", "b", "c"}, []string{"b"}); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Fatal(got)
	}
}

// Cargo prints member-crate paths relative to the package dir; two members with src/lib.rs are told apart by the executable's crate name.
func TestParseCargoTestMemberRelativePath(t *testing.T) {
	root := filepath.FromSlash("D:/w/repo")
	rm := &rustMap{root: root, bySrc: map[string][]rustTarget{}, docUnit: map[string]string{}, pkgName: map[string]string{}, pkgDir: map[string]string{}, pkgDirs: []string{"crates/a", "crates/b"}}
	for _, n := range []string{"a", "b"} {
		k := normPath(filepath.Join(root, "crates", n, "src", "lib.rs"))
		rm.bySrc[k] = []rustTarget{{Unit: "crates/" + n + "::lib", Kind: "lib", Name: "x-" + n, Src: k}}
	}
	run := rm.parseCargoTest("     Running unittests src/lib.rs (target/debug/deps/x_b-0123456789abcdef.exe)\ntest result: FAILED. 0 passed; 1 failed\n")
	if !reflect.DeepEqual(run.Units, map[string]string{"crates/b::lib": "failed"}) || len(run.Unmapped) != 0 {
		t.Fatalf("%+v", run)
	}
}

package rust

import (
	"reflect"
	"testing"
)

func TestDirectOwners(t *testing.T) {
	files := map[string]string{}
	for k, v := range sample {
		files[k] = v
	}
	files["crates/a/tests/common/mod.rs"] = "" // unowned .rs under tests/: group node, only the integration tests
	files["crates/a/tests/data.txt"] = "x"     // unowned data file: package-level, every target of the package
	files["crates/a/src/note.rs"] = ""         // not declared by any module: package-level
	a := analyze(t, files)
	for file, want := range map[string][]string{
		"crates/a/src/m.rs":            {"crates/a::lib"},
		"crates/a/src/lib.rs":          {"crates/a::lib"},
		"crates/a/src/main.rs":         {"crates/a::bin:a"},
		"crates/a/tests/t.rs":          {"crates/a::test:t"},
		"crates/b/examples/e.rs":       {"crates/b::example:e"},
		"crates/a/tests/common/mod.rs": {"crates/a::test:t"},
		"crates/a/tests/data.txt":      {"crates/a::bin:a", "crates/a::doc", "crates/a::lib", "crates/a::test:t"},
		"crates/a/src/note.rs":         {"crates/a::bin:a", "crates/a::doc", "crates/a::lib", "crates/a::test:t"},
		"nope.rs":                      nil,
	} {
		if got := DirectOwners(a, file); !reflect.DeepEqual(got, want) {
			t.Errorf("DirectOwners(%s) = %v, want %v", file, got, want)
		}
	}
}

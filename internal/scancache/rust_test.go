package scancache

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestRustScanRoundTrip(t *testing.T) {
	src := "mod a;\n#[path = \"p.rs\"] mod b;\nmod c { mod d; }\nmacro_rules! m { ($n:ident) => { mod $n; } }\ninclude_str!(\"x.txt\");\ninclude!(concat!(env!(\"OUT_DIR\"), \"/g.rs\"));\n"
	tr := tree.NewMapTree(map[string]string{"src/lib.rs": src, "src/empty.rs": "fn f() {}", "src/bad.rs": "/* open"})
	c := New()
	for _, p := range []string{"src/lib.rs", "src/empty.rs", "src/bad.rs"} {
		r, err := c.Scan(tr, p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decode(encode(r))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !reflect.DeepEqual(got, r) {
			t.Fatalf("%s: round trip mismatch\n got %+v\nwant %+v", p, got, r)
		}
	}
	r, _ := c.Scan(tr, "src/lib.rs")
	if r.Rust == nil || len(r.Rust.Mods) != 4 || len(r.Rust.Includes) != 2 {
		t.Fatalf("unexpected scan: %+v", r.Rust)
	}
	// A truncated trailer must be rejected, not half-decoded.
	b := encode(r)
	for i := len(b) - 1; i > len(b)-8; i-- {
		if _, err := decode(b[:i]); err == nil {
			t.Fatalf("truncated blob (%d of %d bytes) decoded", i, len(b))
		}
	}
}

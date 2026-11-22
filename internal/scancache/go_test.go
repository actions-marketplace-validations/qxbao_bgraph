package scancache

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

func TestGoScanRoundTrip(t *testing.T) {
	src := "package a_test\n\nimport (\n\t\"os\"\n\t_ \"embed\"\n)\n\n//go:embed a.txt all:static\n//go:embed \"open\nvar x string\n\nfunc TestX() { os.ReadFile(\"testdata/x.json\") }\n"
	tr := tree.NewMapTree(map[string]string{"a/a_test.go": src, "a/empty.go": "package a", "a/bad.go": "package", "a/c.go": "package a\nimport \"C\"\n"})
	c := New()
	for _, p := range []string{"a/a_test.go", "a/empty.go", "a/bad.go", "a/c.go"} {
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
	r, _ := c.Scan(tr, "a/a_test.go")
	if r.Go == nil || len(r.Imports) != 2 || len(r.Go.Embeds) != 3 || len(r.Go.Strings) != 1 {
		t.Fatalf("unexpected scan: %+v %+v", r.Imports, r.Go)
	}
	b := encode(r)
	for i := len(b) - 1; i > len(b)-8; i-- {
		if _, err := decode(b[:i]); err == nil {
			t.Fatalf("truncated blob (%d of %d bytes) decoded", i, len(b))
		}
	}
	// Both trailers in one blob, and a repeated trailer, are handled deterministically.
	both := model.ScanResult{Rust: &model.RustScan{}, Go: &model.GoScan{Package: "p"}}
	if got, err := decode(encode(both)); err != nil || !reflect.DeepEqual(got, both) {
		t.Fatalf("rust+go trailer: %+v %v", got, err)
	}
	if _, err := decode(append(encode(r), encode(model.ScanResult{Go: &model.GoScan{}})[4:]...)); err == nil {
		t.Fatal("a repeated Go trailer must be rejected")
	}
}

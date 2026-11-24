package scancache

import (
	"testing"

	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// .inc is a PHP and a C/C++ include extension: the content picks the scanner.
func TestIncScannerByContent(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{
		"config.inc": "<?php\nrequire 'lib/db.php';\n",
		"table.inc":  "#include \"row.h\"\nX(a)\n",
	})
	c := New()
	for p, want := range map[string]model.ImportRef{
		"config.inc": {Kind: model.ImportRequire, Spec: "lib/db.php"},
		"table.inc":  {Kind: model.ImportStatic, Spec: "row.h"},
	} {
		r, err := c.Scan(tr, p)
		if err != nil {
			t.Fatal(err)
		}
		if r.Uncertain || len(r.Imports) != 1 || r.Imports[0].Kind != want.Kind || r.Imports[0].Spec != want.Spec {
			t.Errorf("%s: scan = %+v, want one %s import of %s", p, r, want.Kind, want.Spec)
		}
	}
}

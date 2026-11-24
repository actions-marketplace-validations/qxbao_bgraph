package selector_test

import (
	"context"
	"testing"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/php"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/selector"
	"github.com/qxbao/bgraph/internal/tree"
)

// TestPreviouslyFailedSubUnit: a failed sub-unit ID ("<unit>#<file>", as the dynamic layer lists narrowed sub-units) selects
// its unit and stays in AlwaysRun, so that the dynamic layer keeps that sub-unit.
func TestPreviouslyFailedSubUnit(t *testing.T) {
	files := map[string]string{
		"composer.json":   "{}\n",
		"phpunit.xml":     "<phpunit><testsuites><testsuite name=\"u\"><directory>tests</directory></testsuite></testsuites></phpunit>\n",
		"src/A.php":       "<?php\n",
		"tests/ATest.php": "<?php\n",
		"README.md":       "x\n",
	}
	head := map[string]string{}
	for k, v := range files {
		head[k] = v
	}
	head["README.md"] = "y\n"
	in := selector.Input{
		Base: "b", Head: "h", BaseTree: tree.NewMapTree(files), HeadTree: tree.NewMapTree(head),
		Changed: []selector.Changed{{Path: "README.md", Status: 'M', OldMode: "100644", NewMode: "100644"}},
		Ecos:    []eco.Ecosystem{php.New(nil)},
		Scanner: scancache.New(),
	}
	in.PreviouslyFailed = []string{".::php#tests/ATest.php", "nope::php#x"}
	res := selector.Run(context.Background(), in)
	found := false
	for _, st := range res.Selection.Tests {
		found = found || st.Unit == ".::php"
	}
	if !found {
		t.Fatalf("tests %+v", res.Selection.Tests)
	}
	if len(res.Selection.AlwaysRun) != 1 || res.Selection.AlwaysRun[0] != ".::php#tests/ATest.php" {
		t.Fatalf("always_run %v", res.Selection.AlwaysRun)
	}
}

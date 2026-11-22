package evalkit

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestParseGoName(t *testing.T) {
	for _, c := range []struct {
		in   string
		want goTestName
	}{
		{"TestX", goTestName{"top", "TestX", ""}},
		{"TestX/sub/case", goTestName{"top", "TestX", ""}},
		{"Test_apiRun_HEAD", goTestName{"top", "Test_apiRun_HEAD", ""}},
		{"BenchmarkFoo", goTestName{"top", "BenchmarkFoo", ""}},
		{"ExampleFoo_bar", goTestName{"top", "ExampleFoo_bar", ""}},
		{"github.com/a/b/c/TestX/s", goTestName{"pkg", "TestX", "github.com/a/b/c"}},
		{"k8s.io/kops/cmd/kops.TestNvidia", goTestName{"pkg", "TestNvidia", "k8s.io/kops/cmd/kops"}},
		{"github.com/casbin/casbin/v2::TestBLPModel", goTestName{"pkg", "TestBLPModel", "github.com/casbin/casbin/v2"}},
		{"MySuite.TestMethod", goTestName{"method", "TestMethod", "MySuite"}},
		{"Config Suite :: Config :: Upstream parsing :: with negative port", goTestName{form: "other"}},
		{"github.com/a/Testing/x", goTestName{form: "package", pkg: "github.com/a/Testing/x"}},
		{"/tmp/TestX123/001/a.md", goTestName{form: "other"}},
		{"Removed zsh from: /tmp/TestU1/001/.zsh/_gh", goTestName{form: "other"}},
		{"github.com/NVIDIA/gpu-operator/controllers", goTestName{form: "package", pkg: "github.com/NVIDIA/gpu-operator/controllers"}},
	} {
		if got := parseGoName(c.in); got != c.want {
			t.Errorf("%q: got %+v want %+v", c.in, got, c.want)
		}
	}
}

func TestLocateGo(t *testing.T) {
	mt := tree.NewMapTree(map[string]string{
		"go.mod":                 "module example.com/m\n\ngo 1.21\n",
		"a/a_test.go":            "package a\n\nfunc TestX(t *testing.T) {}\nfunc TestDup(t *testing.T) {}\n",
		"b/b_test.go":            "package b\n\nfunc TestDup(t *testing.T) {}\nfunc (s *FooSuite) TestMeth() {}\n",
		"b/b2_test.go":           "package b\n\nfunc TestY(t *testing.T) {}\nfunc BenchmarkZ(b *testing.B) {}\n",
		"testdata/x/x_test.go":   "package x\n\nfunc TestIgnored(t *testing.T) {}\n",
		"vendor/v/v_test.go":     "package v\n\nfunc TestVendored(t *testing.T) {}\n",
		"tool/go.mod":            "module example.com/tool\n",
		"tool/t_test.go":         "package t\n\nfunc TestTool(t *testing.T) {}\n",
		"root_test.go":           "package m\n\nfunc TestRoot(t *testing.T) {}\n",
		"gopath/g_test.go":       "package g\n\nfunc TestG(t *testing.T) {}\n",
		"ginkgo/suite_test.go":   "package s\n\nimport \"github.com/onsi/ginkgo/v2\"\n\nfunc TestSuite(t *testing.T) {}\n",
		"a/not_a_test_file.go":   "package a\n\nfunc TestNope() {}\n",
		"a/lower_test.go":        "package a\n\nfunc Testing(t *testing.T) {}\n",
		"b/unused_lower_test.go": "package b\n",
	})
	names := []string{"TestX/sub", "TestDup", "example.com/m/b/TestDup", "example.com/m/TestDup", "TestRoot", "TestTool", "TestIgnored", "TestVendored",
		"BenchmarkZ", "FooSuite.TestMeth", "TestMeth", "TestNope", "some ginkgo text spec", "TestMissing", "example.com/m/b", "example.com/m"}
	patch := map[string]bool{"b/b2_test.go": true, "a/a_test.go": true}
	got := LocateGoTests(mt, names, patch)
	type exp struct {
		units    []string
		amb, nf  bool
		inPatch  bool
		form     string
		pkgHit   bool
		noModule bool
	}
	want := []exp{
		{units: []string{"go:a"}, form: "top", inPatch: true},
		{units: []string{"go:a", "go:b"}, amb: true, form: "top", inPatch: true},
		{units: []string{"go:b"}, form: "pkg", pkgHit: true},
		{units: []string{"go:a", "go:b"}, amb: true, form: "pkg", inPatch: true}, // import path matches no candidate: all kept
		{units: []string{"go:."}, form: "top"},
		{units: []string{"go:tool"}, form: "top"},
		{nf: true, form: "top"},
		{nf: true, form: "top"},
		{units: []string{"go:b"}, form: "top", inPatch: true},
		{units: []string{"go:b"}, form: "method"},
		{units: []string{"go:b"}, form: "method"},
		{nf: true, form: "top"},
		{nf: true, form: "ginkgo"},
		{nf: true, form: "top"},
		{units: []string{"go:b"}, form: "package", pkgHit: true, inPatch: true},
		{units: []string{"go:."}, form: "package", pkgHit: true},
	}
	for i, w := range want {
		g := got[i]
		if !reflect.DeepEqual(g.Units, w.units) || g.Ambiguous != w.amb || g.NotFound != w.nf || g.InPatch != w.inPatch || g.Form != w.form || g.PkgHit != w.pkgHit {
			t.Errorf("%q: got %+v want %+v", names[i], g, w)
		}
	}
	// A file under no go.mod is flagged.
	mt2 := tree.NewMapTree(map[string]string{"g/g_test.go": "package g\n\nfunc TestG(t *testing.T) {}\n"})
	if l := LocateGoTests(mt2, []string{"TestG"}, nil); len(l) != 1 || !l[0].NoModule || l[0].Units[0] != "go:g" {
		t.Errorf("no-module: %+v", l)
	}
}

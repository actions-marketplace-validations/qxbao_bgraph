package evalkit

import (
	"reflect"
	"testing"
)

func TestCheckRequired(t *testing.T) {
	fx := "packages/babel-parser/test/fixtures/es2017/async/a/input.js"
	fx2 := "packages/babel-parser/test/fixtures/es2017/async/a/output.js"
	cases := []struct {
		name    string
		req     []string
		sel     []SelUnit
		missing []string
		nfix    int
		by      string
	}{
		{"satisfied", []string{fx, fx2}, []SelUnit{{"u1", []string{"packages/babel-parser/test/index.js"}}}, nil, 2, "u1"},
		{"satisfied deep", []string{fx}, []SelUnit{{"u1", []string{"packages/babel-parser/test/sub/deep/x.js"}}}, nil, 1, "u1"},
		{"unit in fixtures dir does not count", []string{fx}, []SelUnit{{"u1", []string{"packages/babel-parser/test/fixtures/other/t.js"}}}, []string{fx}, 1, ""},
		{"unit in other package", []string{fx}, []SelUnit{{"u1", []string{"packages/babel-core/test/index.js"}}}, []string{fx}, 1, ""},
		{"no selection", []string{fx}, nil, []string{fx}, 1, ""},
		{"no test-root ancestor: parent of fixtures", []string{"lib/fixtures/a/input.js"},
			[]SelUnit{{"u1", []string{"lib/runner.js"}}}, nil, 1, "u1"},
		{"no test-root ancestor, unsatisfied", []string{"lib/fixtures/a/input.js"},
			[]SelUnit{{"u1", []string{"other/runner.js"}}}, []string{"lib/fixtures/a/input.js"}, 1, ""},
		{"__testfixtures__ with __tests__ root", []string{"src/__tests__/__testfixtures__/a.js"},
			[]SelUnit{{"u1", []string{"src/__tests__/a.test.js"}}}, nil, 1, "u1"},
		{"mixed: fixture ok, normal missing", []string{fx, "src/a.test.js"},
			[]SelUnit{{"u1", []string{"packages/babel-parser/test/index.js"}}}, []string{"src/a.test.js"}, 1, "u1"},
		{"mixed: normal ok, fixture missing", []string{fx, "src/a.test.js"},
			[]SelUnit{{"u1", []string{"src/a.test.js"}}}, []string{fx}, 1, ""},
		{"normal strict: fixtures-like name only in filename", []string{"test/fixtures.js"},
			[]SelUnit{{"u1", []string{"test/other.js"}}}, []string{"test/fixtures.js"}, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			miss, fixes := CheckRequired(c.req, c.sel)
			if !reflect.DeepEqual(miss, c.missing) {
				t.Errorf("missing=%v want %v", miss, c.missing)
			}
			if len(fixes) != c.nfix {
				t.Fatalf("fixtures=%v want %d", fixes, c.nfix)
			}
			if c.nfix > 0 && fixes[0].SatisfiedBy != c.by {
				t.Errorf("satisfied_by=%q want %q", fixes[0].SatisfiedBy, c.by)
			}
		})
	}
}

func TestFixtureRoot(t *testing.T) {
	_, r, ok := fixtureRoot("a/test/fixtures/b/test/c.js")
	if !ok || r != "a/test" {
		t.Fatalf("got %q %v", r, ok)
	}
	if _, _, ok := fixtureRoot("a/test/b.js"); ok {
		t.Fatal("not a fixture")
	}
}

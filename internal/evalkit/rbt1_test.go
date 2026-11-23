package evalkit

import (
	"reflect"
	"testing"
)

func TestRbMissing(t *testing.T) {
	info := &RbT1Info{Reqs: []RbReq{
		{Name: "a", Units: []string{"spec/a_spec.rb"}},
		{Name: "amb", Units: []string{"spec/b_spec.rb", "spec/c_spec.rb"}, PatchUnits: []string{"spec/c_spec.rb"}, Ambiguous: true},
		{Name: "sub", Units: []string{"sub/test/x_test.rb"}},
	}}
	req := []string{"spec/a_spec.rb", "spec/b_spec.rb", "spec/c_spec.rb", "sub/test/x_test.rb"}
	sel := []SelUnit{{Unit: "spec/c_spec.rb"}, {Unit: "<rb-all-tests:sub>", Whole: true, WholeProject: "sub"}}
	miss := rbMissing(req, sel, info, []string{"lib/a.rb"})
	if want := []string{"spec/a_spec.rb", "spec/b_spec.rb"}; !reflect.DeepEqual(miss, want) {
		t.Fatalf("missing %v, want %v", miss, want)
	}
	if want := []string{"spec/a_spec.rb"}; !reflect.DeepEqual(info.PatchOnlyMissing, want) {
		t.Errorf("patch-only missing %v, want %v", info.PatchOnlyMissing, want)
	}
	if len(info.Violations) != 2 || info.Violations[0].PatchOnlyGone || !info.Violations[1].PatchOnlyGone {
		t.Errorf("violations %+v", info.Violations)
	}
}

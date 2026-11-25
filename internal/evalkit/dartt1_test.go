package evalkit

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
)

func TestDartParseWholeUnitID(t *testing.T) {
	for id, want := range map[string]string{"<dart-all-tests:pkgs/a>": "pkgs/a", "<dart-all-tests:.>": "", "<dart-all-tests:>": ""} {
		if d, ok := dartParseWholeUnitID(id); !ok || d != want {
			t.Errorf("%s -> %q %v", id, d, ok)
		}
	}
	if _, ok := dartParseWholeUnitID("pkgs/a/test/x_test.dart"); ok {
		t.Error("file unit parsed as whole")
	}
}

func TestDartRequirementsAndMissing(t *testing.T) {
	a := &eco.Analysis{Tests: []model.TestUnit{
		{ID: "test/a_test.dart", Project: ".", Files: []string{"test/a_test.dart"}},
		{ID: "test/b_test.dart", Project: ".", Files: []string{"test/b_test.dart"}},
		{ID: "<dart-all-tests:pkgs/w>", Project: "pkgs/w", Whole: true},
	}}
	info := &DartT1Info{Kind: "f2p", pkgOf: map[string]string{
		"test/a_test.dart": "", "test/b_test.dart": "", "pkgs/w/test/c_test.dart": "pkgs/w", "pkgs/x/test/d_test.dart": "pkgs/x",
	}}
	locs := []DartTestLoc{
		{Name: "a", Files: []string{"test/a_test.dart"}},
		{Name: "amb", Files: []string{"test/a_test.dart", "test/b_test.dart"}, PatchFiles: []string{"test/a_test.dart"}, Ambiguous: true},
		{Name: "c", Files: []string{"pkgs/w/test/c_test.dart"}},
		{Name: "d", Files: []string{"pkgs/x/test/d_test.dart"}}, // no unit: package without tests in the analysis
		{Name: "(setUpAll)", Hook: true},
		{Name: "gone", NotFound: true},
		{Name: "lib", Files: []string{"lib/x.dart"}, NotUnit: true},
	}
	req, err := dartRequirements(locs, dartUnitsFor(a, dartWithin(info)), "f2p", info)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"<dart-all-tests:pkgs/w>", "test/a_test.dart", "test/b_test.dart"}; !reflect.DeepEqual(req, want) {
		t.Errorf("required %v", req)
	}
	if info.Kept != 3 || info.Ambiguous != 1 || info.NotFound != 1 || info.NoOwner != 1 || len(info.Dropped) != 2 ||
		!reflect.DeepEqual(info.NoOwnerFiles, []string{"pkgs/x/test/d_test.dart"}) {
		t.Errorf("info %+v", info)
	}
	// The root package's whole unit does not run the nested package pkgs/w; the ambiguous name's other candidate is missing, but
	// not under the touched-candidate policy.
	sel := []SelUnit{{Unit: "<dart-all-tests:.>", Whole: true, WholeProject: "."}}
	miss := dartMissing(req, sel, info, []string{"lib/a.dart"})
	if want := []string{"<dart-all-tests:pkgs/w>"}; !reflect.DeepEqual(miss, want) {
		t.Errorf("missing %v", miss)
	}
	sel = []SelUnit{{Unit: "test/a_test.dart"}, {Unit: "<dart-all-tests:pkgs/w>", Whole: true, WholeProject: "pkgs/w"}}
	info.Violations, info.PatchOnlyMissing = nil, nil
	miss = dartMissing(req, sel, info, nil)
	if !reflect.DeepEqual(miss, []string{"test/b_test.dart"}) || len(info.PatchOnlyMissing) != 0 || len(info.Violations) != 1 || !info.Violations[0].PatchOnlyGone {
		t.Errorf("missing %v patch-only %v violations %+v", miss, info.PatchOnlyMissing, info.Violations)
	}
}

func TestDartEcosystemLookup(t *testing.T) {
	// Before the Dart ecosystem is registered the lookup fails cleanly; afterwards it returns it.
	if e, err := dartEcosystem(); err == nil && e.Name() != "dart" {
		t.Errorf("got %s", e.Name())
	}
}

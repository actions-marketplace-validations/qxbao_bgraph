package evalkit

import (
	"slices"
	"testing"

	"github.com/qxbao/bgraph/internal/eco/cpp"
	"github.com/qxbao/bgraph/internal/tree"
)

func TestLocateCPPTestsRetainsAmbiguity(t *testing.T) {
	project := tree.NewMapTree(map[string]string{
		"tests/a.cpp":          "TEST_F(FormatTest, Works) {}\n// TEST(FormatTest, IsComment)\n",
		"tests/b.cpp":          "TEST(FormatTest, Works) {}\nTEST_CASE(\"phrase\") {}\n",
		"tests/meson.build":    "test('phrase', phrase)\n",
		"tests/CMakeLists.txt": "add_test(\n  NAME native::entry\n  COMMAND exe)\n",
	})
	names := []string{"FormatTest.Works", "FormatTest.IsComment", "phrase", "native::entry"}
	locs, err := LocateCPPTests(project, "owner/repo", names, map[string]bool{"tests/a.cpp": true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(locs[0].Files, []string{"tests/a.cpp", "tests/b.cpp"}) || !locs[0].InPatch {
		t.Errorf("ambiguous patch-linked test = %#v", locs[0])
	}
	if len(locs[1].Files) != 0 {
		t.Errorf("comment matched as a test declaration: %#v", locs[1])
	}
	if !slices.Equal(locs[2].Files, []string{"tests/b.cpp"}) || len(locs[2].Registrations) != 1 {
		t.Errorf("test label and registration = %#v", locs[2])
	}
	if expected := (CPPRegistration{System: cpp.CMake, File: "tests/CMakeLists.txt", Name: "native::entry"}); !slices.Equal(locs[3].Registrations, []CPPRegistration{expected}) {
		t.Errorf("multiline CTest registration = %#v", locs[3])
	}
}

func TestCPPWholeSelectionMustUseNativeRootAndSystem(t *testing.T) {
	registration := CPPRegistration{System: cpp.CMake, File: "build/cmake/tests/CMakeLists.txt", Name: "fuzzer"}
	required := cppRegistrationRequirement("facebook/zstd", registration)
	if required != cpp.UnitID(cpp.CMake, "build/cmake", "fuzzer") {
		t.Fatalf("required native test = %s", required)
	}
	info := &CPPT1Info{patchRequired: map[string]bool{required: true}}
	selection := []SelUnit{
		{Unit: cpp.UnitID(cpp.CMake, "build/cmake/tests", ""), Whole: true},
		{Unit: cpp.UnitID(cpp.Meson, "build/cmake", ""), Whole: true},
	}
	if got := cppMissing([]string{required}, selection, info); !slices.Equal(got, []string{required}) {
		t.Fatalf("nested or wrong-system whole unit covered a requirement: %v", got)
	}
	selection = append(selection, SelUnit{Unit: cpp.UnitID(cpp.CMake, "build/cmake", ""), Whole: true})
	if got := cppMissing([]string{required}, selection, &CPPT1Info{}); len(got) != 0 {
		t.Fatalf("missing from whole native scope: %v", got)
	}
}

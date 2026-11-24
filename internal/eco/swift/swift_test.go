package swift

import (
	"context"
	"testing"

	"github.com/qxbao/bgraph/internal/graph"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/tree"
)

func TestOwnership(t *testing.T) {
	files := map[string]string{
		"Package.swift": tools + `import PackageDescription
let package = Package(name: "Demo", targets: [
    .target(name: "First"), .target(name: "Second"),
    .testTarget(name: "One", dependencies: ["First"]),
    .testTarget(name: "Two", dependencies: ["Second"], path: "OtherTests"),
])`,
		"Sources/First/file.swift":  "struct First {}",
		"Sources/Second/file.swift": "struct Second {}",
		"Tests/One/check.swift":     "import XCTest",
		"OtherTests/check.swift":    "import XCTest",
		"fixtures/input.txt":        "data",
	}
	a, err := New().Analyze(context.Background(), tree.NewMapTree(files), scancache.New())
	if err != nil || len(a.Tests) != 2 {
		t.Fatalf("analysis: %+v, %v", a, err)
	}
	g := graph.Build(a)
	for _, test := range []struct {
		file     string
		one, two bool
	}{
		{"Sources/First/file.swift", true, false},
		{"Sources/Second/file.swift", false, true},
		{"OtherTests/check.swift", false, true},
		{"fixtures/input.txt", true, true},
	} {
		walk := g.Walk([]graph.Seed{{Path: test.file}})
		for name, want := range map[string]bool{"One": test.one, "Two": test.two} {
			_, have := walk[UnitNode(".", name)]
			if have != want {
				t.Errorf("%s → %s = %v, want %v", test.file, name, have, want)
			}
		}
	}
}

func TestOpaqueManifest(t *testing.T) {
	tree := tree.NewMapTree(map[string]string{"Package.swift": tools + "import PackageDescription\nlet package = makePackage()"})
	a, err := New().Analyze(context.Background(), tree, scancache.New())
	if err != nil || len(a.Tests) != 1 || !a.Tests[0].Whole || a.Tests[0].ID != "swift:.::all" || len(a.Unresolved) == 0 {
		t.Fatalf("analysis: %+v, %v", a, err)
	}
	ws, claimed := New().ClassifyChange("Package.swift", nil, tree, a)
	if !claimed || len(ws) != 1 || ws[0].Reason != model.ReasonConfigIsCode || !ws[0].Exact || ws[0].Eco != "swift" {
		t.Fatalf("classification: %+v, %v", ws, claimed)
	}
}

func TestFlatSwiftPMSourceLayout(t *testing.T) {
	files := map[string]string{
		"Package.swift":       tools + "import PackageDescription\nlet package = Package(name: \"Demo\", targets: [.target(name: \"Library\"), .testTarget(name: \"Checks\", dependencies: [\"Library\"])])",
		"src/Library.swift":   "struct Library {}",
		"Tests/Example.swift": "import XCTest",
	}
	a, err := New().Analyze(context.Background(), tree.NewMapTree(files), scancache.New())
	if err != nil || len(a.Tests) != 1 || a.Tests[0].Dir != "Tests" {
		t.Fatalf("flat layout analysis: %+v, %v", a, err)
	}
	if a.Roles["Tests/Example.swift"] != model.RoleTest {
		t.Fatalf("flat test file role: %v", a.Roles)
	}
	g := graph.Build(a)
	if _, got := g.Walk([]graph.Seed{{Path: "src/Library.swift"}})[targetNode(".", target{name: "Library"})]; !got {
		t.Fatal("flat library source does not reach its library target")
	}
}

func TestUnitIDRoundTrip(t *testing.T) {
	for _, parts := range [][2]string{{".", ""}, {"Nested/Package", "Tests+X"}, {"weird::test:dir%", "Target::all"}, {"weird::all", ""}} {
		gotDir, gotTarget, ok := ParseUnitID(UnitID(parts[0], parts[1]))
		if !ok || gotDir != parts[0] || gotTarget != parts[1] {
			t.Errorf("round trip of %v = %q, %q, %v", parts, gotDir, gotTarget, ok)
		}
	}
}

func TestFlatTestsInSources(t *testing.T) {
	files := map[string]string{
		"Package.swift":         tools + "import PackageDescription\nlet package = Package(name: \"Demo\", targets: [.testTarget(name: \"Checks\")])",
		"Sources/Example.swift": "import XCTest",
	}
	a, err := New().Analyze(context.Background(), tree.NewMapTree(files), scancache.New())
	if err != nil || len(a.Tests) != 1 || a.Tests[0].Dir != "Sources" || a.Roles["Sources/Example.swift"] != model.RoleTest {
		t.Fatalf("flat tests in Sources: %+v, %v", a, err)
	}
}

func TestExecutableProductAliasUsesOneDependency(t *testing.T) {
	files := map[string]string{
		"Package.swift": tools + `import PackageDescription
let package = Package(name: "Demo", products: [.executable(name: "tool-alias", targets: ["Tool"])], targets: [
    .executableTarget(name: "Tool"), .testTarget(name: "Checks"),
])`,
		"Sources/Tool/main.swift": "print(1)",
		"Tests/Checks/Run.swift":  "import Foundation\nlet tool = Process()",
	}
	a, err := New().Analyze(context.Background(), tree.NewMapTree(files), scancache.New())
	if err != nil {
		t.Fatal(err)
	}
	var refs []model.Edge
	for _, e := range a.Edges {
		if e.From == UnitNode(".", "Checks") && e.To == targetNode(".", target{name: "Tool"}) {
			refs = append(refs, e)
		}
	}
	if len(refs) != 1 {
		t.Fatalf("got subprocess edges %+v, want one", refs)
	}
}

func TestResolvedFixtureIsPackageData(t *testing.T) {
	files := map[string]string{
		"Package.swift":                          tools + "import PackageDescription\nlet package = Package(name: \"Demo\", targets: [.target(name: \"Lib\"), .testTarget(name: \"Checks\")])",
		"Package.resolved":                       "{}",
		"Sources/Lib/a.swift":                    "struct A {}",
		"Tests/Checks/Fixtures/Package.resolved": "{}",
		"Tests/Checks/a.swift":                   "import XCTest",
	}
	a, err := New().Analyze(context.Background(), tree.NewMapTree(files), scancache.New())
	if err != nil {
		t.Fatal(err)
	}
	if a.Roles["Package.resolved"] != model.RoleLock || a.Roles["Tests/Checks/Fixtures/Package.resolved"] != model.RoleData {
		t.Fatalf("roles: %v", a.Roles)
	}
	if _, ok := graph.Build(a).Walk([]graph.Seed{{Path: "Tests/Checks/Fixtures/Package.resolved"}})[UnitNode(".", "Checks")]; !ok {
		t.Fatal("fixture Package.resolved does not reach the test target")
	}
}

func TestDeletedNestedPackageIsNotAnOrphan(t *testing.T) {
	manifest := func(name string) string {
		return tools + "import PackageDescription\nlet package = Package(name: \"" + name + "\", targets: [.target(name: \"" + name + "\")])"
	}
	base := tree.NewMapTree(map[string]string{
		"Kit/Package.swift": manifest("Kit"), "Kit/Sources/Kit/a.swift": "struct A {}",
		"Legacy/Package.swift": manifest("Legacy"), "Legacy/Sources/Legacy/b.swift": "struct B {}",
	})
	head := tree.NewMapTree(map[string]string{
		"Kit/Package.swift": manifest("Kit"), "Kit/Sources/Kit/a.swift": "struct A {}",
		"Stray/c.swift": "struct C {}",
	})
	a, err := New().Analyze(context.Background(), head, scancache.New())
	if err != nil {
		t.Fatal(err)
	}
	if ws, claimed := New().ClassifyChange("Legacy/Sources/Legacy/b.swift", base, head, a); claimed || len(ws) != 0 {
		t.Fatalf("deleted package source: %+v, %v", ws, claimed)
	}
	if ws, claimed := New().ClassifyChange("Stray/c.swift", base, head, a); !claimed || len(ws) != 1 || ws[0].Tier != model.TierFull {
		t.Fatalf("orphan source: %+v, %v", ws, claimed)
	}
}

func TestOutsidePathReportedOnce(t *testing.T) {
	files := map[string]string{
		"Package.swift":   tools + "import PackageDescription\nlet package = Package(name: \"Demo\", targets: [.testTarget(name: \"T\", resources: [.copy(\"../../../outside\")])])",
		"Tests/T/a.swift": "import XCTest",
	}
	a, err := New().Analyze(context.Background(), tree.NewMapTree(files), scancache.New())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, u := range a.Unresolved {
		if u.Spec == "../../../outside" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("outside path reported %d times: %+v", n, a.Unresolved)
	}
}

package swift

import (
	"reflect"
	"testing"
)

const tools = "// swift-tools-version: 5.9\n"

func TestParseManifest(t *testing.T) {
	m, err := parseManifest([]byte(tools + `
import PackageDescription
let package = Package(
    name: #"Demo"#,
    platforms: [.macOS(.v13)],
    products: [.executable(name: "demo", targets: ["Tool"])],
    dependencies: [.package(name: "Local", path: "../Lib"), .package(url: "https://example.com/other.git", from: "1.0.0")],
    targets: [
        .executableTarget(name: "Tool", dependencies: ["Core"]),
        .target(name: "Core", dependencies: [.product(name: "API", package: "Local", condition: .when(platforms: [.macOS]))]),
        .testTarget(name: "DemoTests", dependencies: [.target(name: "Core")], path: "Checks",
            exclude: ["unused.swift"], sources: ["Units"], resources: [.process("Fixtures", localization: .base)])
    ]
)
`))
	if err != nil {
		t.Fatal(err)
	}
	if m.name != "Demo" || !reflect.DeepEqual(m.local, []string{"../Lib"}) || m.programs["demo"] != "Tool" || len(m.targets) != 3 {
		t.Fatalf("manifest: %+v", m)
	}
	tests := m.targets[2]
	if tests.kind != "testTarget" || tests.name != "DemoTests" || tests.path != "Checks" || !reflect.DeepEqual(tests.sources, []string{"Units"}) ||
		!reflect.DeepEqual(tests.excludes, []string{"unused.swift"}) || !reflect.DeepEqual(tests.resources, []string{"Fixtures"}) ||
		!reflect.DeepEqual(tests.deps, []string{"Core"}) {
		t.Fatalf("test target: %+v", tests)
	}
}

func TestManifestFailsClosed(t *testing.T) {
	for _, input := range []string{
		`let package = Package(name: "Demo", targets: targets)`,
		`let package = Package(name: "Demo", targets: [] + [])`,
		`let package = Package(name: "Demo", targets: []); package.targets.append(.testTarget(name: "More"))`,
		`let package = Package(name: "Demo", targets: [.testTarget(name: "Demo\(suffix)")])`,
		`let package = Package(name: "Demo", targets: [.testTarget(name: /Demo/)])`,
		`let package = Package(name: "Demo", targets: [.testTarget(name: "Demo", path: getPath())])`,
		`let package = Package(name: "Demo", targets: [.testTarget(name: "Demo")]`,
		`let package = Package(name: "Demo", targets: [.testTarget(name: "Demo", unknown: "x")])`,
		`let package = Package(name: "Demo", targets: [.testTarget(name: "Demo"), .testTarget(name: "Demo")])`,
	} {
		if _, err := parseManifest([]byte(tools + "import PackageDescription\n" + input)); err == nil {
			t.Errorf("accepted unsupported manifest: %s", input)
		}
	}
}

func TestManifestCanonical(t *testing.T) {
	first := tools + "import PackageDescription\nlet package = Package(name: \"Demo\", targets: [.testTarget(name: \"DemoTests\")])"
	second := "// swift-tools-version:5.9\nimport PackageDescription /* nested /* hint */ */\nlet package=Package( name : \"Demo\",targets : [.testTarget(name:\"DemoTests\")] )"
	a, err := parseManifest([]byte(first))
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseManifest([]byte(second))
	if err != nil || a.canonical != b.canonical {
		t.Fatalf("comment and spacing canonicalization: %+v, %v", b, err)
	}
	c, err := parseManifest([]byte("// swift-tools-version: 6.0" + first[len(tools)-1:]))
	if err != nil || c.canonical == a.canonical {
		t.Fatalf("tools version did not change the canonical form: %+v, %v", c, err)
	}
}

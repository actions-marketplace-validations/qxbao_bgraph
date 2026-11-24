package evalkit

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestLocateSwiftTests(t *testing.T) {
	project := tree.NewMapTree(map[string]string{
		"Tests/A.swift": "final class A: XCTestCase {\nfunc testFormat() {}\n}",
		"Tests/B.swift": "extension B {\nfunc testFormat() {}\n}",
		"Tests/Modern.swift": "/* outer /* nested */ func testIgnored() {} */\n" +
			"let literal = #\"func testIgnored() {}\"#\n" +
			"@Testing.Test(\"rejects empty input\", arguments: [1, 2])\n@MainActor\nfunc rejectsInput(i: Int) {}\n" +
			"@Test(arguments: [1, 2])\nfunc " + string(rune(96)) + "works with spaces" + string(rune(96)) + "(i: Int) {}",
	})
	names := []string{
		"Module.A testFormat()", "testFormat", "rejectsInput(i:)", "rejects empty input",
		"\"works with spaces\" with 2 test cases", "testIgnored", swiftCommandResult,
	}
	got, err := LocateSwiftTests(project, names, map[string]bool{"Tests/A.swift": true})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"Tests/A.swift"}, {"Tests/A.swift", "Tests/B.swift"}, {"Tests/Modern.swift"}, {"Tests/Modern.swift"},
		{"Tests/Modern.swift"}, nil, nil,
	}
	for i, l := range got {
		if !reflect.DeepEqual(l.Files, want[i]) || l.InPatch != (i < 2) {
			t.Errorf("location for %q = %#v, want files %v and in-patch %v", names[i], l, want[i], i < 2)
		}
	}
	if got[6].Form != "command" {
		t.Errorf("command result = %#v", got[6])
	}
}

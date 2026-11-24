package swift

import (
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func TestRuntimeFileOperands(t *testing.T) {
	result := Scan([]byte(`
let child = Process()
FileManager.default.enumerator(atPath: ".")
let unknown = try Data(contentsOf: inputURL)
let computed = try String(contentsOf: URL(fileURLWithPath: "root/" + variable))
var values: [String] = []
values.append(contentsOf: others)
`))
	var dot, computed, process bool
	for _, ref := range result.Imports {
		dot = dot || (ref.Spec == "." && ref.Suffix == "file" && !ref.Dynamic)
		computed = computed || (ref.Spec == "" && ref.Suffix == "" && ref.Dynamic)
		process = process || (ref.Spec == "" && ref.Suffix == "process" && ref.Dynamic)
	}
	if result.Uncertain || !dot || !computed || !process {
		t.Fatalf("runtime references: %+v", result)
	}
	collections := Scan([]byte("values.append(contentsOf: others)"))
	for _, ref := range collections.Imports {
		if ref.Kind == model.ImportRead {
			t.Fatalf("collection append is a file read: %+v", ref)
		}
	}
}

func TestScopedSwiftImports(t *testing.T) {
	result := Scan([]byte("#if os(Linux) && canImport(Feature_Kit)\n@_implementationOnly import struct Foo_Bar.Value\n#endif\nimport XCTest"))
	if len(result.Imports) != 3 || result.Imports[0].Kind != model.ImportStatic || result.Imports[0].Spec != "Feature_Kit" ||
		result.Imports[1].Kind != model.ImportStatic || result.Imports[1].Spec != "Foo_Bar" ||
		result.Imports[2].Kind != model.ImportStatic || result.Imports[2].Spec != "XCTest" {
		t.Fatalf("source imports: %+v", result)
	}
}

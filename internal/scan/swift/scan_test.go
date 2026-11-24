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

func TestComputedFoundationFileOperands(t *testing.T) {
	for _, code := range []string{
		`let data: Data = try .init(contentsOf: file)`,
		`let data = try Data(contentsOf: URL(fileURLWithPath: "data").appending(path: name))`,
		`let data = try Data(contentsOf: Bundle.module.resourceURL!.deletingLastPathComponent().appending(path: name))`,
		`let data = try Data(contentsOf: Bundle.module.url(forResource: "base", withExtension: nil)!.appending(path: name))`,
		`let files = try FileManager.default.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil)`,
	} {
		found := false
		for _, ref := range Scan([]byte(code)).Imports {
			found = found || (ref.Kind == model.ImportRead && ref.Dynamic && ref.Spec == "")
		}
		if !found {
			t.Errorf("computed file operand was not reported in %s", code)
		}
	}
}

func TestDirectFoundationFileOperandsStayPrecise(t *testing.T) {
	for _, code := range []string{
		`let data = try Data(contentsOf: URL(fileURLWithPath: "./data.bin"))`,
		`let data = try Data(contentsOf: Bundle.module.url(forResource: "base", withExtension: "txt")!)`,
		`values.append(contentsOf: others)`,
	} {
		for _, ref := range Scan([]byte(code)).Imports {
			if ref.Kind == model.ImportRead && ref.Dynamic {
				t.Errorf("direct operand widened in %s: %+v", code, ref)
			}
		}
	}
}

func TestProcessCallsNeedDirectCall(t *testing.T) {
	for code, want := range map[string]bool{
		`Text("a.b").font(.system(size: 12))`: false,
		`let system = "x/y"`:                  false,
		`let rc = system("tool/run.sh")`:      true,
		`let f = popen("ls", "r")`:            true,
		`let p = Process()`:                   true,
	} {
		got := false
		for _, ref := range Scan([]byte(code)).Imports {
			got = got || ref.Suffix == "process"
		}
		if got != want {
			t.Errorf("process launch in %s = %v, want %v", code, got, want)
		}
	}
}

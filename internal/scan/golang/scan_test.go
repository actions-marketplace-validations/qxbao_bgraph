package golang

import (
	"reflect"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func specs(r model.ScanResult) []string {
	var out []string
	for _, im := range r.Imports {
		out = append(out, im.Spec)
	}
	return out
}

func TestImports(t *testing.T) {
	src := `// Package a.
//go:build linux && !cgo

package a

import "fmt"
import (
	x "example.com/m/b"
	. "example.com/m/c"
	_ "example.com/m/d" // side effect
	/* block */ "C"
)

func f() { fmt.Println("import \"nope\"") }
`
	r := Scan("a/a.go", []byte(src))
	if r.Uncertain {
		t.Fatal(r.UncertainReason)
	}
	if got, want := specs(r), []string{"fmt", "example.com/m/b", "example.com/m/c", "example.com/m/d", "C"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("imports %v, want %v", got, want)
	}
	if r.Go == nil || r.Go.Package != "a" {
		t.Fatalf("package: %+v", r.Go)
	}
	if r.Imports[1].Line != 8 {
		t.Fatalf("line of the second import: %d", r.Imports[1].Line)
	}
	if len(r.Go.Strings) != 0 {
		t.Fatalf("strings collected in a non-test file: %v", r.Go.Strings)
	}
}

func TestHeaderParseErrorIsUncertain(t *testing.T) {
	for _, src := range []string{"", "package", "package a\nimport (\n\"x\"\n", "package a\nimport 12\n", "func f() {}", "package a\nimport \"a\nb\"\n"} {
		if r := Scan("a.go", []byte(src)); !r.Uncertain || r.Go != nil {
			t.Errorf("%q: want uncertain, got %+v", src, r)
		}
	}
	// A body error does not matter: the imports are known.
	r := Scan("a.go", []byte("package a\nimport \"b\"\nfunc f( {"))
	if r.Uncertain || len(r.Imports) != 1 {
		t.Fatalf("body error: %+v", r)
	}
}

func TestEmbeds(t *testing.T) {
	src := "package a\n\nimport _ \"embed\"\n\n//go:embed a.txt dir/*.tmpl\nvar x string\n\n//go:embed \"with space.txt\" `raw`\n//go:embed all:static\nvar y embed.FS\n/*\n//go:embed not/a/directive\n*/\nvar s = \"//go:embed nope\"\n//go:embed\n//go:embed \"open\n//go:embedded nope\n"
	r := Scan("a.go", []byte(src))
	if r.Uncertain {
		t.Fatal(r.UncertainReason)
	}
	want := []model.GoEmbed{
		{Pattern: "a.txt", Line: 5}, {Pattern: "dir/*.tmpl", Line: 5},
		{Pattern: "with space.txt", Line: 8}, {Pattern: "raw", Line: 8},
		{Pattern: "all:static", Line: 9},
		{Line: 15, Unknown: "no pattern"},
		{Line: 16, Unknown: "unterminated quoted pattern"},
	}
	if !reflect.DeepEqual(r.Go.Embeds, want) {
		t.Fatalf("embeds\n got %+v\nwant %+v", r.Go.Embeds, want)
	}
}

func TestTestStrings(t *testing.T) {
	src := "package a_test\n\nimport (\n\t\"os\"\n\t\"path/filepath\"\n)\n\nfunc TestX(t *testing.T) {\n" +
		"\tos.ReadFile(\"testdata/in.json\")\n" +
		"\tos.ReadFile(filepath.Join(\"..\", \"shared\", \"golden\", name))\n" +
		"\tos.ReadFile(`..\\win\\x.txt`)\n" +
		"\t_ = \"no path here\"\n" +
		"\t_ = \"multi\\nline/x\"\n" +
		"\t_ = fp.Join(\"a\", b, \"c\")\n" +
		"\t_ = \"testdata/in.json\"\n" +
		"}\n"
	r := Scan("a/a_test.go", []byte(src))
	if r.Uncertain {
		t.Fatal(r.UncertainReason)
	}
	want := []string{"path/filepath", "testdata/in.json", "../shared/golden", "..", `..\win\x.txt`}
	if !reflect.DeepEqual(r.Go.Strings, want) {
		t.Fatalf("strings\n got %q\nwant %q", r.Go.Strings, want)
	}
	if r.Go.Package != "a_test" {
		t.Fatalf("package %q", r.Go.Package)
	}
}

func TestTooLarge(t *testing.T) {
	src := "package a\n" + strings.Repeat("// x\n", MaxFileSize/5+1)
	if r := Scan("a.go", []byte(src)); !r.Uncertain {
		t.Fatal("a file over the size limit must be uncertain")
	}
}

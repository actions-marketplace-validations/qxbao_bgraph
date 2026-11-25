package cpp

import (
	"reflect"
	"slices"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func TestBuildComments(t *testing.T) {
	for _, tc := range []struct {
		parse                  func([]byte) ([]Call, error)
		declaration, commented string
	}{
		{CMake, "add_executable(t t.c)", "add_executable(#[=[ignored ) ]=]t # another )\n t.c)"},
		{Meson, "t = executable('t', ['a.c', 'b.c'])", "t = executable('t', ['a.c', # ignored\n'b.c'])"},
	} {
		a, err := tc.parse([]byte(tc.declaration))
		if err != nil {
			t.Fatal(err)
		}
		b, err := tc.parse([]byte(tc.commented))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("build calls changed after comment insertion: %+v, %+v", a, b)
		}
	}
}

func TestSourceReferences(t *testing.T) {
	got := Source([]byte(`const auto x = u8R"cpp(
#include "fake-in-raw.h"
)cpp";
// #include "fake-in-comment.h"
#include /* comment */ "real-header.h"
%:include_next <real-other.h>
#embed "fixture.bin"
`))
	if got.Uncertain {
		t.Fatal(got.UncertainReason)
	}
	want := []string{"real-header.h", "real-other.h", "fixture.bin"}
	var specs []string
	for _, ref := range got.Imports {
		specs = append(specs, ref.Spec)
	}
	if !slices.Equal(specs, want) {
		t.Fatalf("source references = %q, want %q", specs, want)
	}
}

func TestSourceUncertainty(t *testing.T) {
	for _, src := range []string{
		"#include DYNAMIC_HEADER",
		"#if __has_include(\"ever.h\")\n#endif",
		"int f() { return system(\"test-helper\"); }",
		"std::filesystem::exists(variable);",
		"import named_module;",
		"#pragma comment(lib, \"custom.lib\")",
	} {
		if got := Source([]byte(src)); !got.Uncertain {
			t.Errorf("source %q should be uncertain", src)
		}
	}
}

func TestSourceOrdinaryDirectivesAndNumbers(t *testing.T) {
	got := Source([]byte(`#if __cplusplus < 201103L
#error "needs C++11 // or later"
#endif
#define LT(a, b) ((a) < (b))
#define QUOTE '"'
#if LIMIT > 1'000'000
#endif
#include <vector>
#include "local.h"
constexpr int N = 1'000;
constexpr unsigned M = 0xFF'FF;
char c = 'x';
`))
	if got.Uncertain {
		t.Fatal(got.UncertainReason)
	}
	want := []model.ImportRef{
		{Kind: model.ImportSystem, Spec: "vector", Suffix: "include", Line: 8},
		{Kind: model.ImportStatic, Spec: "local.h", Suffix: "include", Line: 9},
	}
	if !reflect.DeepEqual(got.Imports, want) {
		t.Fatalf("imports = %+v, want %+v", got.Imports, want)
	}
	for _, src := range []string{"#include <unterminated.h\n", "#define S \"unterminated\n"} {
		if got := Source([]byte(src)); !got.Uncertain {
			t.Errorf("source %q should be uncertain", src)
		}
	}
}

func FuzzScanners(f *testing.F) {
	f.Add([]byte("add_test(NAME x COMMAND binary)\n"))
	f.Add([]byte("t = executable('t', ['foo.c', 'bar.c'])\ntest('foo', t)"))
	f.Add([]byte("#include \"file.h\"\n/* comment */\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = CMake(b)
		_, _ = Meson(b)
		for _, ref := range Source(b).Imports {
			if ref.Spec == "" || ref.Line < 1 || ref.Kind != model.ImportStatic && ref.Kind != model.ImportSystem {
				t.Errorf("invalid C/C++ input reference: %+v", ref)
			}
		}
	})
}

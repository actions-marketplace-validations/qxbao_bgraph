package cpp

import (
	"context"
	"testing"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

func analyze(t *testing.T, files map[string]string) *eco.Analysis {
	t.Helper()
	a, err := (&Eco{}).Analyze(context.Background(), tree.NewMapTree(files), nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func hasEdge(a *eco.Analysis, from, to string) bool {
	for _, e := range a.Edges {
		if e.From == from && e.To == to {
			return true
		}
	}
	return false
}

func unresolvedOn(a *eco.Analysis, file string) []model.Unresolved {
	var out []model.Unresolved
	for _, u := range a.Unresolved {
		if u.File == file {
			out = append(out, u)
		}
	}
	return out
}

const rootCMake = `cmake_minimum_required(VERSION 3.20)
project(p C)
enable_testing()
add_library(core STATIC core.c)
add_subdirectory(src)
add_executable(t t.c)
target_link_libraries(t core)
add_test(NAME t COMMAND t)
`

// A target_sources entry from another directory names an existing file under
// one of CMake's two readings; the project stays analyzable.
func TestTargetSourcesFromAnotherDirectory(t *testing.T) {
	for _, tc := range []struct{ name, file string }{
		{"caller directory", "src/extra.c"},
		{"declaring directory", "extra.c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := analyze(t, map[string]string{
				"CMakeLists.txt":     rootCMake,
				"src/CMakeLists.txt": "target_sources(core PRIVATE extra.c)\n",
				"core.c":             "int core(void) { return 0; }\n",
				"t.c":                "int main(void) { return 0; }\n",
				tc.file:              "int extra(void) { return 1; }\n",
			})
			for _, u := range a.Unresolved {
				if u.Class == model.UnresolvedDynamic {
					t.Fatalf("project is opaque: %+v", u)
				}
			}
			if len(a.Tests) != 1 || a.Tests[0].Whole || a.Tests[0].Package != "t" {
				t.Fatalf("tests = %+v, want the named test t", a.Tests)
			}
			p := &project{system: CMake, dir: "."}
			if !hasEdge(a, p.node("target", "core"), tc.file) {
				t.Fatalf("no edge from core to %s: %+v", tc.file, a.Edges)
			}
		})
	}
	a := analyze(t, map[string]string{
		"CMakeLists.txt":     rootCMake,
		"src/CMakeLists.txt": "target_sources(core PRIVATE missing.c)\n",
		"core.c":             "int core(void) { return 0; }\n",
		"t.c":                "int main(void) { return 0; }\n",
	})
	if len(a.Tests) != 1 || !a.Tests[0].Whole {
		t.Fatalf("a missing source must make the project opaque: %+v", a.Tests)
	}
}

func TestSystemIncludeDoesNotMatchUnrelatedFile(t *testing.T) {
	a := analyze(t, map[string]string{
		"CMakeLists.txt": "project(p CXX)\nenable_testing()\nadd_executable(t main.cpp)\nadd_test(NAME t COMMAND t)\n",
		"main.cpp":       "#include <version>\n#include <vector>\n#include \"list.def\"\n#include \"Table.DEF\"\nint main() {}\n",
		"VERSION":        "1.0.0\n",
		"vector":         "not a header\n",
		"list.def":       "X(a)\n",
		"table.def":      "unrelated spelling\n",
	})
	for _, f := range []string{"VERSION", "vector", "table.def"} {
		if hasEdge(a, "main.cpp", f) || len(unresolvedOn(a, f)) != 0 {
			t.Errorf("%s must not be linked to main.cpp", f)
		}
	}
	if !hasEdge(a, "main.cpp", "list.def") || len(unresolvedOn(a, "list.def")) == 0 {
		t.Errorf("a quoted nonstandard include must still be linked and uncertain")
	}
}

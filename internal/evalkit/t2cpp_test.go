package evalkit

import (
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func TestCppCTestReportsNativeTimeout(t *testing.T) {
	tests, err := parseCppCTest([]byte(`<testsuite>
<testcase name="slow" status="fail"><failure message="Timeout"/></testcase>
<testcase name="assertion" status="fail"><failure message="Failed"/></testcase>
<testcase name="ok" status="run"/>
</testsuite>`))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"slow": "timeout", "assertion": "failed", "ok": "passed"} {
		if got := tests["cpp:cmake:.::test:"+name]; got != want {
			t.Errorf("test %s = %q, want %q", name, got, want)
		}
	}
}

func TestCppMesonReports(t *testing.T) {
	data := []byte("{\"name\":\"one\",\"result\":\"OK\"}\n" +
		"{\"name\":\"two\",\"result\":\"FAIL\"}\n" +
		"{\"name\":\"three\",\"result\":\"TIMEOUT\"}\n" +
		"{\"name\":\"four\",\"result\":\"EXPECTEDFAIL\"}\n")
	tests, err := parseCppMeson(data)
	if err != nil {
		t.Fatal(err)
	}
	for name, expected := range map[string]string{"one": "passed", "two": "failed", "three": "timeout", "four": "passed"} {
		if actual := tests["cpp:meson:.::test:"+name]; actual != expected {
			t.Errorf("%s: got %q, want %q", name, actual, expected)
		}
	}
	if _, err := parseCppMeson(append(data, data...)); err == nil {
		t.Fatal("accepted duplicate Meson reports")
	}
}

func TestCppWholeCoverageUsesBuildRootAndBackend(t *testing.T) {
	sel := model.Selection{Tests: []model.SelectedTest{
		{Unit: "cpp:cmake:lib::all", Whole: true},
		{Unit: "cpp:meson:lib::test:checks"},
	}}
	for id, want := range map[string]bool{
		"cpp:cmake:lib::test:checks": true,
		"cpp:cmake:.::test:checks":   false,
		"cpp:meson:lib::test:checks": true,
		"cpp:meson:lib::test:other":  false,
	} {
		if got := cppCoversRed(sel, id); got != want {
			t.Errorf("coverage for %s = %v, want %v", id, got, want)
		}
	}
}

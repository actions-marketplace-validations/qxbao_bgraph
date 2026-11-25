package evalkit

import (
	"slices"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func TestSwiftT2ReportMarksMissingEvidence(t *testing.T) {
	q := T2Result{Repos: []T2RepoResult{{Repo: T2Repo{Eco: "swift"}, Error: "baseline failed"}}}
	if got := T2Markdown(q); !strings.HasPrefix(got, "# bgraph-eval t2 — INCOMPLETE") {
		t.Fatalf("missing baseline should make the report incomplete: %s", got)
	}
}

func TestParseSwiftXunit(t *testing.T) {
	report := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
<testsuite name="TestResults" errors="0" tests="3" failures="1">
<testcase classname="ComplexTests.ArithmeticTests" name="testArithmetic"/>
<testcase classname="ComplexTests.PropertyTests" name="testCodable"><failure message="failed"/></testcase>
<testcase classname="RealTests.ApproximateTests" name="testDouble"/>
</testsuite>
</testsuites>`
	got, err := parseSwiftXunit([]byte(report))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !slices.Equal(swiftRed(got, nil), []string{"swift:.::test:ComplexTests"}) {
		t.Fatalf("parsed results = %+v", got)
	}
	if red := swiftRed(got, map[string]bool{"ComplexTests.PropertyTests/testCodable": true}); len(red) != 0 {
		t.Fatalf("excluded test contributed red unit %v", red)
	}
}

func TestRejectIncompleteSwiftXunit(t *testing.T) {
	for _, report := range []string{
		`<testsuites/>`,
		`<testsuites><testsuite tests="2"><testcase classname="A.C" name="a"/></testsuite></testsuites>`,
		`<testsuites><testsuite tests="1"><testcase classname="C" name="a"/></testsuite></testsuites>`,
		`<testsuites><testsuite tests="1"><testcase classname="A.C" name="a"><skipped/></testcase></testsuite></testsuites>`,
		`<testsuites><testsuite tests="2"><testcase classname="A.C" name="a"/><testcase classname="A.C" name="a"/></testsuite></testsuites>`,
		`<testsuites><testsuite tests="1"><testcase classname="A.C" name="a"/>`,
	} {
		if _, err := parseSwiftXunit([]byte(report)); err == nil {
			t.Errorf("accepted incomplete report %s", report)
		}
	}
}

func TestSwiftRedCoverage(t *testing.T) {
	repo := T2Repo{Eco: "swift"}
	sel := model.Selection{Tests: []model.SelectedTest{
		{Unit: "go:sub/module", Whole: true, Project: "sub"},
		{Unit: "swift:.::all", Whole: true, Project: "."},
		{Unit: "swift:sub::test:B", Project: "sub"},
	}}
	for id, want := range map[string]bool{
		"swift:.::test:A":                      true,
		"swift:sub::test:B":                    true,
		"swift:sub::test:A":                    false,
		"swift:sub/nested%3A%3Amodule::test:A": false,
	} {
		if got := repo.coversRed(sel, id); got != want {
			t.Errorf("coversRed(%q) = %t, want %t", id, got, want)
		}
	}
}

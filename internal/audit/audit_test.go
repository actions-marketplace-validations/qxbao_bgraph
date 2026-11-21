package audit

import (
	"testing"
)

func parseFile(t *testing.T, name, format string) Result {
	t.Helper()
	r, err := Parse("testdata/"+name, format)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestVitestJUnit(t *testing.T) {
	r := parseFile(t, "vitest-junit.xml", "auto")
	if r.Format != FormatJUnit || r.Total != 3 || len(r.Failures) != 1 {
		t.Fatalf("got %+v", r)
	}
	f := r.Failures[0]
	if f.Test != "src/a.test.ts > subtracts" || f.Message != "expected 1 to be 2" || f.Candidates[0] != "src/a.test.ts" {
		t.Errorf("failure: %+v", f)
	}
}

func TestJestJUnitWithoutFile(t *testing.T) {
	r := parseFile(t, "jest-junit-nofile.xml", "")
	if len(r.Failures) != 1 {
		t.Fatalf("got %+v", r)
	}
	f := r.Failures[0]
	if f.Message != "Error: expect(received).toBe(expected)" {
		t.Errorf("message: %q", f.Message)
	}
	// No file attribute: only the classname / suite name remain, which are describe names.
	if len(f.Candidates) != 2 || f.Candidates[0] != "math subtracts" || f.Candidates[1] != "math" {
		t.Errorf("candidates: %v", f.Candidates)
	}
}

func TestJestJUnitWithFileAndError(t *testing.T) {
	r := parseFile(t, "jest-junit-file.xml", "junit")
	if len(r.Failures) != 1 || r.Failures[0].Candidates[0] != "src/a.test.ts" || r.Failures[0].Message != "boom" {
		t.Fatalf("got %+v", r)
	}
}

func TestJestJSON(t *testing.T) {
	r := parseFile(t, "jest.json", "auto")
	if r.Format != FormatJestJSON || len(r.Failures) != 2 {
		t.Fatalf("got %+v", r)
	}
	if r.Failures[0].Test != "math subtracts" || r.Failures[0].Message != "Error: expected 1 to be 2" {
		t.Errorf("f0: %+v", r.Failures[0])
	}
	if r.Failures[1].Test != "(test suite failed to run)" || r.Failures[1].Candidates[0] != `D:\Projects\app\src\broken.test.ts` {
		t.Errorf("f1: %+v", r.Failures[1])
	}
}

func TestPlaywrightJSON(t *testing.T) {
	r := parseFile(t, "playwright.json", "auto")
	if r.Format != FormatPlaywright || r.Total != 4 || len(r.Failures) != 2 {
		t.Fatalf("got %+v", r)
	}
	f := r.Failures[0]
	if f.Test != "login > rejects bad password" || f.Candidates[0] != "/work/app/e2e/login.spec.ts" || f.Candidates[1] != "login.spec.ts" {
		t.Errorf("f0: %+v", f)
	}
	if r.Failures[1].Message != "Timeout 30000ms exceeded" {
		t.Errorf("f1: %+v", r.Failures[1])
	}
}

func TestDetectErrors(t *testing.T) {
	for _, in := range []string{"", "plain text", `{"a":1}`, `{bad`} {
		if _, err := ParseBytes([]byte(in), "auto"); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
	if _, err := ParseBytes([]byte("<a/>"), "tap"); err == nil {
		t.Error("unknown format must fail")
	}
	if _, err := ParseBytes([]byte("<testsuites><testsuite>"), "junit"); err == nil {
		t.Error("truncated xml must fail")
	}
}

func TestMapper(t *testing.T) {
	m := &Mapper{
		Root:       `D:\Projects\app`,
		ResultsDir: `D:\Projects\app\reports`,
		Known:      []string{"src/a.test.ts", "src/b.test.ts", "pkg/web/src/a.test.ts", "pkg/web/c.test.ts", "e2e/login.spec.ts"},
		Projects:   []string{".", "pkg/web"},
	}
	cases := []struct {
		name  string
		cands []string
		want  string
		out   Outcome
	}{
		{"repo-relative", []string{"src/a.test.ts"}, "src/a.test.ts", Mapped},
		{"dot slash and backslash", []string{`.\src\b.test.ts`}, "src/b.test.ts", Mapped},
		{"absolute windows, other case", []string{`d:\projects\APP\src\b.test.ts`}, "src/b.test.ts", Mapped},
		{"absolute with forward slashes", []string{`D:/Projects/app/e2e/login.spec.ts`}, "e2e/login.spec.ts", Mapped},
		{"relative to project", []string{"c.test.ts"}, "pkg/web/c.test.ts", Mapped},
		{"unique suffix", []string{"login.spec.ts"}, "e2e/login.spec.ts", Mapped},
		{"ambiguous suffix", []string{"a.test.ts"}, "", Ambiguous},
		{"describe name", []string{"math subtracts", "math"}, "", NotFound},
		{"outside repo", []string{`E:\other\src\a.test.ts`}, "", NotFound},
		{"first usable candidate wins", []string{"nothing.ts", "src/b.test.ts"}, "src/b.test.ts", Mapped},
	}
	for _, c := range cases {
		got, out := m.Map(c.cands)
		if got != c.want || out != c.out {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, got, out, c.want, c.out)
		}
	}
	// A results file next to its tests resolves bare names.
	m2 := &Mapper{Root: "/r", ResultsDir: "/r/src", Known: []string{"src/a.test.ts", "lib/a.test.ts"}}
	if got, out := m2.Map([]string{"a.test.ts"}); got != "src/a.test.ts" || out != Mapped {
		t.Errorf("results-dir relative: got (%q, %v)", got, out)
	}
}

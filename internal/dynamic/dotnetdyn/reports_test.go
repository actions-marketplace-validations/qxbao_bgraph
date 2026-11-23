package dotnetdyn

import (
	"slices"
	"strings"
	"testing"
)

const cobSample = `<?xml version="1.0" encoding="utf-8"?>
<coverage line-rate="0.5" version="1.9">
  <packages>
    <package name="Lib">
      <classes>
        <class name="Lib.Foo" filename="C:\repo\src\Lib\Foo.cs">
          <methods><method name="F"><lines><line number="5" hits="1"/><line number="6" hits="0"/></lines></method></methods>
          <lines><line number="5" hits="1"/><line number="6" hits="0"/></lines>
        </class>
        <class name="Lib.Bar" filename="C:\repo\src\Lib\Bar.cs">
          <lines><line number="3" hits="0"/><line number="4" hits="0"/><line number="9" hits="0"/></lines>
        </class>
        <class name="Lib.Tests.FooTests.&lt;&gt;c" filename="C:\repo\test\FooTests.cs">
          <lines><line number="12" hits="2"/></lines>
        </class>
      </classes>
    </package>
  </packages>
</coverage>`

func TestParseCobertura(t *testing.T) {
	c, err := ParseCobertura(strings.NewReader(cobSample))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Hit[`C:\repo\src\Lib\Foo.cs`] || c.Hit[`C:\repo\src\Lib\Bar.cs`] || !c.Hit[`C:\repo\test\FooTests.cs`] {
		t.Errorf("hit = %v", c.Hit)
	}
	if got := c.Lines[`C:\repo\src\Lib\Foo.cs`]; !slices.Equal(got, []int{5, 6}) {
		t.Errorf("lines Foo = %v", got)
	}
	if got := c.Lines[`C:\repo\src\Lib\Bar.cs`]; !slices.Equal(got, []int{3, 4, 9}) {
		t.Errorf("lines Bar = %v", got)
	}
	if !classHit(&c, "Lib.Tests.FooTests") || classHit(&c, "Lib.Bar") || classHit(&c, "Lib.Fo") {
		t.Errorf("classHit wrong: %v", c.Classes)
	}
}

const trxSample = `<?xml version="1.0" encoding="UTF-8"?>
<TestRun xmlns="http://microsoft.com/schemas/VisualStudio/TeamTest/2010">
  <Results>
    <UnitTestResult testId="1" outcome="Passed"/>
    <UnitTestResult testId="2" outcome="Failed"/>
    <UnitTestResult testId="3" outcome="NotExecuted"/>
    <UnitTestResult testId="4" outcome="Passed">
      <InnerResults><UnitTestResult testId="4" outcome="Failed"/></InnerResults>
    </UnitTestResult>
  </Results>
  <TestDefinitions>
    <UnitTest id="1"><TestMethod className="A.FooTests" name="X"/></UnitTest>
    <UnitTest id="2"><TestMethod className="A.BarTests+Nested" name="Y"/></UnitTest>
    <UnitTest id="3"><TestMethod className="A.SkipTests" name="Z"/></UnitTest>
    <UnitTest id="4"><TestMethod className="A.Gen` + "`" + `1" name="W"/></UnitTest>
  </TestDefinitions>
</TestRun>`

func TestParseTRX(t *testing.T) {
	got, err := ParseTRX(strings.NewReader(trxSample))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]TestOutcome{"A.FooTests": {1, 0}, "A.BarTests": {1, 1}, "A.SkipTests": {1, 0}, "A.Gen": {1, 1}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestRanges(t *testing.T) {
	for _, ls := range [][]int{nil, {1}, {1, 2, 3, 5, 7, 8}, {10, 12}} {
		s := Ranges(ls)
		back, ok := ParseRanges(s)
		if !ok || !slices.Equal(back, ls) && !(len(back) == 0 && len(ls) == 0) {
			t.Errorf("%v -> %q -> %v %v", ls, s, back, ok)
		}
	}
	if Ranges([]int{1, 2, 3, 5, 7, 8}) != "1-3,5,7-8" {
		t.Error(Ranges([]int{1, 2, 3, 5, 7, 8}))
	}
	for _, bad := range []string{"x", "3-1", "0", "1,,2"} {
		if _, ok := ParseRanges(bad); ok {
			t.Errorf("ParseRanges(%q) accepted", bad)
		}
	}
}

func TestFilters(t *testing.T) {
	if got := ClassFilter("A.B"); got != `FullyQualifiedName~A.B.|FullyQualifiedName~A.B+` {
		t.Error(got)
	}
	if got := EscapeFilter(`a(b)|c&d=e!f~g\h`); got != `a\(b\)\|c\&d\=e\!f\~g\\h` {
		t.Error(got)
	}
	if TopLevelClass("A.B+C+D") != "A.B" || TopLevelClass("A.G`2") != "A.G" || TopLevelClass("A.B/C") != "A.B" {
		t.Error("TopLevelClass")
	}
}

func TestUnsafeAndFloating(t *testing.T) {
	if r := UnsafeReference([]byte(`<PackageReference Include="Microsoft.CodeAnalysis.CSharp" Version="4.0.0" />`)); r == "" {
		t.Error("Roslyn not flagged")
	}
	if r := UnsafeReference([]byte(`<PackageReference Include="Microsoft.CodeAnalysisX" />` + "\n" + `<PackageReference Include="xunit" />`)); r != "" {
		t.Errorf("flagged %q", r)
	}
	for _, v := range []string{"1.*", "[1.0,2.0)", "(,2.0]"} {
		tr := treeOf(map[string]string{"a/a.csproj": `<Project><ItemGroup><PackageReference Include="X" Version="` + v + `" /></ItemGroup></Project>`})
		if FloatingVersions(tr) == "" {
			t.Errorf("%s not flagged", v)
		}
	}
	tr := treeOf(map[string]string{"Directory.Packages.props": "<Project><ItemGroup><PackageVersion Include=\"X\">\n<Version>2.*</Version></PackageVersion></ItemGroup></Project>"})
	if FloatingVersions(tr) == "" {
		t.Error("element version not flagged")
	}
	tr = treeOf(map[string]string{"a/a.csproj": `<Project><ItemGroup><PackageReference Include="X" Version="1.2.3" /></ItemGroup></Project>`})
	if f := FloatingVersions(tr); f != "" {
		t.Errorf("pinned version flagged: %s", f)
	}
}

func TestParseTRXAborted(t *testing.T) {
	base := `<?xml version="1.0"?><TestRun xmlns="http://microsoft.com/schemas/VisualStudio/TeamTest/2010"><Results/><TestDefinitions/>`
	for name, sum := range map[string]string{
		"outcome":  `<ResultSummary outcome="Aborted"><Counters/></ResultSummary>`,
		"counters": `<ResultSummary outcome="Failed"><Counters error="1"/></ResultSummary>`,
		"run info": `<ResultSummary outcome="Failed"><Counters/><RunInfos><RunInfo><Text>The active test run was aborted. Reason: Test host process crashed</Text></RunInfo></RunInfos></ResultSummary>`,
	} {
		if _, err := ParseTRX(strings.NewReader(base + sum + "</TestRun>")); err == nil {
			t.Errorf("%s: aborted run accepted", name)
		}
	}
	ok := `<ResultSummary outcome="Failed"><Counters failed="1"/><RunInfos><RunInfo><Text>[xUnit.net] X [FAIL]</Text></RunInfo></RunInfos></ResultSummary>`
	if _, err := ParseTRX(strings.NewReader(base + ok + "</TestRun>")); err != nil {
		t.Errorf("failed run rejected: %v", err)
	}
}

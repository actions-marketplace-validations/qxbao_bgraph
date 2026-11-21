package evalkit

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestLocateDotnet(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{
		"test/Serilog.Tests/LogTests.cs":                     "namespace Serilog.Tests;\npublic class LogTests { [Fact] public void Writes() {} }\n",
		"tests/Moq.Tests/Regressions/IssueReportsFixture.cs": "namespace Moq.Tests.Regressions\n{\n public class IssueReportsFixture\n {\n  public class Issue1054 { public void String_constant() {} }\n }\n}\n",
		"tests/Marten/LinqTests/where_clauses_fsharp.cs":     "namespace LinqTests.Acceptance { public class where_clauses_fsharp { public class with_duplicated_fields {} } }\n",
		"tests/A.Tests/PartialTests.cs":                      "namespace A.Tests { public partial class PartialTests { } }\n",
		"tests/A.Tests/PartialTests.Part2.cs":                "namespace A.Tests { public partial class PartialTests { } }\n",
		"tests/Dup1/FooTests.cs":                             "namespace X.Tests { public class FooTests { } }\n",
		"tests/Dup2/FooTests.cs":                             "namespace Y.Tests { public class FooTests { } }\n",
		"tests/Other/Misc.cs":                                "namespace Misc.Tests {\n public class Helper { }\n public class RealTests {\n  [Fact]\n  public async Task EnableCae() {}\n  [Fact] public void Other() {}\n }\n}\n",
		"src/Lib/NotATest.cs":                                "namespace Lib { public class NotATest { public void EnableCae() {} } }\n",
		"tests/Other/Calls.cs":                               "namespace Misc.Tests { public class Calls { void M() { await EnableCae(); } } }\n",
	})
	type exp struct {
		files []string
		class string
		form  string
		amb   bool
		nf    bool
		scan  bool
	}
	cases := []struct {
		name string
		want exp
	}{
		{"Serilog.Tests.LogTests.Writes", exp{files: []string{"test/Serilog.Tests/LogTests.cs"}, class: "Serilog.Tests.LogTests", form: "dotted"}},
		{"Serilog.Tests.LogTests.Writes(a: 1, b: \"x.y\")", exp{files: []string{"test/Serilog.Tests/LogTests.cs"}, class: "Serilog.Tests.LogTests", form: "dotted"}},
		{"src/Serilog.Tests::Serilog.Tests.LogTests.Writes", exp{files: []string{"test/Serilog.Tests/LogTests.cs"}, class: "Serilog.Tests.LogTests", form: "prefixed"}},
		{"Moq.Tests.Regressions.IssueReportsFixture+Issue1054.String_constant", exp{files: []string{"tests/Moq.Tests/Regressions/IssueReportsFixture.cs"}, class: "Moq.Tests.Regressions.IssueReportsFixture", form: "nested"}},
		{"LinqTests.Acceptance.where_clauses_fsharp.with_duplicated_fields(description: \"x\")", exp{files: []string{"tests/Marten/LinqTests/where_clauses_fsharp.cs"}, class: "LinqTests.Acceptance.where_clauses_fsharp", form: "dotted"}},
		{"A.Tests.PartialTests.M", exp{files: []string{"tests/A.Tests/PartialTests.Part2.cs", "tests/A.Tests/PartialTests.cs"}, class: "A.Tests.PartialTests", form: "dotted", amb: true}},
		{"X.Tests.FooTests.M", exp{files: []string{"tests/Dup1/FooTests.cs"}, class: "X.Tests.FooTests", form: "dotted"}},
		// class in a file with another name: full scan
		{"Misc.Tests.RealTests.Other", exp{files: []string{"tests/Other/Misc.cs"}, class: "Misc.Tests.RealTests", form: "dotted", scan: true}},
		// bare method name: method-declaration search in test-like files only; a call is not a declaration
		{"EnableCae", exp{files: []string{"tests/Other/Misc.cs"}, form: "bare", scan: true}},
		{"Serilog.Tests.LogTests.Writes(ipv6: \"2001:db8::\")", exp{files: []string{"test/Serilog.Tests/LogTests.cs"}, class: "Serilog.Tests.LogTests", form: "dotted"}},
		{"Nope.Missing.M", exp{nf: true, form: "dotted"}},
		{"Nope", exp{nf: true, form: "bare"}},
	}
	names := make([]string, len(cases))
	for i, c := range cases {
		names[i] = c.name
	}
	got := LocateDotnetTests(tr, names)
	for i, c := range cases {
		g := got[i]
		if !reflect.DeepEqual(g.Files, c.want.files) || g.Class != c.want.class || g.Form != c.want.form || g.Ambiguous != c.want.amb ||
			g.NotFound != c.want.nf || g.Scanned != c.want.scan {
			t.Errorf("%q:\n got %+v\nwant %+v", c.name, g, c.want)
		}
	}
}

package dotnetdyn

import (
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestBodyGuard(t *testing.T) {
	cls := func(body string) string {
		return "using System;\n\nnamespace A.B\n{\n    public class C\n    {\n" + body + "\n    }\n}\n"
	}
	cases := []struct {
		name, base, head string
		refuse           bool
		member           string // expected What of the single reported member ("" for none)
	}{
		{"unchanged", cls("int F() { return 1; }"), cls("int F() { return 1; }"), false, ""},
		{"method body", cls("int F()\n{\n    return 1;\n}"), cls("int F()\n{\n    return 2;\n}"), false, "method F"},
		{"same-line body", cls("int F() { return 1; }"), cls("int F() { return 2; }"), false, "method F"},
		{"statement added", cls("void F()\n{\n    G();\n}"), cls("void F()\n{\n    G();\n    H();\n}"), false, "method F"},
		{"statement added first", cls("void F()\n{\n    G();\n}"), cls("void F()\n{\n    H();\n    G();\n}"), false, "method F"},
		{"body emptied", cls("void F()\n{\n    G();\n}"), cls("void F()\n{\n}"), false, "method F"},
		{"whitespace", cls("int F() { return 1; }"), cls("int F()\n{\n    return   1;\n}"), false, ""},
		{"comment only", cls("// a\nint F() { return 1; } /* x */"), cls("// b\nint F() { return 1; } /* y\n z */"), false, ""},
		{"xml doc", cls("/// <summary>A</summary>\nconst int X = 1;"), cls("/// <summary>B</summary>\nconst int X = 1;"), false, ""},
		{"expression body", cls("int F() => 1;"), cls("int F() => 2;"), false, "method F"},
		{"expression property", cls("int P => 1;"), cls("int P => 2;"), false, "property P"},
		{"accessor get", cls("int P { get { return 1; } set { } }"), cls("int P { get { return 2; } set { } }"), false, "accessor get of P"},
		{"accessor set expression", cls("int P { get; set => _p = value; }"), cls("int P { get; set => _p = value + 1; }"), false, "accessor set of P"},
		{"accessor init", cls("int P { get; init { _p = value; } }"), cls("int P { get; init { _p = 1; } }"), false, "accessor init of P"},
		{"event accessors", cls("event Action E { add { a += value; } remove { } }"), cls("event Action E { add { b += value; } remove { } }"), false, "accessor add of E"},
		{"indexer", cls("int this[int i] { get => i; }"), cls("int this[int i] { get => i + 1; }"), false, "accessor get of this[]"},
		{"indexer parameter", cls("int this[int i] { get => i; }"), cls("int this[long i] { get => 0; }"), true, ""},
		{"auto property to full", cls("int P { get; set; }"), cls("int P { get => 1; set; }"), true, ""},
		{"field initializer", cls("private int _x = 1;"), cls("private int _x = 2;"), false, "initializer of field _x"},
		{"static readonly initializer", cls("static readonly string S = \"a\";"), cls("static readonly string S = \"b\";"), false, "initializer of field S"},
		{"second declarator", cls("int a = 1, b = 2;"), cls("int a = 1, b = 3;"), false, "initializer of field b"},
		{"declarator added", cls("int a = 1;"), cls("int a = 1, b = 3;"), true, ""},
		{"generic initializer", cls("Dictionary<int, string> d = new Dictionary<int, string>();"), cls("Dictionary<int, string> d = new Dictionary<int, string>(4);"), false, "initializer of field d"},
		{"property initializer", cls("public int P { get; set; } = 1;"), cls("public int P { get; set; } = 2;"), false, "initializer of property P"},
		{"lambda field", cls("Func<int, int> f = x => x;"), cls("Func<int, int> f = x => { return x + 1; };"), false, "initializer of field f"},
		{"field type", cls("private int _x = 1;"), cls("private long _x = 1;"), true, ""},
		{"field without initializer", cls("private int _x;"), cls("private int _y;"), true, ""},
		{"initializer added", cls("private int _x;"), cls("private int _x = 1;"), true, ""},
		{"const", cls("const int X = 1;"), cls("const int X = 2;"), true, ""},
		{"const local", cls("int F()\n{\n    const int k = 1;\n    return k;\n}"), cls("int F()\n{\n    const int k = 2;\n    return k;\n}"), false, "method F"},
		{"enum member", cls("enum E { A, B }"), cls("enum E { A, B, C }"), true, ""},
		{"enum value", "namespace N;\npublic enum E\n{\n    A = 1,\n}\n", "namespace N;\npublic enum E\n{\n    A = 2,\n}\n", true, ""},
		{"attribute", cls("[Obsolete(\"a\")]\nvoid F() { }"), cls("[Obsolete(\"b\")]\nvoid F() { }"), true, ""},
		{"attribute added", cls("void F() { }"), cls("[Fact]\nvoid F() { }"), true, ""},
		{"signature", cls("void F(int a) { }"), cls("void F(long a) { }"), true, ""},
		{"default parameter", cls("void F(int a = 1) { G(a); }"), cls("void F(int a = 2) { G(a); }"), true, ""},
		{"modifier", cls("public void F() { }"), cls("internal void F() { }"), true, ""},
		{"method added", cls("void F() { }"), cls("void F() { }\nvoid G() { }"), true, ""},
		{"method removed", cls("void F() { }\nvoid G() { }"), cls("void F() { }"), true, ""},
		{"method renamed", cls("void F() { }"), cls("void G() { }"), true, ""},
		{"constraint", cls("void F<T>() where T : class { }"), cls("void F<T>() where T : struct { }"), true, ""},
		{"generic method body", cls("void F<T>() where T : class { G(1); }"), cls("void F<T>() where T : class { G(2); }"), false, "method F"},
		{"constructor body", cls("public C(int a) : base(a) { G(1); }"), cls("public C(int a) : base(a) { G(2); }"), false, "constructor C"},
		{"constructor initializer", cls("public C(int a) : base(a) { }"), cls("public C(int a) : base(a + 1) { }"), true, ""},
		{"static constructor", cls("static C() { G(1); }"), cls("static C() { G(2); }"), false, "constructor C"},
		{"finalizer", cls("~C() { G(1); }"), cls("~C() { G(2); }"), false, "finalizer ~C"},
		{"operator", cls("public static C operator +(C a, C b) { return a; }"), cls("public static C operator +(C a, C b) { return b; }"), false, "operator operator +"},
		{"conversion", cls("public static implicit operator int(C c) => 1;"), cls("public static implicit operator int(C c) => 2;"), false, "operator operator int"},
		{"abstract method", cls("public abstract void F(int a);"), cls("public abstract void F(long a);"), true, ""},
		{"partial method declaration", cls("partial void F();"), cls("partial void F(int a);"), true, ""},
		{"local function", cls("int F()\n{\n    return G();\n    int G() { return 1; }\n}"), cls("int F()\n{\n    return G();\n    int G() { return 2; }\n}"), false, "method F"},
		{"lambda", cls("void F()\n{\n    Run(() => { G(1); });\n}"), cls("void F()\n{\n    Run(() => { G(2); });\n}"), false, "method F"},
		{"strings with braces", cls("string F()\n{\n    var a = \"{ // \";\n    return a;\n}\nconst int K = 1;"),
			cls("string F()\n{\n    var a = \"{ // \";\n    return a + \"}\";\n}\nconst int K = 1;"), false, "method F"},
		{"verbatim string", cls("string F()\n{\n    return @\"a \"\" } \n {\";\n}"), cls("string F()\n{\n    return @\"b \"\" } \n {\";\n}"), false, "method F"},
		{"interpolated string", cls("string F(int x)\n{\n    return $\"{x:N2} {{ {(x > 0 ? \"}\" : \"{\")} {new { A = 1 }}\";\n}"),
			cls("string F(int x)\n{\n    return $\"{x:N3} {{ {(x > 0 ? \"}\" : \"{\")} {new { A = 1 }}\";\n}"), false, "method F"},
		{"verbatim interpolated", cls("string F(int x)\n{\n    return $@\"{x}\n\"\" {{\" + @$\"{x}\";\n}"), cls("string F(int x)\n{\n    return $@\"{x + 1}\n\"\" {{\" + @$\"{x}\";\n}"), false, "method F"},
		{"raw string", cls("string F()\n{\n    return \"\"\"\n        { \"quoted\" // \"\"\n        \"\"\";\n}"),
			cls("string F()\n{\n    return \"\"\"\n        { \"quoted2\" // \"\"\n        \"\"\";\n}"), false, "method F"},
		{"interpolated raw", cls("string F(int x)\n{\n    return $$\"\"\"\n        { \"a\": {{x}} }\n        \"\"\";\n}"),
			cls("string F(int x)\n{\n    return $$\"\"\"\n        { \"a\": {{x + 1}} }\n        \"\"\";\n}"), false, "method F"},
		{"utf8 string", cls("ReadOnlySpan<byte> F() => \"a\"u8;"), cls("ReadOnlySpan<byte> F() => \"b\"u8;"), false, "method F"},
		{"char literals", cls("char F() { return '{'; }\nchar G() => '\\'';"), cls("char F() { return '}'; }\nchar G() => '\\'';"), false, "method F"},
		{"verbatim identifier", cls("int @class() { return 1; }"), cls("int @class() { return 2; }"), false, "method class"},
		{"using added", cls("void F() { }"), "using System.Text;\n" + cls("void F() { }"), true, ""},
		{"using removed", cls("void F() { }"), strings.Replace(cls("void F() { }"), "using System;\n", "", 1), true, ""},
		{"using static", "using static System.Math;\nclass C { }\n", "using static System.Console;\nclass C { }\n", true, ""},
		{"using alias changed", "using static System.Math;\nusing X = System.Int32;\nclass C { }\n", "using static System.Math;\nusing X = System.Int64;\nclass C { }\n", true, ""},
		{"using after an alias", "using X = System.Int32;\nusing System;\nclass C { }\n", "using X = System.Int32;\nusing System.IO;\nclass C { }\n", true, ""},
		{"using before an alias", "using System;\nusing X = System.Int32;\nclass C { }\n", "using System.IO;\nusing X = System.Int32;\nclass C { }\n", true, ""},
		{"using in namespace", "namespace N\n{\n    using System;\n    class C { }\n}\n", "namespace N\n{\n    using System.IO;\n    class C { }\n}\n", true, ""},
		{"using moved into namespace", "using System;\nnamespace N\n{\n    class C { }\n}\n", "namespace N\n{\n    using System;\n    class C { }\n}\n", true, ""},
		{"global using", "global using System;\nclass C { }\n", "global using System.IO;\nclass C { }\n", true, ""},
		{"global using added", "using System;\nclass C { }\n", "global using System.IO;\nusing System;\nclass C { }\n", true, ""},
		{"if region in body", cls("void F()\n{\n#if NET\n    G(1);\n#endif\n}"), cls("void F()\n{\n#if NET\n    G(2);\n#endif\n}"), true, ""},
		{"else branch", cls("void F()\n{\n#if NET\n    G(1);\n#else\n    G(3);\n#endif\n}"), cls("void F()\n{\n#if NET\n    G(1);\n#else\n    G(4);\n#endif\n}"), true, ""},
		{"if added", cls("void F()\n{\n    G(1);\n}"), cls("void F()\n{\n#if NET\n    G(1);\n#endif\n}"), true, ""},
		{"outside if region", cls("void F()\n{\n#if NET\n    G(1);\n#endif\n    H(1);\n}"), cls("void F()\n{\n#if NET\n    G(1);\n#endif\n    H(2);\n}"), false, "method F"},
		{"unbalanced if branches", cls("#if A\nvoid F(int a) {\n#else\nvoid F() {\n#endif\n    G(1);\n}\nvoid H() { G(1); }"),
			cls("#if A\nvoid F(int a) {\n#else\nvoid F() {\n#endif\n    G(1);\n}\nvoid H() { G(2); }"), false, "method H"},
		{"pragma in body", cls("void F()\n{\n#pragma warning disable CS0168\n    G(1);\n}"), cls("void F()\n{\n#pragma warning disable CS0169\n    G(1);\n}"), false, ""},
		{"pragma and code in body", cls("void F()\n{\n    G(1);\n}"), cls("void F()\n{\n#pragma warning disable CS0169\n    G(2);\n}"), false, "method F"},
		{"pragma at type level", cls("#pragma warning disable CA1031\nvoid F() { }"), cls("void F() { }"), false, ""},
		{"pragma with code", cls("void F() { }"), cls("#pragma warning disable CA1031\n[Obsolete]\nvoid F() { }"), true, ""},
		{"nullable directive", cls("void F()\n{\n    G(1);\n}"), cls("void F()\n{\n#nullable enable\n    G(1);\n}"), true, ""},
		{"region directive", cls("#region A\nvoid F() { }\n#endregion"), cls("#region B\nvoid F() { }\n#endregion"), false, ""},
		{"record primary constructor", "namespace N;\npublic record R(int X, string Y)\n{\n    public int F() => X;\n}\n", "namespace N;\npublic record R(int X, string Y)\n{\n    public int F() => X + 1;\n}\n", false, "method F"},
		{"record parameter default", "namespace N;\npublic record R(int X = 1);\n", "namespace N;\npublic record R(int X = 2);\n", true, ""},
		{"record struct", "namespace N;\npublic readonly record struct R(int X)\n{\n    public int F() { return 1; }\n}\n", "namespace N;\npublic readonly record struct R(int X)\n{\n    public int F() { return 2; }\n}\n", false, "method F"},
		{"file-scoped namespace", "namespace N.M;\n\nclass C\n{\n    int F() => 1;\n}\n", "namespace N.M;\n\nclass C\n{\n    int F() => 2;\n}\n", false, "method F"},
		{"namespace renamed", "namespace N.M;\nclass C { }\n", "namespace N.K;\nclass C { }\n", true, ""},
		{"partial modifier", strings.Replace(cls("void F() { }"), "public class C", "public partial class C", 1), cls("void F() { }"), true, ""},
		{"base type", cls("void F() { }"), strings.Replace(cls("void F() { }"), "public class C", "public class C : IDisposable", 1), true, ""},
		{"interface default method", "interface I\n{\n    int F() { return 1; }\n    int G();\n}\n", "interface I\n{\n    int F() { return 2; }\n    int G();\n}\n", false, "method F"},
		{"interface method declaration", "interface I\n{\n    int G();\n}\n", "interface I\n{\n    int G(int a);\n}\n", true, ""},
		{"nested type body", cls("class In\n{\n    int F() => 1;\n}"), cls("class In\n{\n    int F() => 2;\n}"), false, "method F"},
		{"nested type added", cls("void F() { }"), cls("void F() { }\nclass In { }"), true, ""},
		{"tuple return", cls("(int, string) F() => (1, \"a\");"), cls("(int, string) F() => (2, \"a\");"), false, "method F"},
		{"tuple property", cls("(int a, int b) P { get { return (1, 2); } }"), cls("(int a, int b) P { get { return (1, 3); } }"), false, "accessor get of P"},
		{"extension block", "static class E\n{\n    extension(string s)\n    {\n        public int F() => 1;\n    }\n}\n", "static class E\n{\n    extension(string s)\n    {\n        public int F(int a) => 1;\n    }\n}\n", true, ""},
		{"extension block body", "static class E\n{\n    extension(string s)\n    {\n        public int F() => 1;\n    }\n}\n", "static class E\n{\n    extension(string s)\n    {\n        public int F() => 2;\n    }\n}\n", false, "method F"},
		{"body moved to another method", cls("void F() { G(); }\nvoid H() { }"), cls("void F() { }\nvoid H() { G(); }"), false, ""},
		{"brace moved out", cls("void F() { G(); }"), cls("void F() { } G(); }"), true, ""},
		{"top-level statements", "using System;\nConsole.WriteLine(1);\nclass C { int F() => 1; }\n", "using System;\nConsole.WriteLine(2);\nclass C { int F() => 1; }\n", true, ""},
		{"top-level statements type body", "using System;\nusing var x = Open();\nConsole.WriteLine(1);\nclass C { int F() => 1; }\n", "using System;\nusing var x = Open();\nConsole.WriteLine(1);\nclass C { int F() => 2; }\n", false, "method F"},
		{"unterminated string", cls("string F() { return \"a\"; }"), cls("string F() { return \"a; }"), true, ""},
		{"unterminated comment", cls("void F() { }"), cls("/* void F() { }"), true, ""},
		{"unterminated raw", cls("string F() => \"\"\"\n a\n \"\"\";"), cls("string F() => \"\"\"\n a\n \"\";"), true, ""},
		{"unbalanced", cls("void F() { G(); }"), cls("void F() { G(); "), true, ""},
		{"missing endif", cls("void F() { G(); }"), cls("#if A\nvoid F() { G(); }"), true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			members, why := BodyGuard("src/C.cs", []byte(c.base), []byte(c.head))
			if (why != "") != c.refuse {
				t.Fatalf("BodyGuard = %v %q, want refuse=%v", members, why, c.refuse)
			}
			if why != "" {
				if !strings.HasPrefix(why, "src/C.cs") {
					t.Errorf("reason %q does not start with the path", why)
				}
				t.Logf("%s", why)
				return
			}
			if c.member == "" && len(members) > 0 && c.name != "body moved to another method" {
				t.Errorf("members %v, want none", members)
			}
			if c.member != "" && (len(members) != 1 || members[0].What != c.member) {
				t.Errorf("members %v, want one %q", members, c.member)
			}
		})
	}
	if _, why := BodyGuard("a.vb", []byte("a"), []byte("b")); why == "" {
		t.Error("non-C# file accepted")
	}
}

func TestBodyGuardSpan(t *testing.T) {
	base := "namespace N;\n\nclass C\n{\n    [Fact]\n    public void F()\n    {\n        G(1);\n    }\n\n    int P { get; } = 1;\n}\n"
	head := strings.Replace(strings.Replace(base, "G(1)", "G(2)", 1), "} = 1", "} = 3", 1)
	members, why := BodyGuard("C.cs", []byte(base), []byte(head))
	want := []Span{{5, 9, "method F"}, {11, 11, "initializer of property P"}}
	if why != "" || !slices.Equal(members, want) {
		t.Errorf("BodyGuard = %v %q, want %v", members, why, want)
	}
}

func TestTopLevelTypes(t *testing.T) {
	cases := []struct {
		name, src string
		want      []string
		ok        bool
	}{
		{"nested namespaces", "using System;\nnamespace A.B\n{\n    namespace C\n    {\n        public class X<T> where T : class { class In { } }\n        internal static partial class Y { }\n    }\n    struct Z { }\n}\nclass G { }\n",
			[]string{"A.B.C.X", "A.B.C.Y", "A.B.Z", "G"}, true},
		{"file-scoped", "namespace A.B;\n\n[Serializable]\npublic sealed record R(int X);\npublic record class RC { }\npublic readonly record struct RS(int Y);\npublic interface I<in T> { }\npublic enum E : byte { A, B }\npublic delegate void D<T>(T x);\nfile class F { }\n",
			[]string{"A.B.R", "A.B.RC", "A.B.RS", "A.B.I", "A.B.E", "A.B.D", "A.B.F"}, true},
		{"global namespace", "class A { delegate void In(); enum E { X } }\n", []string{"A"}, true},
		{"top-level statements", "using System;\nConsole.WriteLine(\"{\");\nif (true) { Run(); }\nstatic void Run() { }\npublic partial class Program { }\n", []string{"Program"}, true},
		{"conditional type", "namespace N;\n#if NET\npublic class A { }\n#else\npublic class B { }\n#endif\n", nil, false},
		{"conditional member", "namespace N;\npublic class A\n{\n#if NET\n    void F() { }\n#else\n    void G() { }\n#endif\n}\n", []string{"N.A"}, true},
		{"garbage", "class A { void F() { ", nil, false},
		{"unterminated string", "class A { string s = \"x; }", nil, false},
	}
	for _, c := range cases {
		got, ok := TopLevelTypes("A.cs", []byte(c.src))
		if ok != c.ok || !slices.Equal(got, c.want) {
			t.Errorf("%s: %v %v, want %v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestDiffHunks(t *testing.T) {
	a := strings.Split("a b c d e f g", " ")
	b := strings.Split("a B c d e g h", " ")
	got := diffHunks(a, b)
	want := []hunk{{1, 2, 1, 2}, {5, 6, 5, 5}, {7, 7, 6, 7}}
	if !slices.Equal(got, want) {
		t.Errorf("hunks %v want %v", got, want)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// TestLexRealRepos lexes and places every tracked .cs file at HEAD of the repositories in BGRAPH_CS_REPOS (';'-separated
// paths, read only).
func TestLexRealRepos(t *testing.T) {
	repos := os.Getenv("BGRAPH_CS_REPOS")
	if repos == "" {
		t.Skip("BGRAPH_CS_REPOS not set")
	}
	for _, repo := range strings.Split(repos, ";") {
		files := strings.Split(strings.TrimSpace(gitOut(t, repo, "ls-files", "-z", "*.cs")), "\x00")
		var failed []string
		n := 0
		for _, f := range files {
			if f == "" {
				continue
			}
			n++
			src := gitOut(t, repo, "show", "HEAD:"+f)
			if _, why := analyze([]byte(src)); why != "" {
				failed = append(failed, f+": "+why)
			} else if _, ok := TopLevelTypes(f, []byte(src)); !ok {
				failed = append(failed, f+": TopLevelTypes not ok")
			}
		}
		t.Logf("%s: %d .cs files at HEAD, %d failures", repo, n, len(failed))
		for _, f := range failed {
			t.Errorf("  %s", f)
		}
	}
}

// reasonCategories classify refusal reasons (first match wins).
var reasonCategories = []struct {
	name string
	re   *regexp.Regexp
}{
	{"cannot be lexed", regexp.MustCompile(`cannot be lexed`)},
	{"#if region", regexp.MustCompile(`an #if region`)},
	{"preprocessor directive", regexp.MustCompile(`preprocessor directive`)},
	{"member or type added/removed", regexp.MustCompile(`\(added or removed\)`)},
	{"attribute", regexp.MustCompile(`touches an attribute`)},
	{"const field", regexp.MustCompile(`touches const field`)},
	{"enum", regexp.MustCompile(`touches enum`)},
	{"type-level members", regexp.MustCompile(`touches the members of`)},
	{"member declaration", regexp.MustCompile(`touches the declaration of [a-z]+ .* outside its body`)},
	{"type declaration", regexp.MustCompile(`touches the declaration of`)},
	{"namespace", regexp.MustCompile(`touches namespace`)},
}

// TestGuardHistory runs BodyGuard over the first-parent history of the repositories in BGRAPH_GUARD_REPOS (';'-separated
// paths, read only; "path|prefix,prefix" restricts the files to those prefixes): commits whose changes are only modified
// .cs files, and how many pass.
func TestGuardHistory(t *testing.T) {
	repos := os.Getenv("BGRAPH_GUARD_REPOS")
	if repos == "" {
		t.Skip("BGRAPH_GUARD_REPOS not set")
	}
	for _, spec := range strings.Split(repos, ";") {
		repo, prefixes, _ := strings.Cut(spec, "|")
		inScope := func(f string) bool {
			if prefixes == "" {
				return true
			}
			for _, p := range strings.Split(prefixes, ",") {
				if strings.HasPrefix(f, p) {
					return true
				}
			}
			return false
		}
		commits := strings.Fields(gitOut(t, repo, "rev-list", "--first-parent", "-n", "500", "HEAD"))
		codeOnly, pass, files, filesPass := 0, 0, 0, 0
		categories := map[string]int{}
		var samples []string
		for _, c := range commits {
			out, err := exec.Command("git", "-C", repo, "diff-tree", "--no-commit-id", "-r", "--name-status", "--no-renames", c+"^", c).Output()
			if err != nil {
				continue // root commit
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			ok := len(lines) > 0 && lines[0] != ""
			var changed []string
			for _, l := range lines {
				f := strings.SplitN(l, "\t", 2)
				if len(f) != 2 || f[0] != "M" || path.Ext(f[1]) != ".cs" || !inScope(f[1]) {
					ok = false
					break
				}
				changed = append(changed, f[1])
			}
			if !ok {
				continue
			}
			codeOnly++
			first := ""
			for _, f := range changed {
				base := gitOut(t, repo, "show", c+"^:"+f)
				head := gitOut(t, repo, "show", c+":"+f)
				files++
				if _, why := BodyGuard(f, []byte(base), []byte(head)); why != "" {
					if first == "" {
						first = why
					}
				} else {
					filesPass++
				}
			}
			if first == "" {
				pass++
				continue
			}
			cat := "other"
			for _, rc := range reasonCategories {
				if rc.re.MatchString(first) {
					cat = rc.name
					break
				}
			}
			categories[cat]++
			samples = append(samples, c[:10]+" "+first)
		}
		t.Logf("%s: %d commits, %d code-only (M .cs), %d pass; files %d, %d pass", spec, len(commits), codeOnly, pass, files, filesPass)
		var cats []string
		for k, v := range categories {
			cats = append(cats, k+"="+strconv.Itoa(v))
		}
		sort.Strings(cats)
		t.Logf("  refusals by first reason: %s", strings.Join(cats, ", "))
		for _, s := range samples {
			t.Logf("  refused %s", s)
		}
	}
}

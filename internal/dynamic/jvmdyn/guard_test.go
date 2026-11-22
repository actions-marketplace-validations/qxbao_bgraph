package jvmdyn

import (
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"
	"testing"
)

func TestConstantGuard(t *testing.T) {
	cls := func(body string) string {
		return "package a.b;\n\nimport java.util.List;\n\npublic class A {\n" + body + "\n}\n"
	}
	cases := []struct {
		name, file, base, head string
		refuse                 bool
	}{
		{"unchanged", "A.java", cls("  static final int X = 1;"), cls("  static final int X = 1;"), false},
		{"whitespace in method", "A.java", cls("  void f() {\n    int y = 1;\n  }"), cls("  void f() {\n      int y = 1;\n  }"), false},
		{"method body", "A.java", cls("  static final int X = 1;\n  int f() {\n    return 1;\n  }"),
			cls("  static final int X = 1;\n  int f() {\n    return 2;\n  }"), false},
		{"import", "A.java", "package a;\nimport x.Y;\nclass A { static final int X = 1; }\n",
			"package a;\nimport x.Z;\nclass A { static final int X = 1; }\n", false},
		{"javadoc above constant", "A.java", cls("  /** Old. */\n  static final int X = 1;"), cls("  /** New. */\n  static final int X = 1;"), false},
		{"static final int", "A.java", cls("  static final int X = 1;"), cls("  static final int X = 2;"), true},
		{"multi-line constant", "A.java", cls("  public static final String S =\n      \"a\"\n      + \"b\";"),
			cls("  public static final String S =\n      \"a\"\n      + \"c\";"), true},
		{"instance final", "A.java", cls("  final int x = 5;"), cls("  final int x = 6;"), true},
		{"private static final String", "A.java", cls("  private static final String S = \"a\";"), cls("  private static final String S = \"b\";"), true},
		{"boxed final", "A.java", cls("  static final Integer I = 1;"), cls("  static final Integer I = 2;"), true},
		{"java.lang.String", "A.java", cls("  static final java.lang.String S = \"a\";"), cls("  static final java.lang.String S = \"b\";"), true},
		{"final List", "A.java", cls("  static final List<String> L = List.of(\"a\");"), cls("  static final List<String> L = List.of(\"b\");"), false},
		{"final Foo", "A.java", cls("  final Foo f = new Foo(1);"), cls("  final Foo f = new Foo(2);"), false},
		{"final array", "A.java", cls("  static final int[] A = {1, 2};"), cls("  static final int[] A = {1, 3};"), false},
		{"non-final static", "A.java", cls("  static int counter = 1;"), cls("  static int counter = 2;"), false},
		{"final without initializer", "A.java", cls("  final int x;\n  A() { x = 1; }"), cls("  final int x;\n  A() { x = 2; }"), false},
		{"anonymous class in initializer", "A.java", cls("  static final Runnable R = new Runnable() {\n    public void run() { int k = 1; }\n  };"),
			cls("  static final Runnable R = new Runnable() {\n    public void run() { int k = 2; }\n  };"), false},
		{"interface field", "I.java", "package a;\ninterface I {\n  int X = 1;\n  default int f() { return 1; }\n}\n",
			"package a;\ninterface I {\n  int X = 2;\n  default int f() { return 1; }\n}\n", true},
		{"interface default method", "I.java", "package a;\ninterface I {\n  int X = 1;\n  default int f() {\n    return 1;\n  }\n}\n",
			"package a;\ninterface I {\n  int X = 1;\n  default int f() {\n    return 2;\n  }\n}\n", false},
		{"nested interface field", "A.java", cls("  interface N {\n    String S = \"a\";\n  }"), cls("  interface N {\n    String S = \"b\";\n  }"), true},
		{"annotation default", "J.java", "package a;\n@interface J {\n  boolean serialize() default true;\n}\n",
			"package a;\n@interface J {\n  boolean serialize() default false;\n}\n", true},
		{"add constant", "A.java", cls("  void f() {}"), cls("  static final int NEW = 3;\n  void f() {}"), true},
		{"delete constant", "A.java", cls("  static final int OLD = 3;\n  void f() {}"), cls("  void f() {}"), true},
		{"enum constant argument", "E.java", "package a;\nenum E {\n  A(1),\n  B(2) { int g() { return 0; } };\n  static final int K = 1;\n  E(int v) {}\n}\n",
			"package a;\nenum E {\n  A(5),\n  B(2) { int g() { return 0; } };\n  static final int K = 1;\n  E(int v) {}\n}\n", false},
		{"enum body constant", "E.java", "package a;\nenum E {\n  A;\n  static final int K = 1;\n}\n",
			"package a;\nenum E {\n  A;\n  static final int K = 2;\n}\n", true},
		{"record body constant", "R.java", "package a;\nrecord R(int x, String y) {\n  static final int K = 1;\n  int f() { return x; }\n}\n",
			"package a;\nrecord R(int x, String y) {\n  static final int K = 1;\n  int f() { return x + 1; }\n}\n", false},
		{"record constant change", "R.java", "package a;\nrecord R(int x) {\n  static final int K = 1;\n}\n",
			"package a;\nrecord R(int x) {\n  static final int K = 2;\n}\n", true},
		{"compact record constructor", "R.java", "package a;\nrecord R(int i) {\n  R {\n    i += 1;\n  }\n}\n",
			"package a;\nrecord R(int i) {\n  R {\n    i += 2;\n  }\n}\n", false},
		{"package-info", "package-info.java", "/** a */\n@Deprecated\npackage a;\n", "/** b */\n@Deprecated\npackage a;\n", false},
		{"sealed permits", "S.java", "package a;\npublic sealed interface S permits S.A {\n  non-sealed class A implements S {\n    int f() { return 1; }\n  }\n}\n",
			"package a;\npublic sealed interface S permits S.A {\n  non-sealed class A implements S {\n    int f() { return 2; }\n  }\n}\n", false},
		{"annotated member with args", "A.java", cls("  @SuppressWarnings({\"a\", \"b\"})\n  static final int X = 1;\n  void f() { g(1); }"),
			cls("  @SuppressWarnings({\"a\", \"b\"})\n  static final int X = 1;\n  void f() { g(2); }"), false},
		// The unicode escape \u0022 is a quote: "\u0022 + {" closes the string, so the brace below is real code (a method body).
		{"unicode escaped quote", "A.java", cls("  String s = \"\\u0022;\n  static final int X = 1;\n  void f() { int k = 1; }"),
			cls("  String s = \"\\u0022;\n  static final int X = 1;\n  void f() { int k = 2; }"), false},
		{"unicode escape hides constant", "A.java", cls("  static final int X = 1; // \\u000a"), cls("  static final int X = 1; // \\u000a x"), true},
		{"text block with braces", "A.java", cls("  String t = \"\"\"\n    { not code\n    \"\"\";\n  static final int X = 1;\n  void f() { int k = 1; }"),
			cls("  String t = \"\"\"\n    { not code\n    \"\"\";\n  static final int X = 1;\n  void f() { int k = 2; }"), false},
		{"unbalanced", "A.java", cls("  void f() { int k = 1; }"), cls("  void f() { int k = 1; "), true},
		{"unterminated comment", "A.java", cls("  void f() {}"), cls("  /* void f() {}"), true},
		{"not a source", "a.groovy", "x", "y", true},
		// Kotlin
		{"kotlin const val", "K.kt", "package a\n\nconst val X = 1\n\nfun f() = 1\n", "package a\n\nconst val X = 2\n\nfun f() = 1\n", true},
		{"kotlin const continuation", "K.kt", "package a\n\nobject O {\n  private const val X = 1 +\n    2\n  fun f() = 1\n}\n",
			"package a\n\nobject O {\n  private const val X = 1 +\n    3\n  fun f() = 1\n}\n", true},
		{"kotlin const neighbour", "K.kt", "package a\n\nobject O {\n  const val X = 1\n  fun f() = 1\n}\n",
			"package a\n\nobject O {\n  const val X = 1\n  fun f() = 2\n}\n", false},
		{"kotlin val", "K.kt", "package a\n\nval x = 1\n", "package a\n\nval x = 2\n", false},
		{"kotlin inline fun", "K.kt", "package a\n\ninline fun <reified T> f(): Int {\n  return 1\n}\n", "package a\n\ninline fun <reified T> f(): Int {\n  return 2\n}\n", false},
		{"kotlin annotation default", "Json.kt", "package a\n\nannotation class Json(val name: String = UNSET, val ignore: Boolean = false)\n\nfun g() = 1\n",
			"package a\n\nannotation class Json(val name: String = UNSET, val ignore: Boolean = true)\n\nfun g() = 1\n", true},
		{"kotlin multi-line annotation", "Json.kt", "package a\n\npublic annotation class Json(\n  val name: String = UNSET,\n  val ignore: Boolean = false,\n) {\n  companion object { const val UNSET = \"x\" }\n}\n",
			"package a\n\npublic annotation class Json(\n  val name: String = UNSET,\n  val ignore: Boolean = true,\n) {\n  companion object { const val UNSET = \"x\" }\n}\n", true},
		{"kotlin template braces", "K.kt", "package a\n\nfun f(x: Int) = \"${ if (x > 0) { \"}\" } else \"{\" }\"\nconst val X = 1\nfun g() = 1\n",
			"package a\n\nfun f(x: Int) = \"${ if (x > 0) { \"}\" } else \"{\" }\"\nconst val X = 1\nfun g() = 2\n", false},
		{"kotlin raw string", "K.kt", "package a\n\nval s = \"\"\"\n  { ${1} \"\n\"\"\"\nfun g() = 1\n", "package a\n\nval s = \"\"\"\n  { ${1} \"\n\"\"\"\nfun g() = 2\n", false},
		{"kotlin nested comment", "K.kt", "package a\n/* a /* b */ { */\nconst val X = 1\nfun g() = 1\n", "package a\n/* a /* b */ { */\nconst val X = 1\nfun g() = 2\n", false},
		{"kotlin unbalanced", "K.kt", "package a\nfun g() = 1\n", "package a\nfun g() { = 1\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			why := ConstantGuard(c.file, []byte(c.base), []byte(c.head))
			if (why != "") != c.refuse {
				t.Errorf("ConstantGuard = %q, want refuse=%v", why, c.refuse)
			}
		})
	}
}

func TestTopLevelTypes(t *testing.T) {
	got, ok := TopLevelTypes("A.java", []byte("package a.b;\nimport x.Y;\n@Deprecated\npublic class A {\n  class In {}\n}\nfinal class B<T> extends A {}\ninterface C {}\nenum D { X }\nrecord E(int x) {}\n@interface F {}\n"))
	want := []string{"a.b.A", "a.b.B", "a.b.C", "a.b.D", "a.b.E", "a.b.F"}
	if !ok || !slices.Equal(got, want) {
		t.Errorf("java: %v %v", got, ok)
	}
	got, ok = TopLevelTypes("K.kt", []byte("@file:JvmName(\"X\")\npackage a.b\n\nclass K {\n  class In\n  companion object\n}\nobject O\nfun f() = K::class\nval v = object : Runnable { override fun run() {} }\ndata class D(val x: Int)\nfun interface G { fun g() }\n"))
	want = []string{"a.b.K", "a.b.O", "a.b.D", "a.b.G"}
	if !ok || !slices.Equal(got, want) {
		t.Errorf("kotlin: %v %v", got, ok)
	}
	if _, ok := TopLevelTypes("A.java", []byte("class A { void f() { ")); ok {
		t.Error("garbage java accepted")
	}
	if _, ok := TopLevelTypes("A.kt", []byte("class A { \"x")); ok {
		t.Error("garbage kotlin accepted")
	}
}

func TestPackageName(t *testing.T) {
	for _, c := range []struct{ file, src, want string }{
		{"A.java", "/* c */\npackage a.b.c;\nclass A {}", "a.b.c"},
		{"A.java", "class A {}", ""},
		{"A.kt", "@file:JvmName(\"X\")\n@file:Suppress(\"a\")\npackage com.x\n\nclass A", "com.x"},
		{"A.kt", "fun f() = 1", ""},
	} {
		got, ok := PackageName(c.file, []byte(c.src))
		if !ok || got != c.want {
			t.Errorf("%s %q: %q %v", c.file, c.src, got, ok)
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

// TestGuardHistory runs ConstantGuard over the first-parent history of the repositories in BGRAPH_GUARD_REPOS (';'-separated
// paths, read only): commits whose changes are only modified .java/.kt files, and how many pass.
func TestGuardHistory(t *testing.T) {
	repos := os.Getenv("BGRAPH_GUARD_REPOS")
	if repos == "" {
		t.Skip("BGRAPH_GUARD_REPOS not set")
	}
	git := func(dir string, args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return string(out)
	}
	for _, repo := range strings.Split(repos, ";") {
		parsed, failed := 0, []string{}
		for _, f := range strings.Fields(git(repo, "ls-files", "*.java", "*.kt")) {
			src := git(repo, "show", "HEAD:"+f)
			parsed++
			if _, ok := TopLevelTypes(f, []byte(src)); !ok {
				failed = append(failed, f)
			} else if why := ConstantGuard(f, []byte(src), []byte(src+"\n")); why != "" && !strings.Contains(why, "touches") {
				failed = append(failed, f+" ("+why+")")
			}
		}
		t.Logf("%s: %d sources at HEAD, %d not lexed: %v", repo, parsed, len(failed), failed)
		commits := strings.Fields(git(repo, "rev-list", "--first-parent", "-n", "500", "HEAD"))
		codeOnly, pass := 0, 0
		var samples []string
		for _, c := range commits {
			out := git(repo, "diff-tree", "--no-commit-id", "-r", "--name-status", "--no-renames", c+"^", c)
			lines := strings.Split(strings.TrimSpace(out), "\n")
			ok := len(lines) > 0 && lines[0] != ""
			var files []string
			for _, l := range lines {
				f := strings.SplitN(l, "\t", 2)
				if len(f) != 2 || f[0] != "M" || (path.Ext(f[1]) != ".java" && path.Ext(f[1]) != ".kt") {
					ok = false
					break
				}
				files = append(files, f[1])
			}
			if !ok {
				continue
			}
			codeOnly++
			why := ""
			for _, f := range files {
				base := git(repo, "show", c+"^:"+f)
				head := git(repo, "show", c+":"+f)
				if why = ConstantGuard(f, []byte(base), []byte(head)); why != "" {
					break
				}
			}
			if why == "" {
				pass++
			} else {
				samples = append(samples, c[:10]+" "+why)
			}
		}
		t.Logf("%s: %d commits, %d code-only (M .java/.kt), %d pass the constant guard", repo, len(commits), codeOnly, pass)
		for _, s := range samples {
			t.Logf("  refused %s", s)
		}
	}
}

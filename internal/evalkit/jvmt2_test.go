package evalkit

import (
	"testing"
)

func TestCFScanJava(t *testing.T) {
	src := `package a;
import java.util.List;
class A {
  boolean f(int x, List<String> l) {
    // if (x == 1) comment
    String s = "x == 2 && y";
    if (x == 3 && l != null) { return true; }
    int y = x + 1;
    int z = x - 1;
    x++;
    return x > 4 || x < 2;
  }
}
`
	ops := map[string]int{}
	for _, c := range cfScan([]byte(src), false) {
		ops[c.Op]++
	}
	want := map[string]int{"eq-ne": 1, "and-or": 1, "ne-eq": 1, "true-false": 1, "add-sub": 1, "sub-add": 1, "gt-le": 1, "or-and": 1, "lt-ge": 1}
	for k, v := range want {
		if ops[k] != v {
			t.Errorf("op %s: got %d want %d (all: %v)", k, ops[k], v, ops)
		}
	}
	if len(ops) != len(want) {
		t.Errorf("unexpected candidates: %v", ops)
	}
}

func TestCFScanCSharp(t *testing.T) {
	src := "using System;\nnamespace N {\n class C {\n  [Obsolete(\"a == b\")]\n  bool F(int a) { var s = $\"{a} == {a}\"; var v = @\"x \"\"== y\"\"\"; return a >= 1 && s != null; }\n }\n}\n"
	ops := map[string]int{}
	for _, c := range cfScan([]byte(src), true) {
		ops[c.Op]++
	}
	if ops["ge-lt"] != 1 || ops["and-or"] != 1 || ops["ne-eq"] != 1 || len(ops) != 3 {
		t.Errorf("%v", ops)
	}
}

func TestCandidateFiles(t *testing.T) {
	for f, want := range map[string]bool{
		"gson/src/main/java/com/google/gson/Gson.java":     true,
		"gson/src/test/java/com/google/gson/GsonTest.java": false,
		"moshi/src/main/java/a/B.kt":                       true,
		"x/src/main/java/module-info.java":                 false,
		"extras/src/main/java/a/Foo.java":                  true,
	} {
		if jvmCandidateFile(f) != want {
			t.Errorf("jvm %s: want %v", f, want)
		}
	}
	for f, want := range map[string]bool{
		"src/Polly.Core/Retry/Foo.cs":       true,
		"test/Polly.Core.Tests/FooTests.cs": false,
		"src/Polly.Core/obj/Foo.g.cs":       false,
		"src/Polly.Core/Foo.Designer.cs":    false,
	} {
		if dotnetCandidateFile(f) != want {
			t.Errorf("dotnet %s: want %v", f, want)
		}
	}
}

package evalkit

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestLocateJVMForms(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{
		"gson/src/test/java/com/google/gson/FooTest.java":           "package com.google.gson;\nclass FooTest {}\n",
		"core/src/test/kotlin/org/x/RuleTest.kt":                    "package org.x\nclass RuleTest {}\n",
		"core/src/integrationTest/java/org/x/SlowIT.java":           "package org.x;\nclass SlowIT {}\n",
		"core/src/test/groovy/org/x/SpecTest.groovy":                "package org.x\nclass SpecTest {}\n",
		"a/src/test/java/dup/DupTest.java":                          "package dup;\nclass DupTest {}\n",
		"b/src/test/java/dup/DupTest.java":                          "package dup;\nclass DupTest {}\n",
		"a/src/main/java/dup/DupTest.java":                          "package dup;\nclass DupTest {}\n", // main file with the same name: test location wins
		"test/transform/src/lombok/transform/TestWithDelombok.java": "package lombok.transform;\nclass TestWithDelombok {}\n",
		"src/test/java/NodeTest.java":                               "class NodeTest {}\n",
		"weird/tests/Layout.java":                                   "package org.weird;\npublic class Layout {}\nclass Helper {}\nclass Extra {}\n",
		"jib-cli/build.gradle":                                      "",
		"jib/jib-plugins-common/build.gradle.kts":                   "",
		"core/src/test/java/org/x/Outer.java":                       "package org.x;\nclass Outer { class Inner {} }\n",
		"src/test/kotlin/org/y/Multi.kt":                            "package org.y\nclass One\nclass Two\n",
	})
	type exp struct {
		files []string
		class string
		form  string
		amb   bool
		nf    bool
		task  bool
		scan  bool
	}
	cases := []struct {
		name string
		want exp
	}{
		{"com.google.gson.FooTest", exp{files: []string{"gson/src/test/java/com/google/gson/FooTest.java"}, class: "com.google.gson.FooTest", form: "dotted"}},
		{"com.google.gson.FooTest#testX", exp{files: []string{"gson/src/test/java/com/google/gson/FooTest.java"}, class: "com.google.gson.FooTest", form: "hash"}},
		{"com.google.gson.FooTest.testX", exp{files: []string{"gson/src/test/java/com/google/gson/FooTest.java"}, class: "com.google.gson.FooTest", form: "dotted"}},
		{"testX(com.google.gson.FooTest)", exp{files: []string{"gson/src/test/java/com/google/gson/FooTest.java"}, class: "com.google.gson.FooTest", form: "junit4"}},
		{"com.google.gson.FooTest > testX", exp{files: []string{"gson/src/test/java/com/google/gson/FooTest.java"}, class: "com.google.gson.FooTest", form: "gradle"}},
		{"com.google.gson.FooTest.testX[1]", exp{files: []string{"gson/src/test/java/com/google/gson/FooTest.java"}, class: "com.google.gson.FooTest", form: "dotted"}},
		{"com.google.gson.FooTest#testX[2] (a, b)", exp{files: []string{"gson/src/test/java/com/google/gson/FooTest.java"}, class: "com.google.gson.FooTest", form: "hash"}},
		{"com.google.gson.FooTest.testX(java.lang.String)", exp{files: []string{"gson/src/test/java/com/google/gson/FooTest.java"}, class: "com.google.gson.FooTest", form: "dotted"}},
		// Kotlin / Groovy
		{"org.x.RuleTest.Given a thing then something()", exp{files: []string{"core/src/test/kotlin/org/x/RuleTest.kt"}, class: "org.x.RuleTest", form: "dotted"}},
		{"org.x.RuleTest$Given a basic construct$Given a call.Given nested()", exp{files: []string{"core/src/test/kotlin/org/x/RuleTest.kt"}, class: "org.x.RuleTest", form: "dotted"}},
		{"org.x.SlowIT.runs", exp{files: []string{"core/src/integrationTest/java/org/x/SlowIT.java"}, class: "org.x.SlowIT", form: "dotted"}},
		{"org.x.SpecTest > feature", exp{files: []string{"core/src/test/groovy/org/x/SpecTest.groovy"}, class: "org.x.SpecTest", form: "gradle"}},
		// nested class: the top-level class's file
		{"org.x.Outer$Inner.testY", exp{files: []string{"core/src/test/java/org/x/Outer.java"}, class: "org.x.Outer", form: "dotted"}},
		// same class in two modules, and a main-source namesake that must not count
		{"dup.DupTest#t", exp{files: []string{"a/src/test/java/dup/DupTest.java", "b/src/test/java/dup/DupTest.java"}, class: "dup.DupTest", form: "hash", amb: true}},
		// lombok: data file name + (class)
		{"javac-ValInLambda.java(lombok.transform.TestWithDelombok)", exp{files: []string{"test/transform/src/lombok/transform/TestWithDelombok.java"}, class: "lombok.transform.TestWithDelombok", form: "junit4"}},
		// default package
		{"NodeTest", exp{files: []string{"src/test/java/NodeTest.java"}, class: "NodeTest", form: "dotted"}},
		// content scan: class in a file with another name, and a non-standard directory
		{"org.weird.Layout.testZ", exp{files: []string{"weird/tests/Layout.java"}, class: "org.weird.Layout", form: "dotted", scan: true}},
		{"org.weird.Helper#testZ", exp{files: []string{"weird/tests/Layout.java"}, class: "org.weird.Helper", form: "hash", scan: true}},
		{"org.y.Two > works", exp{files: []string{"src/test/kotlin/org/y/Multi.kt"}, class: "org.y.Two", form: "gradle", scan: true}},
		// not found
		{"org.nope.Missing#x", exp{nf: true, form: "hash"}},
		// Gradle tasks
		{"jib-cli:test", exp{task: true, form: "task"}},
		{"jib-plugins-common:test", exp{task: true, form: "task"}},
		{"compileTestJava", exp{task: true, form: "task"}},
	}
	names := make([]string, len(cases))
	for i, c := range cases {
		names[i] = c.name
	}
	got := LocateJVMTests(tr, names)
	for i, c := range cases {
		g := got[i]
		if !reflect.DeepEqual(g.Files, c.want.files) || g.Class != c.want.class || g.Form != c.want.form || g.Ambiguous != c.want.amb ||
			g.NotFound != c.want.nf || g.Task != c.want.task || g.Scanned != c.want.scan {
			t.Errorf("%q:\n got %+v\nwant %+v", c.name, g, c.want)
		}
	}
	if d := got[len(got)-3].Dirs; !reflect.DeepEqual(d, []string{"jib-cli"}) {
		t.Errorf("task dirs: %v", d)
	}
	if d := got[len(got)-2].Dirs; !reflect.DeepEqual(d, []string{"jib/jib-plugins-common"}) {
		t.Errorf("task dirs: %v", d)
	}
}

func TestPatchFiles(t *testing.T) {
	p := "diff --git a/x/A.java b/x/A.java\nindex 1..2\n--- a/x/A.java\n+++ b/x/A.java\n@@\ndiff --git a/old.txt b/new dir/B.txt\nrename from old.txt\n"
	got := patchFiles(p)
	if !got["x/A.java"] || !got["new dir/B.txt"] || len(got) != 2 {
		t.Errorf("%v", got)
	}
}

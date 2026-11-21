package jvm

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/tree"
)

func TestUnitIDRoundTrip(t *testing.T) {
	cases := []struct {
		kind, dir, path, id string
	}{
		{RunnerMaven, ".", "", "maven:."},
		{RunnerMaven, "a/b", "", "maven:a/b"},
		{RunnerGradle, ".", ":", "gradle::"},
		{RunnerGradle, ".", ":core:api", "gradle::core:api"},
		{RunnerGradle, "build-logic", ":convention", "gradle:build-logic:convention"},
		{RunnerGradle, "tools/shared", ":", "gradle:tools/shared:"},
	}
	for _, c := range cases {
		id := UnitID(c.kind, c.dir, c.path)
		if id != c.id {
			t.Errorf("UnitID(%v) = %q, want %q", c, id, c.id)
		}
		k, d, p, ok := ParseUnitID(id)
		if !ok || k != c.kind || d != c.dir || p != c.path {
			t.Errorf("ParseUnitID(%q) = %q %q %q %v", id, k, d, p, ok)
		}
	}
	for _, bad := range []string{"", "maven:", "gradle:core", "npm:x", "a/b::lib"} {
		if _, _, _, ok := ParseUnitID(bad); ok {
			t.Errorf("ParseUnitID(%q) accepted", bad)
		}
	}
}

func TestLaunchConfig(t *testing.T) {
	b, w := ParseLaunchConfig(LaunchConfig("a/pom.xml", "mvnw"))
	if b != "a/pom.xml" || w != "mvnw" {
		t.Fatalf("got %q %q", b, w)
	}
	b, w = ParseLaunchConfig(LaunchConfig("pom.xml", ""))
	if b != "pom.xml" || w != "" {
		t.Fatalf("got %q %q", b, w)
	}
}

func lex(t *testing.T, src string, kotlin bool) []token {
	t.Helper()
	toks, ok := lexGradle(src, kotlin)
	if !ok {
		t.Fatalf("lexGradle(%q) failed", src)
	}
	return toks
}

func texts(toks []token) []string {
	var out []string
	for _, tk := range toks {
		out = append(out, string(tk.kind)+":"+tk.text)
	}
	return out
}

func TestLexGradleStringsAndComments(t *testing.T) {
	src := "// project(':a')\n/* project(':b') */ x(\"p $d\", 'q', \"\"\"t\"\"\", '''u''') /* a /* nested */ b */ y"
	got := texts(lex(t, src, true))
	want := []string{"i:x", "p:(", "s:p $d", "p:,", "s:q", "p:,", "s:t", "p:,", "s:u", "p:)", "i:y"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	toks := lex(t, `"a ${b("}")} c"`, false)
	if len(toks) != 1 || !toks[0].dyn {
		t.Fatalf("interpolated string: %+v", toks)
	}
	if _, ok := lexGradle("x = 'abc", false); ok {
		t.Fatal("unterminated string accepted")
	}
	if _, ok := lexGradle("x /* never closed", false); ok {
		t.Fatal("unterminated comment accepted")
	}
}

func FuzzLexGradle(f *testing.F) {
	for _, s := range []string{"include 'a'", "x(\"$a\")", "/* /* */", "'''", "\"\"\"x", "`a b`", "\"${", "$", "'\\"} {
		f.Add(s, true)
		f.Add(s, false)
	}
	f.Fuzz(func(t *testing.T, s string, kotlin bool) {
		lexGradle(s, kotlin)
		parseSettings(s, kotlin)
		(&scriptRefs{}).scan(func() []token { toks, _ := lexGradle(s, kotlin); return toks }())
	})
}

func TestLexIgnoresCommentedProjectRefs(t *testing.T) {
	sr := &scriptRefs{}
	sr.scan(lex(t, "// project(':a')\n/* projects.b */\nx = 'project(\":c\")'\nimplementation project(':d')", false))
	var got []string
	for _, r := range sr.refs {
		got = append(got, r.val)
	}
	if !reflect.DeepEqual(got, []string{":d"}) {
		t.Fatalf("refs = %q", got)
	}
}

func TestParseSettings(t *testing.T) {
	groovy := `
rootProject.name = 'demo'
include ':a', ':b:c',
        'd'
includeFlat 'flat'
includeBuild('../other')
project(':a').projectDir = file('modules/a')
project(':d').projectDir = new File(settingsDir, 'dd')
apply from: "$rootDir/gradle/more-settings.gradle"
`
	si := parseSettings(groovy, false)
	if si.rootName != "demo" {
		t.Errorf("rootName %q", si.rootName)
	}
	if !slices.Equal(si.includes, []string{":a", ":b:c", "d"}) || !slices.Equal(si.flat, []string{"flat"}) ||
		!slices.Equal(si.includeBuilds, []string{"../other"}) {
		t.Errorf("includes %q flat %q builds %q", si.includes, si.flat, si.includeBuilds)
	}
	if si.dirs[":a"] != "modules/a" || si.dirs[":d"] != "dd" {
		t.Errorf("dirs %v", si.dirs)
	}
	if !slices.Equal(si.applied, []string{"gradle/more-settings.gradle"}) || len(si.dynamic) != 0 {
		t.Errorf("applied %q dynamic %q", si.applied, si.dynamic)
	}

	kts := "include(\":a\", \":b\",)\nproject(\":b\").projectDir = file(\"x/b\")\n"
	si = parseSettings(kts, true)
	if !slices.Equal(si.includes, []string{":a", ":b"}) || si.dirs[":b"] != "x/b" || len(si.dynamic) != 0 {
		t.Errorf("kts: %+v", si)
	}

	for _, dyn := range []string{
		"rootDir.listFiles().each { include it.name }",
		"mods.forEach { include(it) }",
		"include(*arrayOf(\"a\"))",
		"include(\":$x\")",
		"rootProject.children.each { it.projectDir = file(it.name) }",
		"project(':a').projectDir = file(computeDir())",
		"project(':a').buildFileName = 'a.gradle'",
		"apply from: someVariable",
	} {
		if si := parseSettings(dyn, strings.Contains(dyn, "forEach")); len(si.dynamic) == 0 {
			t.Errorf("%q is not reported as dynamic", dyn)
		}
	}
	// A method of another object named include is not the settings call.
	if si := parseSettings("fileTree('x').include('**/*.java')", false); len(si.includes) != 0 || len(si.dynamic) != 0 {
		t.Errorf("foo.include treated as a project include: %+v", si)
	}
}

func TestAccessorResolution(t *testing.T) {
	if accessorName("my-lib") != "myLib" || accessorName("a_b.c") != "aBC" || accessorName("core") != "core" {
		t.Fatalf("accessorName: %q %q", accessorName("my-lib"), accessorName("a_b.c"))
	}
	b := &gbuild{rootName: "demo", projects: map[string]*gproj{}}
	for _, p := range []string{":", ":core", ":core:api", ":my-lib", ":x", ":x:y"} {
		pr := &gproj{path: p, b: b}
		b.projects[p] = pr
		b.order = append(b.order, pr)
	}
	for _, c := range []struct {
		segs string
		want []string
	}{
		{"core.api", []string{":core:api"}},
		{"core", []string{":core"}},
		{"myLib.dependencyProject", []string{":my-lib"}},
		{"x.y.dependencyProject.name", []string{":x:y"}},
		{"demo", []string{":"}},
		{"nothing", nil},
	} {
		got := b.resolveAccessor(strings.Split(c.segs, "."))
		if !slices.Equal(got, c.want) {
			t.Errorf("projects.%s -> %q, want %q", c.segs, got, c.want)
		}
	}
}

func TestScriptRefs(t *testing.T) {
	src := `
dependencies {
    implementation project(':core')
    testImplementation(project(path = ":util", configuration = "x"))
    api(projects.lib.sub)
    implementation(project(":$name"))
}
subprojects { dependencies { testImplementation project(':tu') } }
tasks.register("t") { dependsOn(":core:jar") }
sourceSets { main { java.srcDir("../shared/src") } }
apply from: "$rootDir/gradle/x.gradle"
`
	sr := &scriptRefs{}
	sr.scan(lex(t, src, false))
	var got []string
	for _, r := range sr.refs {
		got = append(got, string(rune('0'+int(r.kind)))+":"+r.val+":"+string(rune('0'+int(r.sc.kind))))
	}
	want := []string{
		"0::core:0", "0::util:0", "2:lib.sub:0", "3:project(...) at line 6:0",
		"0::tu:2", "1::core:jar:0", "4:../shared/src:0", "5:gradle/x.gradle:0",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("refs\n got %q\nwant %q", got, want)
	}
	if len(sr.configures) != 1 || sr.configures[0].kind != scopeSub {
		t.Fatalf("configures %+v", sr.configures)
	}
}

func TestMavenInterpolationAndParents(t *testing.T) {
	files := map[string]string{
		"pom.xml": `<project><groupId>g</groupId><artifactId>root</artifactId><version>${revision}</version>
<properties><revision>3</revision><lib>core</lib></properties><modules><module>a</module></modules></project>`,
		"a/pom.xml": `<project><parent><groupId>g</groupId><artifactId>root</artifactId><version>${revision}</version></parent>
<artifactId>a</artifactId><properties><own>${lib}-x</own></properties></project>`,
	}
	m := loadMaven(tree.NewMapTree(files))
	a := m.poms["a/pom.xml"]
	if a == nil || a.localParent != "pom.xml" {
		t.Fatalf("module a / parent not found: %+v", a)
	}
	for in, want := range map[string]string{
		"${project.groupId}": "g", "${project.version}": "3", "${own}": "core-x", "${unknown}": "${unknown}",
		"x${lib}y": "xcorey", "${project.parent.version}": "3",
	} {
		if got := m.interp(a, in); got != want {
			t.Errorf("interp(%q) = %q, want %q", in, got, want)
		}
	}
	if got := m.ga["g:a"]; len(got) != 1 {
		t.Errorf("GA index: %v", got)
	}
}

func TestMavenNestedFixtureAndBrokenPom(t *testing.T) {
	files := map[string]string{
		"pom.xml":                         `<project><artifactId>root</artifactId></project>`,
		"src/test/resources/x/pom.xml":    `<project><artifactId>fixture</artifactId></project>`,
		"mod/pom.xml":                     `<project><artifactId>mod</artifactId>`,
		"mod/target/classes/leftover.xml": `x`,
	}
	m := loadMaven(tree.NewMapTree(files))
	if len(m.poms) != 2 || m.poms["src/test/resources/x/pom.xml"] != nil {
		t.Fatalf("poms: %v", m.order)
	}
	if m.poms["mod/pom.xml"].err == nil {
		t.Fatal("broken pom parsed")
	}
}

func analyze(t *testing.T, files map[string]string) *eco.Analysis {
	t.Helper()
	a, err := New(nil).Analyze(context.Background(), tree.NewMapTree(files), scancache.New())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNoJVMFilesIsFree(t *testing.T) {
	e := analyze(t, map[string]string{"a.go": "package a", "web/package.json": "{}"})
	if len(e.Tests) != 0 || len(e.Edges) != 0 || len(e.Projects) != 0 {
		t.Fatalf("analysis of a non-JVM tree is not empty: %+v", e)
	}
}

func TestUnreadablePomDependsOnEveryModule(t *testing.T) {
	e := analyze(t, map[string]string{
		"a/pom.xml": `<project><artifactId>a</artifactId></project>`,
		"b/pom.xml": `<<<not xml`,
	})
	found := false
	for _, ed := range e.Edges {
		if ed.From == Node("maven:b") && ed.To == Node("maven:a") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no edge from the broken module to the other one: %+v", e.Edges)
	}
}

func TestRolesAndOwnership(t *testing.T) {
	e := analyze(t, map[string]string{
		"pom.xml":                         `<project><artifactId>root</artifactId><modules><module>m</module></modules></project>`,
		"m/pom.xml":                       `<project><artifactId>m</artifactId></project>`,
		"m/src/main/java/A.java":          "class A {}",
		"m/src/test/java/ATest.java":      "class ATest {}",
		"m/src/main/resources/app.yml":    "a: 1",
		"m/target/classes/Generated.java": "class G {}",
	})
	want := map[string]model.Role{
		"m/pom.xml": model.RoleManifest, "m/src/main/java/A.java": model.RoleSource,
		"m/src/test/java/ATest.java": model.RoleTest, "m/src/main/resources/app.yml": model.RoleData,
	}
	for p, r := range want {
		if e.Roles[p] != r {
			t.Errorf("role of %s = %q, want %q", p, e.Roles[p], r)
		}
	}
	if _, ok := e.Roles["m/target/classes/Generated.java"]; ok {
		t.Error("a file under target/ is owned")
	}
}

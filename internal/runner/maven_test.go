package runner

import (
	"reflect"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/eco/jvm"
	"github.com/qxbao/bgraph/internal/model"
)

func withGOOS(t *testing.T, goos string) {
	t.Helper()
	old := jvmGOOS
	jvmGOOS = goos
	t.Cleanup(func() { jvmGOOS = old })
}

func mvnTest(dir, config string) model.SelectedTest {
	return model.SelectedTest{Unit: "maven:" + dir, Project: dir, Runner: "maven", Config: config, Files: []string{"<jvm:maven:" + dir + ">"}}
}

func gradleTest(buildDir, projectPath, dir, config string) model.SelectedTest {
	return model.SelectedTest{Unit: jvm.UnitID("gradle", buildDir, projectPath), Project: dir, Runner: "gradle", Config: config,
		Package: projectPath, Files: []string{"x"}}
}

func plan(t *testing.T, tests ...model.SelectedTest) Plan {
	t.Helper()
	p, err := Commands(model.Selection{Scope: model.TierGraph, Tests: tests}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMavenTwoStepWithWrapper(t *testing.T) {
	withGOOS(t, "linux")
	cfg := jvm.LaunchConfig("pom.xml", "mvnw")
	p := plan(t, mvnTest("core", cfg), mvnTest("app/web", cfg), mvnTest("core", cfg))
	if len(p.Commands) != 2 {
		t.Fatalf("commands: %+v", p.Commands)
	}
	want1 := []string{"./mvnw", "-pl", "app/web,core", "-am", "-DskipTests", "install"}
	want2 := []string{"./mvnw", "-pl", "app/web,core", "verify"}
	if !reflect.DeepEqual(p.Commands[0].Argv, want1) || !reflect.DeepEqual(p.Commands[1].Argv, want2) {
		t.Fatalf("argv:\n%q\n%q", p.Commands[0].Argv, p.Commands[1].Argv)
	}
	if p.Commands[0].Project != "." || p.Commands[0].Runner != "maven" {
		t.Fatalf("command: %+v", p.Commands[0])
	}
	if !strings.Contains(strings.Join(p.Notes, "\n"), "~/.m2") {
		t.Fatalf("notes: %q", p.Notes)
	}
}

func TestMavenReactorInSubdirAndNoWrapper(t *testing.T) {
	withGOOS(t, "linux")
	cfg := jvm.LaunchConfig("backend/pom.xml", "")
	p := plan(t, mvnTest("backend/core", cfg), mvnTest("backend", cfg))
	c := p.Commands[0]
	if c.Project != "backend" || c.Argv[0] != "mvn" || c.Argv[2] != ".,core" && c.Argv[2] != "core,." {
		t.Fatalf("command: %+v", c)
	}
}

func TestMavenWrapperAboveReactorRootAndWindows(t *testing.T) {
	cfg := jvm.LaunchConfig("backend/pom.xml", "mvnw")
	withGOOS(t, "linux")
	if got := plan(t, mvnTest("backend/core", cfg)).Commands[0].Argv[0]; got != "../mvnw" {
		t.Errorf("linux launcher %q", got)
	}
	withGOOS(t, "windows")
	if got := plan(t, mvnTest("backend/core", cfg)).Commands[0].Argv[0]; got != `..\mvnw.cmd` {
		t.Errorf("windows launcher %q", got)
	}
	if got := plan(t, mvnTest("core", jvm.LaunchConfig("pom.xml", "mvnw"))).Commands[0].Argv[0]; got != `.\mvnw.cmd` {
		t.Errorf("windows launcher at the root %q", got)
	}
}

func TestMavenUnsafeModuleNameAndLongList(t *testing.T) {
	withGOOS(t, "linux")
	cfg := jvm.LaunchConfig("pom.xml", "")
	p := plan(t, mvnTest("-odd", cfg), mvnTest("a,b", cfg))
	if len(p.Commands) != 1 || !reflect.DeepEqual(p.Commands[0].Argv, []string{"mvn", "verify"}) {
		t.Fatalf("a module the -pl list cannot carry must run the whole reactor: %+v", p.Commands)
	}
	p = plan(t, mvnTest("-odd", cfg))
	if p.Commands[0].Argv[2] != "./-odd" {
		t.Fatalf("option-like module: %q", p.Commands[0].Argv)
	}
	var tests []model.SelectedTest
	for i := 0; i < 2000; i++ {
		tests = append(tests, mvnTest("module-number-"+strings.Repeat("x", 10)+string(rune('a'+i%26))+strings.Repeat("y", i%7)+itoaT(i), cfg))
	}
	p = plan(t, tests...)
	if len(p.Commands) != 1 || p.Commands[0].Argv[1] != "verify" {
		t.Fatalf("a list longer than the command line must run the whole reactor: %d commands", len(p.Commands))
	}
}

func itoaT(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for ; i > 0; i /= 10 {
		s = string(rune('0'+i%10)) + s
	}
	return s
}

func TestGradleTasks(t *testing.T) {
	withGOOS(t, "linux")
	cfg := jvm.LaunchConfig("settings.gradle.kts", "gradlew")
	p := plan(t, gradleTest(".", ":core:api", "core/api", cfg), gradleTest(".", ":", ".", cfg), gradleTest(".", ":app", "app", cfg))
	if len(p.Commands) != 1 {
		t.Fatalf("commands: %+v", p.Commands)
	}
	want := []string{"./gradlew", ":app:test", ":core:api:test", ":test"}
	if !reflect.DeepEqual(p.Commands[0].Argv, want) {
		t.Fatalf("argv %q", p.Commands[0].Argv)
	}
	if p.Commands[0].Project != "." {
		t.Fatalf("project %q", p.Commands[0].Project)
	}
}

func TestGradleIncludedBuildRunsInItsOwnDirectory(t *testing.T) {
	withGOOS(t, "windows")
	p := plan(t,
		gradleTest(".", ":app", "app", jvm.LaunchConfig("settings.gradle", "gradlew")),
		gradleTest("tools/shared", ":lib", "tools/shared/lib", jvm.LaunchConfig("tools/shared/settings.gradle", "gradlew")))
	if len(p.Commands) != 2 {
		t.Fatalf("commands: %+v", p.Commands)
	}
	if p.Commands[0].Project != "." || p.Commands[0].Argv[0] != `.\gradlew.bat` {
		t.Errorf("root build: %+v", p.Commands[0])
	}
	if p.Commands[1].Project != "tools/shared" || p.Commands[1].Argv[0] != `..\..\gradlew.bat` || p.Commands[1].Argv[1] != ":lib:test" {
		t.Errorf("included build: %+v", p.Commands[1])
	}
}

func TestGradleWithoutWrapperAndBatching(t *testing.T) {
	withGOOS(t, "linux")
	cfg := jvm.LaunchConfig("settings.gradle", "")
	var tests []model.SelectedTest
	for i := 0; i < 600; i++ {
		tests = append(tests, gradleTest(".", ":module"+itoaT(i), "module"+itoaT(i), cfg))
	}
	p, err := Commands(model.Selection{Scope: model.TierGraph, Tests: tests}, Options{MaxArgChars: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Commands) < 2 || p.Commands[0].Argv[0] != "gradle" || p.Commands[0].Batches != len(p.Commands) {
		t.Fatalf("expected batches: %d commands, first %+v", len(p.Commands), p.Commands[0])
	}
}

func TestMavenAndGradleTogetherKeepOtherRunners(t *testing.T) {
	withGOOS(t, "linux")
	p := plan(t, mvnTest("core", jvm.LaunchConfig("pom.xml", "")), gradleTest(".", ":a", "a", jvm.LaunchConfig("settings.gradle", "")),
		model.SelectedTest{Unit: "web/a.test.ts", Project: "web", Runner: "vitest", Files: []string{"web/a.test.ts"}})
	runners := map[string]int{}
	for _, c := range p.Commands {
		runners[c.Runner]++
	}
	if runners["maven"] != 2 || runners["gradle"] != 1 || runners["vitest"] != 1 {
		t.Fatalf("runners %v", runners)
	}
}

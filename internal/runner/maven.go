package runner

import (
	"path"
	"runtime"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco/jvm"
	"github.com/qxbao/bgraph/internal/model"
)

// jvmGOOS is the platform the launcher names are chosen for (overridden by tests).
var jvmGOOS = runtime.GOOS

const (
	mavenNote = "maven: step 1 installs the upstream modules (-am) into the local repository ~/.m2 so that step 2 tests only the selected modules; " +
		"skip step 1 when CI already installed every module. Without -am Maven would take stale artifacts from ~/.m2"
)

// launcher returns the argv[0] that runs a build tool from directory rootDir: the repo's wrapper when it has one (repo-relative path
// wrapper; ".cmd"/".bat" on Windows), otherwise the tool on PATH.
func launcher(rootDir, wrapper, tool, winExt string) string {
	if wrapper == "" {
		return tool
	}
	rel := relFrom(rootDir, wrapper)
	if jvmGOOS == "windows" {
		return strings.ReplaceAll(pathWithDot(rel), "/", `\`) + winExt
	}
	return pathWithDot(rel)
}

// pathWithDot makes a bare file name an explicit relative path ("mvnw" -> "./mvnw") so it is run from the current directory.
func pathWithDot(rel string) string {
	if !strings.Contains(rel, "/") {
		return "./" + rel
	}
	return rel
}

type jvmKey struct{ root, wrapper, kind string }

// jvmCommands turns selected Maven modules and Gradle projects into build-tool commands, grouped by the build they belong to.
func jvmCommands(tests []model.SelectedTest, opt Options) ([]Command, []string) {
	groups := map[jvmKey][]model.SelectedTest{}
	var keys []jvmKey
	for _, t := range tests {
		buildFile, wrapper := jvm.ParseLaunchConfig(t.Config)
		root := "."
		if buildFile != "" {
			root = cleanProject(path.Dir(buildFile))
		}
		k := jvmKey{root, wrapper, t.Runner}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], t)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.root != b.root {
			return a.root < b.root
		}
		return a.kind < b.kind
	})
	var cmds []Command
	var notes []string
	seen := map[string]bool{}
	for _, k := range keys {
		var c []Command
		var ns []string
		if k.kind == jvm.RunnerMaven {
			c, ns = mavenCommands(k, groups[k], opt)
		} else {
			c, ns = gradleCommands(k, groups[k], opt)
		}
		cmds = append(cmds, c...)
		for _, n := range ns {
			if !seen[n] {
				seen[n] = true
				notes = append(notes, n)
			}
		}
	}
	return cmds, notes
}

// mavenCommands: `mvn -pl <S> -am -DskipTests install` then `mvn -pl <S> verify`, run in the reactor root. S is the list of module
// directories relative to the root. A list too long for the command line (or a directory Maven cannot take in -pl) is replaced by
// the whole reactor.
func mavenCommands(k jvmKey, tests []model.SelectedTest, opt Options) ([]Command, []string) {
	tool := launcher(k.root, k.wrapper, "mvn", ".cmd")
	var mods []string
	whole := false
	for _, t := range tests {
		rel := relFrom(k.root, cleanProject(t.Project))
		if strings.ContainsAny(rel, ",\n") {
			whole = true
		}
		if strings.HasPrefix(rel, "-") || strings.HasPrefix(rel, "!") || strings.HasPrefix(rel, "?") || strings.HasPrefix(rel, ":") {
			rel = "./" + rel
		}
		mods = append(mods, rel)
	}
	mods = dedupe(mods)
	list := strings.Join(mods, ",")
	size := len(tool) + len(" -pl  -am -DskipTests install") + len(list)
	if whole || size > maxArgs(opt) {
		return []Command{{Runner: jvm.RunnerMaven, Project: k.root, Argv: []string{tool, "verify"},
			Note: "the module list does not fit the command line (or cannot be passed to -pl): running the whole reactor"}}, []string{mavenNote}
	}
	cmds := []Command{
		{Runner: jvm.RunnerMaven, Project: k.root, Argv: []string{tool, "-pl", list, "-am", "-DskipTests", "install"},
			Note: "step 1 of 2: build and install the selected modules and their upstream modules without running tests"},
	}
	// The JVM class layer (docs/dynamic-layer.md §9.6): modules narrowed to some test classes run in a separate invocation, because
	// -Dtest applies to every module of an invocation. The class list goes through MAVEN_ARGS (Maven >= 3.9; the environment
	// is not limited like cmd.exe's command line): an older Maven ignores it and runs every class, a harmless superset.
	var plain, filtered, compileOnly, classes []string
	for _, t := range tests {
		switch {
		case !t.ClassFilter:
			plain = append(plain, plEntry(k, t))
		case len(t.Classes) == 0:
			compileOnly = append(compileOnly, plEntry(k, t))
		default:
			filtered = append(filtered, plEntry(k, t))
			for _, c := range t.Classes {
				classes = append(classes, c, c+"$*")
			}
		}
	}
	plain, filtered = dedupe(plain), dedupe(filtered)
	if len(plain) > 0 {
		cmds = append(cmds, Command{Runner: jvm.RunnerMaven, Project: k.root, Argv: []string{tool, "-pl", strings.Join(plain, ","), "verify"},
			Note: "step 2 of 2: test the selected modules only (verify keeps Failsafe integration tests)"})
	}
	if len(filtered) > 0 {
		cmds = append(cmds, Command{Runner: jvm.RunnerMaven, Project: k.root,
			Argv: []string{tool, "-pl", strings.Join(filtered, ","), "-Dsurefire.failIfNoSpecifiedTests=false", "verify"},
			Env:  []string{"MAVEN_ARGS=-Dtest=" + strings.Join(dedupe(classes), ",")},
			Note: "step 2 of 2: test only the classes the dynamic class layer kept in these modules (class list in MAVEN_ARGS)"})
	}
	notes := []string{mavenNote}
	if len(compileOnly) > 0 {
		notes = append(notes, "maven: no test class of "+strings.Join(dedupe(compileOnly), ", ")+
			" reaches a changed file (dynamic class layer): step 1 builds and compiles them, nothing runs there")
	}
	return cmds, notes
}

// plEntry is the -pl entry of a unit (as in the module list above).
func plEntry(k jvmKey, t model.SelectedTest) string {
	rel := relFrom(k.root, cleanProject(t.Project))
	if strings.HasPrefix(rel, "-") || strings.HasPrefix(rel, "!") || strings.HasPrefix(rel, "?") || strings.HasPrefix(rel, ":") {
		rel = "./" + rel
	}
	return rel
}

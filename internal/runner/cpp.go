package runner

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco/cpp"
	"github.com/qxbao/bgraph/internal/model"
)

// Restrict native filters to portable names. Meson interprets wildcards and
// colons, and CTest uses a different regex engine from Go's.
var nativeTestName = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_.-]*$`)

// CMakeBuildRunner labels the build step that precedes a ctest command.
const CMakeBuildRunner = "cmake"

// The default filesystems on Windows and macOS fold case, so two spellings
// of a build directory there are the same directory.
var caseInsensitiveFS = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

func cppCommands(tests []model.SelectedTest, opt Options) ([]Command, error) {
	if strings.ContainsAny(opt.BuildDir+opt.BuildConfig, "\x00\r\n") || strings.HasPrefix(opt.BuildConfig, "-") {
		return nil, fmt.Errorf("invalid C/C++ build directory or configuration")
	}
	type key struct{ project, runner string }
	type group struct {
		names []string
		whole bool
	}
	groups := map[key]*group{}
	var keys []key
	for _, t := range tests {
		k := key{cleanProject(t.Project), t.Runner}
		g := groups[k]
		if g == nil {
			g = &group{}
			groups[k] = g
			keys = append(keys, k)
		}
		system, dir, name, ok := cpp.ParseUnitID(t.Unit)
		systemMatches := k.runner == cpp.CTestRunner && system == cpp.CMake || k.runner == cpp.MesonRunner && system == cpp.Meson
		if t.Whole || !ok || !systemMatches || dir != k.project || !nativeTestName.MatchString(name) {
			g.whole = true
		} else {
			g.names = append(g.names, name)
		}
	}
	slices.SortFunc(keys, func(a, b key) int {
		if a.project == b.project {
			return strings.Compare(a.runner, b.runner)
		}
		return strings.Compare(a.project, b.project)
	})
	if filepath.IsAbs(opt.BuildDir) && len(keys) > 1 {
		return nil, fmt.Errorf("the absolute C/C++ build directory %s cannot serve the %d selected C/C++ projects: use a directory relative to each project", opt.BuildDir, len(keys))
	}
	var commands []Command
	dirs := map[string]key{}
	for _, k := range keys {
		g := groups[k]
		buildDir := opt.BuildDir
		if buildDir == "" {
			buildDir = "build"
			if groups[key{k.project, cpp.CTestRunner}] != nil && groups[key{k.project, cpp.MesonRunner}] != nil {
				if k.runner == cpp.CTestRunner {
					buildDir = "build-cmake"
				} else {
					buildDir = "build-meson"
				}
			}
		}
		dir := filepath.ToSlash(buildDir)
		if !filepath.IsAbs(buildDir) {
			dir = path.Join(k.project, dir)
		}
		dir = path.Clean(dir)
		if caseInsensitiveFS {
			dir = strings.ToLower(dir)
		}
		if old, exists := dirs[dir]; exists {
			return nil, fmt.Errorf("C/C++ test commands for %s (%s) and %s (%s) share a build directory", old.project, old.runner, k.project, k.runner)
		}
		dirs[dir] = k
		if strings.HasPrefix(buildDir, "-") {
			buildDir = "./" + buildDir
		}
		cmd := Command{Runner: k.runner, Project: k.project}
		var build []string
		switch k.runner {
		case cpp.CTestRunner:
			build = []string{"cmake", "--build", buildDir}
			cmd.Argv = []string{"ctest", "--test-dir", buildDir, "--output-on-failure", "--no-tests=error"}
			if opt.BuildConfig != "" {
				build = append(build, "--config", opt.BuildConfig)
				cmd.Argv = append(cmd.Argv, "--build-config", opt.BuildConfig)
			}
		case cpp.MesonRunner:
			cmd.Argv = []string{"meson", "test", "-C", buildDir, "--print-errorlogs"}
		}
		if !cppLineFits(k.project, cmd.Argv, maxArgs(opt)) || (build != nil && !cppLineFits(k.project, build, maxArgs(opt))) {
			return nil, fmt.Errorf("C/C++ command for %s exceeds the argument limit", k.project)
		}
		if g.whole {
			cmd.Note = "a selected test has no portable native filter: running the whole project"
		} else {
			slices.Sort(g.names)
			filter := slices.Compact(g.names)
			if k.runner == cpp.CTestRunner {
				for i, name := range filter {
					filter[i] = regexp.QuoteMeta(name)
				}
				filter = []string{"-R", "^(" + strings.Join(filter, "|") + ")$"}
			}
			args := append(slices.Clone(cmd.Argv), filter...)
			if cppLineFits(k.project, args, maxArgs(opt)) {
				cmd.Argv = args
			} else {
				cmd.Note = "the native test filter exceeds the argument limit: running the whole project"
			}
		}
		if build != nil {
			commands = append(commands, Command{Runner: CMakeBuildRunner, Project: k.project, Argv: build,
				Note: "builds the tests of the next ctest command: stop if it fails"})
		}
		commands = append(commands, cmd)
	}
	return commands, nil
}

func cppLineFits(project string, argv []string, maxChars int) bool {
	// Include a conservative allowance for shell quotes and the leading cd.
	length := len(project) + 11
	for _, arg := range argv {
		length += len(arg) + 3 + 4*strings.Count(arg, "'")
	}
	return length <= maxChars
}

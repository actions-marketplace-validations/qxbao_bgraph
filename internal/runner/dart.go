package runner

import (
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco/dart"
	"github.com/qxbao/bgraph/internal/model"
)

const integrationNote = "integration tests run the app on a device or emulator: pass -d <device> when more than one is available"

// dartCommands turns selected Dart units into commands, one per package directory (run there, paths relative to it):
//   - `dart test <files>` or `flutter test <files>`; a whole unit, a file outside the package or a command line over the
//     limit runs the package's whole suite (`dart test` / `flutter test` with no path);
//   - integration_test/ files of a Flutter package: a separate `flutter test <files>` (a whole `flutter test integration_test`
//     when the list is too long), which needs a device.
func dartCommands(tests []model.SelectedTest, opt Options) []Command {
	type group struct {
		runner             string
		whole, wholeInteg  bool
		files, integration []string
	}
	groups := map[string]*group{}
	var dirs []string
	for _, t := range tests {
		dir := cleanProject(t.Project)
		g := groups[dir]
		if g == nil {
			g = &group{runner: dart.RunnerDart}
			groups[dir] = g
			dirs = append(dirs, dir)
		}
		if t.Runner == dart.RunnerFlutter {
			g.runner = dart.RunnerFlutter
		}
		if t.Whole || dart.IsWholeUnitID(t.Unit) {
			g.whole = true
			continue
		}
		files := t.Files
		if len(files) == 0 {
			files = []string{t.Unit}
		}
		for _, f := range files {
			rel, ok := relTo(dir, f)
			switch {
			case !ok:
				g.whole = true
			case strings.HasPrefix(rel, "integration_test/"):
				g.integration = append(g.integration, rel)
			default:
				g.files = append(g.files, rel)
			}
		}
	}
	slices.Sort(dirs)
	var cmds []Command
	for _, dir := range dirs {
		g := groups[dir]
		prefix := []string{g.runner, "test"}
		switch {
		case g.whole:
			cmds = append(cmds, Command{Runner: g.runner, Project: dir, Argv: prefix,
				Note: "the test file set is not known file by file (or a file lies outside the package): running the whole package"})
		case len(g.files) > 0:
			argv := append(slices.Clone(prefix), dedupe(g.files)...)
			c := Command{Runner: g.runner, Project: dir, Argv: argv}
			if len(strings.Join(argv, " ")) > maxArgs(opt) {
				c.Argv, c.Note = prefix, "the file list exceeds the command-line limit: running the whole package"
			}
			cmds = append(cmds, c)
		}
		if len(g.integration) > 0 {
			argv := append([]string{dart.RunnerFlutter, "test"}, dedupe(g.integration)...)
			c := Command{Runner: dart.RunnerFlutter, Project: dir, Argv: argv, Note: integrationNote}
			if len(strings.Join(argv, " ")) > maxArgs(opt) {
				c.Argv = []string{dart.RunnerFlutter, "test", "integration_test"}
				c.Note = "the file list exceeds the command-line limit: running every integration test; " + integrationNote
			}
			cmds = append(cmds, c)
		}
	}
	return cmds
}

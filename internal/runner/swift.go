package runner

import (
	"regexp"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco/swift"
	"github.com/qxbao/bgraph/internal/model"
)

var swiftModuleName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func swiftCommands(tests []model.SelectedTest, opt Options) []Command {
	type suite struct {
		modules  []string
		selected map[string]bool
		whole    bool
	}
	packages := map[string]*suite{}
	var dirs []string
	for _, test := range tests {
		dir := cleanProject(test.Project)
		if packages[dir] == nil {
			packages[dir] = &suite{modules: test.TestModules, selected: map[string]bool{}}
			dirs = append(dirs, dir)
		}
		group := packages[dir]
		unitDir, target, ok := swift.ParseUnitID(test.Unit)
		if test.Whole || !ok || unitDir != dir || target == "" || !slices.Contains(group.modules, target) || !slices.Equal(group.modules, test.TestModules) {
			group.whole = true
			continue
		}
		group.selected[target] = true
	}
	slices.Sort(dirs)
	cmds := make([]Command, 0, len(dirs))
	for _, dir := range dirs {
		group := packages[dir]
		cmd := Command{Runner: swift.Runner, Project: dir, Argv: []string{"swift", "test"}}
		if group.whole {
			cmd.Note = "test modules could not be determined for this package: running the whole package"
		} else {
			var excluded []string
			for _, module := range group.modules {
				if !group.selected[module] && swiftModuleName.MatchString(module) {
					excluded = append(excluded, module)
				}
			}
			if len(excluded) > 0 {
				// Keep tests discovered in linked libraries and XCTest cases with
				// unqualified Objective-C names. Skip only known module prefixes.
				slices.Sort(excluded)
				pattern := `^(?:` + strings.Join(slices.Compact(excluded), "|") + `)\.`
				argv := append(slices.Clone(cmd.Argv), "--skip", pattern)
				if len(strings.Join(argv, " "))+2 <= maxArgs(opt) {
					cmd.Argv = argv
				} else {
					cmd.Note = "the Swift test skip pattern exceeds the argument limit: running the whole package"
				}
			}
		}
		cmds = append(cmds, cmd)
	}
	return cmds
}

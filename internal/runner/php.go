package runner

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/php"
	"github.com/qxbao/bgraph/internal/model"
)

// phpCommands turns selected PHP units into runner commands. A unit narrowed by the PHP layer (ClassFilter, docs/dynamic-layer.md
// §11.6) runs its kept test files as positional operands after the configuration; with no kept file it runs nothing, and when the
// operands would make the command line too long the unit runs whole (with a note).
func phpCommands(tests []model.SelectedTest, opt Options) ([]Command, []string, bool) {
	var commands []Command
	var notes []string
	for _, t := range tests {
		project := cleanProject(t.Project)
		d, ok := php.ParseUnitID(t.Unit)
		if !ok || project != d || t.BinDir == "/" || t.Whole {
			return nil, nil, false
		}
		wrapper := ""
		if t.BinDir != "" {
			wrapper = path.Join(t.BinDir, t.Runner)
		}
		argv := []string{launcher(project, wrapper, t.Runner, "")}
		if jvmGOOS == "windows" && wrapper != "" {
			// Composer can disable batch shims with bin-compat=proxy.
			argv = append([]string{"php"}, argv...)
		}
		if t.Config != "" {
			config, ok := relTo(project, t.Config)
			if !ok {
				return nil, nil, false
			}
			if strings.HasPrefix(config, "-") {
				config = "./" + config
			}
			argv = append(argv, "--configuration", config)
			if t.Runner == php.RunnerPest && len(t.RunnerPaths) == 1 {
				dir, ok := relTo(project, t.RunnerPaths[0])
				if !ok {
					return nil, nil, false
				}
				argv = append(argv, "--test-directory="+dir)
			}
			if t.ClassFilter {
				if len(t.Classes) == 0 {
					notes = append(notes, fmt.Sprintf("php: PHP layer: no test file of %s is affected; nothing runs for it", t.Unit))
					continue
				}
				narrowed := slices.Clone(argv)
				ok := true
				for _, f := range t.Classes {
					rel, rok := relTo(project, f)
					if !rok {
						ok = false
						break
					}
					if strings.HasPrefix(rel, "-") {
						rel = "./" + rel
					}
					narrowed = append(narrowed, rel)
				}
				if ok && len(strings.Join(narrowed, " ")) <= maxArgs(opt) {
					commands = append(commands, Command{Runner: t.Runner, Project: project, Argv: narrowed,
						Note: fmt.Sprintf("PHP layer: %d test files", len(t.Classes))})
					continue
				}
				notes = append(notes, fmt.Sprintf("php: the %d test files kept by the PHP layer for %s do not fit on one command line: the whole unit runs", len(t.Classes), t.Unit))
			}
			commands = append(commands, Command{Runner: t.Runner, Project: project, Argv: argv})
			continue
		}
		if len(t.RunnerPaths) == 0 {
			return nil, nil, false
		}
		// Older PHPUnit releases accept one positional test operand.
		for _, f := range t.RunnerPaths {
			rel, ok := relTo(project, f)
			if !ok {
				return nil, nil, false
			}
			args := slices.Clone(argv)
			if t.Runner == php.RunnerPest {
				switch {
				case eco.Within(rel, "tests"):
					// Pest bootstraps tests/ by default.
				case eco.Within(rel, "test"):
					args = append(args, "--test-directory=test")
				default:
					return nil, nil, false
				}
			}
			if strings.HasPrefix(rel, "-") {
				rel = "./" + rel
			}
			commands = append(commands, Command{Runner: t.Runner, Project: project, Argv: append(args, rel)})
		}
	}
	for _, c := range commands {
		if len(strings.Join(c.Argv, " ")) > maxArgs(opt) {
			return nil, nil, false
		}
	}
	return commands, notes, true
}

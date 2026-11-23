package runner

import (
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/php"
	"github.com/qxbao/bgraph/internal/model"
)

func phpCommands(tests []model.SelectedTest, opt Options) ([]Command, bool) {
	var commands []Command
	for _, t := range tests {
		project := cleanProject(t.Project)
		d, ok := php.ParseUnitID(t.Unit)
		if !ok || project != d || t.BinDir == "/" || t.Whole {
			return nil, false
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
				return nil, false
			}
			if strings.HasPrefix(config, "-") {
				config = "./" + config
			}
			argv = append(argv, "--configuration", config)
			if t.Runner == php.RunnerPest && len(t.RunnerPaths) == 1 {
				dir, ok := relTo(project, t.RunnerPaths[0])
				if !ok {
					return nil, false
				}
				argv = append(argv, "--test-directory="+dir)
			}
			commands = append(commands, Command{Runner: t.Runner, Project: project, Argv: argv})
			continue
		}
		if len(t.RunnerPaths) == 0 {
			return nil, false
		}
		// Older PHPUnit releases accept one positional test operand.
		for _, f := range t.RunnerPaths {
			rel, ok := relTo(project, f)
			if !ok {
				return nil, false
			}
			args := slices.Clone(argv)
			if t.Runner == php.RunnerPest {
				switch {
				case eco.Within(rel, "tests"):
					// Pest bootstraps tests/ by default.
				case eco.Within(rel, "test"):
					args = append(args, "--test-directory=test")
				default:
					return nil, false
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
			return nil, false
		}
	}
	return commands, true
}

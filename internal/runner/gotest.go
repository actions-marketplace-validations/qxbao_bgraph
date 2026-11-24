package runner

import (
	"fmt"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco/golang"
	"github.com/qxbao/bgraph/internal/model"
)

// goNote is attached to every plan that contains go commands.
const goNote = "go: one `go test` per module, run in the module directory with package paths relative to it (a go.work above it still applies); go test has no argument file, so long lists are split into batches"

// goRunFilter returns the -run value that runs exactly the given top-level test functions ("^$": none, the package is still
// compiled and vetted).
func goRunFilter(names []string) string {
	if len(names) == 0 {
		return "^$"
	}
	return "^(" + strings.Join(names, "|") + ")$"
}

// goCommands turns selected Go package units into `go test ./<rel> ...` commands, one group per module (Project). A unit whose ID
// cannot be interpreted, or that lies outside its module directory, makes the module run whole (`go test ./...`). A unit narrowed
// by the Go layer (ClassFilter) gets a command of its own with `-run '^(A|B)$'` (`-run '^$'` when no function is kept: the
// package still compiles and vets); when that command line is too long the unit runs whole with the others.
func goCommands(tests []model.SelectedTest, opt Options) ([]Command, []string) {
	groups := map[string][]string{}
	whole := map[string]bool{}
	filtered := map[string][]Command{}
	seen := map[string]bool{}
	var notes []string
	var mods []string
	for _, t := range tests {
		mod := cleanProject(t.Project)
		if !seen[mod] {
			seen[mod] = true
			mods = append(mods, mod)
		}
		dir, ok := golang.ParseUnitID(t.Unit)
		rel := ""
		if ok {
			rel, ok = relDir(mod, dir)
		}
		if !ok {
			whole[mod] = true
			continue
		}
		if t.ClassFilter {
			argv := []string{"go", "test", "-run", goRunFilter(t.Classes), rel}
			if len(strings.Join(argv, " ")) <= maxArgs(opt) {
				note := fmt.Sprintf("Go layer: %d test functions", len(t.Classes))
				if len(t.Classes) == 0 {
					note = "Go layer: no test function is affected; the package is still compiled and vetted"
				}
				filtered[mod] = append(filtered[mod], Command{Runner: golang.Runner, Project: mod, Argv: argv, Note: note})
				continue
			}
			notes = append(notes, fmt.Sprintf("go: the %d test functions kept by the Go layer for %s do not fit on one command line: the whole package runs", len(t.Classes), t.Unit))
		}
		groups[mod] = append(groups[mod], rel)
	}
	sort.Strings(mods)
	var cmds []Command
	for _, mod := range mods {
		if whole[mod] {
			cmds = append(cmds, Command{Runner: golang.Runner, Project: mod, Argv: []string{"go", "test", "./..."},
				Note: "a selected unit could not be placed in the module: running the whole module"})
			continue
		}
		if len(groups[mod]) > 0 {
			c, n := batch(golang.Runner, mod, []string{"go", "test"}, dedupe(groups[mod]), maxArgs(opt))
			cmds = append(cmds, c...)
			notes = append(notes, n...)
		}
		cmds = append(cmds, filtered[mod]...)
	}
	return cmds, append(notes, goNote)
}

// relDir returns the package directory dir as a `go test` argument relative to the module directory mod ("." or "./a/b").
func relDir(mod, dir string) (string, bool) {
	if dir == mod {
		return ".", true
	}
	rel, ok := relTo(mod, dir)
	if !ok || rel == "" || rel == "." {
		return "", false
	}
	return "./" + rel, true
}

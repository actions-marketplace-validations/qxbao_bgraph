package runner

import (
	"sort"

	"github.com/qxbao/bgraph/internal/eco/golang"
	"github.com/qxbao/bgraph/internal/model"
)

// goNote is attached to every plan that contains go commands.
const goNote = "go: one `go test` per module, run in the module directory with package paths relative to it (a go.work above it still applies); go test has no argument file, so long lists are split into batches"

// goCommands turns selected Go package units into `go test ./<rel> ...` commands, one group per module (Project). A unit whose ID
// cannot be interpreted, or that lies outside its module directory, makes the module run whole (`go test ./...`).
func goCommands(tests []model.SelectedTest, opt Options) ([]Command, []string) {
	groups := map[string][]string{}
	whole := map[string]bool{}
	var mods []string
	for _, t := range tests {
		mod := cleanProject(t.Project)
		if _, ok := groups[mod]; !ok && !whole[mod] {
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
		groups[mod] = append(groups[mod], rel)
	}
	sort.Strings(mods)
	var cmds []Command
	var notes []string
	for _, mod := range mods {
		if whole[mod] {
			cmds = append(cmds, Command{Runner: golang.Runner, Project: mod, Argv: []string{"go", "test", "./..."},
				Note: "a selected unit could not be placed in the module: running the whole module"})
			continue
		}
		c, n := batch(golang.Runner, mod, []string{"go", "test"}, dedupe(groups[mod]), maxArgs(opt))
		cmds = append(cmds, c...)
		notes = append(notes, n...)
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

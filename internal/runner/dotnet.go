package runner

import (
	"fmt"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/model"
)

// dotnetNote is attached to every plan that contains dotnet commands.
const dotnetNote = "dotnet: units are MSBuild projects, one `dotnet test` per project (it accepts a single project); a selected project that is not a test project is skipped by dotnet test"

// dotnetCommands turns selected .NET project units into one `dotnet test` command each. `dotnet test` takes one project (or solution)
// argument, so projects cannot share a command. A unit whose Config names a global.json that selects the Microsoft.Testing.Platform
// runner uses `--project` (the positional form is VSTest-only in that mode, and MTP accepts one --project per invocation).
//
// A unit narrowed by the class layer (ClassFilter, docs/dynamic-layer.md §9) runs `dotnet test --filter` with one
// FullyQualifiedName~<class> clause per kept class (a contains match: it may also run classes whose names extend a kept one,
// never fewer); with no kept class the project is only built.
func dotnetCommands(tests []model.SelectedTest, opt Options) ([]Command, []string) {
	sorted := append([]model.SelectedTest(nil), tests...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Unit < sorted[j].Unit })
	var cmds []Command
	var notes []string
	seen := map[string]bool{}
	mtp := false
	for _, t := range sorted {
		proj := t.Package
		if proj == "" {
			// No project file recorded: run the project directory (dotnet finds the single project in it).
			proj = cleanProject(t.Project)
		}
		proj = cleanProject(proj)
		if seen[proj] {
			continue
		}
		seen[proj] = true
		if t.ClassFilter {
			if len(t.Classes) == 0 {
				cmds = append(cmds, Command{Runner: "dotnet", Project: ".", Argv: []string{"dotnet", "build", proj},
					Note: "class layer: no test class of this project is affected; it is only built"})
				continue
			}
			if f := dotnetClassFilter(t.Classes); len(f) <= opt.MaxArgChars {
				argv := []string{"dotnet", "test", proj, "--filter", f}
				if t.Config != "" {
					argv = []string{"dotnet", "test", "--project", proj, "--filter", f}
					mtp = true
				}
				cmds = append(cmds, Command{Runner: "dotnet", Project: ".", Argv: argv,
					Note: fmt.Sprintf("class layer: %d test classes", len(t.Classes))})
				continue
			}
			notes = append(notes, fmt.Sprintf("dotnet: the class filter of %s is longer than %d characters: the whole project runs", proj, opt.MaxArgChars))
		}
		argv := []string{"dotnet", "test"}
		if t.Config != "" {
			argv = append(argv, "--project", proj)
			mtp = true
		} else {
			argv = append(argv, proj)
		}
		cmds = append(cmds, Command{Runner: "dotnet", Project: ".", Argv: argv})
	}
	notes = append(notes, dotnetNote)
	if mtp {
		notes = append(notes, "dotnet: global.json selects Microsoft.Testing.Platform (.NET 10 SDK), which takes `--project <file>` once per invocation")
	}
	return cmds, notes
}

// dotnetClassFilter is the VSTest filter running the given top-level classes (and their nested classes).
func dotnetClassFilter(classes []string) string {
	parts := make([]string, 0, len(classes))
	for _, c := range classes {
		var b strings.Builder
		for _, r := range c {
			if strings.ContainsRune(`\()|&=!~`, r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		parts = append(parts, "FullyQualifiedName~"+b.String())
	}
	return strings.Join(parts, "|")
}

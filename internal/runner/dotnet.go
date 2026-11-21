package runner

import (
	"sort"

	"github.com/qxbao/bgraph/internal/model"
)

// dotnetNote is attached to every plan that contains dotnet commands.
const dotnetNote = "dotnet: units are MSBuild projects, one `dotnet test` per project (it accepts a single project); a selected project that is not a test project is skipped by dotnet test"

// dotnetCommands turns selected .NET project units into one `dotnet test` command each. `dotnet test` takes one project (or solution)
// argument, so projects cannot share a command. A unit whose Config names a global.json that selects the Microsoft.Testing.Platform
// runner uses `--project` (the positional form is VSTest-only in that mode, and MTP accepts one --project per invocation).
func dotnetCommands(tests []model.SelectedTest) ([]Command, []string) {
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

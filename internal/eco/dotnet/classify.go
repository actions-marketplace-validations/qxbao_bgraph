package dotnet

import (
	"path"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// everyUnit widens to every .NET unit of the repository and to no other ecosystem's (module scope "." restricted to Eco "dotnet"):
// the plan's "full" for inputs that only .NET reads.
func everyUnit(reason, p, detail string) []model.Widening {
	return []model.Widening{{Tier: model.TierModule, Reason: reason, Path: p, Scope: ".", Eco: Name, Detail: detail}}
}

// ClassifyChange classifies .NET configuration (plan §6.5, §6.8 level L0). Project files, props/targets and the files they name are graph
// nodes: a change to them selects the projects that depend on them without any widening. global.json is in the global table
// (internal/classify) and never reaches here.
func (e *Eco) ClassifyChange(p string, _, _ tree.Tree, _ *eco.Analysis) ([]model.Widening, bool) {
	base := path.Base(p)
	lower := strings.ToLower(base)
	switch {
	case lower == "nuget.config":
		return everyUnit(model.ReasonGlobalInput, p, "NuGet configuration changed: package sources and mappings are resolved for every project"), true
	case base == "dotnet-tools.json" && path.Base(path.Dir(p)) == ".config":
		return everyUnit(model.ReasonGlobalInput, p, "local .NET tool manifest changed"), true
	case base == "Directory.Solution.props" || base == "Directory.Solution.targets":
		return everyUnit(model.ReasonGlobalInput, p, "solution-level MSBuild file changed"), true
	case strings.HasSuffix(lower, ".runsettings"):
		return everyUnit(model.ReasonTestConfig, p, "test run settings changed (they can be selected from any props file)"), true
	case base == "packages.lock.json":
		return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonLockChanged, Path: p, Scope: path.Dir(p), Eco: Name,
			Detail: "lockfile at level L0 (closure not compared)"}}, true
	}
	switch strings.ToLower(path.Ext(p)) {
	case ".sln", ".slnx", ".slnf":
		return nil, true // the projects it lists bring their own files
	case ".csproj", ".fsproj", ".vbproj", ".shproj":
		return nil, true // graph nodes: the project itself and its dependents
	}
	switch base {
	case "Directory.Build.props", "Directory.Build.targets", "Directory.Packages.props":
		return nil, true // every project below depends on it (edges), nothing else to widen
	}
	return nil, false
}

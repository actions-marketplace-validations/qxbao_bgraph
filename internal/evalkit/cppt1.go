package evalkit

import (
	"context"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/cpp"
	"github.com/qxbao/bgraph/internal/scancache"
)

// CPPT1Info separates missing source or registration evidence from a
// demonstrated native unit absent from the selected set.
type CPPT1Info struct {
	Names            []CPPTestLoc        `json:"names"`
	Unmapped         []string            `json:"unmapped,omitempty"`
	Untouched        int                 `json:"untouched,omitempty"`
	Requirements     map[string][]string `json:"requirements_by_name,omitempty"`
	PatchOnlyMissing []string            `json:"patch_only_missing,omitempty"`
	patchRequired    map[string]bool
}

type cppProject struct {
	system string
	dir    string
}

func cppTestUnits(a *eco.Analysis) (map[cppProject]map[string]string, map[string][]string) {
	edges := map[string][]string{}
	for _, edge := range a.Edges {
		edges[edge.From] = append(edges[edge.From], edge.To)
	}
	units := map[cppProject]map[string]string{}
	owners := map[string][]string{}
	for _, unit := range a.Tests {
		system, dir, name, ok := cpp.ParseUnitID(unit.ID)
		if !ok {
			continue
		}
		key := cppProject{system, dir}
		if units[key] == nil {
			units[key] = map[string]string{}
		}
		units[key][name] = unit.ID
		if name == "" {
			continue
		}
		for _, node := range unit.Files {
			for _, dep := range edges[node] {
				if cppOracleSource(dep) {
					owners[dep] = append(owners[dep], unit.ID)
				}
				if strings.HasPrefix(dep, "<cpp:") && strings.Contains(dep, "::target:") {
					for _, file := range edges[dep] {
						if cppOracleSource(file) {
							owners[file] = append(owners[file], unit.ID)
						}
					}
				}
			}
		}
	}
	return units, owners
}

// A registration under add_subdirectory belongs to the native launch root.
// Determine that root from the benchmark project layout and independent of
// the analyzer so a missing or mis-scoped native unit can become a violation.
func cppRegistrationRequirement(repo string, reg CPPRegistration) string {
	root := "."
	switch strings.ToLower(repo) {
	case "facebook/zstd":
		switch {
		case reg.System == cpp.CMake && strings.HasPrefix(reg.File, "build/cmake/"):
			root = "build/cmake"
		case reg.System == cpp.Meson && strings.HasPrefix(reg.File, "build/meson/"):
			root = "build/meson"
		case reg.System == cpp.Meson && strings.HasPrefix(reg.File, "contrib/meson/"):
			root = "contrib/meson"
		default:
			return ""
		}
	case "catchorg/catch2":
		if !strings.HasPrefix(reg.File, "projects/") && !strings.HasPrefix(reg.File, "tests/") {
			return ""
		}
	default:
		if reg.File != "CMakeLists.txt" && reg.File != "meson.build" &&
			!strings.HasPrefix(reg.File, "test/") && !strings.HasPrefix(reg.File, "tests/") {
			return ""
		}
	}
	return cpp.UnitID(reg.System, root, reg.Name)
}

// These projects use a root CMake scope to drive the respective test sources.
// The benchmark manifests and project source layouts establish this ownership
// when bgraph can emit only a whole-project fallback.
func cppCMakeTestSource(repo, file string) bool {
	switch strings.ToLower(repo) {
	case "fmtlib/fmt", "nlohmann/json", "yhirose/cpp-httplib":
		return strings.HasPrefix(file, "test/") || strings.HasPrefix(file, "tests/")
	case "simdjson/simdjson":
		return strings.HasPrefix(file, "tests/")
	case "catchorg/catch2":
		return strings.HasPrefix(file, "projects/SelfTest/") || strings.HasPrefix(file, "tests/SelfTest/")
	}
	return false
}

func cppSourceRequirements(repo, file string, owners map[string][]string, units map[cppProject]map[string]string) []string {
	required := slices.Clone(owners[file])
	if whole := units[cppProject{cpp.CMake, "."}][""]; whole != "" && cppCMakeTestSource(repo, file) {
		required = append(required, whole)
	}
	return required
}

func cppRequired(ctx context.Context, dir, commit, repo string, names []string, touched map[string]bool, kind string) ([]string, *CPPT1Info, error) {
	t, err := newGitTreeWant(ctx, dir, commit, cppOracleInput)
	if err != nil {
		return nil, nil, err
	}
	defer t.Close()
	locs, err := LocateCPPTests(t, repo, names, touched)
	if err != nil {
		return nil, nil, err
	}
	info := &CPPT1Info{Names: locs, Requirements: map[string][]string{}, patchRequired: map[string]bool{}}
	a, err := cpp.New().Analyze(ctx, t, scancache.New())
	if err != nil {
		return nil, info, err
	}
	units, owners := cppTestUnits(a)
	var required []string
	for _, loc := range locs {
		if kind == "n2p" && !loc.InPatch {
			info.Untouched++
			continue
		}
		var candidates, patch []string
		for _, file := range loc.Files {
			ids := cppSourceRequirements(repo, file, owners, units)
			if len(ids) == 0 {
				info.Unmapped = append(info.Unmapped, loc.Name+" ("+file+")")
			}
			if touched[file] {
				patch = append(patch, ids...)
			}
			candidates = append(candidates, ids...)
		}
		for _, reg := range loc.Registrations {
			if id := cppRegistrationRequirement(repo, reg); id != "" {
				patch = append(patch, id)
				candidates = append(candidates, id)
			}
		}
		if len(candidates) == 0 {
			info.Unmapped = append(info.Unmapped, loc.Name)
			continue
		}
		if len(patch) == 0 {
			patch = candidates
		}
		for _, id := range patch {
			info.patchRequired[id] = true
		}
		info.Requirements[loc.Name] = uniqSorted(candidates)
		required = append(required, candidates...)
	}
	info.Unmapped = uniqSorted(info.Unmapped)
	return uniqSorted(required), info, nil
}

func cppMissing(required []string, selected []SelUnit, info *CPPT1Info) []string {
	have := map[string]bool{}
	whole := map[cppProject]bool{}
	for _, unit := range selected {
		have[unit.Unit] = true
		if system, dir, _, ok := cpp.ParseUnitID(unit.Unit); ok && unit.Whole {
			whole[cppProject{system, dir}] = true
		}
	}
	var missing []string
	for _, id := range required {
		system, dir, _, _ := cpp.ParseUnitID(id)
		if !have[id] && !whole[cppProject{system, dir}] {
			missing = append(missing, id)
			if info.patchRequired[id] {
				info.PatchOnlyMissing = append(info.PatchOnlyMissing, id)
			}
		}
	}
	return missing
}

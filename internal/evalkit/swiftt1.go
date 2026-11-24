package evalkit

import (
	"context"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/swift"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
)

// SwiftT1Info records source candidates and the SwiftPM units that cover them.
type SwiftT1Info struct {
	Names            []SwiftTestLoc      `json:"names"`
	NotFound         []string            `json:"not_found,omitempty"`
	NoOwner          []string            `json:"no_owner,omitempty"`
	CommandResults   int                 `json:"command_results,omitempty"`
	Untouched        int                 `json:"untouched,omitempty"`
	PatchOnlyMissing []string            `json:"patch_only_missing,omitempty"`
	Requirements     map[string][]string `json:"requirements_by_name,omitempty"`
	patchReqs        map[string][]string
}

func swiftOpaqueUnit(u model.TestUnit) bool {
	return u.ID == swift.UnitID(u.Project, "")
}

// swiftFileOwners finds direct target ownership and follows implicit package
// inputs for opaque units, whose manifest supplies no test roots.
func swiftFileOwners(a *eco.Analysis) map[string][]string {
	implicit := map[string][]string{}
	for _, e := range a.Edges {
		if e.Kind == model.EdgeImplicit {
			implicit[e.From] = append(implicit[e.From], e.To)
		}
	}
	owners := map[string][]string{}
	for _, u := range a.Tests {
		for _, node := range u.Files {
			pending := implicit[node]
			if !swiftOpaqueUnit(u) {
				for _, file := range pending {
					if a.Roles[file] == model.RoleTest {
						owners[file] = append(owners[file], u.ID)
					}
				}
				continue
			}
			queue := append([]string(nil), pending...)
			seen := map[string]bool{}
			for i := 0; i < len(queue); i++ {
				input := queue[i]
				if seen[input] {
					continue
				}
				seen[input] = true
				if _, ok := a.Roles[input]; ok {
					project, owned := eco.NearestProject(a.Projects, input)
					if owned && project.Dir == u.Project {
						owners[input] = append(owners[input], u.ID)
					}
				}
				queue = append(queue, implicit[input]...)
			}
		}
	}
	return owners
}

func swiftRequired(ctx context.Context, dir, p string, ids []string, touched map[string]bool, kind string) ([]string, *SwiftT1Info, error) {
	gt, err := newGitTreeWant(ctx, dir, p, swiftSource)
	if err != nil {
		return nil, nil, err
	}
	defer gt.Close()
	locs, err := LocateSwiftTests(gt, ids, touched)
	if err != nil {
		return nil, nil, err
	}
	info := &SwiftT1Info{Names: locs, Requirements: map[string][]string{}, patchReqs: map[string][]string{}}
	a, err := swift.New().Analyze(ctx, gt, scancache.New())
	if err != nil {
		return nil, info, err
	}
	owners := swiftFileOwners(a)
	var req []string
	for _, l := range locs {
		switch {
		case l.Form == "command":
			info.CommandResults++
			continue
		case len(l.Files) == 0:
			info.NotFound = append(info.NotFound, l.Name)
			continue
		case kind == "n2p" && !l.InPatch:
			info.Untouched++
			continue
		}
		for _, file := range l.Files {
			units := owners[file]
			if len(units) == 0 {
				info.NoOwner = append(info.NoOwner, file)
				continue
			}
			info.Requirements[l.Name] = append(info.Requirements[l.Name], units...)
			if touched[file] {
				info.patchReqs[l.Name] = append(info.patchReqs[l.Name], units...)
			}
			req = append(req, units...)
		}
	}
	info.NoOwner = uniqSorted(info.NoOwner)
	return uniqSorted(req), info, nil
}

func swiftMissing(req []string, selected []SelUnit, info *SwiftT1Info) []string {
	have := map[string]bool{}
	whole := map[string]bool{}
	for _, u := range selected {
		have[u.Unit] = true
		if _, _, ok := swift.ParseUnitID(u.Unit); u.Whole && ok {
			whole[u.WholeProject] = true
		}
	}
	var missing []string
	patchOnly := map[string]bool{}
	for name, units := range info.Requirements {
		if patch := info.patchReqs[name]; len(patch) != 0 {
			units = patch
		}
		for _, unit := range units {
			patchOnly[unit] = true
		}
	}
	for _, unit := range req {
		project, _, _ := swift.ParseUnitID(unit)
		if !have[unit] && !whole[project] {
			missing = append(missing, unit)
			if patchOnly[unit] {
				info.PatchOnlyMissing = append(info.PatchOnlyMissing, unit)
			}
		}
	}
	return missing
}

// SwiftRepoKind supplies the category of each Longtail-SWE repository.
func SwiftRepoKind(repo string) string {
	switch strings.ToLower(repo) {
	case "phranck/tuikit", "awslabs/swift-aws-lambda-runtime":
		return "framework"
	case "bretth18/seeleseek", "corveil/crow", "dodobrands/peekie", "hylo-lang/hylo-new", "openclaw/imsg", "steipete/codexbar", "umputun/agterm", "vernissageapp/vernissageserver":
		return "application"
	default:
		return "library"
	}
}

package evalkit

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/dotnet"
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
)

// dotnetWholeSuite is the size of a FAIL_TO_PASS list from which it is treated as a whole-suite list (the test patch broke the build, so
// every test of the project "fails before"): only the names located in files the test patch touches become requirements.
const dotnetWholeSuite = 50

// DotnetReq is the requirement derived from one FAIL_TO_PASS name: the nearest project of every candidate test file (the test project
// that dotnet test runs). Ancestors lists the other projects that own a candidate file by ancestor ownership (informational).
type DotnetReq struct {
	Name      string   `json:"name"`
	Files     []string `json:"files"`
	Units     []string `json:"units"`
	Ancestors []string `json:"ancestors,omitempty"`
	Ambiguous bool     `json:"ambiguous,omitempty"`
}

// DotnetViolation is one required unit missing from the selection, with what a reviewer needs to classify it.
type DotnetViolation struct {
	Unit    string   `json:"unit"`
	Name    string   `json:"name"`
	Files   []string `json:"files"`
	Changed []string `json:"changed"`
	// ReachedChanged: changed files reachable from the unit's node along the dependency edges at P (the project's own files, its
	// ProjectReferences' files, imports...). Empty with a non-empty Changed means no static path exists.
	ReachedChanged []string `json:"reached_changed,omitempty"`
	ReachSize      int      `json:"forward_reach_size"`
	// ReachedProjects: project files in the forward reach (the transitive ProjectReference closure), capped.
	ReachedProjects []string `json:"reached_projects,omitempty"`
	// ChangedOwners: for each changed file, the projects that own it by ancestor ownership at P (what a hand check compares with the
	// project's ProjectReferences).
	ChangedOwners map[string][]string `json:"changed_owners,omitempty"`
	Class         string              `json:"class,omitempty"` // first pass: "real-miss?" (a path exists) | "no-path" (needs a reviewer)
}

// DotnetT1Info records how the .NET oracle treated one task.
type DotnetT1Info struct {
	Names     int `json:"names"`
	Kept      int `json:"kept"`
	Untouched int `json:"untouched,omitempty"` // whole-suite list: name located in a file the test patch does not touch
	NotFound  int `json:"not_found,omitempty"` // no candidate file: skipped
	NoOwner   int `json:"no_owner,omitempty"`  // candidate file(s) in no project directory: skipped
	Ambiguous int `json:"ambiguous,omitempty"`
	// AncestorsDiffer: kept names whose candidate files are owned by more than one project (nested projects: the nearest is required,
	// the others are selected by bgraph anyway through ancestor ownership).
	AncestorsDiffer int               `json:"ancestors_differ,omitempty"`
	Whole           bool              `json:"whole_suite,omitempty"`
	Reqs            []DotnetReq       `json:"reqs,omitempty"`
	NoOwnerFor      []string          `json:"no_owner_files,omitempty"`
	Violations      []DotnetViolation `json:"violations,omitempty"`

	a     *eco.Analysis
	files []string
}

// dotnetGitTree opens the P tree of a task with the blobs the .NET analysis reads (MSBuild files, global.json) prefetched in one batch.
func dotnetGitTree(ctx context.Context, dir, p string) (*gitTree, error) {
	return newGitTreeWant(ctx, dir, p, func(f string) bool {
		switch strings.ToLower(path.Ext(f)) {
		case ".csproj", ".fsproj", ".vbproj", ".props", ".targets", ".projitems", ".shproj", ".proj", ".editorconfig", ".globalconfig", ".rsp", ".slnx":
			return true
		}
		return path.Base(f) == "global.json"
	})
}

// dotnetRequired derives the required unit IDs of a task. dir is a repo directory holding commit p (base + test patch); tp is the set of
// files the test patch touches.
func dotnetRequired(ctx context.Context, dir, p string, ids []string, tp map[string]bool) ([]string, *DotnetT1Info, error) {
	info := &DotnetT1Info{Names: len(ids), Whole: len(ids) > dotnetWholeSuite}
	gt, err := dotnetGitTree(ctx, dir, p)
	if err != nil {
		return nil, info, err
	}
	defer gt.Close()
	a, err := dotnet.New(nil).Analyze(ctx, gt, scancache.New())
	if err != nil {
		return nil, info, err
	}
	info.a, info.files = a, gt.Files()
	projects := map[string]string{} // project file -> unit ID
	for _, u := range a.Tests {
		if pf, ok := dotnet.ParseUnitID(u.ID); ok {
			projects[pf] = u.ID
		}
	}
	oi := newDotnetOwnerIndex(projects)
	set := map[string]bool{}
	noOwn := map[string]bool{}
	for _, l := range LocateDotnetTests(jvmGitTree{gt, ctx, dir}, ids) {
		if l.NotFound {
			info.NotFound++
			continue
		}
		if info.Whole {
			touched := false
			for _, f := range l.Files {
				touched = touched || tp[f]
			}
			if !touched {
				info.Untouched++
				continue
			}
		}
		req := DotnetReq{Name: l.Name, Files: l.Files, Ambiguous: l.Ambiguous}
		nearest, anc := map[string]bool{}, map[string]bool{}
		for _, f := range l.Files {
			owners := oi.owners(f) // nearest first
			if len(owners) == 0 {
				noOwn[f] = true
				continue
			}
			nearest[projects[owners[0]]] = true
			for _, o := range owners {
				anc[projects[o]] = true
			}
		}
		if len(nearest) == 0 {
			info.NoOwner++
			continue
		}
		for u := range nearest {
			req.Units = append(req.Units, u)
			delete(anc, u)
		}
		for u := range anc {
			req.Ancestors = append(req.Ancestors, u)
		}
		sort.Strings(req.Units)
		sort.Strings(req.Ancestors)
		info.Kept++
		if l.Ambiguous {
			info.Ambiguous++
		}
		if len(req.Ancestors) > 0 {
			info.AncestorsDiffer++
		}
		for _, u := range req.Units {
			set[u] = true
		}
		info.Reqs = append(info.Reqs, req)
	}
	for f := range noOwn {
		info.NoOwnerFor = append(info.NoOwnerFor, f)
	}
	sort.Strings(info.NoOwnerFor)
	var out []string
	for u := range set {
		out = append(out, u)
	}
	sort.Strings(out)
	return out, info, nil
}

// dotnetOwnerIndex answers "which projects own file f" (every project whose directory contains it), nearest first.
type dotnetOwnerIndex map[string][]string // project directory -> project files

func newDotnetOwnerIndex(projects map[string]string) dotnetOwnerIndex {
	m := dotnetOwnerIndex{}
	for pf := range projects {
		m[path.Dir(pf)] = append(m[path.Dir(pf)], pf)
	}
	for _, v := range m {
		sort.Strings(v)
	}
	return m
}

func (m dotnetOwnerIndex) owners(f string) []string {
	var out []string
	for d := path.Dir(f); ; d = path.Dir(d) {
		out = append(out, m[d]...)
		if d == "." || d == "/" || d == "" {
			break
		}
	}
	return out
}

// dotnetMissing returns the required unit IDs the selection lacks and records the violations (per name) in info.
func dotnetMissing(req []string, sel []SelUnit, info *DotnetT1Info, changed []string) []string {
	have := map[string]bool{}
	for _, u := range sel {
		have[u.Unit] = true
	}
	var miss []string
	for _, u := range req {
		if !have[u] {
			miss = append(miss, u)
		}
	}
	if info != nil {
		for _, r := range info.Reqs {
			for _, u := range r.Units {
				if !have[u] {
					info.Violations = append(info.Violations, DotnetViolation{Unit: u, Name: r.Name, Files: r.Files, Changed: changed})
				}
			}
		}
	}
	return miss
}

// dotnetDiagnose fills the reach fields and Class of the violations from the .NET graph at P.
func dotnetDiagnose(info *DotnetT1Info) {
	if info == nil || len(info.Violations) == 0 || info.a == nil {
		return
	}
	a := info.a
	fwd := map[string][]string{}
	patsFrom := map[string][]*glob.Pattern{}
	for _, e := range a.Edges {
		if e.Kind == model.EdgePattern {
			if pt, err := glob.Compile(e.To); err == nil {
				patsFrom[e.From] = append(patsFrom[e.From], pt)
			}
			continue
		}
		fwd[e.From] = append(fwd[e.From], e.To)
	}
	projects := map[string]string{}
	for _, u := range a.Tests {
		if pf, ok := dotnet.ParseUnitID(u.ID); ok {
			projects[pf] = u.ID
		}
	}
	oi := newDotnetOwnerIndex(projects)
	for i := range info.Violations {
		v := &info.Violations[i]
		pf, _ := dotnet.ParseUnitID(v.Unit)
		start := dotnet.Node(pf)
		seen := map[string]bool{start: true}
		q := []string{start}
		for len(q) > 0 {
			c := q[0]
			q = q[1:]
			next := fwd[c]
			for _, pt := range patsFrom[c] {
				for _, f := range info.files {
					if pt.Match(f) {
						next = append(next, f)
					}
				}
			}
			for _, n := range next {
				if !seen[n] {
					seen[n] = true
					q = append(q, n)
				}
			}
		}
		v.ReachSize = len(seen)
		for n := range seen {
			if strings.HasPrefix(n, "<dotnet:") && n != start && len(v.ReachedProjects) < 60 {
				v.ReachedProjects = append(v.ReachedProjects, strings.TrimSuffix(strings.TrimPrefix(n, "<dotnet:"), ">"))
			}
		}
		v.ChangedOwners = map[string][]string{}
		for _, c := range v.Changed {
			if seen[c] {
				v.ReachedChanged = append(v.ReachedChanged, c)
			}
			if len(v.ChangedOwners) < 25 {
				v.ChangedOwners[c] = oi.owners(c)
			}
		}
		sort.Strings(v.ReachedChanged)
		sort.Strings(v.ReachedProjects)
		v.Class = "no-path"
		if len(v.ReachedChanged) > 0 {
			v.Class = "real-miss?"
		}
	}
}

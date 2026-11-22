package bgraph

import (
	"context"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/graph"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/selector"
)

// DependentsQuery asks who depends on a set of files.
type DependentsQuery struct {
	Files []string // repo-relative (or absolute, under the repo root) paths
	Rev   string   // "" = working tree
}

// Dependent is a file reached by the reverse walk. Chain explains the dependency, from the queried file to File:
// the first element is the queried file, each following element reads "X --kind-> Y" (X depends on Y).
type Dependent struct {
	File  string   `json:"file"`
	Test  bool     `json:"test"`
	Chain []string `json:"chain"`
}

// DependentsResult is the answer of Index.Dependents.
type DependentsResult struct {
	Files      []string           `json:"files"`      // normalized queried files
	Missing    []string           `json:"missing"`    // queried paths that are not nodes of the graph
	Dependents []Dependent        `json:"dependents"` // tests first, then by path; queried files are excluded
	Tests      []SelectedTest     `json:"tests"`      // test units reached
	Unresolved []model.Unresolved `json:"unresolved"` // unresolved imports in scope of a queried file: they MAY also depend on it
}

// Dependents answers "who depends on these files" with the same graph the selector uses, and explains every answer with a chain.
// A file that is not a graph node is reported in Missing (it may still be used outside the graph: see Unresolved and the selector's widenings).
func (ix *Index) Dependents(ctx context.Context, q DependentsQuery) (DependentsResult, error) {
	res := DependentsResult{Files: []string{}, Missing: []string{}, Dependents: []Dependent{}, Tests: []SelectedTest{}, Unresolved: []model.Unresolved{}}
	an, err := ix.analyzeRev(ctx, q.Rev)
	if err != nil {
		return res, err
	}
	g := graph.Build(append(slices.Clip(an.Analyses), eco.LinkAnalysis(an.Tree))...)

	var seeds []graph.Seed
	for _, f := range q.Files {
		p := ix.normalizePath(f)
		if slices.Contains(res.Files, p) {
			continue
		}
		res.Files = append(res.Files, p)
		if g.IsNode(p) {
			seeds = append(seeds, graph.Seed{Path: p, Reason: "queried"})
		} else {
			res.Missing = append(res.Missing, p)
		}
	}
	reach := g.Walk(seeds)

	for f := range reach {
		if slices.Contains(res.Files, f) {
			continue
		}
		chain := reach.Chain(f)
		slices.Reverse(chain)
		res.Dependents = append(res.Dependents, Dependent{File: f, Test: g.Role(f) == model.RoleTest, Chain: chain})
	}
	slices.SortFunc(res.Dependents, func(a, b Dependent) int {
		if a.Test != b.Test {
			if a.Test {
				return -1
			}
			return 1
		}
		return strings.Compare(a.File, b.File)
	})

	var units []model.TestUnit
	for _, a := range an.Analyses {
		units = append(units, a.Tests...)
	}
	for _, u := range selector.DedupUnits(units) {
		if f, ok := selector.ReachedFile(u, reach); ok {
			res.Tests = append(res.Tests, SelectedTest{Unit: u.ID, Project: u.Project, Runner: u.Runner, Files: u.Files, Whole: u.Whole,
				Reasons: []string{strings.Join(reach.Chain(f), " ; ")}})
			continue
		}
		if !u.Whole {
			continue
		}
		for f := range reach {
			if eco.Within(f, u.Project) {
				res.Tests = append(res.Tests, SelectedTest{Unit: u.ID, Project: u.Project, Runner: u.Runner, Files: u.Files, Whole: true,
					Reasons: []string{"project " + u.Project + " runs as a whole (test file set unknown) and contains " + f}})
				break
			}
		}
	}

	for _, u := range g.Unresolved {
		for _, f := range res.Files {
			if eco.Triggers(u, f) {
				res.Unresolved = append(res.Unresolved, u)
				break
			}
		}
	}
	return res, nil
}

// normalizePath turns a user-supplied path into the repo-relative, forward-slash form used by the graph.
func (ix *Index) normalizePath(p string) string {
	if filepath.IsAbs(p) {
		if rel, ok := relUnder(ix.Root(), p); ok {
			return rel
		}
		// Different spellings of the same directory (symlinks, Windows 8.3 short names such as RUNNER~1): compare the
		// resolved forms. The file itself may not exist (deleted file), so resolve its nearest existing ancestor.
		if root, err := filepath.EvalSymlinks(ix.Root()); err == nil {
			dir, rest := filepath.Clean(p), ""
			for {
				if r, err := filepath.EvalSymlinks(dir); err == nil {
					if rel, ok := relUnder(root, filepath.Join(r, rest)); ok {
						return rel
					}
					break
				}
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
				rest = filepath.Join(filepath.Base(dir), rest)
				dir = parent
			}
		}
	}
	return path.Clean(strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./"))
}

// relUnder returns p relative to root (forward slashes) when p lies inside root.
func relUnder(root, p string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

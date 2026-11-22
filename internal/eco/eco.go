// Package eco defines the interface of an ecosystem (node, python, rust...) towards the selector (docs/architecture.md).
//
// An ecosystem analyzes ONE tree (base or current) into an Analysis; the selector takes the union of the Analyses of both trees.
// It also classifies a changed file as its own manifest/config (widened by its own rules), based on both trees.
package eco

import (
	"context"
	"path"
	"strings"

	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// Project is a project of the ecosystem (package.json, crate, module...). Dir is the project root directory ("." is the repo root).
type Project struct {
	Eco  string `json:"eco"`
	Dir  string `json:"dir"`
	Name string `json:"name,omitempty"`
}

// Analysis is the result of analyzing one tree.
type Analysis struct {
	Projects   []Project
	Roles      map[string]model.Role // files claimed by this ecosystem (source, test, config, manifest, lock, data)
	Edges      []model.Edge          // From depends on To; when Kind=pattern, To is a glob evaluated on both trees
	Unresolved []model.Unresolved
	Tests      []model.TestUnit
	// Warnings do not change the scope (case_mismatch...); Path is the file containing the problem.
	Warnings []model.Widening
}

// Scanner returns the import scan result of a file, cached by content key.
type Scanner interface {
	Scan(t tree.Tree, p string) (model.ScanResult, error)
}

// Ecosystem is an ecosystem.
type Ecosystem interface {
	Name() string
	// Version changes when the analysis rules change (invalidates stored analysis snapshots).
	Version() string
	// Analyze analyzes tree t. A returned error makes the selector widen to full (internal_error); local errors should become Unresolved.
	Analyze(ctx context.Context, t tree.Tree, sc Scanner) (*Analysis, error)
	// ClassifyChange: is p (changed between base and head) a manifest/config/lockfile of this ecosystem? Returns the widenings and
	// true if the file was claimed. base may be nil (no base).
	ClassifyChange(p string, base, head tree.Tree, a *Analysis) ([]model.Widening, bool)
}

// Within reports whether p lies inside directory dir (dir "." or "" contains everything).
func Within(p, dir string) bool {
	if dir == "" || dir == "." {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// Triggers reports whether the changed path p makes the unresolved import u fire: p lies within u.ScopeDir and, when u.TriggerGlobs is
// set, matches one of them.
func Triggers(u model.Unresolved, p string) bool {
	if !Within(p, u.ScopeDir) {
		return false
	}
	if len(u.TriggerGlobs) == 0 {
		return true
	}
	for _, g := range u.TriggerGlobs {
		if glob.Match(g, p) {
			return true
		}
	}
	return false
}

// NearestProject returns the deepest project containing p.
func NearestProject(projects []Project, p string) (Project, bool) {
	best, ok := Project{}, false
	for _, pr := range projects {
		if Within(p, pr.Dir) && (!ok || depth(pr.Dir) > depth(best.Dir)) {
			best, ok = pr, true
		}
	}
	return best, ok
}

func depth(dir string) int {
	if dir == "." || dir == "" {
		return 0
	}
	return strings.Count(path.Clean(dir), "/") + 1
}

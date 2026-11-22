// Package graph holds the file-level graph as the UNION of the Analyses (base tree and current tree, every ecosystem) and walks backwards
// with no depth limit from the set of changed files (docs/architecture.md, "The file graph").
package graph

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
)

type patternEdge struct {
	edge model.Edge
	pat  *glob.Pattern
}

// Graph is the reverse graph: for each file To, the edges From→To.
type Graph struct {
	rev      map[string][]model.Edge
	patterns []patternEdge
	// byPrefix: pattern index keyed by the glob's static prefix (directory); "" is a pattern with no prefix (matches anywhere).
	byPrefix   map[string][]int
	badPattern []model.Edge // globs that fail to compile: the selector widens
	roles      map[string]model.Role
	Unresolved []model.Unresolved
}

// unresolvedKey identifies an Unresolved for deduplication (every field).
func unresolvedKey(u model.Unresolved) string {
	return strings.Join(append([]string{u.File, u.Spec, string(u.Class), strconv.Itoa(u.Line), fmt.Sprint(u.ScopeTier), u.ScopeDir,
		u.Detail}, u.TriggerGlobs...), "\x00")
}

// Build unions the Analyses. Duplicate edges (From, To, Kind) are kept once.
func Build(as ...*eco.Analysis) *Graph {
	g := &Graph{rev: map[string][]model.Edge{}, roles: map[string]model.Role{}}
	type key struct {
		from, to string
		kind     model.EdgeKind
	}
	seen := map[key]bool{}
	seenU := map[string]bool{}
	compiled := map[string]*glob.Pattern{}
	for _, a := range as {
		if a == nil {
			continue
		}
		for p, r := range a.Roles {
			// The test role wins (a file that is a test in either tree is still a test).
			if old, ok := g.roles[p]; !ok || r == model.RoleTest || (old != model.RoleTest && rolePrio(r) > rolePrio(old)) {
				g.roles[p] = r
			}
		}
		for _, e := range a.Edges {
			k := key{e.From, e.To, e.Kind}
			if seen[k] {
				continue
			}
			seen[k] = true
			if e.Kind == model.EdgePattern {
				// The same pattern recurs on many edges (one per project that names it): compile it once.
				pat, ok := compiled[e.To]
				var err error
				if !ok {
					pat, err = glob.Compile(e.To)
					if err == nil {
						compiled[e.To] = pat
					}
				}
				if err != nil {
					g.badPattern = append(g.badPattern, e)
					continue
				}
				if g.byPrefix == nil {
					g.byPrefix = map[string][]int{}
				}
				pre := pat.StaticPrefix()
				g.byPrefix[pre] = append(g.byPrefix[pre], len(g.patterns))
				g.patterns = append(g.patterns, patternEdge{e, pat})
				continue
			}
			g.rev[e.To] = append(g.rev[e.To], e)
		}
		for _, u := range a.Unresolved {
			if k := unresolvedKey(u); !seenU[k] {
				seenU[k] = true
				g.Unresolved = append(g.Unresolved, u)
			}
		}
	}
	for to := range g.rev {
		slices.SortFunc(g.rev[to], func(a, b model.Edge) int {
			if a.From != b.From {
				if a.From < b.From {
					return -1
				}
				return 1
			}
			if a.Kind < b.Kind {
				return -1
			} else if a.Kind > b.Kind {
				return 1
			}
			return 0
		})
	}
	return g
}

func rolePrio(r model.Role) int {
	switch r {
	case model.RoleTest:
		return 5
	case model.RoleSource:
		return 4
	case model.RoleConfig, model.RoleManifest, model.RoleLock:
		return 3
	case model.RoleData, model.RoleGenerated:
		return 2
	}
	return 1
}

// Role returns the role of a file (empty if no ecosystem claims it).
func (g *Graph) Role(p string) model.Role { return g.roles[p] }

// BadPatterns returns the glob edges that fail to compile.
func (g *Graph) BadPatterns() []model.Edge { return g.badPattern }

// IsNode: something depends on p (a direct incoming edge or via a glob), or p has a role in an ecosystem.
func (g *Graph) IsNode(p string) bool {
	if _, ok := g.roles[p]; ok {
		return true
	}
	return g.HasDependents(p)
}

// HasDependents: p has an incoming edge (direct or glob). A symlink edge counts only when its alias (the path that reads p through a
// link) is itself a test or has dependents: an alias nobody uses does not make p used.
func (g *Graph) HasDependents(p string) bool { return g.hasDependents(p, map[string]bool{p: true}) }

func (g *Graph) hasDependents(p string, seen map[string]bool) bool {
	for _, e := range g.rev[p] {
		if e.Kind != model.EdgeSymlink {
			return true
		}
		if seen[e.From] {
			continue
		}
		seen[e.From] = true
		if g.roles[e.From] == model.RoleTest || g.hasDependents(e.From, seen) {
			return true
		}
	}
	found := false
	g.candidates(p, func(pe patternEdge) bool {
		if pe.edge.From != p && pe.pat.Match(p) {
			found = true
			return false
		}
		return true
	})
	return found
}

// Aliases returns the paths that read p through a symbolic link (the sources of the symlink edges to p), sorted.
func (g *Graph) Aliases(p string) []string {
	var out []string
	for _, e := range g.rev[p] {
		if e.Kind == model.EdgeSymlink {
			out = append(out, e.From)
		}
	}
	return out
}

// candidates calls f with every pattern that could match p: those whose static prefix is "" or an ancestor directory of p
// (a glob only matches paths under its static prefix). f returns false to stop.
func (g *Graph) candidates(p string, f func(patternEdge) bool) {
	if len(g.byPrefix) == 0 {
		return
	}
	for d := p; ; {
		i := strings.LastIndexByte(d, '/')
		if i < 0 {
			d = ""
		} else {
			d = d[:i]
		}
		for _, idx := range g.byPrefix[d] {
			if !f(g.patterns[idx]) {
				return
			}
		}
		if d == "" {
			return
		}
	}
}

// Step is one step on the backward path: a file reached through Edge from file Via (empty Via: a seed).
type Step struct {
	Via    string
	Edge   model.Edge
	Seed   bool
	Reason string // the seed's reason (changed, unresolved...)
}

// Reach is the set of files reached when walking backwards.
type Reach map[string]Step

// Seed is the starting point of the backward walk.
type Seed struct {
	Path   string
	Reason string
}

// Walk walks backwards from seeds over every edge kind with no depth limit. The traversal order is deterministic (BFS in seed order,
// edges sorted).
func (g *Graph) Walk(seeds []Seed) Reach {
	r := Reach{}
	queue := make([]string, 0, len(seeds))
	for _, s := range seeds {
		if _, ok := r[s.Path]; ok {
			continue
		}
		r[s.Path] = Step{Seed: true, Reason: s.Reason}
		queue = append(queue, s.Path)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range g.rev[cur] {
			if _, ok := r[e.From]; ok {
				continue
			}
			r[e.From] = Step{Via: cur, Edge: e}
			queue = append(queue, e.From)
		}
		g.candidates(cur, func(pe patternEdge) bool {
			if pe.edge.From == cur {
				return true
			}
			if _, ok := r[pe.edge.From]; ok {
				return true
			}
			if !pe.pat.Match(cur) {
				return true
			}
			r[pe.edge.From] = Step{Via: cur, Edge: pe.edge}
			queue = append(queue, pe.edge.From)
			return true
		})
	}
	return r
}

// Chain returns the reason chain from p back to the seed: "p ← a (import) ← b (changed)".
func (r Reach) Chain(p string) []string {
	var out []string
	for i := 0; i < 1000; i++ {
		s, ok := r[p]
		if !ok {
			break
		}
		if s.Seed {
			out = append(out, p+" ("+s.Reason+")")
			break
		}
		out = append(out, p+" --"+string(s.Edge.Kind)+"-> "+s.Via)
		p = s.Via
	}
	return out
}

// Package graph giữ đồ thị mức tệp là HỢP của các Analysis (cây mốc và cây hiện tại, mọi hệ sinh thái) và đi ngược
// không giới hạn độ sâu từ tập tệp đổi (mục 3.4, 3.5, 4.3 bước 3–4).
package graph

import (
	"slices"

	"bgraph/internal/eco"
	"bgraph/internal/glob"
	"bgraph/internal/model"
)

type patternEdge struct {
	edge model.Edge
	pat  *glob.Pattern
}

// Graph là đồ thị ngược: với mỗi tệp To, các cạnh From→To.
type Graph struct {
	rev        map[string][]model.Edge
	patterns   []patternEdge
	badPattern []model.Edge // glob không biên dịch được: bộ chọn nới
	roles      map[string]model.Role
	Unresolved []model.Unresolved
}

// Build hợp các Analysis. Cạnh trùng (From, To, Kind) chỉ giữ một.
func Build(as ...*eco.Analysis) *Graph {
	g := &Graph{rev: map[string][]model.Edge{}, roles: map[string]model.Role{}}
	type key struct {
		from, to string
		kind     model.EdgeKind
	}
	seen := map[key]bool{}
	seenU := map[model.Unresolved]bool{}
	for _, a := range as {
		if a == nil {
			continue
		}
		for p, r := range a.Roles {
			// Vai trò test thắng (tệp là test ở một trong hai cây thì vẫn là test).
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
				pat, err := glob.Compile(e.To)
				if err != nil {
					g.badPattern = append(g.badPattern, e)
					continue
				}
				g.patterns = append(g.patterns, patternEdge{e, pat})
				continue
			}
			g.rev[e.To] = append(g.rev[e.To], e)
		}
		for _, u := range a.Unresolved {
			if !seenU[u] {
				seenU[u] = true
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

// Role trả vai trò của tệp (rỗng nếu không hệ sinh thái nào nhận).
func (g *Graph) Role(p string) model.Role { return g.roles[p] }

// BadPatterns trả cạnh glob không biên dịch được.
func (g *Graph) BadPatterns() []model.Edge { return g.badPattern }

// IsNode: có ai phụ thuộc p (cạnh vào trực tiếp hoặc qua glob), hoặc p có vai trò trong một hệ sinh thái.
func (g *Graph) IsNode(p string) bool {
	if _, ok := g.roles[p]; ok {
		return true
	}
	return g.HasDependents(p)
}

// HasDependents: có cạnh vào p (trực tiếp hoặc glob).
func (g *Graph) HasDependents(p string) bool {
	if len(g.rev[p]) > 0 {
		return true
	}
	for _, pe := range g.patterns {
		if pe.edge.From != p && pe.pat.Match(p) {
			return true
		}
	}
	return false
}

// Step là một bước trên đường đi ngược: tệp đạt được qua cạnh Edge từ tệp Via (Via rỗng: hạt giống).
type Step struct {
	Via    string
	Edge   model.Edge
	Seed   bool
	Reason string // lý do của hạt giống (changed, unresolved...)
}

// Reach là tập tệp đạt được khi đi ngược.
type Reach map[string]Step

// Seed là hạt giống của phép đi ngược.
type Seed struct {
	Path   string
	Reason string
}

// Walk đi ngược từ seeds qua mọi loại cạnh, không giới hạn độ sâu. Thứ tự duyệt tất định (BFS theo thứ tự hạt giống,
// cạnh đã sắp).
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
		for _, pe := range g.patterns {
			if pe.edge.From == cur || !pe.pat.Match(cur) {
				continue
			}
			if _, ok := r[pe.edge.From]; ok {
				continue
			}
			r[pe.edge.From] = Step{Via: cur, Edge: pe.edge}
			queue = append(queue, pe.edge.From)
		}
	}
	return r
}

// Chain trả chuỗi lý do từ p về hạt giống: "p ← a (import) ← b (changed)".
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

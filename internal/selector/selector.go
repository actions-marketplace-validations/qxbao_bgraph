// Package selector ghép mọi thứ thành một Selection (mục 4.3): tập thay đổi → phân loại → đồ thị hợp mốc/hiện tại →
// đi ngược → đơn vị test → nới. Mọi nhánh "không biết" trả nới, không bao giờ trả rỗng.
package selector

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"bgraph/internal/classify"
	"bgraph/internal/eco"
	"bgraph/internal/glob"
	"bgraph/internal/graph"
	"bgraph/internal/model"
	"bgraph/internal/rules"
	"bgraph/internal/tree"
)

// DefaultMaxChanged là ngưỡng diff quá lớn mặc định (mục 4.3 bước 1).
const DefaultMaxChanged = 5000

// Changed là một tệp đổi giữa mốc và cây hiện tại. Status: 'A','M','D','T'. Mode theo git ("100644", "120000", "160000").
type Changed struct {
	Path    string
	Status  byte
	OldMode string
	NewMode string
}

// Input là đầu vào của Select.
type Input struct {
	Base, Head string
	Changed    []Changed
	BaseTree   tree.Tree // nil: không có mốc → full
	HeadTree   tree.Tree
	Ecos       []eco.Ecosystem
	Scanner    eco.Scanner
	Rules      *rules.File // nil: không có bgraph.toml
	RulesErr   error       // bgraph.toml hỏng → full
	// PreviouslyFailed: id đơn vị test hỏng ở lượt trước (bên dùng cung cấp), luôn chạy lại nếu còn tồn tại.
	PreviouslyFailed []string
	MaxChanged       int // 0 → Rules.Options.MaxChanged hoặc DefaultMaxChanged
}

type state struct {
	in   Input
	sel  model.Selection
	full bool
}

func (s *state) widen(w model.Widening) {
	s.sel.Widening = append(s.sel.Widening, w)
	if w.Tier == model.TierFull {
		s.full = true
	}
}

// Select tính phần chọn.
func Select(ctx context.Context, in Input) model.Selection {
	s := &state{in: in}
	s.sel = model.Selection{Base: in.Base, Head: in.Head, Scope: model.TierNone, Changed: []string{}}
	changed := normalize(in.Changed)
	for _, c := range changed {
		s.sel.Changed = append(s.sel.Changed, c.Path)
	}
	if len(changed) == 0 {
		return s.sel
	}
	if in.RulesErr != nil {
		return s.finishFull(model.Widening{Tier: model.TierFull, Reason: model.ReasonUserRule, Path: "bgraph.toml", Detail: in.RulesErr.Error()})
	}
	if in.BaseTree == nil {
		return s.finishFull(model.Widening{Tier: model.TierFull, Reason: model.ReasonNoBase, Detail: "không có cây mốc"})
	}
	maxChanged := in.MaxChanged
	if maxChanged <= 0 && in.Rules != nil && in.Rules.Options.MaxChanged > 0 {
		maxChanged = in.Rules.Options.MaxChanged
	}
	if maxChanged <= 0 {
		maxChanged = DefaultMaxChanged
	}
	if len(changed) > maxChanged {
		return s.finishFull(model.Widening{Tier: model.TierFull, Reason: model.ReasonDiffTooLarge,
			Detail: fmt.Sprintf("%d tệp đổi > ngưỡng %d", len(changed), maxChanged)})
	}

	// Phân tích hai cây với mọi hệ sinh thái.
	var all []*eco.Analysis
	headA := map[string]*eco.Analysis{}
	var projects []eco.Project
	var units []model.TestUnit
	for _, e := range in.Ecos {
		if err := ctx.Err(); err != nil {
			return s.finishFull(model.Widening{Tier: model.TierFull, Reason: model.ReasonInternalError, Detail: err.Error()})
		}
		ba, err := e.Analyze(ctx, in.BaseTree, in.Scanner)
		if err != nil {
			return s.finishFull(model.Widening{Tier: model.TierFull, Reason: model.ReasonInternalError, Detail: e.Name() + " (mốc): " + err.Error()})
		}
		ha, err := e.Analyze(ctx, in.HeadTree, in.Scanner)
		if err != nil {
			return s.finishFull(model.Widening{Tier: model.TierFull, Reason: model.ReasonInternalError, Detail: e.Name() + ": " + err.Error()})
		}
		all = append(all, ba, ha)
		headA[e.Name()] = ha
		projects = append(projects, ha.Projects...)
		projects = append(projects, ba.Projects...)
		units = append(units, ha.Tests...)
	}
	g := graph.Build(all...)
	for _, e := range g.BadPatterns() {
		s.widen(model.Widening{Tier: model.TierFull, Reason: model.ReasonInternalError, Path: e.From, Detail: "glob không hợp lệ: " + e.To})
	}

	var seeds []graph.Seed
	type ruleTarget struct {
		target string
		reason string
	}
	var ruleTargets []ruleTarget

	for _, c := range changed {
		p := c.Path
		if c.OldMode == "160000" || c.NewMode == "160000" {
			s.widen(model.Widening{Tier: model.TierFull, Reason: model.ReasonSubmodule, Path: p, Detail: "con trỏ submodule đổi"})
			continue
		}
		// 1. Luật người dùng khớp trước.
		skip, ruled := false, false
		if in.Rules != nil {
			for _, r := range in.Rules.Match(p) {
				switch {
				case r.AffectsAll:
					s.widen(model.Widening{Tier: model.TierFull, Reason: model.ReasonUserRule, Path: p, Detail: ruleDetail(r)})
				case r.AffectsNone:
					if g.IsNode(p) {
						s.sel.Warnings = append(s.sel.Warnings, model.Widening{Tier: model.TierNone, Reason: model.ReasonRuleRejected, Path: p,
							Detail: ruleDetail(r) + `: affects="none" bị bỏ qua vì tệp là nút đồ thị`})
					} else {
						skip = true
					}
				default:
					ruled = true
					for _, t := range r.Affects {
						ruleTargets = append(ruleTargets, ruleTarget{t, ruleDetail(r) + " khớp " + p})
					}
				}
			}
		}
		if skip {
			continue
		}
		// Luật có đích đã khai ảnh hưởng của tệp (mục 6.7: luật ghi đè bảng nới): không nới thêm; vẫn đi ngược nếu là nút.
		if ruled {
			if g.IsNode(p) {
				seeds = append(seeds, graph.Seed{Path: p, Reason: "changed"})
			}
			continue
		}
		// 2. Đầu vào toàn cục.
		if w, ok := classify.Global(p); ok {
			s.widen(w)
			continue
		}
		// 3. Manifest / cấu hình / lockfile của hệ sinh thái.
		claimed := false
		for _, e := range in.Ecos {
			ws, ok := e.ClassifyChange(p, in.BaseTree, in.HeadTree, headA[e.Name()])
			if ok {
				claimed = true
				for _, w := range ws {
					s.widen(w)
				}
			}
		}
		// 4. Nút đồ thị: đi ngược (kể cả khi đã nới vì cấu hình: phép hợp không hại).
		if g.IsNode(p) {
			seeds = append(seeds, graph.Seed{Path: p, Reason: "changed"})
			// Tệp mã/dữ liệu không ai phụ thuộc: đi ngược không tới test nào, nhưng test vẫn có thể dùng nó ngoài đồ thị
			// (đọc qua fs, chạy qua child_process). Không biết ai → nới tới dự án (mục 4.3 bước 2, 6.7).
			if r := g.Role(p); !claimed && r != model.RoleTest && r != model.RoleConfig && r != model.RoleManifest && r != model.RoleLock &&
				!g.HasDependents(p) {
				if pr, ok := eco.NearestProject(projects, p); ok {
					s.widen(model.Widening{Tier: model.TierModule, Reason: model.ReasonUnreferenced, Path: p, Scope: pr.Dir,
						Detail:  "không tệp nào trong đồ thị phụ thuộc vào tệp này; test có thể đọc/chạy nó ngoài đồ thị",
						Suggest: suggestRule(p)})
				} else {
					s.widen(model.Widening{Tier: model.TierFull, Reason: model.ReasonUnreferenced, Path: p,
						Detail: "không tệp nào phụ thuộc và ngoài mọi dự án", Suggest: suggestRule(p)})
				}
			}
			continue
		}
		if claimed || classify.Harmless(p) || classify.Doc(p) {
			continue
		}
		// 5. Tệp lạ: nới tới dự án chứa nó, ngoài mọi dự án thì full.
		if pr, ok := eco.NearestProject(projects, p); ok {
			s.widen(model.Widening{Tier: model.TierModule, Reason: model.ReasonUnknownFile, Path: p, Scope: pr.Dir,
				Detail: "tệp không thuộc hệ sinh thái nào và không ai tham chiếu", Suggest: suggestRule(p)})
		} else {
			s.widen(model.Widening{Tier: model.TierFull, Reason: model.ReasonUnknownFile, Path: p,
				Detail: "tệp ngoài mọi dự án và không ai tham chiếu", Suggest: suggestRule(p)})
		}
	}

	// Import chưa phân giải: tệp đổi nằm trong phạm vi của nó → tệp chứa import coi như bị ảnh hưởng.
	for _, u := range g.Unresolved {
		if u.ScopeTier == model.TierNone {
			continue // chỉ để báo cáo: hệ sinh thái đã mã hoá phụ thuộc bằng cạnh (glob)
		}
		hit := ""
		for _, c := range changed {
			if eco.Within(c.Path, u.ScopeDir) {
				hit = c.Path
				break
			}
		}
		if hit == "" {
			continue
		}
		w := model.Widening{Tier: u.ScopeTier, Reason: unresolvedReason(u.Class), Path: u.File, Scope: u.ScopeDir,
			Detail: fmt.Sprintf("%s:%d %q chưa phân giải; %s đổi trong phạm vi", u.File, u.Line, u.Spec, hit)}
		if u.Detail != "" {
			w.Detail += " (" + u.Detail + ")"
		}
		s.widen(w)
		seeds = append(seeds, graph.Seed{Path: u.File, Reason: string(u.Class) + " import, " + hit + " đổi trong " + scopeName(u.ScopeDir)})
	}

	if s.full {
		return s.finishFull()
	}

	reach := g.Walk(seeds)
	// Cảnh báo của cây hiện tại liên quan tới phần chọn (tệp chứa vấn đề nằm trên đường đi ngược).
	for _, e := range in.Ecos {
		if ha := headA[e.Name()]; ha != nil {
			for _, w := range ha.Warnings {
				if _, ok := reach[w.Path]; ok {
					s.sel.Warnings = append(s.sel.Warnings, w)
				}
			}
		}
	}
	sort := func(ts []model.TestUnit) {
		slices.SortFunc(ts, func(a, b model.TestUnit) int { return strings.Compare(a.ID, b.ID) })
	}
	sort(units)
	units = slices.CompactFunc(units, func(a, b model.TestUnit) bool { return a.ID == b.ID })
	chosen := map[string]*model.SelectedTest{}
	choose := func(u model.TestUnit, reason string) {
		st, ok := chosen[u.ID]
		if !ok {
			st = &model.SelectedTest{Unit: u.ID, Project: u.Project, Runner: u.Runner, Files: u.Files, Whole: u.Whole}
			chosen[u.ID] = st
		}
		if !slices.Contains(st.Reasons, reason) {
			st.Reasons = append(st.Reasons, reason)
		}
	}
	for _, u := range units {
		for _, f := range u.Files {
			if _, ok := reach[f]; ok {
				choose(u, strings.Join(reach.Chain(f), " ; "))
				break
			}
		}
	}
	// Nới theo phạm vi (module/package): mọi đơn vị test trong phạm vi.
	for _, w := range s.sel.Widening {
		if w.Tier != model.TierModule && w.Tier != model.TierPackage {
			continue
		}
		if isUnresolvedReason(w.Reason) {
			continue // đã xử lý bằng hạt giống: chỉ phụ thuộc của tệp chứa import bị ảnh hưởng
		}
		for _, u := range units {
			if unitWithin(u, w.Scope) {
				choose(u, fmt.Sprintf("nới %s (%s) do %s", scopeName(w.Scope), w.Reason, w.Path))
			}
		}
	}
	// Luật người dùng có đích.
	for _, rt := range ruleTargets {
		var pat *glob.Pattern
		if strings.ContainsAny(rt.target, "*?[{") {
			pat, _ = glob.Compile(rt.target)
		}
		for _, u := range units {
			if pat == nil && unitWithin(u, path.Clean(rt.target)) {
				choose(u, rt.reason)
				continue
			}
			if pat != nil {
				for _, f := range u.Files {
					if pat.Match(f) {
						choose(u, rt.reason)
						break
					}
				}
			}
		}
	}
	// Đơn vị "cả dự án" (không biết tập tệp test): mọi thay đổi có nghĩa đều có thể chạm một test chưa biết của dự án.
	if len(seeds) > 0 || len(s.sel.Widening) > 0 {
		for _, u := range units {
			if u.Whole {
				choose(u, model.ReasonConfigIsCode+": không biết tập tệp test của dự án "+scopeName(u.Project)+" (mẫu test do code tính)")
			}
		}
	}
	// Test từng hỏng.
	for _, id := range in.PreviouslyFailed {
		for _, u := range units {
			if u.ID == id {
				choose(u, "previously_failed")
				s.sel.AlwaysRun = append(s.sel.AlwaysRun, id)
			}
		}
	}
	// Test mới.
	for _, c := range changed {
		if c.Status == 'A' && g.Role(c.Path) == model.RoleTest {
			s.sel.AlwaysRun = append(s.sel.AlwaysRun, c.Path)
		}
	}
	slices.Sort(s.sel.AlwaysRun)
	s.sel.AlwaysRun = slices.Compact(s.sel.AlwaysRun)

	ids := make([]string, 0, len(chosen))
	for id := range chosen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		s.sel.Tests = append(s.sel.Tests, *chosen[id])
	}
	s.sel.Scope = model.TierNone
	if len(s.sel.Tests) > 0 {
		s.sel.Scope = model.TierGraph
		for _, w := range s.sel.Widening {
			// Import chưa phân giải được xử lý bằng hạt giống (tệp chứa import coi như đổi, rồi đi ngược): phần chọn vẫn tính
			// bằng đồ thị, nên không nâng scope; lần nới vẫn được liệt kê trong Widening để giải thích.
			if isUnresolvedReason(w.Reason) && w.Tier != model.TierFull {
				continue
			}
			s.sel.Scope = model.Max(s.sel.Scope, w.Tier)
		}
	}
	s.sel.IndexStats = map[string]int{"units": len(units), "seeds": len(seeds), "reached": len(reach), "unresolved": len(g.Unresolved)}
	return s.sel
}

func (s *state) finishFull(ws ...model.Widening) model.Selection {
	for _, w := range ws {
		s.widen(w)
	}
	s.sel.Scope = model.TierFull
	s.sel.Tests = nil
	return s.sel
}

func normalize(in []Changed) []Changed {
	out := make([]Changed, 0, len(in))
	for _, c := range in {
		c.Path = path.Clean(strings.ReplaceAll(c.Path, "\\", "/"))
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Changed) int { return strings.Compare(a.Path, b.Path) })
	return slices.CompactFunc(out, func(a, b Changed) bool { return a.Path == b.Path })
}

func unitWithin(u model.TestUnit, dir string) bool {
	if eco.Within(u.Project, dir) {
		return true
	}
	for _, f := range u.Files {
		if eco.Within(f, dir) {
			return true
		}
	}
	return false
}

func ruleDetail(r rules.Rule) string {
	d := fmt.Sprintf("bgraph.toml rule #%d", r.Index+1)
	if r.Reason != "" {
		d += " (" + r.Reason + ")"
	}
	return d
}

func suggestRule(p string) string {
	return fmt.Sprintf("[[rule]]\npaths = [%q]\naffects = [\"<dự án hoặc glob test>\"]  # hoặc \"none\" nếu chắc chắn vô hại", p)
}

func scopeName(dir string) string {
	if dir == "" || dir == "." {
		return "repo"
	}
	return dir
}

func unresolvedReason(c model.UnresolvedClass) string {
	switch c {
	case model.UnresolvedDynamic:
		return model.ReasonDynamicImport
	case model.UnresolvedAmbiguous:
		return model.ReasonAmbiguousImport
	case model.UnresolvedUncertainScan:
		return model.ReasonUncertainScan
	}
	return model.ReasonUnknownImport
}

func isUnresolvedReason(r string) bool {
	switch r {
	case model.ReasonDynamicImport, model.ReasonAmbiguousImport, model.ReasonUncertainScan, model.ReasonUnknownImport:
		return true
	}
	return false
}

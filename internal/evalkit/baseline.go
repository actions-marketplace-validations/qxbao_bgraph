package evalkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"bgraph"
	"bgraph/internal/gitx"
	"bgraph/internal/model"
	"bgraph/internal/scancache"
)

// Đối chứng giai đoạn 0b: chấm các công cụ đồ thị ngoài (codegraph, graphify, gitnexus) bằng đúng oracle T1.
// Hai bước: BaselinePrep dựng P/C (index tạm, không checkout), oracle, danh sách đơn vị test của bgraph và phần chọn của bgraph;
// phía công cụ ngoài (script Python) chỉ cần P, C và tệp đổi. BaselineScore đọc bao đóng tệp của từng công cụ và chấm.

// BUnit là một đơn vị test của bgraph (phân tích cây C), mẫu số chung cho mọi công cụ.
type BUnit struct {
	ID      string   `json:"id"`
	Project string   `json:"project"`
	Files   []string `json:"files,omitempty"`
	Whole   bool     `json:"whole,omitempty"`
}

// BTask là một task T1 đã dựng sẵn cho đối chứng.
type BTask struct {
	ID       string   `json:"id"`
	Repo     string   `json:"repo"`
	Role     string   `json:"role"`
	Mirror   string   `json:"mirror"`
	Base     string   `json:"base"`
	P        string   `json:"p"`
	C        string   `json:"c"`
	Changed  []string `json:"changed"` // tệp đổi bởi patch vàng (P→C), gồm tệp xoá/thêm
	Required []string `json:"required"`
	Units    []BUnit  `json:"units"`

	// bgraph trên cùng P→C
	BgStatus   string   `json:"bg_status"` // ok | full | violation
	BgScope    string   `json:"bg_scope"`
	BgSelected int      `json:"bg_selected"`
	BgUnits    int      `json:"bg_units"` // IndexStats["units"]
	BgMissing  []string `json:"bg_missing,omitempty"`
	BgSel      []string `json:"bg_sel_units,omitempty"`
	Skip       string   `json:"skip,omitempty"`
}

// BaselinePrep dựng danh sách BTask cho mẫu tasks và ghi tasks.json vào out. Dùng lại mirror của work/repos.
func BaselinePrep(ctx context.Context, cfg T1Config) ([]BTask, error) {
	if cfg.Jobs <= 0 {
		cfg.Jobs = 3
	}
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return nil, err
	}
	byRepo := map[string][]Task{}
	var repos []string
	for _, t := range cfg.Tasks {
		if _, ok := byRepo[t.Repo]; !ok {
			repos = append(repos, t.Repo)
		}
		byRepo[t.Repo] = append(byRepo[t.Repo], t)
	}
	var mu sync.Mutex
	var out []BTask
	sem := make(chan struct{}, cfg.Jobs)
	var wg sync.WaitGroup
	for _, repo := range repos {
		wg.Add(1)
		sem <- struct{}{}
		go func(repo string) {
			defer wg.Done()
			defer func() { <-sem }()
			rr := &repoRunner{cfg: &cfg, repo: repo}
			for _, t := range byRepo[repo] {
				if ctx.Err() != nil {
					return
				}
				bt := rr.prepOne(ctx, t)
				cfg.logf("prep %-40s %-10s P=%.8s C=%.8s changed=%d req=%d units=%d bg=%s/%s %d/%d %s", bt.ID, bt.Skip, bt.P, bt.C, len(bt.Changed), len(bt.Required), len(bt.Units), bt.BgStatus, bt.BgScope, bt.BgSelected, bt.BgUnits, "")
				mu.Lock()
				out = append(out, bt)
				mu.Unlock()
			}
		}(repo)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	b, _ := json.MarshalIndent(out, "", " ")
	if err := os.WriteFile(filepath.Join(cfg.Out, "tasks.json"), b, 0o644); err != nil {
		return out, err
	}
	return out, ctx.Err()
}

func (rr *repoRunner) prepOne(ctx context.Context, t Task) (bt BTask) {
	role, _ := rr.cfg.Manifest.Role(t.Repo)
	bt = BTask{ID: t.InstanceID, Repo: t.Repo, Role: role, Base: t.BaseCommit}
	defer func() {
		if p := recover(); p != nil {
			bt.Skip = fmt.Sprint("panic: ", p)
		}
	}()
	ids, err := ParseF2P(t.F2P)
	if err != nil || len(ids) == 0 {
		bt.Skip = "f2p"
		return
	}
	if err := rr.ensureMirror(ctx); err != nil {
		bt.Skip = "clone: " + err.Error()
		return
	}
	bt.Mirror = rr.mirror
	if _, err := git(ctx, rr.mirror, "cat-file", "-e", t.BaseCommit+"^{commit}"); err != nil {
		if _, err := git(ctx, rr.mirror, "fetch", "--filter=blob:none", "origin", t.BaseCommit); err != nil {
			bt.Skip = "nobase"
			return
		}
	}
	rr.seq++
	tag := fmt.Sprintf("bl_%s_%d", repoDirName(rr.repo), rr.seq)
	idir := filepath.Join(rr.cfg.Work, "idx")
	os.MkdirAll(idir, 0o755)
	ib := newIndexBuilder(rr.mirror, filepath.Join(idir, tag+".index"), t.BaseCommit)
	defer ib.Close()
	p, err := ib.Apply(ctx, t.TestPatch, "eval: test patch")
	if err != nil {
		bt.Skip = "apply(index): " + err.Error()
		return
	}
	bt.P = p
	dir := rr.mirror
	ns, err := git(ctx, dir, "diff", "--name-status", "--no-renames", "-z", t.BaseCommit, p)
	if err != nil {
		bt.Skip = "apply"
		return
	}
	files := map[string]string{}
	parts := strings.Split(ns, "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		if parts[i] == "D" {
			continue
		}
		if content, err := git(ctx, dir, "show", p+":"+parts[i+1]); err == nil {
			files[parts[i+1]] = content
		}
	}
	existsInP := func(q string) bool { _, err := git(ctx, dir, "cat-file", "-e", p+":"+q); return err == nil }
	req, _, _ := MapF2P(ids, files, existsInP)
	bt.Required = req
	if len(req) == 0 {
		bt.Skip = "unmapped"
		return
	}
	c, err := ib.Apply(ctx, t.Patch, "eval: gold patch")
	if err != nil {
		bt.Skip = "apply(index): patch: " + err.Error()
		return
	}
	bt.C = c
	nm, err := git(ctx, dir, "diff", "--name-only", "--no-renames", "-z", p, c)
	if err != nil {
		bt.Skip = "diff"
		return
	}
	for _, f := range strings.Split(nm, "\x00") {
		if f != "" {
			bt.Changed = append(bt.Changed, f)
		}
	}
	// Đơn vị test: phân tích cây C như selector (mọi hệ sinh thái, quy tắc rỗng).
	units, err := unitsOf(ctx, dir, c)
	if err != nil {
		bt.Skip = "units: " + err.Error()
		return
	}
	bt.Units = units
	// bgraph trên cùng P→C.
	state := filepath.Join(rr.cfg.Work, "state", repoDirName(rr.repo))
	ix, err := bgraph.Open(ctx, dir, bgraph.Options{StateDir: state})
	if err != nil {
		bt.BgStatus = "error:open"
		return
	}
	defer ix.Close()
	sel, err := ix.Select(ctx, bgraph.Change{Base: p, Head: c})
	if err != nil {
		bt.BgStatus = "error:select"
		return
	}
	bt.BgScope = string(sel.Scope)
	bt.BgSelected = len(sel.Tests)
	bt.BgUnits = sel.IndexStats["units"]
	su := make([]SelUnit, 0, len(sel.Tests))
	for _, st := range sel.Tests {
		su = append(su, SelUnit{Unit: st.Unit, Files: st.Files, Whole: st.Whole, WholeProject: st.Project})
		bt.BgSel = append(bt.BgSel, st.Unit)
	}
	bt.BgMissing, _ = CheckRequired(req, su)
	switch {
	case sel.Scope == model.TierFull:
		bt.BgStatus = "full"
	case len(bt.BgMissing) == 0:
		bt.BgStatus = "ok"
	default:
		bt.BgStatus = "violation"
	}
	return
}

func unitsOf(ctx context.Context, root, commit string) ([]BUnit, error) {
	r, err := gitx.Open(ctx, root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	ct, err := r.CommitTree(ctx, commit)
	if err != nil {
		return nil, err
	}
	var out []BUnit
	seen := map[string]bool{}
	for _, e := range bgraph.Ecosystems(nil) {
		a, err := e.Analyze(ctx, ct, scancache.New())
		if err != nil {
			return nil, err
		}
		for _, u := range a.Tests {
			if seen[u.ID] {
				continue
			}
			seen[u.ID] = true
			out = append(out, BUnit{ID: u.ID, Project: u.Project, Files: u.Files, Whole: u.Whole})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---- Chấm ----

// ToolRun là kết quả của một công cụ ngoài trên một task (do script Python ghi).
type ToolRun struct {
	Status   string   `json:"status"` // ok | timeout | error
	Detail   string   `json:"detail,omitempty"`
	IndexP   float64  `json:"index_p_s"`
	IndexC   float64  `json:"index_c_s"`
	EdgesP   int      `json:"edges_p"`
	EdgesC   int      `json:"edges_c"`
	Closure  []string `json:"closure"` // bao đóng ngược (hợp đồ thị P và C) từ tệp đổi
	NodesAll int      `json:"nodes_all,omitempty"`
}

// BScore là kết quả chấm một (task, công cụ).
type BScore struct {
	ID       string   `json:"id"`
	Repo     string   `json:"repo"`
	Tool     string   `json:"tool"`
	Status   string   `json:"status"` // ok | violation | timeout | error | missing
	Selected int      `json:"selected"`
	Units    int      `json:"units"`
	Whole    int      `json:"whole_units_selected"`
	Missing  []string `json:"missing,omitempty"`
	IndexS   float64  `json:"index_s"` // tổng P + C
	Detail   string   `json:"detail,omitempty"`
}

// ScoreTool chấm bao đóng của một công cụ: đơn vị được chọn nếu có tệp nào của nó nằm trong bao đóng; đơn vị Whole được chọn nếu có tệp
// nào của bao đóng nằm trong thư mục dự án của nó.
func ScoreTool(bt BTask, tool string, tr *ToolRun) BScore {
	sc := BScore{ID: bt.ID, Repo: bt.Repo, Tool: tool, Units: len(bt.Units)}
	if tr == nil {
		sc.Status = "missing"
		return sc
	}
	sc.IndexS = tr.IndexP + tr.IndexC
	sc.Detail = tr.Detail
	if tr.Status != "ok" {
		sc.Status = tr.Status
		return sc
	}
	in := map[string]bool{}
	for _, f := range tr.Closure {
		in[f] = true
	}
	var su []SelUnit
	for _, u := range bt.Units {
		hit := false
		if u.Whole {
			for _, f := range tr.Closure {
				if u.Project == "." || u.Project == "" || under(f, u.Project) {
					hit = true
					break
				}
			}
			if hit {
				sc.Whole++
			}
		} else {
			for _, f := range u.Files {
				if in[f] {
					hit = true
					break
				}
			}
		}
		if hit {
			su = append(su, SelUnit{Unit: u.ID, Files: u.Files, Whole: u.Whole, WholeProject: u.Project})
		}
	}
	sc.Selected = len(su)
	sc.Missing, _ = CheckRequired(bt.Required, su)
	if len(sc.Missing) == 0 {
		sc.Status = "ok"
	} else {
		sc.Status = "violation"
	}
	return sc
}

// LoadBTasks đọc tasks.json.
func LoadBTasks(path string) ([]BTask, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ts []BTask
	return ts, json.Unmarshal(b, &ts)
}

// BaselineScoreAll đọc <dir>/runs/<tool>/<id>.json cho mỗi công cụ, chấm và ghi scores.json.
func BaselineScoreAll(dir string, tasks []BTask, tools []string) ([]BScore, error) {
	var all []BScore
	for _, bt := range tasks {
		if bt.Skip != "" || bt.P == "" || bt.C == "" {
			continue
		}
		for _, tool := range tools {
			var tr *ToolRun
			b, err := os.ReadFile(filepath.Join(dir, "runs", tool, bt.ID+".json"))
			if err == nil {
				var x ToolRun
				if json.Unmarshal(b, &x) == nil {
					tr = &x
				}
			}
			all = append(all, ScoreTool(bt, tool, tr))
		}
	}
	b, _ := json.MarshalIndent(all, "", " ")
	return all, os.WriteFile(filepath.Join(dir, "scores.json"), b, 0o644)
}

// BaselineSummary tổng hợp theo công cụ: số task, ok, vi phạm, timeout, trung vị/p90 selected/units (trên task ok), trung vị thời gian chỉ mục.
type BaselineSummary struct {
	Tool       string
	Tasks      int
	OK         int
	Violations int
	Timeouts   int
	Errors     int
	Missing    int
	MedRatio   float64
	P90Ratio   float64
	MedIndexS  float64
	MedRatioV  float64 // trung vị trên mọi task có kết quả (ok + vi phạm)
}

func SummarizeScores(ss []BScore, tool string) BaselineSummary {
	s := BaselineSummary{Tool: tool}
	var ratios, ratiosAll, idx []float64
	for _, x := range ss {
		if x.Tool != tool {
			continue
		}
		s.Tasks++
		switch x.Status {
		case "ok":
			s.OK++
		case "violation":
			s.Violations++
		case "timeout":
			s.Timeouts++
		case "missing":
			s.Missing++
		default:
			s.Errors++
		}
		if x.Status == "ok" || x.Status == "violation" {
			if x.Units > 0 {
				r := float64(x.Selected) / float64(x.Units)
				ratiosAll = append(ratiosAll, r)
				if x.Status == "ok" {
					ratios = append(ratios, r)
				}
			}
			idx = append(idx, x.IndexS)
		}
	}
	s.MedRatio, s.P90Ratio, s.MedRatioV = Percentile(ratios, 50), Percentile(ratios, 90), Percentile(ratiosAll, 50)
	s.MedIndexS = Percentile(idx, 50)
	return s
}

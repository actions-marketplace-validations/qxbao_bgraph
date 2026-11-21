package evalkit

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// LoadIDList đọc danh sách instance_id (mỗi dòng một id; bỏ dòng trống và chú thích bắt đầu bằng #).
func LoadIDList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if i := strings.Index(l, "#"); i >= 0 {
			l = strings.TrimSpace(l[:i])
		}
		if l != "" {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

// SampleQuick chọn tất định mẫu phân tầng theo repo: mọi task trong regress (tìm trong all) cộng tối đa perRepo task khác mỗi repo
// (từ capped = SelectTasks(all, cap), xếp theo sha256(instance_id)). Kết quả xếp theo repo rồi sha256(id).
func SampleQuick(all, capped []Task, regress []string, perRepo int) (out []Task, missing []string) {
	want := map[string]bool{}
	for _, id := range regress {
		want[id] = true
	}
	picked := map[string]bool{}
	for _, t := range all {
		if want[t.InstanceID] && !picked[t.InstanceID] {
			picked[t.InstanceID] = true
			out = append(out, t)
		}
	}
	for _, id := range regress {
		if !picked[id] {
			missing = append(missing, id)
		}
	}
	byRepo := map[string][]Task{}
	for _, t := range capped {
		if !picked[t.InstanceID] {
			byRepo[t.Repo] = append(byRepo[t.Repo], t)
		}
	}
	for _, ts := range byRepo {
		sort.Slice(ts, func(i, j int) bool { return sha(ts[i].InstanceID) < sha(ts[j].InstanceID) })
		if perRepo > 0 && len(ts) > perRepo {
			ts = ts[:perRepo]
		}
		out = append(out, ts...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return sha(out[i].InstanceID) < sha(out[j].InstanceID)
	})
	return out, missing
}

// QuickConfig cấu hình bộ đánh giá nhanh.
type QuickConfig struct {
	Work, Out string
	Tasks     []Task // mẫu T1 đã chọn
	Manifest  Manifest
	Replay    []string // repo replay
	Commits   int
	Jobs      int
	Checkout  bool
	Serial    bool   // chạy T1 rồi replay thay vì song song
	T2Dir     string // thư mục gốc T2; có oracle.jsonl thì kiểm lại T2 trên oracle cache (rỗng = bỏ qua)
	T2        T2Manifest
	Log       func(format string, a ...any)
}

// QuickResult là kết quả gộp.
type QuickResult struct {
	T1         []T1Result
	Replay     []RepoReplay
	T1Wall     time.Duration
	ReplayWall time.Duration
	Wall       time.Duration
	Violations int // vi phạm T1 + T2 + bất biến replay
	T2         *T2Result
	T2Wall     time.Duration
}

// RunQuick chạy T1 (mẫu) và replay, ghi quick.md cùng t1/ và replay/ trong Out.
func RunQuick(ctx context.Context, cfg QuickConfig) (QuickResult, error) {
	start := time.Now()
	var qr QuickResult
	t1Out, rpOut := filepath.Join(cfg.Out, "t1"), filepath.Join(cfg.Out, "replay")
	var t1Err, rpErr error
	runT1 := func() {
		t := time.Now()
		qr.T1, t1Err = RunT1(ctx, T1Config{Work: cfg.Work, Out: t1Out, Tasks: cfg.Tasks, Manifest: cfg.Manifest, Jobs: cfg.Jobs, Checkout: cfg.Checkout, Log: cfg.Log})
		qr.T1Wall = time.Since(t)
		if err := WriteT1(t1Out, qr.T1); err != nil && t1Err == nil {
			t1Err = err
		}
	}
	runRp := func() {
		if len(cfg.Replay) == 0 {
			return
		}
		t := time.Now()
		qr.Replay, rpErr = RunReplay(ctx, ReplayConfig{Work: cfg.Work, Out: rpOut, Repos: cfg.Replay, Commits: cfg.Commits, Jobs: cfg.Jobs, Manifest: cfg.Manifest, Log: cfg.Log})
		qr.ReplayWall = time.Since(t)
		if err := WriteReplay(rpOut, qr.Replay); err != nil && rpErr == nil {
			rpErr = err
		}
	}
	var t2Err error
	runT2 := func() {
		if cfg.T2Dir == "" || len(cfg.T2.Repos) == 0 {
			return
		}
		if _, err := os.Stat(filepath.Join(cfg.T2Dir, "oracle.jsonl")); err != nil {
			return
		}
		t := time.Now()
		var r T2Result
		r, t2Err = RunT2(ctx, T2Config{Dir: cfg.T2Dir, Out: filepath.Join(cfg.Out, "t2"), Manifest: cfg.T2, Log: cfg.Log})
		qr.T2, qr.T2Wall = &r, time.Since(t)
		if err := WriteT2(filepath.Join(cfg.Out, "t2"), r); err != nil && t2Err == nil {
			t2Err = err
		}
	}
	if cfg.Serial {
		runT1()
		runRp()
		runT2()
	} else {
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); runT1() }()
		go func() { defer wg.Done(); runRp() }()
		go func() { defer wg.Done(); runT2() }()
		wg.Wait()
	}
	qr.Wall = time.Since(start)
	for _, r := range qr.T1 {
		if r.Status == "violation" {
			qr.Violations++
		}
	}
	for _, r := range qr.Replay {
		qr.Violations += len(r.Violations)
	}
	if qr.T2 != nil {
		qr.Violations += qr.T2.Violations
	}
	err := t1Err
	if err == nil {
		err = rpErr
	}
	if err == nil {
		err = t2Err
	}
	if werr := os.WriteFile(filepath.Join(cfg.Out, "quick.md"), []byte(QuickMarkdown(qr)), 0o644); werr != nil && err == nil {
		err = werr
	}
	return qr, err
}

// QuickMarkdown dựng báo cáo gộp: vi phạm trước, rồi trung vị chọn lọc theo repo và thời gian.
func QuickMarkdown(q QuickResult) string {
	var b strings.Builder
	verdict := "PASS (no violations)"
	if q.Violations > 0 {
		verdict = fmt.Sprintf("FAIL: %d violations", q.Violations)
	}
	fmt.Fprintf(&b, "# bgraph-eval quick — %s\n\nTime: total %s (T1 %s, replay %s)\n\n", verdict, q.Wall.Round(time.Second), q.T1Wall.Round(time.Second), q.ReplayWall.Round(time.Second))

	b.WriteString("## Violations\n\n")
	n := 0
	for _, r := range q.T1 {
		if r.Status == "violation" {
			n++
			fmt.Fprintf(&b, "- T1 `%s` (%s) scope=%s missing %v; widenings %v\n", r.ID, r.Role, r.Scope, r.Missing, r.Widenings)
		}
	}
	for _, r := range q.Replay {
		for _, v := range r.Violations {
			n++
			c := v.Commit
			if len(c) > 10 {
				c = c[:10]
			}
			fmt.Fprintf(&b, "- replay %s %s `%s`: %s\n", r.Repo, v.Inv, c, v.Detail)
		}
	}
	if q.T2 != nil {
		for _, r := range q.T2.Repos {
			for _, c := range r.Violations {
				n++
				fmt.Fprintf(&b, "- T2 %s %s %s:%d (%s) missing %v scope=%s widenings %v\n", r.Repo.Name, c.Rec.Mutant.ID, c.Rec.Mutant.File, c.Rec.Mutant.Line, c.Rec.Mutant.Op, c.Missed, c.Scope, c.Widenings)
			}
		}
	}
	if n == 0 {
		b.WriteString("(none)\n")
	}
	var errs []string
	for _, r := range q.T1 {
		if strings.HasPrefix(r.Status, "error:") {
			errs = append(errs, fmt.Sprintf("T1 `%s`: %s %s", r.ID, r.Status, r.Detail))
		}
		if r.NonDeterm {
			errs = append(errs, fmt.Sprintf("T1 `%s`: NON-DETERMINISTIC", r.ID))
		}
	}
	for _, r := range q.Replay {
		if r.Error != "" {
			errs = append(errs, fmt.Sprintf("replay %s: %s", r.Repo, r.Error))
		}
	}
	if len(errs) > 0 {
		b.WriteString("\n## Errors / warnings\n\n")
		for _, e := range errs {
			fmt.Fprintf(&b, "- %s\n", e)
		}
	}

	b.WriteString("\n## T1 by group\n\n| group | tried | skipped | errors | ok | full | violations | median sel/units | p90 |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, role := range []string{"dev", "holdout", ""} {
		s := Summarize(q.T1, role)
		name := s.Role
		if role == "" {
			name = "all"
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %d | %d | %d | %d | %.3f | %.3f |\n", name, s.Attempted, skipStr(s.Skipped), s.Errors, s.OK, s.Full, s.Violations, s.MedianRatio, s.P90Ratio)
	}

	b.WriteString("\n## T1 by repo\n\n| repo | role | tasks | ok | full | violations | skipped | median sel/units | task ms median | p95 |\n|---|---|---|---|---|---|---|---|---|---|\n")
	byRepo := map[string][]T1Result{}
	var names []string
	for _, r := range q.T1 {
		if _, ok := byRepo[r.Repo]; !ok {
			names = append(names, r.Repo)
		}
		byRepo[r.Repo] = append(byRepo[r.Repo], r)
	}
	sort.Strings(names)
	paths := map[string]int{}
	var allMs []float64
	for _, nm := range names {
		var ok, full, vio, sk int
		var ratios, ms []float64
		for _, r := range byRepo[nm] {
			switch {
			case r.Status == "ok":
				ok++
			case r.Status == "full":
				full++
			case r.Status == "violation":
				vio++
			default:
				sk++
			}
			if r.Status == "ok" && r.Units > 0 {
				ratios = append(ratios, float64(r.Selected)/float64(r.Units))
			}
			if r.TaskMs > 0 {
				ms = append(ms, float64(r.TaskMs))
				allMs = append(allMs, float64(r.TaskMs))
			}
			if r.Path != "" {
				paths[r.Path]++
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d | %d | %.3f | %.0f | %.0f |\n", nm, byRepo[nm][0].Role, len(byRepo[nm]), ok, full, vio, sk, Percentile(ratios, 50), Percentile(ms, 50), Percentile(ms, 95))
	}
	fmt.Fprintf(&b, "\nTime per T1 task (build P/C + oracle + 2 Select): median %.0f ms, p95 %.0f ms. Build path: %s\n", Percentile(allMs, 50), Percentile(allMs, 95), mapStr(paths))
	var fbs []string
	for _, r := range q.T1 {
		if r.Fallback != "" {
			fbs = append(fbs, fmt.Sprintf("`%s` (%.80s)", r.ID, r.Fallback))
		}
	}
	if len(fbs) > 0 {
		fmt.Fprintf(&b, "\nTasks that fell back to worktree: %s\n", strings.Join(fbs, "; "))
	}

	if q.T2 != nil {
		fmt.Fprintf(&b, "\n## T2 (re-check on cached oracle, %s)\n\n| repo | mutants | killed | survived | full widening | violations | native ⊄ bgraph | median sel/units bgraph | native |\n|---|---|---|---|---|---|---|---|---|\n", q.T2Wall.Round(time.Millisecond))
		for _, r := range q.T2.Repos {
			fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d | %.3f | %.3f |\n", r.Repo.Name, len(r.Checks), r.Killed, r.Survived, r.Full, len(r.Violations), len(r.NativeNotSubset), r.MedBgraph, r.MedNative)
			if r.Error != "" {
				fmt.Fprintf(&b, "| %s | error: %s |||||||\n", r.Repo.Name, r.Error)
			}
		}
	}

	if len(q.Replay) > 0 {
		b.WriteString("\n## Replay by repo\n\n| repo | role | commits | ordinary | full% (ordinary) | median sel/units | p90 | ms median | ms p95 | errors | violations |\n|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, r := range q.Replay {
			s := foldReplay(ReplaySummary{Repo: r.Repo, Role: r.Role, FullReasons: map[string]int{}, Viol: map[string]int{}}, []RepoReplay{r})
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %.0f%% | %.3f | %.3f | %.0f | %.0f | %d | %s |\n", s.Repo, s.Role, s.Commits, s.Ordinary, s.FullRate*100, s.MedRatio, s.P90Ratio, s.MedMs, s.P95Ms, s.Errors, mapStr(s.Viol))
		}
	}
	return b.String()
}

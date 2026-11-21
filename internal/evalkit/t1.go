package evalkit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/eco/node"
	"github.com/qxbao/bgraph/internal/gitx"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
)

// T1Config configures a T1 run.
type T1Config struct {
	Work       string // working directory: repos/, wt/, patches/, state/
	Out        string // results directory
	Tasks      []Task
	Manifest   Manifest
	Jobs       int
	MaxPerRepo int
	Resume     bool
	Checkout   bool // true: build P/C with a worktree as before (default: temporary index, no checkout)
	Timeout    time.Duration
	Log        func(format string, a ...any)
}

// T1Result is the result of one task.
type T1Result struct {
	ID       string     `json:"instance_id"`
	Repo     string     `json:"repo"`
	Role     string     `json:"role"`
	Source   string     `json:"source,omitempty"`
	Base     string     `json:"base_commit"`
	P        string     `json:"p_commit,omitempty"`
	Status   string     `json:"status"` // ok | full | violation | skipped:<reason> | error:<reason>
	Detail   string     `json:"detail,omitempty"`
	F2P      int        `json:"f2p_count"`
	Required []string   `json:"required,omitempty"`
	Mapping  []MapEntry `json:"mapping,omitempty"`
	Unmapped []string   `json:"unmapped_ids,omitempty"`
	// FixtureReqs: requirements handled by the fixture rule (see CheckRequired), with the satisfying unit.
	FixtureReqs []FixtureReq `json:"fixture_reqs,omitempty"`

	Scope     string           `json:"scope,omitempty"`
	Widenings map[string]int   `json:"widenings,omitempty"`     // "tier:reason" → count
	WideList  []model.Widening `json:"widening_list,omitempty"` // complete on violation, at most 12 otherwise
	Selected  int              `json:"selected"`
	Units     int              `json:"units"`
	Changed   []string         `json:"changed,omitempty"`
	Missing   []string         `json:"missing,omitempty"`
	Ms        int64            `json:"ms"`
	TaskMs    int64            `json:"task_ms,omitempty"` // total task time (building P/C + oracle + Select x2)
	Path      string           `json:"path,omitempty"`    // index | worktree: how P/C was built
	Fallback  string           `json:"fallback,omitempty"`
	NonDeterm bool             `json:"nondeterministic,omitempty"`
	Diag      []FileDiag       `json:"diag,omitempty"`
}

// FileDiag is a diagnosis for a missing required file (for human readers only; does not affect the verdict).
type FileDiag struct {
	File           string            `json:"file"`
	Role           string            `json:"role"`
	InUnit         bool              `json:"in_unit"`
	Units          []string          `json:"units,omitempty"`
	Imports        []string          `json:"imports,omitempty"`
	Unresolved     []string          `json:"unresolved,omitempty"`
	ReachSize      int               `json:"forward_reach_size"`
	ReachedChanged []string          `json:"reached_changed,omitempty"`
	ChangedRoles   map[string]string `json:"changed_roles,omitempty"`
}

func (c *T1Config) logf(f string, a ...any) {
	if c.Log != nil {
		c.Log(f, a...)
	}
}

// SelectTasks deterministically picks at most MaxPerRepo tasks per repo (ordered by sha256(instance_id)), dropping duplicates (repo, base, patch).
func SelectTasks(in []Task, maxPer int) []Task {
	seen := map[string]bool{}
	byRepo := map[string][]Task{}
	for _, t := range in {
		k := strings.ToLower(t.Repo) + "|" + t.BaseCommit + "|" + sha(t.Patch)
		if seen[k] {
			continue
		}
		seen[k] = true
		byRepo[t.Repo] = append(byRepo[t.Repo], t)
	}
	var repos []string
	for r := range byRepo {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	var out []Task
	for _, r := range repos {
		ts := byRepo[r]
		sort.Slice(ts, func(i, j int) bool { return sha(ts[i].InstanceID) < sha(ts[j].InstanceID) })
		if maxPer > 0 && len(ts) > maxPer {
			ts = ts[:maxPer]
		}
		out = append(out, ts...)
	}
	return out
}

// RunT1 runs every task; each repo sequentially (sharing the state directory), repos in parallel up to Jobs.
func RunT1(ctx context.Context, cfg T1Config) ([]T1Result, error) {
	if cfg.Jobs <= 0 {
		cfg.Jobs = 4
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 20 * time.Minute
	}
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return nil, err
	}
	jl := filepath.Join(cfg.Out, "t1.jsonl")
	done := map[string]T1Result{}
	if cfg.Resume {
		if f, err := os.Open(jl); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1<<20), 64<<20)
			for sc.Scan() {
				var r T1Result
				if json.Unmarshal(sc.Bytes(), &r) == nil && !strings.HasPrefix(r.Status, "error:") {
					done[r.ID] = r
				}
			}
			f.Close()
		}
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if !cfg.Resume {
		flag = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	out, err := os.OpenFile(jl, flag, 0o644)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	var mu sync.Mutex
	var results []T1Result
	for _, r := range done {
		results = append(results, r)
	}
	emit := func(r T1Result) {
		mu.Lock()
		defer mu.Unlock()
		b, _ := json.Marshal(r)
		out.Write(append(b, '\n'))
		results = append(results, r)
		cfg.logf("%-40s %-22s scope=%-7s sel=%d/%d %dms %s", r.ID, r.Status, r.Scope, r.Selected, r.Units, r.Ms, r.Detail)
	}

	byRepo := map[string][]Task{}
	var repos []string
	for _, t := range cfg.Tasks {
		if _, ok := done[t.InstanceID]; ok {
			continue
		}
		if _, ok := byRepo[t.Repo]; !ok {
			repos = append(repos, t.Repo)
		}
		byRepo[t.Repo] = append(byRepo[t.Repo], t)
	}
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
				tctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
				res := rr.run(tctx, t)
				cancel()
				emit(res)
			}
		}(repo)
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })
	return results, ctx.Err()
}

type repoRunner struct {
	cfg    *T1Config
	repo   string
	mirror string
	mErr   error
	ready  bool
	seq    int
}

func repoDirName(repo string) string { return strings.ReplaceAll(repo, "/", "__") }

func (rr *repoRunner) ensureMirror(ctx context.Context) error {
	if rr.ready {
		return rr.mErr
	}
	rr.ready = true
	rr.mirror = filepath.Join(rr.cfg.Work, "repos", repoDirName(rr.repo))
	if _, err := os.Stat(filepath.Join(rr.mirror, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(rr.mirror), 0o755); err != nil {
		rr.mErr = err
		return err
	}
	os.RemoveAll(rr.mirror)
	_, err := git(ctx, "", "clone", "--filter=blob:none", "--no-checkout", "https://github.com/"+rr.repo+".git", rr.mirror)
	if err != nil {
		os.RemoveAll(rr.mirror)
		rr.mErr = err
	}
	return rr.mErr
}

// run runs one task. By default builds P/C with a temporary index (no checkout); a `git apply --cached` failure or the Checkout flag → the old worktree path.
func (rr *repoRunner) run(ctx context.Context, t Task) T1Result {
	t0 := time.Now()
	var res T1Result
	if rr.cfg.Checkout {
		res, _ = rr.runMode(ctx, t, true)
		res.Path = "worktree"
	} else {
		var fb bool
		res, fb = rr.runMode(ctx, t, false)
		res.Path = "index"
		if fb {
			fbWhy := res.Detail
			res, _ = rr.runMode(ctx, t, true)
			res.Path, res.Fallback = "worktree", fbWhy
		}
	}
	res.TaskMs = time.Since(t0).Milliseconds()
	return res
}

// runMode runs one task via the index path (useWT=false) or the worktree path. fallback=true (index path only): the patch cannot be applied with the index,
// res.Detail is the reason; the caller re-runs with a worktree.
func (rr *repoRunner) runMode(ctx context.Context, t Task, useWT bool) (res T1Result, fallback bool) {
	role, _ := rr.cfg.Manifest.Role(t.Repo)
	res = T1Result{ID: t.InstanceID, Repo: t.Repo, Role: role, Source: t.Source, Base: t.BaseCommit}
	defer func() {
		if p := recover(); p != nil {
			res.Status, res.Detail = "error:panic", fmt.Sprint(p)
		}
	}()
	skip := func(why string, err error) (T1Result, bool) {
		res.Status = "skipped:" + why
		if err != nil {
			res.Detail = err.Error()
		}
		return res, false
	}
	ids, err := ParseF2P(t.F2P)
	if err != nil || len(ids) == 0 {
		return skip("f2p", err)
	}
	res.F2P = len(ids)
	if err := rr.ensureMirror(ctx); err != nil {
		return skip("clone", err)
	}
	if _, err := git(ctx, rr.mirror, "cat-file", "-e", t.BaseCommit+"^{commit}"); err != nil {
		if _, err := git(ctx, rr.mirror, "fetch", "--filter=blob:none", "origin", t.BaseCommit); err != nil {
			return skip("nobase", err)
		}
	}
	rr.seq++
	tag := fmt.Sprintf("%s_%d", repoDirName(rr.repo), rr.seq)
	// dir: where git reads/writes (the mirror when using the index, the worktree when checking out); root is where bgraph opens.
	dir := rr.mirror
	var ib *indexBuilder
	var wt, gp string
	if !useWT {
		idir := filepath.Join(rr.cfg.Work, "idx")
		if err := os.MkdirAll(idir, 0o755); err != nil {
			return skip("checkout", err)
		}
		ib = newIndexBuilder(rr.mirror, filepath.Join(idir, tag+".index"), t.BaseCommit)
		defer ib.Close()
	} else {
		wt = filepath.Join(rr.cfg.Work, "wt", tag)
		os.RemoveAll(wt)
		git(ctx, rr.mirror, "worktree", "prune")
		if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
			return skip("checkout", err)
		}
		defer func() {
			git(context.Background(), rr.mirror, "worktree", "remove", "--force", wt)
			os.RemoveAll(wt)
			git(context.Background(), rr.mirror, "worktree", "prune")
		}()
		if _, err := git(ctx, rr.mirror, "worktree", "add", "--detach", wt, t.BaseCommit); err != nil {
			return skip("checkout", err)
		}
		dir = wt
		pdir := filepath.Join(rr.cfg.Work, "patches")
		os.MkdirAll(pdir, 0o755)
		tp := filepath.Join(pdir, tag+".test.patch")
		gp = filepath.Join(pdir, tag+".gold.patch")
		os.WriteFile(tp, []byte(t.TestPatch), 0o644)
		os.WriteFile(gp, []byte(t.Patch), 0o644)
		defer os.Remove(tp)
		defer os.Remove(gp)
		if _, err := git(ctx, wt, "apply", "--whitespace=nowarn", tp); err != nil {
			return skip("apply", fmt.Errorf("test_patch: %w", err))
		}
		if _, err := git(ctx, wt, "add", "-A"); err != nil {
			return skip("apply", err)
		}
		if _, err := git(ctx, wt, "commit", "-q", "--no-verify", "--allow-empty", "-m", "eval: test patch"); err != nil {
			return skip("apply", err)
		}
		p, err := git(ctx, wt, "rev-parse", "HEAD")
		if err != nil {
			return skip("apply", err)
		}
		res.P = strings.TrimSpace(p)
	}
	if !useWT {
		p, err := ib.Apply(ctx, t.TestPatch, "eval: test patch")
		if err != nil {
			res.Detail = "test_patch: " + err.Error()
			return res, true
		}
		res.P = p
	}

	// Oracle (independent of bgraph): files changed by test_patch that still exist and contain FAIL_TO_PASS tests (read from P with git, not from the working tree).
	ns, err := git(ctx, dir, "diff", "--name-status", "--no-renames", "-z", t.BaseCommit, res.P)
	if err != nil {
		return skip("apply", err)
	}
	files := map[string]string{}
	parts := strings.Split(ns, "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		if parts[i] == "D" {
			continue
		}
		content, err := git(ctx, dir, "show", res.P+":"+parts[i+1])
		if err != nil {
			continue
		}
		files[parts[i+1]] = content
	}
	existsInP := func(p string) bool {
		_, err := git(ctx, dir, "cat-file", "-e", res.P+":"+p)
		return err == nil
	}
	req, mapping, unmapped := MapF2P(ids, files, existsInP)
	res.Required, res.Mapping, res.Unmapped = req, mapping, unmapped
	if len(req) == 0 {
		return skip("unmapped", nil)
	}

	chg := bgraph.Change{Base: res.P, Worktree: true}
	if useWT {
		if _, err := git(ctx, wt, "apply", "--whitespace=nowarn", gp); err != nil {
			return skip("apply", fmt.Errorf("patch: %w", err))
		}
	} else {
		c, err := ib.Apply(ctx, t.Patch, "eval: gold patch")
		if err != nil {
			res.Detail = "patch: " + err.Error()
			return res, true
		}
		chg = bgraph.Change{Base: res.P, Head: c}
	}

	state := filepath.Join(rr.cfg.Work, "state", repoDirName(rr.repo))
	ix, err := bgraph.Open(ctx, dir, bgraph.Options{StateDir: state})
	if err != nil {
		res.Status, res.Detail = "error:open", err.Error()
		return res, false
	}
	defer ix.Close()
	t0 := time.Now()
	sel, err := ix.Select(ctx, chg)
	res.Ms = time.Since(t0).Milliseconds()
	if err != nil {
		res.Status, res.Detail = "error:select", err.Error()
		return res, false
	}
	sel2, err := ix.Select(ctx, chg)
	if err != nil {
		res.Status, res.Detail = "error:select2", err.Error()
		return res, false
	}
	res.NonDeterm = !reflect.DeepEqual(canon(sel), canon(sel2))
	fillSel(&res, sel)

	su := make([]SelUnit, 0, len(sel.Tests))
	for _, st := range sel.Tests {
		su = append(su, SelUnit{Unit: st.Unit, Files: st.Files, Whole: st.Whole, WholeProject: st.Project})
	}
	res.Missing, res.FixtureReqs = CheckRequired(req, su)
	switch {
	case sel.Scope == model.TierFull:
		res.Status = "full"
	case len(res.Missing) == 0:
		res.Status = "ok"
	default:
		res.Status = "violation"
		res.WideList = sel.Widening
		res.Diag = diagnose(ctx, ix.Root(), res.P, res.Missing, sel.Changed)
	}
	return res, false
}

func canon(s model.Selection) any {
	b, _ := json.Marshal(s)
	var v any
	json.Unmarshal(b, &v)
	return v
}

func fillSel(res *T1Result, sel model.Selection) {
	res.Scope = string(sel.Scope)
	res.Widenings = map[string]int{}
	for _, w := range sel.Widening {
		res.Widenings[string(w.Tier)+":"+w.Reason]++
	}
	res.WideList = sel.Widening
	if len(res.WideList) > 12 {
		res.WideList = res.WideList[:12]
	}
	res.Selected = len(sel.Tests)
	res.Units = sel.IndexStats["units"]
	res.Changed = sel.Changed
	if len(res.Changed) > 40 {
		res.Changed = res.Changed[:40]
	}
}

// diagnose analyses the graph at P (only to diagnose violations).
func diagnose(ctx context.Context, root, p string, missing, changed []string) []FileDiag {
	r, err := gitx.Open(ctx, root)
	if err != nil {
		return nil
	}
	defer r.Close()
	ct, err := r.CommitTree(ctx, p)
	if err != nil {
		return nil
	}
	a, err := node.New(nil).Analyze(ctx, ct, scancache.New())
	if err != nil {
		return nil
	}
	fwd := map[string][]string{}
	for _, e := range a.Edges {
		if e.Kind == model.EdgePattern {
			continue
		}
		fwd[e.From] = append(fwd[e.From], e.To)
	}
	chg := map[string]bool{}
	for _, c := range changed {
		chg[c] = true
	}
	var out []FileDiag
	for _, f := range missing {
		d := FileDiag{File: f, Role: string(a.Roles[f])}
		for _, u := range a.Tests {
			for _, uf := range u.Files {
				if uf == f {
					d.InUnit = true
					d.Units = append(d.Units, u.ID+"["+u.Runner+"]")
				}
			}
		}
		for _, e := range a.Edges {
			if e.From == f && len(d.Imports) < 40 {
				d.Imports = append(d.Imports, fmt.Sprintf("%s->%s(%s)", e.Spec, e.To, e.Kind))
			}
		}
		for _, u := range a.Unresolved {
			if u.File == f && len(d.Unresolved) < 20 {
				d.Unresolved = append(d.Unresolved, fmt.Sprintf("%s [%s]", u.Spec, u.Class))
			}
		}
		seen := map[string]bool{f: true}
		q := []string{f}
		for len(q) > 0 {
			c := q[0]
			q = q[1:]
			for _, n := range fwd[c] {
				if !seen[n] {
					seen[n] = true
					q = append(q, n)
				}
			}
		}
		d.ReachSize = len(seen)
		d.ChangedRoles = map[string]string{}
		for c := range chg {
			if seen[c] {
				d.ReachedChanged = append(d.ReachedChanged, c)
			}
			if len(d.ChangedRoles) < 25 {
				d.ChangedRoles[c] = string(a.Roles[c])
			}
		}
		sort.Strings(d.ReachedChanged)
		out = append(out, d)
	}
	return out
}

// ---- Summary ----

// T1Summary is the aggregate for one group (dev/holdout/all).
type T1Summary struct {
	Role        string         `json:"role"`
	Attempted   int            `json:"attempted"`
	Skipped     map[string]int `json:"skipped"`
	Errors      int            `json:"errors"`
	Evaluated   int            `json:"evaluated"`
	OK          int            `json:"ok"`
	Full        int            `json:"full"`
	Violations  int            `json:"violations"`
	NonDeterm   int            `json:"nondeterministic"`
	FullRate    float64        `json:"full_rate"`
	NonFullN    int            `json:"non_full_n"`
	MedianRatio float64        `json:"median_selected_over_units"`
	P90Ratio    float64        `json:"p90_selected_over_units"`
	FullReasons map[string]int `json:"full_reasons"`
	Widenings   map[string]int `json:"widening_tasks"` // number of tasks with a "tier:reason" widening
	UpperBound  string         `json:"violation_upper_bound_95,omitempty"`

	FixtureTasks int `json:"fixture_tasks"` // number of tasks with fixture requirements
	FixtureReqs  int `json:"fixture_reqs"`  // total number of fixture requirements
	FixtureSat   int `json:"fixture_reqs_satisfied"`
}

// Summarize aggregates results by role ("" = all).
func Summarize(rs []T1Result, role string) T1Summary {
	s := T1Summary{Role: role, Skipped: map[string]int{}, FullReasons: map[string]int{}, Widenings: map[string]int{}}
	var ratios []float64
	for _, r := range rs {
		if role != "" && r.Role != role {
			continue
		}
		s.Attempted++
		switch {
		case strings.HasPrefix(r.Status, "skipped:"):
			s.Skipped[strings.TrimPrefix(r.Status, "skipped:")]++
			continue
		case strings.HasPrefix(r.Status, "error:"):
			s.Errors++
			continue
		}
		s.Evaluated++
		if len(r.FixtureReqs) > 0 {
			s.FixtureTasks++
		}
		for _, fr := range r.FixtureReqs {
			s.FixtureReqs++
			if fr.SatisfiedBy != "" {
				s.FixtureSat++
			}
		}
		if r.NonDeterm {
			s.NonDeterm++
		}
		for k := range r.Widenings {
			s.Widenings[k]++
		}
		switch r.Status {
		case "ok":
			s.OK++
		case "full":
			s.Full++
			for k := range r.Widenings {
				if strings.HasPrefix(k, "full:") {
					s.FullReasons[strings.TrimPrefix(k, "full:")]++
				}
			}
		case "violation":
			s.Violations++
		}
		if r.Status != "full" && r.Units > 0 {
			ratios = append(ratios, float64(r.Selected)/float64(r.Units))
		}
	}
	if s.Evaluated > 0 {
		s.FullRate = float64(s.Full) / float64(s.Evaluated)
	}
	s.NonFullN = s.OK + s.Violations
	s.MedianRatio, s.P90Ratio = Percentile(ratios, 50), Percentile(ratios, 90)
	if s.NonFullN > 0 && s.Violations == 0 {
		s.UpperBound = fmt.Sprintf("%.1f%% (3/N, N=%d)", 300/float64(s.NonFullN), s.NonFullN)
	}
	return s
}

// Percentile: the p-th percentile (0..100), nearest-rank interpolation.
func Percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	if p <= 50 && len(c)%2 == 0 && p == 50 {
		return (c[len(c)/2-1] + c[len(c)/2]) / 2
	}
	i := int(float64(len(c)-1)*p/100 + 0.5)
	return c[i]
}

// WriteT1 writes t1.json and t1.md.
func WriteT1(dir string, rs []T1Result) error {
	sums := []T1Summary{Summarize(rs, "dev"), Summarize(rs, "holdout"), Summarize(rs, "")}
	sums[2].Role = "all"
	b, _ := json.MarshalIndent(map[string]any{"summary": sums, "results": rs}, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "t1.json"), b, 0o644); err != nil {
		return err
	}
	var md bytes.Buffer
	fmt.Fprintf(&md, "# T1 (FAIL_TO_PASS) — hard oracle\n\n")
	fmt.Fprintf(&md, "| group | tried | skipped | errors | evaluated | ok | full | violations | non-deterministic | full rate | median sel/units (non-full) | p90 |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range sums {
		fmt.Fprintf(&md, "| %s | %d | %s | %d | %d | %d | %d | %d | %d | %.0f%% | %.3f | %.3f |\n", s.Role, s.Attempted, skipStr(s.Skipped), s.Errors,
			s.Evaluated, s.OK, s.Full, s.Violations, s.NonDeterm, s.FullRate*100, s.MedianRatio, s.P90Ratio)
	}
	for _, s := range sums {
		fmt.Fprintf(&md, "\n## %s\n\nFull reasons (tasks): %s\n\nWidenings (tasks): %s\n\nFixture requirements: %d tasks, %d requirements, %d satisfied\n", s.Role, mapStr(s.FullReasons), mapStr(s.Widenings), s.FixtureTasks, s.FixtureReqs, s.FixtureSat)
		if s.UpperBound != "" {
			fmt.Fprintf(&md, "\n95%% upper bound of the violation rate: %s\n", s.UpperBound)
		}
	}
	fmt.Fprintf(&md, "\n## Violations\n\n")
	n := 0
	for _, r := range rs {
		if r.Status == "violation" {
			n++
			fmt.Fprintf(&md, "- `%s` (%s) scope=%s missing %v; widenings %v\n", r.ID, r.Role, r.Scope, r.Missing, r.Widenings)
		}
	}
	if n == 0 {
		fmt.Fprintf(&md, "(none)\n")
	}
	fmt.Fprintf(&md, "\n## Tasks by repo\n\n| repo | role | tried | ok | full | violations | skipped |\n|---|---|---|---|---|---|---|\n")
	repos := map[string][]T1Result{}
	var names []string
	for _, r := range rs {
		if _, ok := repos[r.Repo]; !ok {
			names = append(names, r.Repo)
		}
		repos[r.Repo] = append(repos[r.Repo], r)
	}
	sort.Strings(names)
	for _, n := range names {
		var ok, full, vio, sk int
		for _, r := range repos[n] {
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
		}
		fmt.Fprintf(&md, "| %s | %s | %d | %d | %d | %d | %d |\n", n, repos[n][0].Role, len(repos[n]), ok, full, vio, sk)
	}
	return os.WriteFile(filepath.Join(dir, "t1.md"), md.Bytes(), 0o644)
}

func skipStr(m map[string]int) string {
	if len(m) == 0 {
		return "0"
	}
	return mapStr(m)
}

func mapStr(m map[string]int) string {
	if len(m) == 0 {
		return "(none)"
	}
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var b []string
	for _, k := range ks {
		b = append(b, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(b, ", ")
}

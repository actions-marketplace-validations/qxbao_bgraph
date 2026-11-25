package evalkit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/dynamic/phpdyn"
	"github.com/qxbao/bgraph/internal/model"
)

// ---- history helpers ----

// phpFirstParent returns the n first-parent commits ending at rev, newest first.
func phpFirstParent(ctx context.Context, clone, rev string, n int) ([]string, error) {
	out, err := git(ctx, clone, "rev-list", "--first-parent", fmt.Sprintf("-n%d", n), rev)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// phpPrefetch fetches, in batches, every blob of the trees of revs (and their first parents) that a blobless partial clone lacks:
// the selections read many blobs of both trees, and lazy fetches would get them one by one.
func phpPrefetch(ctx context.Context, clone string, revs []string) error {
	var in strings.Builder
	for _, r := range revs {
		in.WriteString(r + "\n")
		if p, err := git(ctx, clone, "rev-parse", "-q", "--verify", r+"^1"); err == nil {
			in.WriteString(strings.TrimSpace(p) + "\n")
		}
	}
	out, err := gitIn(ctx, clone, []string{"GIT_NO_LAZY_FETCH=1"}, in.String(), "rev-list", "--objects", "--no-walk", "--missing=print", "--stdin")
	if err != nil {
		return err
	}
	var miss []string
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "?") {
			miss = append(miss, strings.TrimPrefix(ln, "?"))
		}
	}
	for len(miss) > 0 {
		n := min(len(miss), 4000)
		if _, err := gitIn(ctx, clone, nil, strings.Join(miss[:n], "\n")+"\n", "-c", "gc.auto=0", "-c", "fetch.negotiationAlgorithm=noop", "fetch", "--no-tags",
			"--no-write-fetch-head", "--recurse-submodules=no", "--filter=blob:none", "--stdin", "origin"); err != nil {
			return err
		}
		miss = miss[n:]
	}
	return nil
}

func phpParent(ctx context.Context, clone, c string) (string, error) {
	p, err := git(ctx, clone, "rev-parse", "-q", "--verify", c+"^1")
	return strings.TrimSpace(p), err
}

// phpGuardStage runs the selection p→c with the layer on and no records: "accepted" (the guard passed and the layer stopped at
// "no records"), "refused" (with the reason), "inactive" (no narrowable PHP unit selected) or "error".
func phpGuardStage(ctx context.Context, ix *bgraph.Index, p, c string) (model.Selection, string, string) {
	sel, err := ix.Select(ctx, bgraph.Change{Base: p, Head: c, Dynamic: bgraph.DynamicOn})
	if err != nil {
		return sel, "error", err.Error()
	}
	why := ecoSkipReason(sel, phpdyn.Eco)
	switch {
	case why == "" && sel.Scope == model.TierFull:
		for _, w := range sel.Widening {
			if w.Tier == model.TierFull {
				return sel, "full", fmt.Sprintf("%s on %s", w.Reason, w.Path)
			}
		}
		return sel, "full", "full scope"
	case why == "":
		n := 0
		for _, t := range sel.Tests {
			if phpNarrowable(t) {
				n++
			}
		}
		return sel, "inactive", fmt.Sprintf("scope %s, %d tests selected, %d PHP units", sel.Scope, len(sel.Tests), n)
	case strings.HasPrefix(why, "no records"):
		return sel, "accepted", ""
	}
	return sel, "refused", why
}

// phpLevelsOf runs the function-level refinement on the changed files of p→c.
func phpLevelsOf(ctx context.Context, clone, p, c string, files []string) ([]PHPLevel, error) {
	want := func(f string) bool { return slices.Contains(files, f) }
	bt, err := newGitTreeWant(ctx, clone, p, want)
	if err != nil {
		return nil, err
	}
	defer bt.Close()
	ht, err := newGitTreeWant(ctx, clone, c, want)
	if err != nil {
		return nil, err
	}
	defer ht.Close()
	var present []string
	for _, f := range files {
		if bt.Has(f) && ht.Has(f) {
			present = append(present, f)
		}
	}
	return PHPLevels(phpChanged(ctx, "php", present, bt, ht)), nil
}

// ---- guard rate ----

// PHPGuardCommit is one first-parent commit.
type PHPGuardCommit struct {
	C      string     `json:"c"`
	P      string     `json:"p"`
	Files  []string   `json:"files"`
	Stage  string     `json:"stage"` // accepted | refused | full | inactive | error
	Reason string     `json:"reason,omitempty"`
	Level  string     `json:"level,omitempty"` // accepted: function | mixed | file | none (PHPCommitLevel)
	Levels []PHPLevel `json:"levels,omitempty"`
}

// PHPGuardResult is the guard rate over one repo's history.
type PHPGuardResult struct {
	Repo     string           `json:"repo"`
	Head     string           `json:"head"`
	Examined int              `json:"examined"`
	Commits  []PHPGuardCommit `json:"commits"`
	Ms       int64            `json:"ms"`
}

// RunPHPGuardRate examines the n first-parent commits ending at rev in clone (checked out at rev, keeping its Composer install):
// the selection parent→commit with the PHP layer on and an empty record store, then for accepted commits the function-level
// refinement of the changed files.
func RunPHPGuardRate(ctx context.Context, name, clone, stateDir, rev string, n int, logf func(string, ...any)) (res PHPGuardResult, err error) {
	t0 := time.Now()
	res = PHPGuardResult{Repo: name, Head: rev}
	if h, err := git(ctx, clone, "rev-parse", rev); err == nil {
		res.Head = strings.TrimSpace(h)
	}
	defer func() { res.Ms = time.Since(t0).Milliseconds() }()
	cs, err := phpFirstParent(ctx, clone, rev, n)
	if err != nil {
		return res, err
	}
	if err := phpPrefetch(ctx, clone, cs); err != nil {
		return res, fmt.Errorf("prefetch: %w", err)
	}
	if err := phpCheckout(ctx, clone, rev); err != nil {
		return res, err
	}
	os.Remove(filepath.Join(stateDir, "dynamic", phpdyn.Eco+".jsonl"))
	ix, err := phpOpen(ctx, clone, stateDir)
	if err != nil {
		return res, err
	}
	defer ix.Close()
	for _, c := range cs {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.Examined++
		p, err := phpParent(ctx, clone, c)
		if err != nil {
			continue // root commit
		}
		raw, err := git(ctx, clone, "diff", "--name-only", "--no-renames", p, c)
		if err != nil {
			return res, err
		}
		gc := PHPGuardCommit{C: c, P: p, Files: strings.Fields(raw)}
		_, gc.Stage, gc.Reason = phpGuardStage(ctx, ix, p, c)
		if gc.Stage == "accepted" {
			ls, err := phpLevelsOf(ctx, clone, p, c, gc.Files)
			if err != nil {
				gc.Level = "error: " + err.Error()
			} else {
				gc.Levels, gc.Level = ls, PHPCommitLevel(ls)
			}
		}
		if logf != nil {
			logf("%s guard %s: %d files: %s %s %s", name, c[:10], len(gc.Files), gc.Stage, gc.Level, firstN(gc.Reason, 200))
		}
		res.Commits = append(res.Commits, gc)
	}
	return res, nil
}

// ---- replay ----

// PHPReplayConfig configures one repo's replay.
type PHPReplayConfig struct {
	PHPDynConfig
	Rev      string // newest commit (default Repo.R)
	Commits  int    // first-parent commits ending at Rev
	Composer bool   // run `composer update` when composer.json at the parent differs from the installed one
}

// PHPReplayCommit is one replayed commit.
type PHPReplayCommit struct {
	C      string     `json:"c"`
	P      string     `json:"p"`
	Files  []string   `json:"files"`
	Stage  string     `json:"stage"` // accepted | refused | full | inactive | error (the guard without records)
	Skip   string     `json:"skip,omitempty"`
	Level  string     `json:"level,omitempty"`
	Levels []PHPLevel `json:"levels,omitempty"`
	Total  int        `json:"total,omitempty"`
	Static PHPDynSel  `json:"static"`
	Hybrid PHPDynSel  `json:"hybrid"`
	// AssertFail: the hybrid selection differs from the static one although nothing was narrowed.
	AssertFail string         `json:"assert_fail,omitempty"`
	Record     *PHPRecordStat `json:"record,omitempty"`
	Records    *PHPRecCount   `json:"records,omitempty"`
	Composer   string         `json:"composer,omitempty"`
	FailAtC    []string       `json:"fail_at_c,omitempty"`
	FailAtP    []string       `json:"fail_at_p,omitempty"`
	Misses     []string       `json:"misses,omitempty"`
	MissDetail []string       `json:"miss_detail,omitempty"`
	VerifyMs   int64          `json:"verify_ms,omitempty"`
	Ms         int64          `json:"ms"`
	Error      string         `json:"error,omitempty"`
}

// PHPPlainStat compares recording with plain runs at one parent.
type PHPPlainStat struct {
	At       string `json:"at"`
	Files    int    `json:"files"`
	Jobs     int    `json:"jobs"`
	RecordMs int64  `json:"record_ms"`
	PlainMs  int64  `json:"plain_ms"`  // every listed test file alone, no collector, same parallelism
	WholeMs  int64  `json:"whole_ms"`  // the whole suite in one process
	WholeErr string `json:"whole_err"` // the whole suite's exit or failure
}

// PHPReplayResult is one repo's replay.
type PHPReplayResult struct {
	Repo    string            `json:"repo"`
	Rev     string            `json:"rev"`
	PHP     string            `json:"php"`
	Commits []PHPReplayCommit `json:"commits"`
	Plain   *PHPPlainStat     `json:"plain,omitempty"`
	Ms      int64             `json:"ms"`
	Error   string            `json:"error,omitempty"`
}

// PHPSameTests reports whether two selections select the same units with the same filters.
func PHPSameTests(a, b model.Selection) string {
	if a.Scope != b.Scope || len(a.Tests) != len(b.Tests) {
		return fmt.Sprintf("scope %s/%s, %d/%d units", a.Scope, b.Scope, len(a.Tests), len(b.Tests))
	}
	for i := range a.Tests {
		x, y := a.Tests[i], b.Tests[i]
		if x.Unit != y.Unit || x.ClassFilter != y.ClassFilter || !slices.Equal(x.Classes, y.Classes) {
			return fmt.Sprintf("unit %d: %s/%s filter %v/%v", i, x.Unit, y.Unit, x.ClassFilter, y.ClassFilter)
		}
	}
	if len(a.Narrowed) != len(b.Narrowed) {
		return fmt.Sprintf("narrowed %d/%d", len(a.Narrowed), len(b.Narrowed))
	}
	return ""
}

// RunPHPReplay replays the first-parent commits ending at Rev: a guard pass without records over all of them (hybrid == static
// asserted), then for every accepted commit fresh records at the parent, the selection, and every removed test file run at the
// commit (and at the parent when it fails at the commit).
func RunPHPReplay(ctx context.Context, cfg PHPReplayConfig) (res PHPReplayResult) {
	t0 := time.Now()
	r := cfg.Repo
	rev := cfg.Rev
	if rev == "" {
		rev = r.R
	}
	res = PHPReplayResult{Repo: r.Name, Rev: rev}
	if h, err := git(ctx, cfg.Clone, "rev-parse", rev); err == nil {
		rev = strings.TrimSpace(h)
		res.Rev = rev
	}
	defer func() { res.Ms = time.Since(t0).Milliseconds() }()
	if p, err := phpdyn.RunProbe(ctx, "php"); err == nil {
		res.PHP = "PHP " + p.Version + ", Xdebug " + p.Xdebug
	}
	cs, err := phpFirstParent(ctx, cfg.Clone, rev, cfg.Commits)
	if err == nil {
		err = phpPrefetch(ctx, cfg.Clone, cs)
	}
	if err == nil {
		err = phpCheckout(ctx, cfg.Clone, rev)
	}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	marker := filepath.Join(cfg.Work, "composer-"+r.Dir+".oid")
	if _, err := os.Stat(marker); err != nil {
		if oid, err := git(ctx, cfg.Clone, "rev-parse", rev+":composer.json"); err == nil {
			os.MkdirAll(cfg.Work, 0o755)
			os.WriteFile(marker, []byte(strings.TrimSpace(oid)), 0o644)
		}
	}
	// Pass 1: the guard without records, at rev.
	gstate := filepath.Join(cfg.Work, "replay-guard-state", r.Dir)
	os.Remove(filepath.Join(gstate, "dynamic", phpdyn.Eco+".jsonl"))
	gix, err := phpOpen(ctx, cfg.Clone, gstate)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	for _, c := range cs {
		if ctx.Err() != nil {
			break
		}
		rc := PHPReplayCommit{C: c}
		p, err := phpParent(ctx, cfg.Clone, c)
		if err != nil {
			continue // root commit
		}
		rc.P = p
		raw, _ := git(ctx, cfg.Clone, "diff", "--name-only", "--no-renames", p, c)
		rc.Files = strings.Fields(raw)
		hy, stage, why := phpGuardStage(ctx, gix, p, c)
		rc.Stage, rc.Skip = stage, why
		if stage == "error" {
			rc.Error = why
		} else if stage != "accepted" {
			st, err := gix.Select(ctx, bgraph.Change{Base: p, Head: c, Dynamic: bgraph.DynamicOff})
			if err != nil {
				rc.Error = err.Error()
			} else {
				rc.Static, rc.Hybrid = PHPSummarize(st, nil, nil), PHPSummarize(hy, nil, nil)
				rc.AssertFail = PHPSameTests(st, hy)
			}
		}
		res.Commits = append(res.Commits, rc)
	}
	gix.Close()
	// Pass 2: the accepted commits, with fresh records at the parent.
	for i := range res.Commits {
		if ctx.Err() != nil {
			res.Error = ctx.Err().Error()
			break
		}
		rc := &res.Commits[i]
		if rc.Stage != "accepted" {
			continue
		}
		c0 := time.Now()
		cfg.replayOne(ctx, rc, marker, &res)
		rc.Ms = time.Since(c0).Milliseconds()
		cfg.logf("replay %s: %d files, %s, static %d/%d, hybrid %d, removed %d, fail at C %v, at P %v, misses %v%s%s", rc.C[:10], len(rc.Files),
			rc.Level, rc.Static.Files, rc.Total, rc.Hybrid.Files, len(rc.Hybrid.Removed), rc.FailAtC, rc.FailAtP, rc.Misses,
			map[bool]string{true: " ASSERT " + rc.AssertFail, false: ""}[rc.AssertFail != ""],
			map[bool]string{true: " skipped: " + rc.Hybrid.Skip, false: ""}[rc.Hybrid.Skip != ""]+" "+rc.Error)
	}
	if err := phpCheckout(ctx, cfg.Clone, rev); err != nil {
		cfg.logf("restore the clone at %s: %v", rev, err)
	}
	return res
}

func (cfg PHPReplayConfig) composer(ctx context.Context, rc *PHPReplayCommit, marker string) {
	if !cfg.Composer {
		return
	}
	oid, err := git(ctx, cfg.Clone, "rev-parse", rc.P+":composer.json")
	if err != nil {
		return
	}
	oid = strings.TrimSpace(oid)
	if b, _ := os.ReadFile(marker); string(b) == oid {
		return
	}
	_, tl, to, err := runProc(ctx, cfg.Clone, []string{"XDEBUG_MODE=off", "COMPOSER_NO_INTERACTION=1"}, 20*time.Minute, "composer", "update",
		"--no-interaction", "--no-progress", "--prefer-dist", "--quiet")
	if to || err != nil {
		rc.Composer = fmt.Sprintf("composer update failed (timeout=%v %v): %s", to, err, tail(tl, 300))
		return
	}
	rc.Composer = "composer update for composer.json " + oid[:10]
	os.WriteFile(marker, []byte(oid), 0o644)
}

func (cfg PHPReplayConfig) replayOne(ctx context.Context, rc *PHPReplayCommit, marker string, res *PHPReplayResult) {
	if err := phpCheckout(ctx, cfg.Clone, rc.P); err != nil {
		rc.Error = err.Error()
		return
	}
	cfg.composer(ctx, rc, marker)
	if err := phpCheckout(ctx, cfg.Clone, rc.P); err != nil { // composer may leave files behind
		rc.Error = err.Error()
		return
	}
	state := filepath.Join(cfg.Work, "replay-state", cfg.Repo.Dir, rc.C[:12])
	os.RemoveAll(state)
	ix, err := phpOpen(ctx, cfg.Clone, state)
	if err != nil {
		rc.Error = err.Error()
		return
	}
	defer ix.Close()
	rs := phpRecord(ctx, ix, cfg.Jobs, true)
	rc.Record = &rs
	if rs.Error != "" {
		rc.Error = "record: " + rs.Error
		return
	}
	if err := phpClean(ctx, cfg.Clone); err != nil {
		rc.Error = "after recording: " + err.Error()
		return
	}
	lists := phpLists(state)
	rc.Total = listTotal(lists)
	cnt := phpRecCount(state)
	rc.Records = &cnt
	st, err := ix.Select(ctx, bgraph.Change{Base: rc.P, Head: rc.C, Dynamic: bgraph.DynamicOff})
	if err != nil {
		rc.Error = err.Error()
		return
	}
	hy, err := ix.Select(ctx, bgraph.Change{Base: rc.P, Head: rc.C, Dynamic: bgraph.DynamicOn})
	if err != nil {
		rc.Error = err.Error()
		return
	}
	rc.Static, rc.Hybrid = PHPSummarize(st, lists, nil), PHPSummarize(hy, lists, nil)
	units := map[string]phpdyn.Unit{}
	for _, t := range st.Tests {
		if phpNarrowable(t) {
			units[t.Unit] = phpdyn.UnitOf(t)
		}
	}
	if res.Plain == nil && len(units) > 0 {
		res.Plain = cfg.plain(ctx, rc.P, lists, rs.Ms, units)
		if err := phpCheckout(ctx, cfg.Clone, rc.P); err != nil {
			rc.Error = err.Error()
			return
		}
	}
	if ls, err := phpLevelsOf(ctx, cfg.Clone, rc.P, rc.C, rc.Files); err == nil {
		rc.Levels, rc.Level = ls, PHPCommitLevel(ls)
	}
	if !rc.Hybrid.Narrowed {
		rc.AssertFail = PHPSameTests(st, hy)
		return
	}
	type removed struct{ unit, file string }
	var rm []removed
	for _, n := range hy.Narrowed {
		u, f, ok := strings.Cut(n.Unit, "#")
		if ok {
			rm = append(rm, removed{u, f})
		}
	}
	t0 := time.Now()
	defer func() { rc.VerifyMs = time.Since(t0).Milliseconds() }()
	details := map[string]string{}
	var mu sync.Mutex
	// run runs files at rev, jobs at a time (a red file is run again alone, sequentially, before it counts: parallel runs of
	// test files that share temporary resources may fail spuriously).
	run := func(rev string, files []removed, jobs int) ([]removed, error) {
		if err := phpCheckout(ctx, cfg.Clone, rev); err != nil {
			return nil, err
		}
		for _, x := range files {
			if _, ok := units[x.unit]; !ok {
				return nil, fmt.Errorf("%s: not in the static selection", x.unit)
			}
		}
		one := func(x removed, w int) (bool, error) {
			cache := filepath.Join(cfg.Work, fmt.Sprintf("phpunit-cache-%d", w))
			os.RemoveAll(cache)
			pass, detail, err := phpRunFile(ctx, cfg.Clone, units[x.unit], x.file, cache)
			if err == nil && !pass {
				mu.Lock()
				details[x.file+"@"+rev[:10]] = tail(detail, 600)
				mu.Unlock()
			}
			return pass, err
		}
		var first []removed
		var firstErr error
		ch := make(chan removed)
		var wg sync.WaitGroup
		for w := 0; w < max(1, jobs); w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for x := range ch {
					pass, err := one(x, w)
					mu.Lock()
					if err != nil && firstErr == nil {
						firstErr = err
					}
					if err == nil && !pass {
						first = append(first, x)
					}
					mu.Unlock()
				}
			}(w)
		}
		for _, x := range files {
			ch <- x
		}
		close(ch)
		wg.Wait()
		if firstErr != nil {
			return nil, firstErr
		}
		sort.Slice(first, func(i, j int) bool { return first[i].file < first[j].file })
		var red []removed
		for _, x := range first {
			pass, err := one(x, 0)
			if err != nil {
				return nil, err
			}
			if !pass {
				red = append(red, x)
			}
		}
		return red, phpClean(ctx, cfg.Clone)
	}
	redC, err := run(rc.C, rm, cfg.Jobs)
	if err != nil {
		rc.Error = "verify at C: " + err.Error()
		return
	}
	for _, x := range redC {
		rc.FailAtC = append(rc.FailAtC, x.file)
	}
	if len(redC) == 0 {
		return
	}
	redP, err := run(rc.P, redC, 1)
	if err != nil {
		rc.Error = "verify at P: " + err.Error()
		return
	}
	for _, x := range redP {
		rc.FailAtP = append(rc.FailAtP, x.file)
	}
	for _, x := range redC {
		if !slices.Contains(rc.FailAtP, x.file) {
			rc.Misses = append(rc.Misses, x.file)
			rc.MissDetail = append(rc.MissDetail, x.file+": "+details[x.file+"@"+rc.C[:10]])
		}
	}
}

// plain times every listed test file run alone without the collector (same parallelism as the recording) and the whole suite in
// one process, at the parent.
func (cfg PHPReplayConfig) plain(ctx context.Context, at string, lists map[string][]string, recordMs int64, units map[string]phpdyn.Unit) *PHPPlainStat {
	ps := &PHPPlainStat{At: at, Jobs: cfg.Jobs, RecordMs: recordMs}
	type job struct {
		u    phpdyn.Unit
		file string
	}
	var jobs []job
	for id, files := range lists {
		u, ok := units[id]
		if !ok {
			continue
		}
		for _, f := range files {
			jobs = append(jobs, job{u, f})
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].file < jobs[j].file })
	ps.Files = len(jobs)
	t0 := time.Now()
	ch := make(chan job)
	var wg sync.WaitGroup
	for w := 0; w < max(1, cfg.Jobs); w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			cache := filepath.Join(cfg.Work, fmt.Sprintf("plain-cache-%d", w))
			for j := range ch {
				os.RemoveAll(cache)
				phpRunFile(ctx, cfg.Clone, j.u, j.file, cache)
			}
		}(w)
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	ps.PlainMs = time.Since(t0).Milliseconds()
	for _, u := range units {
		t1 := time.Now()
		bin := filepath.Join(cfg.Clone, filepath.FromSlash(u.BinDir), u.Runner)
		_, tl, to, err := runProc(ctx, filepath.Join(cfg.Clone, filepath.FromSlash(u.Project)), []string{"XDEBUG_MODE=off"}, phpDynSuiteTimeout,
			"php", bin, "--configuration", relTo(u.Project, u.Config), "--do-not-cache-result", "--colors=never")
		ps.WholeMs += time.Since(t1).Milliseconds()
		if to || err != nil {
			ps.WholeErr = fmt.Sprintf("timeout=%v %v %s", to, err, tail(strings.TrimSpace(tl), 200))
		}
	}
	return ps
}

func relTo(project, p string) string {
	if project == "" || project == "." {
		return p
	}
	r, err := filepath.Rel(filepath.FromSlash(project), filepath.FromSlash(p))
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

package evalkit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/dynamic/rbdyn"
	"github.com/qxbao/bgraph/internal/model"
)

// Phase 10 (docs/dynamic-layer.md §10.7): the Ruby method layer checked three ways, all without the collector as oracle.
//
//   - Hybrid T2 (RunRubyDynT2): records made once at R in a clone of the Ruby T2 checkout (`bgraph record --eco ruby`), then for
//     every killed mutant of the cached oracle the static selection and the selection with the Ruby layer on R→M; a violation
//     is an oracle-red test file outside the hybrid selection.
//   - Body guard rate (RunRubyGuardRate): over first-parent commits, the share of commits changing only .rb files that the
//     layer's guards accept (the guard of the selection, then the compile check), without records.
//   - Replay (RunRubyReplay): commits the guards accept; fresh records at the parent, the selection parent→commit, and every
//     removed test file run alone at the commit, as the runner adapter runs it (and at the parent when it fails at the commit).
//
// The test processes run with the T2 repo's bundle: BUNDLE_PATH, BUNDLE_USER_HOME and the repo's env are set in this process
// (the environment fingerprint reads BUNDLE_* from it, at record and at selection time alike), and the T2 checkout's
// Gemfile.lock (untracked in these gems) is copied into the clone after every checkout.

// RbDynConfig configures the Ruby layer evaluation of one T2 repo.
type RbDynConfig struct {
	T2       T2Config // the Ruby T2 set (oracle.jsonl, checkouts, bundles); read only
	Repo     T2Repo
	Work     string // directory of this evaluation's clones and state (t2/ruby-dyn)
	Jobs     int
	Rerecord bool
	// Install: run `bundle install` after every checkout (replay: older commits may need gems the T2 bundle lacks; BUNDLE_PATH
	// then points at a copy of it, see RbDynReplayBundle).
	Install bool
}

func (c RbDynConfig) logf(f string, a ...any) {
	c.T2.logf("%s: "+f, append([]any{c.Repo.Name}, a...)...)
}

func (c RbDynConfig) clone() string { return filepath.Join(c.Work, c.Repo.Dir) }

// rbDynEnvSet are the variables of the last RbDynSetEnv's repo env.
var rbDynEnvSet []string

// RbDynSetEnv sets the environment of the repo's Ruby processes in this process (see above).
func RbDynSetEnv(t2Dir string, r T2Repo) error {
	kv := []string{"BUNDLE_USER_HOME=" + filepath.Join(t2Dir, "bundlehome"), "NO_COLOR=1"}
	if r.Venv != "" {
		kv = append(kv, "BUNDLE_PATH="+filepath.Join(t2Dir, r.Venv))
	}
	if d := os.Getenv("BGRAPH_EVAL_RUBY_BIN"); d != "" && !strings.HasPrefix(os.Getenv("PATH"), d+string(os.PathListSeparator)) {
		kv = append(kv, "PATH="+d+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	kv = append(kv, r.Env...)
	for _, k := range rbDynEnvSet { // the previous repo's variables (CI=spec of rack must not leak into the next repo)
		os.Unsetenv(k)
	}
	rbDynEnvSet = nil
	for _, e := range r.Env {
		k, _, _ := strings.Cut(e, "=")
		rbDynEnvSet = append(rbDynEnvSet, k)
	}
	for _, e := range kv {
		k, v, _ := strings.Cut(e, "=")
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

// rbCheckout checks the clone out at rev (cleaning it) and copies the T2 checkout's untracked lockfile into it.
func (c RbDynConfig) rbCheckout(ctx context.Context, rev string) error {
	src := filepath.Join(c.T2.Dir, c.Repo.Dir)
	if err := localClone(ctx, src, c.clone(), rev); err != nil {
		return err
	}
	for _, n := range []string{"Gemfile.lock", "gems.locked"} {
		if out, _ := git(ctx, c.clone(), "ls-files", n); strings.TrimSpace(out) != "" {
			continue // tracked at rev
		}
		b, err := os.ReadFile(filepath.Join(src, n))
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(c.clone(), n), b, 0o644); err != nil {
			return err
		}
	}
	if c.Install {
		_, tl, to, err := runProc(ctx, c.clone(), nil, 20*time.Minute, rubyExe(), "-S", "bundle", "install", "--quiet")
		if to || err != nil {
			return fmt.Errorf("bundle install at %s: timeout=%v %v %s", rev, to, err, tl)
		}
	}
	return nil
}

// RbDynReplayBundle points BUNDLE_PATH at work/<venv>-replay, a copy of the T2 bundle made once, into which the replay's
// `bundle install` adds the gems older commits need (the T2 bundle stays untouched).
func RbDynReplayBundle(t2Dir, work string, r T2Repo) error {
	if r.Venv == "" {
		return nil
	}
	dst := filepath.Join(work, r.Venv+"-replay")
	if _, err := os.Stat(dst); err != nil {
		if err := os.CopyFS(dst, os.DirFS(filepath.Join(t2Dir, r.Venv))); err != nil {
			return fmt.Errorf("copy the bundle: %w", err)
		}
	}
	return os.Setenv("BUNDLE_PATH", dst)
}

// RbDynCheckoutR makes (or resets) the evaluation clone of the repo at R.
func RbDynCheckoutR(ctx context.Context, c RbDynConfig) error { return c.rbCheckout(ctx, c.Repo.R) }

func (c RbDynConfig) open(ctx context.Context, stateDir string) (*bgraph.Index, error) {
	return bgraph.Open(ctx, c.clone(), bgraph.Options{StateDir: stateDir, Ruby: rubyExe()})
}

// RbRecordStat is one recording.
type RbRecordStat struct {
	Reused  bool                  `json:"reused,omitempty"`
	Summary *bgraph.RecordSummary `json:"summary,omitempty"`
	Ms      int64                 `json:"ms"`
	Error   string                `json:"error,omitempty"`
}

func (c RbDynConfig) record(ctx context.Context, ix *bgraph.Index, rerecord bool) RbRecordStat {
	if !rerecord {
		if cur, err := ix.DynamicStatusOf(ctx, rbdyn.Eco); err == nil && cur.CurrentEnv > 0 {
			return RbRecordStat{Reused: true}
		}
	}
	t0 := time.Now()
	sum, err := ix.Record(ctx, bgraph.RecordOptions{Eco: rbdyn.Eco, Jobs: c.Jobs, Timeout: 10 * time.Minute})
	st := RbRecordStat{Ms: time.Since(t0).Milliseconds()}
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Summary = &sum
	c.logf("record: %d units, %d complete, %d incomplete, %d failed in %s %v", sum.Units, sum.Complete, sum.Incomplete, sum.Failed,
		time.Duration(st.Ms)*time.Millisecond, sum.Reasons)
	return st
}

// RbSel summarizes one selection.
type RbSel struct {
	Scope    string         `json:"scope"`
	Selected int            `json:"selected"` // selected units
	Removed  []string       `json:"removed,omitempty"`
	Skip     string         `json:"skip,omitempty"` // the Ruby layer's dynamic_skipped reason
	Kept     map[string]int `json:"kept,omitempty"` // keep-reason class -> units
	Missed   []string       `json:"missed,omitempty"`
}

var reRbKeptNum = regexp.MustCompile(`[0-9]+`)

// rbKeptClass reduces a kept unit's dynamic note to a class.
func rbKeptClass(note string) string {
	switch {
	case strings.HasPrefix(note, "not narrowed by the Ruby method layer"):
		return "layer skipped"
	case strings.Contains(note, "executed the changed method"):
		return "executed a changed method"
	case strings.Contains(note, "no method table") || strings.Contains(note, "not instrumented") || strings.Contains(note, "cannot be judged"):
		return "changed method not instrumented"
	case strings.Contains(note, "loaded the changed"):
		return "loaded a changed file"
	case strings.Contains(note, "read the changed"):
		return "read a changed file"
	case strings.Contains(note, "record"):
		return "record not usable"
	}
	note = reRbKeptNum.ReplaceAllString(note, "N")
	if len(note) > 80 {
		note = note[:80]
	}
	return note
}

func rbSummarize(sel model.Selection, red []string) RbSel {
	d := RbSel{Scope: string(sel.Scope), Selected: len(sel.Tests), Skip: ecoSkipReason(sel, rbdyn.Eco)}
	for _, n := range sel.Narrowed {
		d.Removed = append(d.Removed, n.Unit)
	}
	for _, t := range sel.Tests {
		if t.Dynamic != "" {
			if d.Kept == nil {
				d.Kept = map[string]int{}
			}
			d.Kept[rbKeptClass(t.Dynamic)]++
		}
	}
	for _, f := range red {
		if !selCovers(sel, f) {
			d.Missed = append(d.Missed, f)
		}
	}
	return d
}

// rbUnits counts the Ruby units of a selection's index (precise units, i.e. test files).
func rbUnits(sel model.Selection) int { return sel.IndexStats["units"] }

// RbMutant is one killed mutant.
type RbMutant struct {
	ID     string   `json:"id"`
	File   string   `json:"file"`
	Line   int      `json:"line"`
	Op     string   `json:"op"`
	Red    []string `json:"red"`
	Units  int      `json:"units"`
	Static RbSel    `json:"static"`
	Hybrid RbSel    `json:"hybrid"`
	Error  string   `json:"error,omitempty"`
}

// RbDynT2Result is the hybrid pass over one repo.
type RbDynT2Result struct {
	Repo    string               `json:"repo"`
	R       string               `json:"r"`
	Record  RbRecordStat         `json:"record"`
	Status  bgraph.DynamicStatus `json:"status"`
	Mutants []RbMutant           `json:"mutants"`
	Error   string               `json:"error,omitempty"`
}

// RunRubyDynT2 runs the hybrid pass on one repo against the cached oracle.
func RunRubyDynT2(ctx context.Context, cfg RbDynConfig, only map[string]bool) RbDynT2Result {
	r := cfg.Repo
	res := RbDynT2Result{Repo: r.Name, R: r.R}
	recs, err := loadOracle(cfg.T2.oraclePath())
	if err != nil {
		res.Error = err.Error()
		return res
	}
	latest := map[string]T2Record{}
	var keys []string
	for _, x := range recs {
		if x.Repo != r.Name || x.R != r.R {
			continue
		}
		if _, ok := latest[x.key()]; !ok {
			keys = append(keys, x.key())
		}
		latest[x.key()] = x
	}
	if err := cfg.rbCheckout(ctx, r.R); err != nil {
		res.Error = err.Error()
		return res
	}
	state := filepath.Join(cfg.Work, "state", r.Dir)
	ix, err := cfg.open(ctx, state)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer ix.Close()
	res.Record = cfg.record(ctx, ix, cfg.Rerecord)
	if res.Record.Error != "" {
		res.Error = "record: " + res.Record.Error
		return res
	}
	res.Status, _ = ix.DynamicStatusOf(ctx, rbdyn.Eco)
	idx := filepath.Join(cfg.Work, "idx")
	for _, k := range keys {
		o := latest[k]
		if o.Status != "killed" || only != nil && !only[o.Mutant.ID] {
			continue
		}
		if ctx.Err() != nil {
			res.Error = ctx.Err().Error()
			return res
		}
		m := RbMutant{ID: o.Mutant.ID, File: o.Mutant.File, Line: o.Mutant.Line, Op: o.Mutant.Op, Red: o.Red}
		commit, _, err := r.MutantCommit(ctx, cfg.clone(), idx, o.Mutant)
		if err == nil {
			var st, hy model.Selection
			st, err = ix.Select(ctx, bgraph.Change{Base: r.R, Head: commit, Dynamic: bgraph.DynamicOff})
			if err == nil {
				hy, err = ix.Select(ctx, bgraph.Change{Base: r.R, Head: commit, Dynamic: bgraph.DynamicOn})
			}
			if err == nil {
				m.Units, m.Static, m.Hybrid = rbUnits(st), rbSummarize(st, o.Red), rbSummarize(hy, o.Red)
			}
		}
		if err != nil {
			m.Error = err.Error()
		}
		cfg.logf("%s %s:%d %s: static %d/%d, hybrid %d (removed %d)%s%s", m.ID, m.File, m.Line, m.Op, m.Static.Selected, m.Units,
			m.Hybrid.Selected, len(m.Hybrid.Removed), map[bool]string{true: " VIOLATION " + strings.Join(m.Hybrid.Missed, ","), false: ""}[len(m.Hybrid.Missed) > 0],
			map[bool]string{true: "; skipped: " + m.Hybrid.Skip, false: ""}[m.Hybrid.Skip != ""]+m.Error)
		res.Mutants = append(res.Mutants, m)
	}
	return res
}

// ---- body guard rate ----

// RbGuardCommit is one first-parent commit changing only .rb files.
type RbGuardCommit struct {
	C      string   `json:"c"`
	P      string   `json:"p"`
	Files  []string `json:"files"`
	Stage  string   `json:"stage"` // accepted | refused | parse | inactive (no narrowable Ruby unit selected) | error
	Reason string   `json:"reason,omitempty"`
}

// RbGuardResult is the guard rate over one repo's history.
type RbGuardResult struct {
	Repo     string          `json:"repo"`
	Head     string          `json:"head"`
	Examined int             `json:"examined"` // first-parent commits examined
	Commits  []RbGuardCommit `json:"commits"`  // those changing only .rb files
}

// RunRubyGuardRate examines the n first-parent commits ending at rev in clone (an index is opened on it; nothing is checked
// out): for every commit whose changed files all end in .rb, the selection parent→commit with the Ruby layer on and no
// records (the layer's guard either refuses, or passes and stops at "no records"), then the compile check of the files the
// body guard accepted.
func RunRubyGuardRate(ctx context.Context, name, clone, stateDir, rev string, n int, logf func(string, ...any)) (RbGuardResult, error) {
	res := RbGuardResult{Repo: name, Head: rev}
	if err := localClone(ctx, clone, clone, rev); err != nil {
		return res, err
	}
	out, err := git(ctx, clone, "rev-list", "--first-parent", fmt.Sprintf("-n%d", n), rev)
	if err != nil {
		return res, err
	}
	ix, err := bgraph.Open(ctx, clone, bgraph.Options{StateDir: stateDir, Ruby: rubyExe()})
	if err != nil {
		return res, err
	}
	defer ix.Close()
	for _, c := range strings.Fields(out) {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.Examined++
		p, err := git(ctx, clone, "rev-parse", c+"^1")
		if err != nil {
			continue // root commit
		}
		p = strings.TrimSpace(p)
		raw, err := git(ctx, clone, "diff", "--name-only", "--no-renames", p, c)
		if err != nil {
			return res, err
		}
		files := strings.Fields(raw)
		ok := len(files) > 0
		for _, f := range files {
			ok = ok && path.Ext(f) == ".rb"
		}
		if !ok {
			continue
		}
		gc := RbGuardCommit{C: c, P: p, Files: files}
		gc.Stage, gc.Reason = rbGuardOne(ctx, ix, clone, p, c)
		if logf != nil {
			logf("%s guard %s: %d files: %s %s", name, c[:10], len(files), gc.Stage, gc.Reason)
		}
		res.Commits = append(res.Commits, gc)
	}
	return res, nil
}

func rbGuardOne(ctx context.Context, ix *bgraph.Index, clone, p, c string) (stage, reason string) {
	sel, err := ix.Select(ctx, bgraph.Change{Base: p, Head: c, Dynamic: bgraph.DynamicOn})
	if err != nil {
		return "error", err.Error()
	}
	why := ecoSkipReason(sel, rbdyn.Eco)
	switch {
	case why == "":
		return "inactive", fmt.Sprintf("scope %s, %d units selected", sel.Scope, len(sel.Tests))
	case !strings.HasPrefix(why, "no records"):
		return "refused", why
	}
	// The guard passed: the compile check of the files the body guard accepts (library code and test support; a changed test
	// file is a selected unit and has no body guard).
	units := map[string]bool{}
	for _, t := range sel.Tests {
		units[t.Unit] = true
	}
	var files []rbdyn.ParseFile
	for _, f := range sel.Changed {
		if units[f] || path.Ext(f) != ".rb" {
			continue
		}
		b, err1 := git(ctx, clone, "cat-file", "blob", p+":"+f)
		h, err2 := git(ctx, clone, "cat-file", "blob", c+":"+f)
		if err1 != nil || err2 != nil {
			return "error", f + ": cannot read both versions"
		}
		if _, bw := rbdyn.BodyGuard(f, []byte(b), []byte(h)); bw != "" {
			continue // test support refused by the body guard: kept as a file, not compiled
		}
		files = append(files, rbdyn.ParseFile{Path: f, Base: []byte(b), Head: []byte(h)})
	}
	if w := rbdyn.ParseCheck(ctx, rubyExe(), files); w != "" {
		return "parse", w
	}
	return "accepted", ""
}

var (
	reRbReasonLine = regexp.MustCompile(`\b(line|lines) [0-9]+(-[0-9]+)?`)
	reRbReasonNum  = regexp.MustCompile(`[0-9]+`)
)

// RbReasonClass reduces a guard refusal to a class: the file prefix and numbers dropped.
func RbReasonClass(why string) string {
	if i := strings.Index(why, ": "); i > 0 && !strings.Contains(why[:i], " ") {
		why = why[i+2:]
	}
	why = reRbReasonLine.ReplaceAllString(why, "$1 N")
	why = reRbReasonNum.ReplaceAllString(why, "N")
	if i := strings.Index(why, " (`"); i > 0 {
		why = why[:i]
	}
	if len(why) > 110 {
		why = why[:110]
	}
	return why
}

// ---- replay ----

// RbReplayCommit is one replayed commit.
type RbReplayCommit struct {
	C        string       `json:"c"`
	P        string       `json:"p"`
	Files    []string     `json:"files"`
	Units    int          `json:"units"`
	Static   RbSel        `json:"static"`
	Hybrid   RbSel        `json:"hybrid"`
	Record   RbRecordStat `json:"record"`
	FailAtC  []string     `json:"fail_at_c,omitempty"`
	FailAtP  []string     `json:"fail_at_p,omitempty"`
	Misses   []string     `json:"misses,omitempty"`
	VerifyMs int64        `json:"verify_ms"`
	Error    string       `json:"error,omitempty"`
}

// RunRubyReplay replays commits (newest first as given) of the T2 repo: fresh records at the parent, the selection with the
// layer, and every removed test file run alone at the commit.
func RunRubyReplay(ctx context.Context, cfg RbDynConfig, commits []string) []RbReplayCommit {
	var out []RbReplayCommit
	for _, c := range commits {
		if ctx.Err() != nil {
			break
		}
		rc := cfg.replayOne(ctx, c)
		cfg.logf("replay %s: %d files, static %d/%d, hybrid %d, removed %d, fail at C %v, at P %v, misses %v %s%s", c[:10], len(rc.Files),
			rc.Static.Selected, rc.Units, rc.Hybrid.Selected, len(rc.Hybrid.Removed), rc.FailAtC, rc.FailAtP, rc.Misses, rc.Error,
			map[bool]string{true: " skipped: " + rc.Hybrid.Skip, false: ""}[rc.Hybrid.Skip != ""])
		out = append(out, rc)
	}
	if err := cfg.rbCheckout(ctx, cfg.Repo.R); err != nil {
		cfg.logf("restore the clone at R: %v", err)
	}
	return out
}

func (cfg RbDynConfig) replayOne(ctx context.Context, c string) (rc RbReplayCommit) {
	rc.C = c
	src := cfg.clone()
	p, err := git(ctx, filepath.Join(cfg.T2.Dir, cfg.Repo.Dir), "rev-parse", c+"^1")
	if err != nil {
		rc.Error = err.Error()
		return rc
	}
	rc.P = strings.TrimSpace(p)
	if err := cfg.rbCheckout(ctx, rc.P); err != nil {
		rc.Error = err.Error()
		return rc
	}
	raw, _ := git(ctx, src, "diff", "--name-only", "--no-renames", rc.P, c)
	rc.Files = strings.Fields(raw)
	state := filepath.Join(cfg.Work, "replay-state", cfg.Repo.Dir, c[:12])
	os.RemoveAll(state)
	ix, err := cfg.open(ctx, state)
	if err != nil {
		rc.Error = err.Error()
		return rc
	}
	defer ix.Close()
	rc.Record = cfg.record(ctx, ix, true)
	if rc.Record.Error != "" {
		rc.Error = "record: " + rc.Record.Error
		return rc
	}
	st, err := ix.Select(ctx, bgraph.Change{Base: rc.P, Head: c, Dynamic: bgraph.DynamicOff})
	if err != nil {
		rc.Error = err.Error()
		return rc
	}
	hy, err := ix.Select(ctx, bgraph.Change{Base: rc.P, Head: c, Dynamic: bgraph.DynamicOn})
	if err != nil {
		rc.Error = err.Error()
		return rc
	}
	rc.Units, rc.Static, rc.Hybrid = rbUnits(st), rbSummarize(st, nil), rbSummarize(hy, nil)
	if len(rc.Hybrid.Removed) == 0 {
		return rc
	}
	units := map[string]rbdyn.Unit{}
	for _, t := range st.Tests {
		units[t.Unit] = rbdyn.Unit{ID: t.Unit, Project: t.Project, Runner: t.Runner, Config: t.Config}
	}
	t0 := time.Now()
	defer func() { rc.VerifyMs = time.Since(t0).Milliseconds() }()
	run := func(rev string, ids []string) ([]string, error) {
		if err := cfg.rbCheckout(ctx, rev); err != nil {
			return nil, err
		}
		var red []string
		for _, id := range ids {
			u, ok := units[id]
			if !ok {
				return nil, fmt.Errorf("%s: not in the static selection", id)
			}
			pass, detail, err := rbRunUnit(ctx, cfg.clone(), u)
			if err != nil {
				return nil, err
			}
			if !pass {
				red = append(red, id)
				cfg.logf("replay %s at %s: %s red: %s", c[:10], rev[:10], id, firstN(tail(detail, 400), 400))
			}
		}
		sort.Strings(red)
		return red, nil
	}
	rc.FailAtC, err = run(c, rc.Hybrid.Removed)
	if err != nil {
		rc.Error = "verify at C: " + err.Error()
		return rc
	}
	if len(rc.FailAtC) == 0 {
		return rc
	}
	rc.FailAtP, err = run(rc.P, rc.FailAtC)
	if err != nil {
		rc.Error = "verify at P: " + err.Error()
		return rc
	}
	for _, f := range rc.FailAtC {
		if !slicesContains(rc.FailAtP, f) {
			rc.Misses = append(rc.Misses, f)
		}
	}
	return rc
}

// rbRunUnit runs one test file in root as the runner adapter runs it (rbdyn.Argv: `bundle exec ruby -I... -e loader FILE`,
// `bundle exec rspec FILE`), without the collector. Passed: exit status 0.
func rbRunUnit(ctx context.Context, root string, u rbdyn.Unit) (bool, string, error) {
	argv, err := rbdyn.Argv(u, rubyExe())
	if err != nil {
		return false, "", err
	}
	out, tl, to, err := runProc(ctx, filepath.Join(root, filepath.FromSlash(u.Project)), nil, 15*time.Minute, argv[0], argv[1:]...)
	if ctx.Err() != nil {
		return false, "", ctx.Err()
	}
	if to {
		return false, "timeout: " + tl, nil
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return false, "", err
	}
	return err == nil, out + "\n" + tl, nil
}

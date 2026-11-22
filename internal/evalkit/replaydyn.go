package evalkit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/model"
)

// Phase 6 replay (docs/dynamic.md §7): recent first-parent commits of a Python repo, selected statically and with the dynamic
// layer. Records are made ONCE at R0 (the parent of the oldest replayed commit) in a separate local clone, and stay usable only
// through content keying (every recorded dependency's blob unchanged at the base, same environment). On commits whose static
// selection has no widened unit the hybrid selection must equal the static one (asserted). A sample of commits where the dynamic
// layer removed units is verified by running the removed units at C (and at P for those that fail): a failure at C that does
// not happen at P is a violation.

// DynReplayConfig configures one repo's replay.
type DynReplayConfig struct {
	T2       T2Config // Dir (checkout + venv), Manifest (for the interpreter)
	Repo     T2Repo
	Commits  int // first-parent commits ending at Repo.R
	Modes    []string
	Sample   int // commits to verify by running the removed units
	Jobs     int
	Rerecord bool
	// Fresh: also evaluate records made at each P (the parent of the replayed commit) with trust_native_env, as a CI that
	// records the base branch would provide; only for widened commits whose guards pass (mode "fresh").
	Fresh bool
}

// DynVerify is the test-run check of one replayed commit.
type DynVerify struct {
	Removed    []string `json:"removed"`              // units removed by any mode
	FailC      []string `json:"fail_c,omitempty"`     // failing at C
	FailP      []string `json:"fail_p,omitempty"`     // of those, failing at P too (pre-existing)
	Violations []string `json:"violations,omitempty"` // failing at C (twice), passing at P
	Err        string   `json:"err,omitempty"`
	Ms         int64    `json:"ms"`
}

// DynReplayCommit is one replayed commit.
type DynReplayCommit struct {
	C       string            `json:"c"`
	P       string            `json:"p"`
	Changed int               `json:"changed"`
	Units   int               `json:"units"`
	Static  DynSel            `json:"static"`
	Widened int               `json:"widened"`
	Hybrid  map[string]DynSel `json:"hybrid"`
	// AssertFail: modes whose hybrid selection differs from the static one although the static selection has no widened unit.
	AssertFail []string   `json:"assert_fail,omitempty"`
	Verify     *DynVerify `json:"verify,omitempty"`
	Err        string     `json:"err,omitempty"`
}

// DynReplayResult is one repo's replay.
type DynReplayResult struct {
	Repo    string            `json:"repo"`
	R       string            `json:"r"`
	R0      string            `json:"r0"`
	Records []DynRecordStat   `json:"records"`
	Commits []DynReplayCommit `json:"commits"`
	Ms      int64             `json:"ms"`
	Error   string            `json:"error,omitempty"`
}

// localClone makes (or reuses) a clone of src at dst sharing its objects, checked out at rev with a clean tree.
func localClone(ctx context.Context, src, dst, rev string) error {
	if _, err := os.Stat(filepath.Join(dst, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if _, err := git(ctx, filepath.Dir(dst), "-c", "core.autocrlf=false", "clone", "-q", "--shared", "--no-checkout", src, dst); err != nil {
			return err
		}
		if _, err := git(ctx, dst, "config", "core.autocrlf", "false"); err != nil {
			return err
		}
	}
	if _, err := git(ctx, dst, "checkout", "-q", "-f", "--detach", rev); err != nil {
		return err
	}
	_, err := git(ctx, dst, "clean", "-q", "-fdx")
	return err
}

// RunDynReplay replays one repo.
func RunDynReplay(ctx context.Context, cfg DynReplayConfig) DynReplayResult {
	t0 := time.Now()
	r := cfg.Repo
	res := DynReplayResult{Repo: r.Name, R: r.R}
	defer func() { res.Ms = time.Since(t0).Milliseconds() }()
	modes := DynOptions{Modes: cfg.Modes}.modes()
	root := filepath.Join(cfg.T2.Dir, r.Dir)
	out, err := git(ctx, root, "rev-list", "--first-parent", "-n", fmt.Sprint(cfg.Commits), r.R)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	cs := strings.Fields(out)
	if len(cs) == 0 {
		res.Error = "no commits"
		return res
	}
	r0, err := git(ctx, root, "rev-parse", cs[len(cs)-1]+"^1")
	if err != nil {
		res.Error = "R0: " + err.Error()
		return res
	}
	res.R0 = strings.TrimSpace(r0)
	wtR0 := filepath.Join(cfg.T2.Dir, "dyn-wt", r.Dir+"-r0")
	if err := localClone(ctx, root, wtR0, res.R0); err != nil {
		res.Error = "clone at R0: " + err.Error()
		return res
	}
	state := filepath.Join(cfg.T2.Dir, "dyn-state-replay", r.Dir)
	py := r.pyPython(&cfg.T2)
	logf := func(f string, a ...any) { cfg.T2.logf("%s replay: "+f, append([]any{r.Name}, a...)...) }
	ixs := map[string]*bgraph.Index{}
	for _, mode := range modes {
		rix, err := openDyn(ctx, wtR0, state, py, mode)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		res.Records = append(res.Records, ensureRecords(ctx, rix, mode, cfg.Rerecord, cfg.Jobs, pyDynEnv(wtR0, r.Env), logf))
		rix.Close()
		ix, err := openDyn(ctx, root, state, py, mode)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		defer ix.Close()
		ixs[mode] = ix
	}
	static := ixs[modes[0]]
	for i := len(cs) - 1; i >= 0; i-- { // oldest first
		if ctx.Err() != nil {
			res.Error = ctx.Err().Error()
			return res
		}
		c := DynReplayCommit{C: cs[i], Hybrid: map[string]DynSel{}}
		p, err := git(ctx, root, "rev-parse", cs[i]+"^1")
		if err != nil {
			c.Err = err.Error()
			res.Commits = append(res.Commits, c)
			continue
		}
		c.P = strings.TrimSpace(p)
		d, sel, err := dynSelect(ctx, static, bgraph.Change{Base: c.P, Head: c.C, Dynamic: bgraph.DynamicOff}, nil)
		if err != nil {
			c.Err = err.Error()
			res.Commits = append(res.Commits, c)
			continue
		}
		c.Static, c.Changed, c.Units, c.Widened = d, len(sel.Changed), sel.IndexStats["units"], widenedCount(sel)
		staticUnits := unitIDs(sel)
		for _, mode := range modes {
			h, hs, err := dynSelect(ctx, ixs[mode], bgraph.Change{Base: c.P, Head: c.C, Dynamic: bgraph.DynamicOn}, nil)
			if err != nil {
				c.Err = err.Error()
				break
			}
			c.Hybrid[mode] = h
			if c.Widened == 0 && (!slices.Equal(unitIDs(hs), staticUnits) || len(hs.Narrowed) > 0 || hs.Scope != sel.Scope) {
				c.AssertFail = append(c.AssertFail, mode)
			}
		}
		res.Commits = append(res.Commits, c)
	}
	logf("%d commits selected", len(res.Commits))
	if cfg.Fresh {
		freshPass(ctx, cfg, &res, root, py)
		modes = append(modes, "fresh")
	}
	if cfg.Sample > 0 {
		verifySample(ctx, cfg, &res, modes)
	}
	return res
}

func unitIDs(sel model.Selection) []string {
	var out []string
	for _, t := range sel.Tests {
		out = append(out, t.Unit)
	}
	slices.Sort(out)
	return out
}

// verifySample runs the removed units of up to cfg.Sample commits (spread over the replay) at C, then the failing ones at P.
func verifySample(ctx context.Context, cfg DynReplayConfig, res *DynReplayResult, modes []string) {
	var cand []int
	for i, c := range res.Commits {
		for _, m := range modes {
			if c.Hybrid[m].Narrowed > 0 {
				cand = append(cand, i)
				break
			}
		}
	}
	if len(cand) == 0 {
		return
	}
	pick := cand
	if len(cand) > cfg.Sample {
		pick = nil
		for k := 0; k < cfg.Sample; k++ {
			pick = append(pick, cand[k*len(cand)/cfg.Sample])
		}
	}
	r := cfg.Repo
	root := filepath.Join(cfg.T2.Dir, r.Dir)
	wt := filepath.Join(cfg.T2.Dir, "dyn-wt", r.Dir+"-verify")
	vr := r
	vr.Dir = r.Dir + "-dynverify" // scratch directory of pySuite: cfg.Dir/tmp/<Dir>
	for _, i := range pick {
		c := &res.Commits[i]
		v := &DynVerify{}
		c.Verify = v
		t0 := time.Now()
		removed := removedUnits(c)
		v.Removed = removed
		run := func(rev string, files []string) (map[string]string, error) {
			if err := localClone(ctx, root, wt, rev); err != nil {
				return nil, err
			}
			// After the checkout: whether the layout has src/ decides PYTHONPATH (the clone's sources must shadow the editable
			// install of the main checkout).
			vr.Env = append(slices.Clone(r.Env), pyDynEnv(wt, nil)...)
			pr, _, to, err := vr.pySuite(ctx, &cfg.T2, wt, 15*time.Minute, files, nil)
			if err != nil {
				return nil, err
			}
			if to {
				return nil, fmt.Errorf("timeout at %s", short(rev))
			}
			if pr.Session {
				return nil, fmt.Errorf("session failure at %s: %s", short(rev), pr.Detail)
			}
			return pr.Files, nil
		}
		atC, err := run(c.C, removed)
		if err != nil {
			v.Err = err.Error()
			continue
		}
		for _, f := range removed {
			if atC[f] == "failed" {
				v.FailC = append(v.FailC, f)
			}
		}
		if len(v.FailC) > 0 {
			atP, err := run(c.P, removed) // the same set: failures can depend on which files run together
			if err != nil {
				v.Err = err.Error()
				continue
			}
			var suspect []string
			for _, f := range v.FailC {
				if atP[f] == "failed" {
					v.FailP = append(v.FailP, f)
				} else {
					suspect = append(suspect, f)
				}
			}
			if len(suspect) > 0 { // confirm at C (flakiness)
				again, err := run(c.C, suspect)
				if err != nil {
					v.Err = err.Error()
					continue
				}
				for _, f := range suspect {
					if again[f] == "failed" {
						v.Violations = append(v.Violations, f)
					}
				}
			}
		}
		v.Ms = time.Since(t0).Milliseconds()
		cfg.T2.logf("%s replay: verify %s removed %d, fail at C %d, at P %d, violations %d", r.Name, short(c.C), len(v.Removed), len(v.FailC), len(v.FailP), len(v.Violations))
	}
}

// removedUnits is the union of the units each mode removed on c.
func removedUnits(c *DynReplayCommit) []string {
	var out []string
	for _, h := range c.Hybrid {
		out = append(out, h.Removed...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// freshPass records at P (trust_native_env) for every widened commit whose guards pass and selects it again (mode "fresh").
func freshPass(ctx context.Context, cfg DynReplayConfig, res *DynReplayResult, root, py string) {
	r := cfg.Repo
	wt := filepath.Join(cfg.T2.Dir, "dyn-wt", r.Dir+"-fresh")
	state := filepath.Join(cfg.T2.Dir, "dyn-state-fresh", r.Dir)
	n := 0
	for i := range res.Commits {
		c := &res.Commits[i]
		h, ok := c.Hybrid["trust"]
		if c.Err != "" || c.Widened == 0 || !ok || (h.Skip != "" && SkipClass(h.Skip) != "no records") {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if err := localClone(ctx, root, wt, c.P); err != nil {
			c.Err = "fresh clone: " + err.Error()
			continue
		}
		rix, err := openDyn(ctx, wt, state, py, "trust")
		if err != nil {
			c.Err = err.Error()
			continue
		}
		t0 := time.Now()
		_, err = rix.Record(ctx, bgraph.RecordOptions{Jobs: cfg.Jobs, Timeout: 10 * time.Minute, Env: pyDynEnv(wt, r.Env)})
		rix.Close()
		if err != nil {
			c.Err = "fresh record: " + err.Error()
			continue
		}
		ix, err := openDyn(ctx, root, state, py, "trust")
		if err != nil {
			c.Err = err.Error()
			continue
		}
		d, _, err := dynSelect(ctx, ix, bgraph.Change{Base: c.P, Head: c.C, Dynamic: bgraph.DynamicOn}, nil)
		ix.Close()
		if err != nil {
			c.Err = err.Error()
			continue
		}
		c.Hybrid["fresh"] = d
		n++
		cfg.T2.logf("%s replay: fresh %s recorded in %s, selected %d/%d (narrowed %d)", r.Name, short(c.C), time.Since(t0).Round(time.Second), d.Selected, c.Units, d.Narrowed)
	}
	cfg.T2.logf("%s replay: fresh records for %d commits", r.Name, n)
}

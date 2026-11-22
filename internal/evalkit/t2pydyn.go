package evalkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/model"
)

// Phase 6 (docs/dynamic.md §7): the hybrid configuration (static graph + dynamic records) checked against the Python T2 mutation
// oracle. The oracle (oracle.jsonl, the whole suite run on R+mutant) never uses the collector; the records are made once at R with
// `bgraph record` (strict default and trust_native_env) and every mutant is selected statically and with the dynamic layer on.

// DynModes are the collector settings evaluated: the strict default and trust_native_env.
var DynModes = []string{"default", "trust"}

// DynOptions configures the hybrid pass.
type DynOptions struct {
	Modes    []string // subset of DynModes; empty: both
	Rerecord bool     // record again even when records of the current environment exist
	Jobs     int      // record parallelism
}

func (o DynOptions) modes() []string {
	if len(o.Modes) == 0 {
		return DynModes
	}
	return o.Modes
}

// dynStateDir is where the hybrid pass keeps the records of one repo (a directory of its own, next to the static T2 state).
func dynStateDir(t2Dir, repoDir string) string { return filepath.Join(t2Dir, "dyn-state", repoDir) }

// pyDynEnv is the extra environment of recorded runs: the checkout's sources first on sys.path (they shadow an editable
// install of another checkout of the same project, e.g. when recording in a worktree).
func pyDynEnv(root string, extra []string) []string {
	pp := root
	if st, err := os.Stat(filepath.Join(root, "src")); err == nil && st.IsDir() {
		pp = filepath.Join(root, "src")
	}
	return append([]string{"PYTHONPATH=" + pp}, extra...)
}

// DynRecordStat summarizes the records of one repo and mode.
type DynRecordStat struct {
	Mode     string                `json:"mode"`
	Reused   bool                  `json:"reused"` // records of the current environment were already there
	Summary  *bgraph.RecordSummary `json:"summary,omitempty"`
	Status   bgraph.DynamicStatus  `json:"status"`
	RecordMs int64                 `json:"record_ms"`
	Error    string                `json:"error,omitempty"`
}

// openDyn opens an index on root with the repo's interpreter and the mode's trust setting.
func openDyn(ctx context.Context, root, stateDir, python, mode string) (*bgraph.Index, error) {
	return bgraph.Open(ctx, root, bgraph.Options{StateDir: stateDir, Python: python, TrustNativeEnv: mode == "trust"})
}

// ensureRecords records root (HEAD) for mode unless usable records of the current environment exist.
func ensureRecords(ctx context.Context, ix *bgraph.Index, mode string, rerecord bool, jobs int, env []string, logf func(string, ...any)) DynRecordStat {
	st := DynRecordStat{Mode: mode}
	cur, err := ix.DynamicStatus(ctx)
	if err == nil && cur.CurrentEnv > 0 && !rerecord {
		st.Reused, st.Status = true, cur
		return st
	}
	t0 := time.Now()
	sum, err := ix.Record(ctx, bgraph.RecordOptions{Jobs: jobs, Timeout: 10 * time.Minute, Env: env})
	st.RecordMs = time.Since(t0).Milliseconds()
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Summary = &sum
	if logf != nil {
		logf("record %s: %d units, %d complete, %d incomplete, %d failed in %s", mode, sum.Units, sum.Complete, sum.Incomplete, sum.Failed,
			time.Duration(st.RecordMs)*time.Millisecond)
	}
	st.Status, _ = ix.DynamicStatus(ctx)
	return st
}

// DynSel is one selection with the dynamic layer's effect.
type DynSel struct {
	Scope    string         `json:"scope"`
	Selected int            `json:"selected"`
	Narrowed int            `json:"narrowed"`
	Removed  []string       `json:"removed,omitempty"` // the narrowed units
	Skip     string         `json:"skip,omitempty"`    // dynamic_skipped detail (guard, no records, interpreter)
	Kept     map[string]int `json:"kept,omitempty"`    // why widened units were kept (reason class -> count)
	Missed   []string       `json:"missed,omitempty"`  // oracle-red files outside this selection (violation)
}

// keptClass reduces a SelectedTest.Dynamic note to a class for histograms.
func keptClass(note string) string {
	note = strings.TrimPrefix(note, "kept by the dynamic layer: ")
	switch {
	case strings.HasPrefix(note, "recorded dependencies include"):
		return "deps include a changed file"
	case strings.Contains(note, "stale"):
		return "stale record"
	case strings.Contains(note, "incomplete record"):
		return "incomplete record"
	case strings.Contains(note, "did not pass"):
		return "failed record"
	case strings.HasPrefix(note, "no record for the current environment"):
		return "other environment"
	case strings.HasPrefix(note, "no record"):
		return "no record"
	case strings.HasPrefix(note, "whole-project"):
		return "whole-project unit"
	}
	return note
}

// dynSelect runs one selection and summarizes the dynamic layer's effect; red are the oracle's red files (nil: not checked).
func dynSelect(ctx context.Context, ix *bgraph.Index, ch bgraph.Change, red []string) (DynSel, model.Selection, error) {
	sel, err := ix.Select(ctx, ch)
	if err != nil {
		return DynSel{}, sel, err
	}
	d := DynSel{Scope: string(sel.Scope), Selected: len(sel.Tests), Narrowed: len(sel.Narrowed)}
	for _, n := range sel.Narrowed {
		d.Removed = append(d.Removed, n.Unit)
	}
	for _, w := range sel.Warnings {
		if w.Reason == model.ReasonDynamicSkipped {
			if d.Skip != "" {
				d.Skip += "; "
			}
			d.Skip += w.Detail
		}
	}
	for _, t := range sel.Tests {
		if t.Dynamic != "" {
			if d.Kept == nil {
				d.Kept = map[string]int{}
			}
			d.Kept[keptClass(t.Dynamic)]++
		}
	}
	if sel.Scope != model.TierFull {
		for _, f := range red {
			if !selCovers(sel, f) {
				d.Missed = append(d.Missed, f)
			}
		}
	}
	return d, sel, nil
}

func widenedCount(sel model.Selection) int {
	n := 0
	for _, t := range sel.Tests {
		if t.Widened {
			n++
		}
	}
	return n
}

// DynCheck is one mutant: the static selection and the hybrid selection per mode.
type DynCheck struct {
	Mutant  string            `json:"mutant"`
	File    string            `json:"file"`
	Status  string            `json:"status"` // oracle status: killed | survived
	Red     []string          `json:"red,omitempty"`
	Units   int               `json:"units"`
	Static  DynSel            `json:"static"`
	Widened int               `json:"widened"` // widened units of the static selection
	Hybrid  map[string]DynSel `json:"hybrid"`
	Err     string            `json:"err,omitempty"`
}

// DynRepoResult is the hybrid pass over one repo.
type DynRepoResult struct {
	Repo    string          `json:"repo"`
	Set     string          `json:"set"` // T2 directory
	R       string          `json:"r"`
	Records []DynRecordStat `json:"records"`
	Checks  []DynCheck      `json:"checks"`
	Error   string          `json:"error,omitempty"`
}

// RunT2Dyn runs the hybrid pass for the repos of cfg against the cached oracle (cfg.Dir/oracle.jsonl). Repos run one after another
// (recording is parallel inside a repo).
func RunT2Dyn(ctx context.Context, cfg T2Config, opt DynOptions) ([]DynRepoResult, error) {
	store := &OracleStore{Path: cfg.oraclePath()}
	recs, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("oracle %s: %w", store.Path, err)
	}
	latest := map[string]T2Record{}
	for _, x := range recs {
		latest[x.key()] = x // later lines win (re-confirmations)
	}
	var out []DynRepoResult
	for _, r := range cfg.selected() {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if r.Eco != "python" {
			continue
		}
		rr := DynRepoResult{Repo: r.Name, Set: cfg.Dir, R: r.R}
		root := filepath.Join(cfg.Dir, r.Dir)
		if err := r.ensureClean(ctx, root); err != nil {
			rr.Error = err.Error()
			out = append(out, rr)
			continue
		}
		var mine []T2Record
		for _, x := range latest {
			if x.Repo == r.Name && x.R == r.R && (x.Status == "killed" || x.Status == "survived") {
				mine = append(mine, x)
			}
		}
		sort.Slice(mine, func(i, j int) bool { return mine[i].Mutant.ID < mine[j].Mutant.ID })
		state := dynStateDir(cfg.Dir, r.Dir)
		py := r.pyPython(&cfg)
		ixs := map[string]*bgraph.Index{}
		for _, mode := range opt.modes() {
			ix, err := openDyn(ctx, root, state, py, mode)
			if err != nil {
				rr.Error = err.Error()
				break
			}
			defer ix.Close()
			ixs[mode] = ix
			logf := func(f string, a ...any) { cfg.logf("%s: "+f, append([]any{r.Name}, a...)...) }
			rr.Records = append(rr.Records, ensureRecords(ctx, ix, mode, opt.Rerecord, opt.Jobs, pyDynEnv(root, r.Env), logf))
		}
		if rr.Error == "" {
			static := ixs[opt.modes()[0]]
			for _, rec := range mine {
				c := DynCheck{Mutant: rec.Mutant.ID, File: rec.Mutant.File, Status: rec.Status, Red: rec.Red, Hybrid: map[string]DynSel{}}
				m, _, err := r.MutantCommit(ctx, root, filepath.Join(cfg.Dir, "idx"), rec.Mutant)
				if err != nil {
					c.Err = err.Error()
					rr.Checks = append(rr.Checks, c)
					continue
				}
				d, sel, err := dynSelect(ctx, static, bgraph.Change{Base: r.R, Head: m, Dynamic: bgraph.DynamicOff}, rec.Red)
				if err != nil {
					c.Err = err.Error()
					rr.Checks = append(rr.Checks, c)
					continue
				}
				c.Static, c.Units, c.Widened = d, sel.IndexStats["units"], widenedCount(sel)
				for _, mode := range opt.modes() {
					h, _, err := dynSelect(ctx, ixs[mode], bgraph.Change{Base: r.R, Head: m, Dynamic: bgraph.DynamicOn}, rec.Red)
					if err != nil {
						c.Err = err.Error()
						break
					}
					c.Hybrid[mode] = h
				}
				rr.Checks = append(rr.Checks, c)
			}
			cfg.logf("%s: %d mutants checked", r.Name, len(rr.Checks))
		}
		if err := r.ensureClean(ctx, root); err != nil && rr.Error == "" {
			rr.Error = err.Error()
		}
		out = append(out, rr)
	}
	return out, nil
}

// PrepareDynMutants writes cfg.Dir/mutants-<dir>.json for r with mutants only in the source files whose modification makes the
// static layer widen (a widened unit appears when a comment is appended to the file), so that the hybrid configuration is
// exercised. The oracle builder (bgraph-eval t2 --ecosystem python --run-tests) then uses that file. Returns the target files.
func PrepareDynMutants(ctx context.Context, cfg T2Config, r T2Repo) ([]string, error) {
	root := filepath.Join(cfg.Dir, r.Dir)
	if err := r.ensureClean(ctx, root); err != nil {
		return nil, err
	}
	srcs, err := r.pyCandidateSources(ctx, root)
	if err != nil {
		return nil, err
	}
	ix, err := bgraph.Open(ctx, root, bgraph.Options{StateDir: filepath.Join(cfg.Dir, "state", r.Dir)})
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	var keep []RustMutantSource
	var targets []string
	for _, s := range srcs {
		probe := T2Mutant{ID: "probe-" + seedHash(s.Path)[:8], File: s.Path, Start: len(s.Data), End: len(s.Data), Repl: "\n# bgraph widening probe\n", Blob: s.Blob}
		m, _, err := r.MutantCommit(ctx, root, filepath.Join(cfg.Dir, "idx"), probe)
		if err != nil {
			return nil, err
		}
		sel, err := ix.Select(ctx, bgraph.Change{Base: r.R, Head: m, Dynamic: bgraph.DynamicOff})
		if err != nil {
			return nil, err
		}
		if sel.Scope != model.TierFull && widenedCount(sel) > 0 {
			keep = append(keep, s)
			targets = append(targets, s.Path)
		}
	}
	if len(keep) == 0 {
		return nil, fmt.Errorf("%s: no source file whose edit widens", r.Name)
	}
	muts := GenPyMutants(keep, r.Seed, r.Dir, r.rustTries())
	b, _ := json.MarshalIndent(map[string]any{"r": r.R, "seed": r.Seed, "count": r.rustTries(), "mutants": muts, "targets": targets}, "", " ")
	return targets, os.WriteFile(filepath.Join(cfg.Dir, "mutants-"+r.Dir+".json"), b, 0o644)
}

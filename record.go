package bgraph

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/dynamic/pydyn"
	"github.com/qxbao/bgraph/internal/eco/python"
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/selector"
)

// DynamicRecord is one record of the dynamic layer (docs/dynamic.md §3).
type DynamicRecord = dynamic.Record

// RecordOptions configures Index.Record.
type RecordOptions struct {
	Units      string        // glob on unit IDs (test files); empty: every unit
	Jobs       int           // parallel units; <= 0: half the CPUs
	Timeout    time.Duration // per unit; <= 0: 10 minutes
	AllowDirty bool          // record a working tree that differs from HEAD (dependencies keyed by the content on disk)
	// Out: write the records to this JSONL file (to import elsewhere with ImportRecords) instead of the state directory.
	Out string
	// Env: extra environment variables for the test processes (e.g. PYTHONPATH=<checkout>/src so that a worktree's sources
	// shadow an editable install of another checkout). They are not part of the environment fingerprint.
	Env []string
	// Progress is called after each unit.
	Progress func(done, total int, rec DynamicRecord, elapsed time.Duration, tail string)
}

// RecordSummary is the outcome of Index.Record.
type RecordSummary struct {
	Units      int            `json:"units"`
	Recorded   int            `json:"recorded"`
	Complete   int            `json:"complete"`   // complete and passed: usable at the recorded commit
	Incomplete int            `json:"incomplete"` // not complete (passed or not)
	Failed     int            `json:"failed"`     // the run did not pass
	Reasons    map[string]int `json:"reasons"`    // incomplete reasons (one count per record and reason)
	Skipped    []string       `json:"skipped,omitempty"`
	Commit     string         `json:"commit"`
	Env        string         `json:"env"`
	Store      string         `json:"store"`
	Elapsed    time.Duration  `json:"elapsed"`
	Warnings   []string       `json:"warnings,omitempty"`
}

// recordUnits lists the Python test units of the working tree.
func (ix *Index) recordUnits(ctx context.Context) ([]pydyn.Unit, []string, *analyzed, error) {
	an, err := ix.analyzeRev(ctx, "")
	if err != nil {
		return nil, nil, nil, err
	}
	var units []model.TestUnit
	for _, a := range an.Analyses {
		for _, u := range a.Tests {
			if u.Eco == python.Name {
				units = append(units, u)
			}
		}
	}
	var out []pydyn.Unit
	var skipped []string
	for _, u := range selector.DedupUnits(units) {
		if u.Whole {
			skipped = append(skipped, u.ID+": whole-project unit (test files unknown)")
			continue
		}
		if !an.Tree.Has(u.ID) {
			skipped = append(skipped, u.ID+": directory unit (pytest collection hook; its items are not files of the record)")
			continue
		}
		out = append(out, pydyn.Unit{ID: u.ID, Project: u.Project, Runner: u.Runner, Config: u.Config, Files: u.Files})
	}
	return out, skipped, an, nil
}

// Record runs every Python test unit of the working tree in its own process with the collector and stores the records
// (docs/dynamic.md §4). It refuses a working tree that differs from HEAD unless AllowDirty.
func (ix *Index) Record(ctx context.Context, opt RecordOptions) (RecordSummary, error) {
	t0 := time.Now()
	var sum RecordSummary
	head, err := ix.repo.RevParse(ctx, "HEAD")
	if err != nil {
		return sum, err
	}
	change, wt, err := ix.repo.Snapshot(ctx, head)
	if err != nil {
		return sum, err
	}
	if len(change.Files) > 0 && !opt.AllowDirty {
		ex := make([]string, 0, 3)
		for i, f := range change.Files {
			if i == 3 {
				break
			}
			ex = append(ex, f.Path)
		}
		return sum, fmt.Errorf("the working tree differs from HEAD (%d files, e.g. %s): commit or stash, or use --allow-dirty",
			len(change.Files), strings.Join(ex, ", "))
	}
	if opt.Out == "" && ix.stateDir == "" {
		return sum, errors.New("no state directory to store records in (use --out)")
	}
	rf, err := loadRules(wt)
	if err != nil {
		return sum, err
	}
	py, trust := ix.dynamicConfig(rf)
	exe, err := pydyn.ResolvePython(ix.repo.Root, py)
	if err != nil {
		return sum, fmt.Errorf("python interpreter: %w", err)
	}
	in, err := ix.probes.Get(ctx, exe, ix.repo.Root, ix.tmpDir())
	if err != nil {
		return sum, fmt.Errorf("python interpreter: %w", err)
	}
	collector := pydyn.Collector(trust)
	env := pydyn.Fingerprint(collector, in.Lines, wt)
	units, skipped, _, err := ix.recordUnits(ctx)
	if err != nil {
		return sum, err
	}
	if opt.Units != "" {
		pat, err := glob.Compile(opt.Units)
		if err != nil {
			return sum, fmt.Errorf("--units: %w", err)
		}
		units = slices.DeleteFunc(units, func(u pydyn.Unit) bool { return !pat.Match(u.ID) })
	}
	sum.Units, sum.Skipped, sum.Commit, sum.Env = len(units), skipped, head, env
	if len(units) == 0 {
		return sum, errors.New("no Python test unit to record")
	}
	jobs := opt.Jobs
	if jobs <= 0 {
		jobs = pydyn.DefaultJobs()
	}
	now := time.Now()
	byID := map[string]pydyn.Unit{}
	for _, u := range units {
		byID[u.ID] = u
	}
	ropt := pydyn.RunOptions{Python: exe, Root: ix.repo.Root, TmpDir: ix.tmpDir(), Timeout: opt.Timeout, Jobs: jobs, TrustNative: trust, Env: opt.Env}
	if opt.Progress != nil {
		ropt.Progress = func(done, total int, o pydyn.Outcome) {
			opt.Progress(done, total, pydyn.RecordOf(o, byID[o.Unit], wt, ix.repo.Root, env, collector, head, now), o.Elapsed, o.Tail)
		}
	}
	outs, err := pydyn.Collect(ctx, ropt, units)
	if err != nil {
		return sum, err
	}
	sum.Reasons = map[string]int{}
	var recs []dynamic.Record
	for i, o := range outs {
		r := pydyn.RecordOf(o, units[i], wt, ix.repo.Root, env, collector, head, now)
		recs = append(recs, r)
		sum.Recorded++
		if !r.Pass {
			sum.Failed++
		}
		if !r.Complete {
			sum.Incomplete++
			for _, why := range strings.Split(r.Incomplete, "; ") {
				sum.Reasons[reasonKey(why)]++
			}
		} else if r.Pass {
			sum.Complete++
		}
	}
	if opt.Out != "" {
		sum.Store = opt.Out
		err = dynamic.Save(opt.Out, dynamic.Merge(nil, recs))
	} else {
		sum.Store = dynamic.StorePath(ix.stateDir, python.Name)
		sum.Warnings, err = dynamic.Add(sum.Store, recs)
	}
	sum.Elapsed = time.Since(t0)
	return sum, err
}

// reasonKey drops the file name from per-file reasons so that the histogram groups them.
func reasonKey(why string) string {
	for _, pre := range []string{"reads untracked file ", "cannot hash ", "the unit's own file "} {
		if strings.HasPrefix(why, pre) {
			return strings.TrimSpace(pre)
		}
	}
	return why
}

// ImportRecords merges the records of a JSONL file (made by Record with Out, typically on CI) into the state directory.
func (ix *Index) ImportRecords(file string) (int, []string, error) {
	if ix.stateDir == "" {
		return 0, nil, errors.New("no state directory")
	}
	if _, err := os.Stat(file); err != nil {
		return 0, nil, err
	}
	recs, warn := dynamic.Load(file)
	byEco := map[string][]dynamic.Record{}
	for _, r := range recs {
		if !dynamic.Supported(r.Eco) {
			warn = append(warn, fmt.Sprintf("%s: ecosystem %q has no dynamic layer (record ignored)", r.Unit, r.Eco))
			continue
		}
		byEco[r.Eco] = append(byEco[r.Eco], r)
	}
	n := 0
	for eco, rs := range byEco {
		w, err := dynamic.Add(dynamic.StorePath(ix.stateDir, eco), rs)
		warn = append(warn, w...)
		if err != nil {
			return n, warn, err
		}
		n += len(rs)
	}
	return n, warn, nil
}

// DynamicStatus describes the record store of one ecosystem against HEAD.
type DynamicStatus struct {
	Eco        string         `json:"eco"`
	Store      string         `json:"store"`
	Records    int            `json:"records"`
	Units      int            `json:"units"`       // distinct units with a record
	TestUnits  int            `json:"test_units"`  // units of the ecosystem in the working tree
	CurrentEnv int            `json:"current_env"` // records made in the current environment
	Usable     int            `json:"usable"`      // usable at HEAD (complete, passed, current env and collector, deps equal at HEAD)
	Unusable   map[string]int `json:"unusable"`    // why the current-environment records are not usable at HEAD
	Reasons    map[string]int `json:"reasons"`     // incomplete reasons over all records
	Env        string         `json:"env,omitempty"`
	EnvError   string         `json:"env_error,omitempty"`
	Warnings   []string       `json:"warnings,omitempty"`
}

// DynamicStatus reports the Python record store: records, how many are usable at HEAD, incomplete reasons.
func (ix *Index) DynamicStatus(ctx context.Context) (DynamicStatus, error) {
	st := DynamicStatus{Eco: python.Name, Unusable: map[string]int{}, Reasons: map[string]int{}}
	if ix.stateDir == "" {
		return st, errors.New("no state directory")
	}
	st.Store = dynamic.StorePath(ix.stateDir, python.Name)
	recs, warn := dynamic.Load(st.Store)
	st.Warnings = warn
	st.Records = len(recs)
	head, err := ix.repo.RevParse(ctx, "HEAD")
	if err != nil {
		return st, err
	}
	ht, err := ix.memo.commitTree(ctx, ix.repo, head)
	if err != nil {
		return st, err
	}
	an, err := ix.analyzeRev(ctx, head)
	if err != nil {
		return st, err
	}
	for _, a := range an.Analyses {
		for _, u := range a.Tests {
			if u.Eco == python.Name && !u.Whole {
				st.TestUnits++
			}
		}
	}
	env, collector, err := ix.pythonEnv(ctx, an.Rules, ht)
	if err != nil {
		st.EnvError = err.Error()
	}
	st.Env = env
	units := map[string]bool{}
	p := dynamic.Params{Eco: python.Name, Collector: collector, Env: env, BaseOID: func(p string) string {
		if !ht.Has(p) {
			return ""
		}
		return ht.Key(p)
	}}
	for _, r := range recs {
		units[r.Unit] = true
		if !r.Complete {
			for _, why := range strings.Split(r.Incomplete, "; ") {
				st.Reasons[reasonKey(why)]++
			}
		}
		if env == "" || r.Env != env {
			continue
		}
		st.CurrentEnv++
		why := dynamic.Usable(r, []string{r.Unit}, p)
		if why == "" {
			st.Usable++
			continue
		}
		if strings.HasPrefix(why, "incomplete record: ") {
			why = "incomplete record"
		} else if strings.HasPrefix(why, "stale: ") {
			why = "stale (a dependency changed since)"
		}
		st.Unusable[why]++
	}
	st.Units = len(units)
	return st, nil
}

// SortedReasons returns the keys of a reason histogram, most frequent first (for printing).
func SortedReasons(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

package bgraph

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/dynamic/dotnetdyn"
	"github.com/qxbao/bgraph/internal/dynamic/nodedyn"
	"github.com/qxbao/bgraph/internal/dynamic/phpdyn"
	"github.com/qxbao/bgraph/internal/dynamic/pydyn"
	"github.com/qxbao/bgraph/internal/dynamic/rbdyn"
	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/jvm"
	"github.com/qxbao/bgraph/internal/eco/node"
	"github.com/qxbao/bgraph/internal/eco/python"
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/selector"
	"github.com/qxbao/bgraph/internal/tree"
)

// DynamicRecord is one record of the dynamic layer (docs/dynamic-layer.md §3).
type DynamicRecord = dynamic.Record

// RecordOptions configures Index.Record.
type RecordOptions struct {
	// Eco: the ecosystem to record ("python", "node", "jvm", "dotnet", "ruby" or "php"). Empty: the ecosystems of bgraph.toml [dynamic_layer] ecosystems when
	// set, otherwise every ecosystem with a dynamic layer that has test units.
	Eco        string
	Units      string        // glob on unit IDs (test files); empty: every unit
	Jobs       int           // parallel units; <= 0: half the CPUs (Python), a quarter (Node)
	Timeout    time.Duration // per unit; <= 0: 10 minutes
	AllowDirty bool          // record a working tree that differs from HEAD (dependencies keyed by the content on disk)
	// Out: write the records to this JSONL file (to import elsewhere with ImportRecords) instead of the state directory.
	Out string
	// Env: extra environment variables for the test processes (e.g. PYTHONPATH=<checkout>/src so that a worktree's sources
	// shadow an editable install of another checkout). They are not part of the environment fingerprint.
	Env []string
	// BuildArgs: extra Maven/Gradle arguments of the JVM recording build (e.g. -Dmaven.repo.local=...), or extra arguments of
	// `dotnet build` / `dotnet test` of the .NET recording (e.g. -c Release).
	BuildArgs []string
	// KeepLogs keeps the JVM collector logs (for debugging; they are removed by default).
	KeepLogs bool
	// Progress is called after each unit.
	Progress func(done, total int, rec DynamicRecord, elapsed time.Duration, tail string)
}

// RecordSummary is the outcome of Index.Record.
type RecordSummary struct {
	Ecos       []string       `json:"ecos"`
	Units      int            `json:"units"`
	Recorded   int            `json:"recorded"`
	Complete   int            `json:"complete"`   // complete and passed: usable at the recorded commit
	Incomplete int            `json:"incomplete"` // not complete (passed or not)
	Failed     int            `json:"failed"`     // the run did not pass
	Reasons    map[string]int `json:"reasons"`    // incomplete reasons (one count per record and reason)
	Skipped    []string       `json:"skipped,omitempty"`
	Commit     string         `json:"commit"`
	Env        string         `json:"env"` // fingerprint of the last recorded ecosystem
	Store      string         `json:"store"`
	Elapsed    time.Duration  `json:"elapsed"`
	Warnings   []string       `json:"warnings,omitempty"`
	// Toolchain: allowlisted native addons loaded by the recorded Node units, with their number of records
	// (docs/dynamic-layer.md §8.3).
	Toolchain map[string]int `json:"toolchain,omitempty"`
}

// recordUnits lists the Python test units of the working tree.
func (ix *Index) recordUnits(an *analyzed) ([]pydyn.Unit, []string) {
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
	return out, skipped
}

// nodeRecordUnits lists the Node test units of the working tree, with the incomplete reasons known before running them
// (docs/dynamic-layer.md §8.3 rule 4: a vite/vitest config with an import bgraph cannot resolve; a jest config naming a hasteImpl).
func (ix *Index) nodeRecordUnits(an *analyzed) ([]nodedyn.Unit, []string) {
	var units []model.TestUnit
	var badVite, hasteImpl []string // directories of such configs
	for _, a := range an.Analyses {
		for _, u := range a.Tests {
			if u.Eco == node.Name {
				units = append(units, u)
			}
		}
		for _, un := range a.Unresolved {
			if node.ViteConfig(un.File) {
				badVite = append(badVite, path.Dir(un.File))
			}
		}
	}
	for _, p := range an.Tree.Files() {
		if node.InNodeModules(p) || !(node.JestConfigFile(p) || path.Base(p) == "package.json") {
			continue
		}
		if b, err := an.Tree.Read(p); err == nil && strings.Contains(string(b), "hasteImplModulePath") {
			hasteImpl = append(hasteImpl, path.Dir(p))
		}
	}
	related := func(proj string, dirs []string) bool {
		for _, d := range dirs {
			if eco.Within(proj, d) || eco.Within(d, proj) {
				return true
			}
		}
		return false
	}
	var out []nodedyn.Unit
	var skipped []string
	for _, u := range selector.DedupUnits(units) {
		switch {
		case u.Whole:
			skipped = append(skipped, u.ID+": whole-project unit (test files unknown)")
			continue
		case u.Runner == "playwright":
			skipped = append(skipped, u.ID+": browser runner (playwright)")
			continue
		}
		nu := nodedyn.Unit{ID: u.ID, Project: u.Project, Runner: u.Runner, Files: u.Files}
		if u.Runner == "vitest" && related(u.Project, badVite) {
			nu.Incomplete = append(nu.Incomplete, "the vite/vitest configuration has imports bgraph cannot resolve (bundled natively)")
		}
		if u.Runner == "jest" && related(u.Project, hasteImpl) {
			nu.Incomplete = append(nu.Incomplete, "jest hasteImplModulePath (content-derived haste names)")
		}
		out = append(out, nu)
	}
	return out, skipped
}

// recordEcos returns the ecosystems Record records.
func recordEcos(opt RecordOptions, rf *rules.File, an *analyzed) ([]string, error) {
	if opt.Eco != "" {
		if !dynamic.Supported(opt.Eco) {
			return nil, fmt.Errorf("ecosystem %q has no dynamic layer", opt.Eco)
		}
		return []string{opt.Eco}, nil
	}
	if rf != nil && len(rf.Dynamic.Ecosystems) > 0 {
		return rf.Dynamic.Ecosystems, nil
	}
	var ecos []string
	for _, e := range rules.DynamicEcosystems {
		for _, a := range an.Analyses {
			if slices.ContainsFunc(a.Tests, func(u model.TestUnit) bool { return u.Eco == e }) {
				ecos = append(ecos, e)
				break
			}
		}
	}
	return ecos, nil
}

// Record runs every test unit of the working tree in its own process (tree) with the collector of its ecosystem and stores the
// records (docs/dynamic-layer.md §4, §8). It refuses a working tree that differs from HEAD unless AllowDirty.
func (ix *Index) Record(ctx context.Context, opt RecordOptions) (RecordSummary, error) {
	t0 := time.Now()
	sum := RecordSummary{Reasons: map[string]int{}}
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
	an, err := ix.analyzeRev(ctx, "")
	if err != nil {
		return sum, err
	}
	keep := func(string) bool { return true }
	if opt.Units != "" {
		pat, err := glob.Compile(opt.Units)
		if err != nil {
			return sum, fmt.Errorf("--units: %w", err)
		}
		keep = pat.Match
	}
	ecos, err := recordEcos(opt, rf, an)
	if err != nil {
		return sum, err
	}
	sum.Commit = head
	now := time.Now()
	stores := map[string][]dynamic.Record{}
	var all []dynamic.Record
	for _, e := range ecos {
		var recs []dynamic.Record
		var skipped []string
		switch e {
		case python.Name:
			recs, skipped, err = ix.recordPython(ctx, opt, rf, an, wt, head, now, keep, &sum)
		case node.Name:
			recs, skipped, err = ix.recordNode(ctx, opt, rf, an, wt, head, now, keep, &sum)
		case jvm.Name:
			recs, skipped, err = ix.recordJVM(ctx, opt, rf, an, wt, head, now, keep, &sum)
		case dotnetdyn.Eco:
			recs, skipped, err = ix.recordDotnet(ctx, opt, an, wt, head, now, keep, &sum)
		case rbdyn.Eco:
			recs, skipped, err = ix.recordRuby(ctx, opt, rf, an, wt, head, now, keep, &sum)
		case phpdyn.Eco:
			recs, skipped, err = ix.recordPHP(ctx, opt, rf, an, wt, head, now, keep, &sum)
		}
		sum.Skipped = append(sum.Skipped, skipped...)
		if err != nil {
			return sum, err
		}
		if len(recs) > 0 {
			sum.Ecos = append(sum.Ecos, e)
			stores[e] = recs
			all = append(all, recs...)
		}
	}
	if sum.Units == 0 {
		return sum, errors.New("no test unit to record")
	}
	for _, r := range all {
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
		for _, t := range r.Toolchain {
			if sum.Toolchain == nil {
				sum.Toolchain = map[string]int{}
			}
			sum.Toolchain[t]++
		}
	}
	if opt.Out != "" {
		sum.Store = opt.Out
		err = dynamic.Save(opt.Out, dynamic.Merge(nil, all))
	} else {
		var paths []string
		for _, e := range sum.Ecos {
			p := dynamic.StorePath(ix.stateDir, e)
			paths = append(paths, p)
			w, aerr := dynamic.Add(p, stores[e])
			sum.Warnings = append(sum.Warnings, w...)
			if aerr != nil && err == nil {
				err = aerr
			}
		}
		sum.Store = strings.Join(paths, ", ")
	}
	sum.Elapsed = time.Since(t0)
	return sum, err
}

// recordPython records the Python units (docs/dynamic-layer.md §4).
func (ix *Index) recordPython(ctx context.Context, opt RecordOptions, rf *rules.File, an *analyzed, wt tree.Tree, head string,
	now time.Time, keep func(string) bool, sum *RecordSummary) ([]dynamic.Record, []string, error) {
	units, skipped := ix.recordUnits(an)
	units = slices.DeleteFunc(units, func(u pydyn.Unit) bool { return !keep(u.ID) })
	if len(units) == 0 {
		return nil, skipped, nil
	}
	py, _, trust := ix.dynamicConfig(rf)
	exe, err := pydyn.ResolvePython(ix.repo.Root, py)
	if err != nil {
		return nil, skipped, fmt.Errorf("python interpreter: %w", err)
	}
	in, err := ix.probes.Get(ctx, exe, ix.repo.Root, ix.tmpDir())
	if err != nil {
		return nil, skipped, fmt.Errorf("python interpreter: %w", err)
	}
	collector := pydyn.Collector(trust)
	env := pydyn.Fingerprint(collector, in.Lines, wt)
	sum.Units += len(units)
	sum.Env = env
	jobs := opt.Jobs
	if jobs <= 0 {
		jobs = pydyn.DefaultJobs()
	}
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
		return nil, skipped, err
	}
	recs := make([]dynamic.Record, 0, len(outs))
	for i, o := range outs {
		recs = append(recs, pydyn.RecordOf(o, units[i], wt, ix.repo.Root, env, collector, head, now))
	}
	return recs, skipped, nil
}

// recordNode records the Node units (docs/dynamic-layer.md §8).
func (ix *Index) recordNode(ctx context.Context, opt RecordOptions, rf *rules.File, an *analyzed, wt tree.Tree, head string,
	now time.Time, keep func(string) bool, sum *RecordSummary) ([]dynamic.Record, []string, error) {
	units, skipped := ix.nodeRecordUnits(an)
	units = slices.DeleteFunc(units, func(u nodedyn.Unit) bool { return !keep(u.ID) })
	if len(units) == 0 {
		return nil, skipped, nil
	}
	_, nodeCfg, trust := ix.dynamicConfig(rf)
	exe, err := nodedyn.ResolveNode(ix.repo.Root, nodeCfg)
	if err != nil {
		return nil, skipped, fmt.Errorf("node: %w", err)
	}
	probe, err := ix.nodeProbes.Get(ctx, exe, ix.repo.Root)
	if err != nil {
		return nil, skipped, fmt.Errorf("node: %w", err)
	}
	installed, _, err := nodedyn.Installed(ix.repo.Root, wt)
	if err != nil {
		return nil, skipped, err
	}
	collector := nodedyn.Collector(trust)
	env := nodedyn.Fingerprint(collector, probe, installed, wt)
	sum.Units += len(units)
	sum.Env = env
	jobs := opt.Jobs
	if jobs <= 0 {
		jobs = nodedyn.DefaultJobs()
	}
	byID := map[string]nodedyn.Unit{}
	for _, u := range units {
		byID[u.ID] = u
	}
	ropt := nodedyn.RunOptions{Node: exe, Root: ix.repo.Root, TmpDir: ix.tmpDir(), Timeout: opt.Timeout, Jobs: jobs, Trust: trust, Env: opt.Env}
	if opt.Progress != nil {
		ropt.Progress = func(done, total int, o nodedyn.Outcome) {
			opt.Progress(done, total, nodedyn.RecordOf(o, byID[o.Unit], wt, ix.repo.Root, env, collector, head, now), o.Elapsed, o.Tail)
		}
	}
	outs, err := nodedyn.Collect(ctx, ropt, units)
	if err != nil {
		return nil, skipped, err
	}
	recs := make([]dynamic.Record, 0, len(outs))
	for i, o := range outs {
		recs = append(recs, nodedyn.RecordOf(o, units[i], wt, ix.repo.Root, env, collector, head, now))
	}
	return recs, skipped, nil
}

// reasonKey drops the file name from per-file reasons so that the histogram groups them.
func reasonKey(why string) string {
	for _, pre := range []string{"reads untracked file ", "cannot hash ", "the unit's own file ", "native addon in the repository: ",
		"unmapped repository class ", "repository class loaded before the agent: ", "maps repository file ", "starts a process: ",
		"native library in the repository: ", "native library outside the JDK and the OS: ", "reads class file that no longer exists: ",
		"reads an archive built in the repository: ", "reads build output that is not a copy of a tracked file: ", "build: ",
		"forks ran classes the runner did not report: ", "loads untracked file ", "reads untracked file ",
		"native extension in the repository: ", "starts a non-Ruby process: ", "starts a Ruby process that wrote no coverage: ",
		"starts a Ruby process whose pid is unknown: ", "exec of a Ruby process that wrote no coverage: ", "the collector raised ",
		"the tests called Coverage.", "unreadable collector output ", "includes untracked file ", "sees untracked file ",
		"starts a process (", "another coverage user: ", "forks the process (", "replaces the process (pcntl_exec) at ",
		"loads a PHP extension (dl) at ", "calls native code through FFI at ", "the runner started ",
		"lists a test file that is not tracked: ", "lists a test file outside the configured test directories: ", "test file ",
		"listing the test files: ", "malformed line table for ", "cannot install the collector's output buffer", "reads a file without the stream wrapper (", "writes a file outside the repository and its temporary directory: ",
		"reads a file written into the vendor directory after composer install: ", "reads shared state another test file wrote: "} {
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

// DynamicStatus reports the Python record store (see DynamicStatusOf).
func (ix *Index) DynamicStatus(ctx context.Context) (DynamicStatus, error) {
	return ix.DynamicStatusOf(ctx, python.Name)
}

// DynamicStatusOf reports the record store of ecosystem eco: records, how many are usable at HEAD, incomplete reasons.
func (ix *Index) DynamicStatusOf(ctx context.Context, eco string) (DynamicStatus, error) {
	st := DynamicStatus{Eco: eco, Unusable: map[string]int{}, Reasons: map[string]int{}}
	if !dynamic.Supported(eco) {
		return st, fmt.Errorf("ecosystem %q has no dynamic layer", eco)
	}
	if ix.stateDir == "" {
		return st, errors.New("no state directory")
	}
	st.Store = dynamic.StorePath(ix.stateDir, eco)
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
			if u.Eco == eco && !u.Whole {
				st.TestUnits++
			}
		}
	}
	// envOf returns the current environment of a record's unit: one per ecosystem, except PHP (one per project's vendor directory).
	var envOf func(unit string) string
	var collector string
	if eco == phpdyn.Eco {
		collector = phpdyn.CollectorID
		envs := map[string]string{}
		if _, probe, perr := ix.phpProbe(ctx, an.Rules); perr != nil {
			st.EnvError = perr.Error()
		} else {
			units, _ := phpRecordUnits(an)
			fp := ix.phpUnitEnvs(probe)
			for _, u := range units {
				if e, why := fp(u.BinDir); e != "" {
					envs[u.ID] = e
				} else if st.EnvError == "" {
					st.EnvError = u.ID + ": " + why
				}
			}
		}
		envOf = func(unit string) string { u, _, _ := strings.Cut(unit, "#"); return envs[u] }
	} else {
		env, c, err := ix.ecoEnv(ctx, eco, an.Rules, ht)
		if err != nil {
			st.EnvError = err.Error()
		}
		st.Env, collector = env, c
		envOf = func(string) string { return env }
	}
	units := map[string]bool{}
	for _, r := range recs {
		units[r.Unit] = true
		if !r.Complete {
			for _, why := range strings.Split(r.Incomplete, "; ") {
				st.Reasons[reasonKey(why)]++
			}
		}
		env := envOf(r.Unit)
		if env == "" || r.Env != env {
			continue
		}
		p := dynamic.Params{Eco: eco, Collector: collector, Env: env, BaseOID: func(p string) string {
			if !ht.Has(p) {
				return ""
			}
			return ht.Key(p)
		}}
		st.CurrentEnv++
		files := []string{r.Unit}
		switch {
		case eco == jvm.Name || eco == dotnetdyn.Eco:
			files = nil // class and class-list records: the class's file is among its dependencies, not named by the unit
		case eco == phpdyn.Eco:
			files = nil // file-list record
			if _, f, ok := strings.Cut(r.Unit, "#"); ok {
				files = []string{f}
			}
		}
		why := dynamic.Usable(r, files, p)
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

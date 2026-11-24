package bgraph

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/dynamic/phpdyn"
	"github.com/qxbao/bgraph/internal/eco/php"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/selector"
	"github.com/qxbao/bgraph/internal/tree"
)

// phpExe resolves the PHP executable of the dynamic layer: Options.PHP, then bgraph.toml [dynamic_layer] php, then php on PATH.
func (ix *Index) phpExe(rf *rules.File) (string, error) {
	cfg := ""
	if rf != nil {
		cfg = rf.Dynamic.PHP
	}
	if ix.opt.PHP != "" {
		cfg = ix.opt.PHP
	}
	return phpdyn.ResolvePHP(ix.repo.Root, cfg)
}

// phpProbe runs (or reuses) the probe of the configured PHP executable.
func (ix *Index) phpProbe(ctx context.Context, rf *rules.File) (string, phpdyn.Probe, error) {
	exe, err := ix.phpExe(rf)
	if err != nil {
		return "", phpdyn.Probe{}, fmt.Errorf("php: %w", err)
	}
	p, err := ix.phpProbes.Get(ctx, exe)
	if err != nil {
		return "", phpdyn.Probe{}, fmt.Errorf("php: %w", err)
	}
	return exe, p, nil
}

// phpUnitEnvs returns a function giving the environment fingerprint of a PHP unit (docs/dynamic-layer.md §11.5): the PHP probe
// and the installed dependency state of the unit's vendor directory on disk. Results are cached per bin directory.
func (ix *Index) phpUnitEnvs(probe phpdyn.Probe) func(binDir string) (string, string) {
	type res struct{ env, why string }
	cache := map[string]res{}
	return func(binDir string) (string, string) {
		if r, ok := cache[binDir]; ok {
			return r.env, r.why
		}
		env, err := phpdyn.UnitFingerprint(probe, ix.repo.Root, binDir)
		r := res{env: env}
		if err != nil {
			r = res{why: err.Error()}
		}
		cache[binDir] = r
		return r.env, r.why
	}
}

// applyPHPFiles runs the PHP layer (docs/dynamic-layer.md §11) on a selection: a selected Composer / PHPUnit project runs only
// the test files whose records reach a changed file (or a changed function of it). Anything that prevents it leaves the
// selection unchanged with a dynamic_skipped warning.
func (ix *Index) applyPHPFiles(ctx context.Context, in selector.Input, res selector.Result, sel Selection) Selection {
	candidate := slices.ContainsFunc(sel.Tests, func(t model.SelectedTest) bool { return phpdyn.Narrowable(t, res.UnitEco[t.Unit]) })
	if !candidate {
		return sel
	}
	changes := make([]dynamic.Change, 0, len(res.Changes))
	for _, c := range res.Changes {
		roles := c.Roles[phpdyn.Eco]
		other := false
		for e, r := range c.Roles {
			other = other || e != phpdyn.Eco && (r[0] != "" || r[1] != "")
		}
		changes = append(changes, dynamic.Change{Path: c.Path, Status: c.Status, OldMode: c.OldMode, NewMode: c.NewMode,
			BaseRole: roles[0], HeadRole: roles[1], Global: c.Global, Claimed: len(c.Claimed) > 0,
			Declared: dynamic.Declared(in.Rules, c.Path), Rule: c.Rule, Referenced: c.Dependents, OtherRole: other, Linked: c.Linked})
	}
	changed, why := phpdyn.Guard(sel, changes)
	if why != "" {
		return phpSkip(sel, res.UnitEco, why)
	}
	if in.BaseTree == nil || in.HeadTree == nil {
		return phpSkip(sel, res.UnitEco, "no base or head tree")
	}
	if ix.stateDir == "" {
		return phpSkip(sel, res.UnitEco, "no state directory: no records")
	}
	recs, warns := dynamic.Load(dynamic.StorePath(ix.stateDir, phpdyn.Eco))
	for _, w := range warns {
		sel.Warnings = append(sel.Warnings, model.Widening{Tier: model.TierNone, Reason: model.ReasonDynamicSkipped, Detail: w})
	}
	if len(recs) == 0 {
		return phpSkip(sel, res.UnitEco, "no records (run `bgraph record --eco php`)")
	}
	exe, probe, err := ix.phpProbe(ctx, in.Rules)
	if err != nil {
		return phpSkip(sel, res.UnitEco, "environment fingerprint: "+err.Error())
	}
	// Function-level refinement: the body guard, then PHP's own tokenizer on the files it accepted.
	changed = phpdyn.Refine(changed, in.BaseTree, in.HeadTree)
	files := phpdyn.CheckFiles(changed, in.BaseTree, in.HeadTree)
	checked := map[string]bool{}
	for _, f := range files {
		checked[f.Path] = true
	}
	changed = phpdyn.Downgrade(changed, phpdyn.CrossCheck(ctx, exe, files), checked)
	envOf := ix.phpUnitEnvs(probe)
	return phpdyn.Narrow(sel, phpdyn.Params{Records: recs, Base: in.BaseTree, UnitEco: res.UnitEco, Changed: changed,
		Env: func(t model.SelectedTest) (string, string) { return envOf(t.BinDir) }})
}

// phpSkip returns sel with a dynamic_skipped warning for the PHP layer, and the reason on every PHP unit it would have considered
// (bgraph explain shows it: PHP units are precise, so the warning alone would not be attached to them).
func phpSkip(sel Selection, unitEco map[string]string, why string) Selection {
	sel = dynamic.Skip(sel, phpdyn.Eco, why)
	sel.Tests = slices.Clone(sel.Tests)
	for i, t := range sel.Tests {
		if phpdyn.Narrowable(t, unitEco[t.Unit]) && t.Dynamic == "" {
			sel.Tests[i].Dynamic = "not narrowed by the PHP layer: " + why
		}
	}
	return sel
}

// phpRecordUnits lists the PHP units the layer can record, and why the others are skipped.
func phpRecordUnits(an *analyzed) ([]phpdyn.Unit, []string) {
	var units []model.TestUnit
	for _, a := range an.Analyses {
		for _, u := range a.Tests {
			if u.Eco == php.Name {
				units = append(units, u)
			}
		}
	}
	var out []phpdyn.Unit
	var skipped []string
	for _, u := range selector.DedupUnits(units) {
		pu := phpdyn.Unit{ID: u.ID, Project: u.Project, Runner: u.Runner, Config: u.Config, BinDir: u.BinDir,
			RunnerPaths: u.RunnerPaths, Vendor: phpdyn.VendorOf(u.BinDir)}
		if pu.Project == "" {
			pu.Project = "."
		}
		switch {
		case u.Whole:
			skipped = append(skipped, u.ID+": the suite is run by a Composer script (test files unknown)")
		case u.Runner != phpdyn.RunnerPHPUnit && u.Runner != phpdyn.RunnerPest:
			skipped = append(skipped, u.ID+": runner "+u.Runner+" is not supported")
		case u.Config == "":
			skipped = append(skipped, u.ID+": no PHPUnit XML configuration (the test files cannot be listed)")
		case pu.Vendor == "":
			skipped = append(skipped, u.ID+": the runner is not installed in a Composer vendor directory (bin directory "+u.BinDir+")")
		default:
			out = append(out, pu)
		}
	}
	return out, skipped
}

// recordPHP records the PHP units (docs/dynamic-layer.md §11.4): the file list of each project, then every test file in a process
// tree of its own with the collector prepended through PHP_INI_SCAN_DIR.
func (ix *Index) recordPHP(ctx context.Context, opt RecordOptions, rf *rules.File, an *analyzed, wt tree.Tree, head string,
	now time.Time, keep func(string) bool, sum *RecordSummary) ([]dynamic.Record, []string, error) {
	units, skipped := phpRecordUnits(an)
	// --units matches unit IDs or test-file sub-unit IDs (<unit>#<file>): units are listed, then their test files filtered.
	all := units
	if len(units) == 0 {
		return nil, skipped, nil
	}
	exe, probe, err := ix.phpProbe(ctx, rf)
	if err != nil {
		return nil, skipped, err
	}
	if why := phpdyn.Check(probe); why != "" {
		return nil, skipped, fmt.Errorf("php: %s (%s)", why, exe)
	}
	vendors := phpdyn.Vendors(all)
	envOf := ix.phpUnitEnvs(probe)
	tmp := ix.tmpDir()
	ropt := phpdyn.RunOptions{PHP: exe, Probe: probe, Root: ix.repo.Root, TmpDir: tmp, Timeout: opt.Timeout, Jobs: opt.Jobs,
		Env: opt.Env, Vendors: vendors}
	if ropt.Jobs <= 0 {
		ropt.Jobs = phpdyn.DefaultJobs()
	}
	type listed struct {
		u   phpdyn.Unit
		env string
		lo  phpdyn.ListOutcome
	}
	var lists []listed
	var files []phpdyn.FileUnit
	m := map[string]string{} // sub-unit → env
	for _, u := range units {
		env, why := envOf(u.BinDir)
		if env == "" {
			skipped = append(skipped, u.ID+": no environment fingerprint: "+why)
			continue
		}
		lo := phpdyn.List(ctx, ropt, u)
		if lo.Err != "" {
			if keep(u.ID) {
				lists = append(lists, listed{u, env, lo})
				sum.Warnings = append(sum.Warnings, u.ID+": "+lo.Err+" (the unit will not be narrowed)")
			}
			continue
		}
		n := 0
		for _, abs := range lo.Files {
			rel := phpdyn.RepoRel(ix.repo.Root, abs)
			if rel == "" || !wt.Has(rel) {
				continue // ListRecordOf marks the list incomplete
			}
			fu := phpdyn.FileUnit{Unit: u, File: rel}
			if !keep(u.ID) && !keep(fu.ID()) {
				continue
			}
			files = append(files, fu)
			m[fu.ID()] = env
			n++
		}
		if n > 0 || keep(u.ID) {
			lists = append(lists, listed{u, env, lo})
		}
	}
	sum.Units += len(files)
	byID := map[string]phpdyn.FileUnit{}
	for _, f := range files {
		byID[f.ID()] = f
	}
	if opt.Progress != nil {
		ropt.Progress = func(done, total int, o phpdyn.Outcome) {
			f := byID[o.Unit]
			opt.Progress(done, total, phpdyn.RecordOf(o, f, wt, ix.repo.Root, m[o.Unit], head, now, vendors), o.Elapsed, o.Tail)
		}
	}
	outs, err := phpdyn.Collect(ctx, ropt, files)
	if err != nil {
		return nil, skipped, err
	}
	var recs []dynamic.Record
	failed := map[string][]string{} // unit → test files that could not be recorded alone
	for i, o := range outs {
		f := files[i]
		r := phpdyn.RecordOf(o, f, wt, ix.repo.Root, m[f.ID()], head, now, vendors)
		recs = append(recs, r)
		sum.Env = r.Env
		if o.Err != "" && !o.TimedOut && o.Exit < 0 {
			failed[f.Unit.ID] = append(failed[f.Unit.ID], f.File)
		}
	}
	recs = phpdyn.SharedStateTaint(outs, recs)
	for _, l := range lists {
		r := phpdyn.ListRecordOf(l.lo, l.u, wt, ix.repo.Root, l.env, head, now, vendors, failed[l.u.ID])
		if !r.Complete && l.lo.Err == "" {
			sum.Warnings = append(sum.Warnings, l.u.ID+": file list not usable: "+r.Incomplete)
		}
		recs = append(recs, r)
	}
	return recs, skipped, nil
}

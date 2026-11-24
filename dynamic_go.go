package bgraph

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/dynamic/godyn"
	"github.com/qxbao/bgraph/internal/eco/golang"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/selector"
	"github.com/qxbao/bgraph/internal/tree"
)

// goExe resolves the go command of the dynamic layer: Options.Go, then bgraph.toml [dynamic_layer] go, then go on PATH.
func (ix *Index) goExe(rf *rules.File) (string, error) {
	cfg := ""
	if rf != nil {
		cfg = rf.Dynamic.Go
	}
	if ix.opt.Go != "" {
		cfg = ix.opt.Go
	}
	return godyn.ResolveGo(ix.repo.Root, cfg)
}

// goProbe runs (or reuses) the probe of the configured go command, from the repository root.
func (ix *Index) goProbe(ctx context.Context, rf *rules.File) (string, godyn.Probe, error) {
	exe, err := ix.goExe(rf)
	if err != nil {
		return "", godyn.Probe{}, fmt.Errorf("go: %w", err)
	}
	p, err := ix.goProbes.Get(ctx, exe, ix.repo.Root)
	if err != nil {
		return "", godyn.Probe{}, fmt.Errorf("go: %w", err)
	}
	return exe, p, nil
}

// goFingerprint is the Go environment fingerprint at selection time (no build arguments: bgraph's go test commands carry none).
func (ix *Index) goFingerprint(ctx context.Context, rf *rules.File) (string, error) {
	_, p, err := ix.goProbe(ctx, rf)
	if err != nil {
		return "", err
	}
	return godyn.Fingerprint(p, nil), nil
}

// applyGoTests runs the Go layer on a selection: a selected Go package runs only the top-level test functions whose records
// executed a changed function (or cannot be judged), and still compiles when none is kept. Anything that prevents it leaves the
// selection unchanged with a dynamic_skipped warning.
func (ix *Index) applyGoTests(ctx context.Context, in selector.Input, res selector.Result, sel Selection) Selection {
	if !slices.ContainsFunc(sel.Tests, func(t model.SelectedTest) bool { return godyn.Narrowable(t, res.UnitEco[t.Unit]) }) {
		return sel
	}
	changes := make([]dynamic.Change, 0, len(res.Changes))
	for _, c := range res.Changes {
		roles := c.Roles[godyn.Eco]
		other := false
		for e, r := range c.Roles {
			other = other || e != godyn.Eco && (r[0] != "" || r[1] != "")
		}
		changes = append(changes, dynamic.Change{Path: c.Path, Status: c.Status, OldMode: c.OldMode, NewMode: c.NewMode,
			BaseRole: roles[0], HeadRole: roles[1], Global: c.Global, Claimed: len(c.Claimed) > 0,
			Declared: dynamic.Declared(in.Rules, c.Path), Rule: c.Rule, Referenced: c.Dependents, OtherRole: other, Linked: c.Linked})
	}
	changed, why := godyn.Guard(sel, changes)
	if why != "" {
		return goSkip(sel, res.UnitEco, why)
	}
	if in.BaseTree == nil || in.HeadTree == nil {
		return goSkip(sel, res.UnitEco, "no base or head tree")
	}
	if res.Graph == nil {
		return goSkip(sel, res.UnitEco, "no graph")
	}
	if changed, why = godyn.Refine(changed, in.BaseTree, in.HeadTree); why != "" {
		return goSkip(sel, res.UnitEco, why)
	}
	if ix.stateDir == "" {
		return goSkip(sel, res.UnitEco, "no state directory: no records")
	}
	recs, warns := dynamic.Load(dynamic.StorePath(ix.stateDir, godyn.Eco))
	for _, w := range warns {
		sel.Warnings = append(sel.Warnings, model.Widening{Tier: model.TierNone, Reason: model.ReasonDynamicSkipped, Detail: w})
	}
	if len(recs) == 0 {
		return goSkip(sel, res.UnitEco, "no records (run `bgraph record --eco go`)")
	}
	env, err := ix.goFingerprint(ctx, in.Rules)
	if err != nil {
		return goSkip(sel, res.UnitEco, "environment fingerprint: "+err.Error())
	}
	// Units the static graph reaches from a changed file through an edge the records cannot judge are kept whole.
	var seeds []string
	for _, c := range res.Changes {
		if res.Graph.IsNode(c.Path) {
			seeds = append(seeds, c.Path)
		}
	}
	reached := res.Graph.WalkTainted(seeds, godyn.PlainEdge)
	tainted := map[string]bool{}
	for _, t := range sel.Tests {
		if slices.ContainsFunc(t.Files, func(f string) bool { return reached[f] }) {
			tainted[t.Unit] = true
		}
	}
	return godyn.Narrow(sel, godyn.Params{Env: env, Records: recs, Base: in.BaseTree, UnitEco: res.UnitEco, Changed: changed,
		Tainted: tainted})
}

// goSkip returns sel with a dynamic_skipped warning for the Go layer, and the reason on every Go unit it would have considered.
func goSkip(sel Selection, unitEco map[string]string, why string) Selection {
	sel = dynamic.Skip(sel, godyn.Eco, why)
	sel.Tests = slices.Clone(sel.Tests)
	for i, t := range sel.Tests {
		if godyn.Narrowable(t, unitEco[t.Unit]) && t.Dynamic == "" {
			sel.Tests[i].Dynamic = "not narrowed by the Go layer: " + why
		}
	}
	return sel
}

// goRecordUnits lists the Go package units of the working tree.
func goRecordUnits(an *analyzed) []godyn.Unit {
	var units []model.TestUnit
	for _, a := range an.Analyses {
		for _, u := range a.Tests {
			if u.Eco == golang.Name && !u.Whole {
				units = append(units, u)
			}
		}
	}
	var out []godyn.Unit
	for _, u := range selector.DedupUnits(units) {
		dir, ok := golang.ParseUnitID(u.ID)
		if !ok {
			continue
		}
		mod := u.Project
		if mod == "" {
			mod = "."
		}
		out = append(out, godyn.Unit{ID: u.ID, Dir: dir, Module: mod})
	}
	return out
}

// recordGo records the Go units: one test binary per package (coverage of the repository's packages, os and syscall), then every
// top-level test function alone in a process tree of its own.
func (ix *Index) recordGo(ctx context.Context, opt RecordOptions, rf *rules.File, an *analyzed, wt tree.Tree, head string,
	now time.Time, keep func(string) bool, sum *RecordSummary) ([]dynamic.Record, []string, error) {
	all := goRecordUnits(an)
	// --units matches package unit IDs or test-function sub-unit IDs (go:<dir>#<Name>); a pattern with '#' needs every package
	// listed first.
	var units []godyn.Unit
	for _, u := range all {
		if keep(u.ID) || strings.Contains(opt.Units, "#") {
			units = append(units, u)
		}
	}
	if len(units) == 0 {
		return nil, nil, nil
	}
	if why := godyn.CheckFlags(opt.BuildArgs, godyn.GOFLAGS(opt.Env)); why != "" {
		return nil, nil, fmt.Errorf("go: %s", why)
	}
	exe, probe, err := ix.goProbe(ctx, rf)
	if err != nil {
		return nil, nil, err
	}
	env := godyn.Fingerprint(probe, opt.BuildArgs)
	ropt := godyn.RunOptions{Go: exe, Probe: probe, Root: ix.repo.Root, TmpDir: ix.tmpDir(), Timeout: opt.Timeout, Jobs: opt.Jobs,
		Env: opt.Env, BuildArgs: opt.BuildArgs, Keep: func(unit, sub string) bool { return keep(unit) || keep(sub) }}
	if ropt.Jobs <= 0 {
		ropt.Jobs = godyn.DefaultJobs()
	}
	if opt.Progress != nil {
		ropt.Progress = func(done, total int, o godyn.Outcome) {
			opt.Progress(done, total, godyn.RecordOf(o, wt, ix.repo.Root, env, head, now), o.Elapsed, o.Tail)
		}
	}
	pkgs, outs, err := godyn.Collect(ctx, ropt, units)
	if err != nil {
		return nil, nil, err
	}
	var recs []dynamic.Record
	recorded := map[string]bool{}
	for _, o := range outs {
		recs = append(recs, godyn.RecordOf(o, wt, ix.repo.Root, env, head, now))
		recorded[o.Pkg.Unit.ID] = true
	}
	sum.Units += len(outs)
	sum.Env = env
	var skipped []string
	for _, p := range pkgs {
		if p.Err != "" {
			sum.Warnings = append(sum.Warnings, p.Unit.ID+": "+p.Err+" (no record: the package will not be narrowed)")
			continue
		}
		if !recorded[p.Unit.ID] && !keep(p.Unit.ID) {
			continue
		}
		r := godyn.ListRecordOf(p, wt, env, head, now)
		if !r.Complete {
			skipped = append(skipped, p.Unit.ID+": "+r.Incomplete)
		}
		recs = append(recs, r)
	}
	return recs, skipped, nil
}

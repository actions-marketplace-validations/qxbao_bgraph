package bgraph

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/dynamic/dotnetdyn"
	"github.com/qxbao/bgraph/internal/eco/dotnet"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/selector"
	"github.com/qxbao/bgraph/internal/tree"
)

// dotnetEnv computes the .NET environment fingerprint for tree t (docs/dynamic-layer.md §9.9). env overrides the process
// environment of the probes (DOTNET_ROOT, PATH...).
// buildArgs are the extra dotnet arguments of the recording (none at selection time: the narrowed commands run without them).
func (ix *Index) dotnetEnv(ctx context.Context, t tree.Tree, env, buildArgs []string) (string, error) {
	sdk, err := dotnetdyn.SDK(ctx, ix.repo.Root, env)
	if err != nil {
		return "", fmt.Errorf("dotnet SDK: %w", err)
	}
	return dotnetdyn.Fingerprint(sdk, buildArgs, t), nil
}

// applyDotnetClasses runs the .NET class layer (docs/dynamic-layer.md §9) on a selection: selected test projects run only the
// test classes whose records reach a changed file. Anything that prevents it leaves the selection unchanged with a
// dynamic_skipped warning.
func (ix *Index) applyDotnetClasses(ctx context.Context, in selector.Input, res selector.Result, sel Selection) Selection {
	candidate := slices.ContainsFunc(sel.Tests, func(t model.SelectedTest) bool { return dotnetdyn.Narrowable(t, res.UnitEco[t.Unit]) })
	if !candidate {
		return sel
	}
	changes := make([]dynamic.Change, 0, len(res.Changes))
	for _, c := range res.Changes {
		roles := c.Roles[dotnetdyn.Eco]
		other := false
		for e, r := range c.Roles {
			other = other || e != dotnetdyn.Eco && (r[0] != "" || r[1] != "")
		}
		changes = append(changes, dynamic.Change{Path: c.Path, Status: c.Status, OldMode: c.OldMode, NewMode: c.NewMode,
			BaseRole: roles[0], HeadRole: roles[1], Global: c.Global, Claimed: len(c.Claimed) > 0,
			Declared: dynamic.Declared(in.Rules, c.Path), Rule: c.Rule, Referenced: c.Dependents, OtherRole: other, Linked: c.Linked})
	}
	g, why := dotnetdyn.Guard(sel, changes, in.BaseTree, in.HeadTree)
	if why != "" {
		return dynamic.Skip(sel, dotnetdyn.Eco, why)
	}
	if ix.stateDir == "" {
		return dynamic.Skip(sel, dotnetdyn.Eco, "no state directory: no records")
	}
	recs, warns := dynamic.Load(dynamic.StorePath(ix.stateDir, dotnetdyn.Eco))
	for _, w := range warns {
		sel.Warnings = append(sel.Warnings, model.Widening{Tier: model.TierNone, Reason: model.ReasonDynamicSkipped, Detail: w})
	}
	if len(recs) == 0 {
		return dynamic.Skip(sel, dotnetdyn.Eco, "no records (run `bgraph record --eco dotnet`)")
	}
	env, err := ix.dotnetEnv(ctx, in.BaseTree, nil, nil)
	if err != nil {
		return dynamic.Skip(sel, dotnetdyn.Eco, "environment fingerprint: "+err.Error())
	}
	return dotnetdyn.Narrow(sel, dotnetdyn.Params{Env: env, Records: recs, Base: in.BaseTree, Head: in.HeadTree, UnitEco: res.UnitEco,
		Guarded: g})
}

// recordDotnet records the .NET test projects (docs/dynamic-layer.md §9.9): per project, one build, the whole suite per target
// framework for the class list, then every class per framework in a test host of its own with code coverage.
func (ix *Index) recordDotnet(ctx context.Context, opt RecordOptions, an *analyzed, wt tree.Tree, head string, now time.Time,
	keep func(string) bool, sum *RecordSummary) ([]dynamic.Record, []string, error) {
	var units []dotnetdyn.Unit
	var skipped []string
	for _, a := range an.Analyses {
		for _, u := range a.Tests {
			if u.Eco != dotnet.Name || !keep(u.ID) {
				continue
			}
			switch {
			case u.Whole || u.Package == "":
				skipped = append(skipped, u.ID+": whole-project unit")
				continue
			}
			units = append(units, dotnetdyn.Unit{ID: u.ID, Project: u.Package, Dir: path.Dir(u.Package), MTP: u.Config != ""})
		}
	}
	if len(units) == 0 {
		return nil, skipped, nil
	}
	env, err := ix.dotnetEnv(ctx, wt, opt.Env, opt.BuildArgs)
	if err != nil {
		return nil, skipped, err
	}
	tmp := ix.tmpDir()
	if tmp == "" {
		return nil, skipped, fmt.Errorf("dotnet: no state directory for the collector")
	}
	sum.Env = env
	m := &dotnetdyn.Mapper{Root: ix.repo.Root, Tree: wt}
	var recs []dynamic.Record
	for _, u := range units {
		ropt := dotnetdyn.RunOptions{Root: ix.repo.Root, TmpDir: tmp, Jobs: opt.Jobs, Timeout: opt.Timeout, Env: opt.Env,
			BuildArgs: opt.BuildArgs}
		o, err := dotnetdyn.Run(ctx, ropt, u)
		if errors.Is(err, dotnetdyn.ErrNotTestProject) {
			continue // a library project: nothing to record
		}
		if err != nil {
			return nil, skipped, err
		}
		if o.Why != "" {
			// Not a test project (dotnet test ran nothing) or not buildable: one incomplete class-list record says why.
			sum.Warnings = append(sum.Warnings, u.ID+": "+o.Why)
		}
		sum.Units++
		rs := dotnetdyn.Records(m, o, env, head, now)
		if opt.Progress != nil {
			for i, r := range rs {
				opt.Progress(i+1, len(rs), r, o.Elapsed, "")
			}
		}
		recs = append(recs, rs...)
	}
	return recs, skipped, nil
}

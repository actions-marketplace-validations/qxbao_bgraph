package bgraph

import (
	"context"
	"errors"
	"slices"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/dynamic/jvmdyn"
	"github.com/qxbao/bgraph/internal/dynamic/nodedyn"
	"github.com/qxbao/bgraph/internal/dynamic/pydyn"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/selector"
	"github.com/qxbao/bgraph/internal/tree"
)

// DynamicMode switches the dynamic layer (docs/dynamic-layer.md) for one selection.
type DynamicMode string

const (
	DynamicDefault DynamicMode = ""    // bgraph.toml [dynamic_layer] ecosystems decide
	DynamicOn      DynamicMode = "on"  // every ecosystem with a dynamic layer
	DynamicOff     DynamicMode = "off" // never
)

// NarrowedTest is a widened unit removed by the dynamic layer.
type NarrowedTest = model.NarrowedTest

// dynamicEcos returns the ecosystems the dynamic layer narrows for this selection.
func dynamicEcos(mode DynamicMode, rf *rules.File) []string {
	switch mode {
	case DynamicOff:
		return nil
	case DynamicOn:
		return rules.DynamicEcosystems
	}
	if rf == nil {
		return nil
	}
	return rf.Dynamic.Ecosystems
}

// dynamicConfig is the [dynamic_layer] configuration in effect (bgraph.toml of the given tree, then Options).
func (ix *Index) dynamicConfig(rf *rules.File) (python, nodeExe string, trust bool) {
	if rf != nil {
		python, nodeExe, trust = rf.Dynamic.Python, rf.Dynamic.Node, rf.Dynamic.TrustNativeEnv
	}
	if ix.opt.Python != "" {
		python = ix.opt.Python
	}
	if ix.opt.Node != "" {
		nodeExe = ix.opt.Node
	}
	if ix.opt.TrustNativeEnv {
		trust = true
	}
	return python, nodeExe, trust
}

// ecoEnv computes the environment fingerprint and collector ID of ecosystem eco for tree t.
func (ix *Index) ecoEnv(ctx context.Context, eco string, rf *rules.File, t tree.Tree) (env, collector string, err error) {
	switch eco {
	case "python":
		return ix.pythonEnv(ctx, rf, t)
	case "node":
		return ix.nodeEnv(ctx, rf, t)
	case jvmdyn.Eco:
		env, collector, _, err = ix.jvmEnv(rf, t, nil)
		return env, collector, err
	}
	return "", "", errors.New("no dynamic layer for ecosystem " + eco)
}

// nodeEnv computes the Node environment fingerprint for tree t (docs/dynamic-layer.md §8.4).
func (ix *Index) nodeEnv(ctx context.Context, rf *rules.File, t tree.Tree) (env, collector string, err error) {
	_, nodeExe, trust := ix.dynamicConfig(rf)
	exe, err := nodedyn.ResolveNode(ix.repo.Root, nodeExe)
	if err != nil {
		return "", "", err
	}
	probe, err := ix.nodeProbes.Get(ctx, exe, ix.repo.Root)
	if err != nil {
		return "", "", err
	}
	installed, _, err := nodedyn.Installed(ix.repo.Root, t)
	if err != nil {
		return "", "", err
	}
	collector = nodedyn.Collector(trust)
	return nodedyn.Fingerprint(collector, probe, installed, t), collector, nil
}

// pythonEnv computes the Python environment fingerprint for tree t (docs/dynamic-layer.md §5).
func (ix *Index) pythonEnv(ctx context.Context, rf *rules.File, t tree.Tree) (env, collector string, err error) {
	py, _, trust := ix.dynamicConfig(rf)
	exe, err := pydyn.ResolvePython(ix.repo.Root, py)
	if err != nil {
		return "", "", err
	}
	in, err := ix.probes.Get(ctx, exe, ix.repo.Root, ix.tmpDir())
	if err != nil {
		return "", "", err
	}
	collector = pydyn.Collector(trust)
	return pydyn.Fingerprint(collector, in.Lines, t), collector, nil
}

// tmpDir is where the dynamic layer writes short-lived files (the state directory when there is one).
func (ix *Index) tmpDir() string {
	if ix.stateDir != "" {
		return ix.stateDir
	}
	return ""
}

// applyDynamic runs the dynamic layer on a selection (docs/dynamic-layer.md §2, §3). Anything that prevents it (no state directory,
// interpreter that cannot run, a guard) leaves the selection unchanged with a dynamic_skipped warning.
func (ix *Index) applyDynamic(ctx context.Context, ch Change, in selector.Input, res selector.Result) Selection {
	sel := res.Selection
	for _, eco := range dynamicEcos(ch.Dynamic, in.Rules) {
		if eco == jvmdyn.Eco {
			sel = ix.applyJVMClasses(ctx, in, res, sel) // per-test-class layer: narrows precise units (docs/dynamic-layer.md §9)
			continue
		}
		candidate := false
		for _, t := range sel.Tests {
			if t.Widened && res.UnitEco[t.Unit] == eco {
				candidate = true
				break
			}
		}
		if !candidate {
			continue // nothing the layer could remove: no note, no interpreter run
		}
		changes := make([]dynamic.Change, 0, len(res.Changes))
		for _, c := range res.Changes {
			roles := c.Roles[eco]
			other := false
			for _, r := range c.Roles {
				other = other || r[0] != "" || r[1] != ""
			}
			changes = append(changes, dynamic.Change{Path: c.Path, Status: c.Status, OldMode: c.OldMode, NewMode: c.NewMode,
				BaseRole: roles[0], HeadRole: roles[1], Global: c.Global, Claimed: len(c.Claimed) > 0,
				Declared: dynamic.Declared(in.Rules, c.Path), Rule: c.Rule, Referenced: c.Dependents, OtherRole: other, Linked: c.Linked})
		}
		if why := dynamic.Guard(sel, changes, eco); why != "" {
			sel = dynamic.Skip(sel, eco, why)
			continue
		}
		if ix.stateDir == "" {
			sel = dynamic.Skip(sel, eco, "no state directory: no records")
			continue
		}
		recs, warns := dynamic.Load(dynamic.StorePath(ix.stateDir, eco))
		for _, w := range warns {
			sel.Warnings = append(sel.Warnings, model.Widening{Tier: model.TierNone, Reason: model.ReasonDynamicSkipped, Detail: w})
		}
		if len(recs) == 0 {
			sel = dynamic.Skip(sel, eco, "no records (run `bgraph record`)")
			continue
		}
		env, collector, err := ix.ecoEnv(ctx, eco, in.Rules, in.BaseTree)
		if err != nil || env == "" {
			msg := "no environment fingerprint"
			if err != nil {
				msg = "environment fingerprint: " + err.Error()
			}
			sel = dynamic.Skip(sel, eco, msg)
			continue
		}
		bt := in.BaseTree
		sel = dynamic.Narrow(sel, changes, dynamic.Params{Eco: eco, Collector: collector, Env: env, Records: recs, UnitEco: res.UnitEco,
			BaseOID: func(p string) string {
				if !bt.Has(p) {
					return ""
				}
				return bt.Key(p)
			}})
	}
	if len(sel.Narrowed) > 1 {
		slices.SortFunc(sel.Narrowed, func(a, b model.NarrowedTest) int {
			if a.Unit < b.Unit {
				return -1
			}
			if a.Unit > b.Unit {
				return 1
			}
			return 0
		})
	}
	return sel
}

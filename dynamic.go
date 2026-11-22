package bgraph

import (
	"context"
	"slices"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/dynamic/pydyn"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/selector"
	"github.com/qxbao/bgraph/internal/tree"
)

// DynamicMode switches the dynamic layer (docs/dynamic.md) for one selection.
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
func (ix *Index) dynamicConfig(rf *rules.File) (python string, trust bool) {
	if rf != nil {
		python, trust = rf.Dynamic.Python, rf.Dynamic.TrustNativeEnv
	}
	if ix.opt.Python != "" {
		python = ix.opt.Python
	}
	if ix.opt.TrustNativeEnv {
		trust = true
	}
	return python, trust
}

// pythonEnv computes the Python environment fingerprint for tree t (docs/dynamic.md §5).
func (ix *Index) pythonEnv(ctx context.Context, rf *rules.File, t tree.Tree) (env, collector string, err error) {
	py, trust := ix.dynamicConfig(rf)
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

// applyDynamic runs the dynamic layer on a selection (docs/dynamic.md §2, §3). Anything that prevents it (no state directory,
// interpreter that cannot run, a guard) leaves the selection unchanged with a dynamic_skipped warning.
func (ix *Index) applyDynamic(ctx context.Context, ch Change, in selector.Input, res selector.Result) Selection {
	sel := res.Selection
	for _, eco := range dynamicEcos(ch.Dynamic, in.Rules) {
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
			changes = append(changes, dynamic.Change{Path: c.Path, Status: c.Status, OldMode: c.OldMode, NewMode: c.NewMode,
				BaseRole: roles[0], HeadRole: roles[1], Global: c.Global, Claimed: len(c.Claimed) > 0,
				Declared: dynamic.Declared(in.Rules, c.Path)})
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
		var env, collector string
		var err error
		switch eco {
		case "python":
			env, collector, err = ix.pythonEnv(ctx, in.Rules, in.BaseTree)
		}
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

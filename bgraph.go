// Package bgraph is the stable library API (docs/plan.md §7.1): multi-language affected-test selection, fail-closed.
//
//	idx, err := bgraph.Open(root, bgraph.Options{})
//	sel, err := idx.Select(ctx, bgraph.Change{Base: "origin/main", Worktree: true})
package bgraph

import (
	"context"
	"fmt"
	"slices"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/node"
	"github.com/qxbao/bgraph/internal/eco/rust"
	"github.com/qxbao/bgraph/internal/gitx"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/selector"
	"github.com/qxbao/bgraph/internal/tree"
)

// Public types (aliases of the internal types, to keep a single definition).
type (
	Selection    = model.Selection
	SelectedTest = model.SelectedTest
	Widening     = model.Widening
	Tier         = model.Tier
)

// Options configures Index.
type Options struct {
	// StateDir: durable index directory (Bint passes its state directory). Empty → the user's cache directory
	// (scancache.DefaultDir), never inside the repo.
	StateDir string
	// MemoryOnly: do not write the durable cache (tests, one-shot runs).
	MemoryOnly bool
}

// Index is the index of one repo. It keeps the scan cache between Select calls (the MCP server lives for the whole session).
type Index struct {
	repo  *gitx.Repo
	cache *scancache.Cache
	opt   Options
	memo  memo
}

// Open opens the repo containing root.
func Open(ctx context.Context, root string, opt Options) (*Index, error) {
	r, err := gitx.Open(ctx, root)
	if err != nil {
		return nil, err
	}
	ix := &Index{repo: r, cache: scancache.New(), opt: opt}
	if !opt.MemoryOnly {
		dir := opt.StateDir
		if dir == "" {
			dir, err = scancache.DefaultDir(r.Root)
		}
		if err == nil && dir != "" {
			if c, err := scancache.Open(dir); err == nil {
				ix.cache = c
			}
			ix.memo.dir = dir
		}
	}
	return ix, nil
}

// Warnings returns warnings about the durable cache store (downgraded to memory, corrupt files replaced...). They do not affect correctness.
func (ix *Index) Warnings() []string { return ix.cache.Warnings() }

// Root is the repo root.
func (ix *Index) Root() string { return ix.repo.Root }

// Close writes the durable cache and releases the long-lived git process.
func (ix *Index) Close() error {
	ix.cache.Close()
	return ix.repo.Close()
}

// Change describes the change to select tests for.
type Change struct {
	Base string // base ref or OID; empty → full (no_base)
	// Head: target commit; empty or Worktree=true → the working tree (including staged, unstaged and untracked parts).
	Head             string
	Worktree         bool
	PreviouslyFailed []string
}

// Select computes the selection. Infrastructure errors (git cannot run) return an error; everything else uncertain becomes a widening in the Selection.
func (ix *Index) Select(ctx context.Context, ch Change) (Selection, error) {
	in := selector.Input{Head: ch.Head, PreviouslyFailed: ch.PreviouslyFailed, Scanner: ix.cache}
	if ch.Base == "" {
		return noBase(in, "no base"), nil
	}
	base, err := ix.repo.RevParse(ctx, ch.Base)
	if err != nil {
		msg := err.Error()
		if sh, e2 := ix.repo.IsShallow(ctx); e2 == nil && sh {
			msg = "shallow clone has no base: " + msg
		}
		return noBase(in, msg), nil
	}
	in.Base = base
	if ch.Worktree || ch.Head == "" {
		change, wt, err := ix.repo.Snapshot(ctx, base)
		if err != nil {
			return Selection{}, err
		}
		in.HeadTree = wt
		for _, f := range change.Files {
			in.Changed = append(in.Changed, selector.Changed{Path: f.Path, Status: byte(f.Status), OldMode: f.OldMode, NewMode: f.NewMode})
		}
	} else {
		head, err := ix.repo.RevParse(ctx, ch.Head)
		if err != nil {
			return Selection{}, fmt.Errorf("head: %w", err)
		}
		in.Head = head
		ht, err := ix.memo.commitTree(ctx, ix.repo, head)
		if err != nil {
			return Selection{}, err
		}
		in.HeadTree = ht
	}
	bt, err := ix.memo.commitTree(ctx, ix.repo, base)
	if err != nil {
		return Selection{}, err
	}
	in.BaseTree = bt
	if in.Changed == nil && !ch.Worktree && ch.Head != "" {
		in.Changed = diffTrees(bt, in.HeadTree.(*gitx.CommitTree))
	}
	rf, rerr := loadRules(in.HeadTree)
	in.Rules, in.RulesErr = rf, rerr
	rulesKey := ""
	if b, err := in.HeadTree.Read(rules.FileName); err == nil {
		rulesKey = string(b)
	}
	for _, e := range Ecosystems(rf) {
		in.Ecos = append(in.Ecos, memoEco{Ecosystem: e, m: &ix.memo, rules: rulesKey})
	}
	// Scan the cache-missing parts of both trees in parallel (unchanged files hit the cache by blob OID and are not read).
	if err := ix.cache.Prefetch(ctx, in.HeadTree, in.HeadTree.Files(), 0); err != nil {
		return Selection{}, err
	}
	if err := ix.cache.Prefetch(ctx, bt, bt.Files(), 0); err != nil {
		return Selection{}, err
	}
	sel := selector.Select(ctx, in)
	ix.cache.Flush()
	return sel, nil
}

// Ecosystems are the supported ecosystems.
func Ecosystems(rf *rules.File) []eco.Ecosystem {
	return []eco.Ecosystem{node.New(rf), rust.New(rf)}
}

func loadRules(t tree.Tree) (*rules.File, error) {
	if !t.Has(rules.FileName) {
		return nil, nil
	}
	b, err := t.Read(rules.FileName)
	if err != nil {
		return nil, err
	}
	return rules.Parse(rules.FileName, b)
}

func noBase(in selector.Input, detail string) Selection {
	return Selection{Scope: model.TierFull, Base: in.Base, Head: in.Head, Changed: []string{},
		Widening: []model.Widening{{Tier: model.TierFull, Reason: model.ReasonNoBase, Detail: detail}}}
}

// diffTrees compares two commit trees by blob OID and mode (no git spawn cost).
func diffTrees(a, b *gitx.CommitTree) []selector.Changed {
	var out []selector.Changed
	for _, p := range b.Files() {
		if !a.Has(p) {
			out = append(out, selector.Changed{Path: p, Status: 'A', NewMode: b.Mode(p)})
		} else if a.Key(p) != b.Key(p) || a.Mode(p) != b.Mode(p) {
			out = append(out, selector.Changed{Path: p, Status: 'M', OldMode: a.Mode(p), NewMode: b.Mode(p)})
		}
	}
	for _, p := range a.Files() {
		if !b.Has(p) {
			out = append(out, selector.Changed{Path: p, Status: 'D', OldMode: a.Mode(p)})
		}
	}
	sa, sb := a.Submodules(), b.Submodules()
	for _, p := range sb {
		if !slices.Contains(sa, p) {
			out = append(out, selector.Changed{Path: p, Status: 'A', NewMode: "160000"})
		}
	}
	for _, p := range sa {
		if !slices.Contains(sb, p) {
			out = append(out, selector.Changed{Path: p, Status: 'D', OldMode: "160000"})
		} else {
			// CommitTree does not expose a gitlink's OID: cannot prove the pointer is unchanged → treat as changed (widening).
			out = append(out, selector.Changed{Path: p, Status: 'M', OldMode: "160000", NewMode: "160000"})
		}
	}
	return out
}

// Package bgraph is the library API for Go programs embedding bgraph: multi-language affected-test selection, fail-closed.
//
//	idx, err := bgraph.Open(root, bgraph.Options{})
//	sel, err := idx.Select(ctx, bgraph.Change{Base: "origin/main", Worktree: true})
package bgraph

import (
	"context"
	"fmt"
	"slices"

	"github.com/qxbao/bgraph/internal/dynamic/godyn"
	"github.com/qxbao/bgraph/internal/dynamic/nodedyn"
	"github.com/qxbao/bgraph/internal/dynamic/phpdyn"
	"github.com/qxbao/bgraph/internal/dynamic/pydyn"
	"github.com/qxbao/bgraph/internal/dynamic/rbdyn"
	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/cpp"
	"github.com/qxbao/bgraph/internal/eco/dart"
	"github.com/qxbao/bgraph/internal/eco/dotnet"
	"github.com/qxbao/bgraph/internal/eco/golang"
	"github.com/qxbao/bgraph/internal/eco/jvm"
	"github.com/qxbao/bgraph/internal/eco/node"
	"github.com/qxbao/bgraph/internal/eco/php"
	"github.com/qxbao/bgraph/internal/eco/python"
	"github.com/qxbao/bgraph/internal/eco/ruby"
	"github.com/qxbao/bgraph/internal/eco/rust"
	"github.com/qxbao/bgraph/internal/eco/swift"
	"github.com/qxbao/bgraph/internal/gitx"
	"github.com/qxbao/bgraph/internal/glob"
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
	// UnresolvedImport is an import the graph could not resolve (Index.Unresolved, DependentsResult.Unresolved).
	UnresolvedImport = model.Unresolved
)

// Options configures Index.
type Options struct {
	// StateDir: durable index directory (an embedding program may pass its own state directory). Empty → the user's cache directory
	// (scancache.DefaultDir), never inside the repo.
	StateDir string
	// MemoryOnly: do not write the durable cache (tests, one-shot runs). The dynamic layer still reads and writes its records
	// under StateDir when one is given.
	MemoryOnly bool
	// Python: interpreter of the dynamic layer (docs/dynamic-layer.md §5), overriding bgraph.toml [dynamic_layer] python. A path with
	// a separator is relative to the repo root; a bare name is looked up on PATH.
	Python string
	// TrustNativeEnv forces [dynamic_layer] trust_native_env = true (records and fingerprints use the trusting collector ID).
	TrustNativeEnv bool
	// Node: node executable of the dynamic layer (docs/dynamic-layer.md §8), overriding bgraph.toml [dynamic_layer] node.
	Node string
	// Ruby: Ruby executable of the dynamic layer (docs/dynamic-layer.md §10), overriding bgraph.toml [dynamic_layer] ruby.
	Ruby string
	// PHP: PHP executable of the dynamic layer (docs/dynamic-layer.md §11), overriding bgraph.toml [dynamic_layer] php.
	PHP string
	// Go: go command of the dynamic layer, overriding bgraph.toml [dynamic_layer] go.
	Go string
}

// Index is the index of one repo. It keeps the scan cache between Select calls (the MCP server lives for the whole session).
type Index struct {
	repo  *gitx.Repo
	cache *scancache.Cache
	opt   Options
	memo  memo
	// stateDir: where the dynamic layer keeps its records (empty: none, the layer cannot narrow).
	stateDir string
	probes   pydyn.ProbeCache
	// nodeProbes caches the node executable's description (docs/dynamic-layer.md §8.4).
	nodeProbes nodedyn.ProbeCache
	// rubyProbes caches the Ruby executable's description (docs/dynamic-layer.md §10.5).
	rubyProbes rbdyn.ProbeCache
	// phpProbes caches the PHP executable's probe (docs/dynamic-layer.md §11.5).
	phpProbes phpdyn.ProbeCache
	// goProbes caches the go command's probe (go env).
	goProbes godyn.ProbeCache
}

// Open opens the repo containing root.
func Open(ctx context.Context, root string, opt Options) (*Index, error) {
	r, err := gitx.Open(ctx, root)
	if err != nil {
		return nil, err
	}
	ix := &Index{repo: r, cache: scancache.New(), opt: opt, stateDir: opt.StateDir}
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
			ix.stateDir = dir
		}
	}
	r.LinkCache = ix.cache
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
	// Dynamic: whether the dynamic layer (docs/dynamic-layer.md) narrows widened units. DynamicDefault follows bgraph.toml
	// [dynamic_layer] ecosystems.
	Dynamic DynamicMode
	// Exclude: glob patterns (bgraph.toml syntax, forward slashes, relative to the repository root, e.g. ".tool/state/**") of
	// changed files that are inputs of no test, such as an embedding program's own state files in the working tree. Matching
	// changed files are dropped from the change set before selection (not in Selection.Changed, no widening, no seed); the
	// trees and the graph are unchanged. A plain data file is dropped even inside a project or package; a file is kept, with a
	// rule_rejected warning, when something references it (import, embed, include, a path literal in a test...), when it is
	// code, a test, a manifest, a config, a lockfile or generated output, or when a symbolic link or a submodule is involved.
	// An invalid pattern makes Select return an error.
	Exclude []string
}

// Select computes the selection. Infrastructure errors (git cannot run) return an error; everything else uncertain becomes a widening in the Selection.
func (ix *Index) Select(ctx context.Context, ch Change) (Selection, error) {
	in := selector.Input{Head: ch.Head, PreviouslyFailed: ch.PreviouslyFailed, Scanner: ix.cache}
	for _, e := range ch.Exclude {
		p, err := glob.Compile(e)
		if err != nil {
			return Selection{}, fmt.Errorf("exclude: %w", err)
		}
		in.Exclude = append(in.Exclude, p)
	}
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
	res := selector.Run(ctx, in)
	ix.cache.Flush()
	return ix.applyDynamic(ctx, ch, in, res), nil
}

// Ecosystems are the supported ecosystems.
func Ecosystems(rf *rules.File) []eco.Ecosystem {
	return []eco.Ecosystem{node.New(rf), rust.New(rf), python.New(rf), dotnet.New(rf), jvm.New(rf), golang.New(rf), php.New(rf), ruby.New(rf), swift.New(), cpp.New(), dart.New(rf)}
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

// diffTrees compares the entries of two commit trees by blob OID and mode (no git spawn cost). Symbolic links are entries here
// (a changed target text is a changed link), not followed.
func diffTrees(a, b *gitx.CommitTree) []selector.Changed {
	var out []selector.Changed
	for _, p := range b.Entries() {
		if a.Mode(p) == "" {
			out = append(out, selector.Changed{Path: p, Status: 'A', NewMode: b.Mode(p)})
		} else if a.EntryOID(p) != b.EntryOID(p) || a.Mode(p) != b.Mode(p) {
			out = append(out, selector.Changed{Path: p, Status: 'M', OldMode: a.Mode(p), NewMode: b.Mode(p)})
		}
	}
	for _, p := range a.Entries() {
		if b.Mode(p) == "" {
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
		} else if oa, ob := a.SubmoduleOID(p), b.SubmoduleOID(p); oa == "" || oa != ob {
			// The gitlink points elsewhere (or its OID is unknown): changed (widening).
			out = append(out, selector.Changed{Path: p, Status: 'M', OldMode: "160000", NewMode: "160000"})
		}
	}
	return out
}

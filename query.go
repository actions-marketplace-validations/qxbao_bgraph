package bgraph

import (
	"context"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/tree"
)

// analyzed is one tree with every ecosystem's analysis of it.
type analyzed struct {
	Tree     tree.Tree
	Rules    *rules.File
	Analyses []*eco.Analysis
}

// analyzeRev analyzes a tree: rev == "" → the working tree (tracked + untracked non-ignored, relative to HEAD);
// otherwise the commit tree of rev. Analyses go through the memo (and its durable snapshots), like Select.
// A malformed bgraph.toml is an error here: callers that query the graph cannot fail closed by widening.
func (ix *Index) analyzeRev(ctx context.Context, rev string) (*analyzed, error) {
	var t tree.Tree
	if rev == "" {
		head, err := ix.repo.RevParse(ctx, "HEAD")
		if err != nil {
			return nil, err
		}
		_, wt, err := ix.repo.Snapshot(ctx, head)
		if err != nil {
			return nil, err
		}
		t = wt
	} else {
		oid, err := ix.repo.RevParse(ctx, rev)
		if err != nil {
			return nil, err
		}
		ct, err := ix.memo.commitTree(ctx, ix.repo, oid)
		if err != nil {
			return nil, err
		}
		t = ct
	}
	if err := ix.cache.Prefetch(ctx, t, t.Files(), 0); err != nil {
		return nil, err
	}
	defer ix.cache.Flush()
	rf, err := loadRules(t)
	if err != nil {
		return nil, err
	}
	rulesKey := ""
	if b, err := t.Read(rules.FileName); err == nil {
		rulesKey = string(b)
	}
	out := &analyzed{Tree: t, Rules: rf}
	for _, e := range Ecosystems(rf) {
		a, err := memoEco{Ecosystem: e, m: &ix.memo, rules: rulesKey}.Analyze(ctx, t, ix.cache)
		if err != nil {
			return nil, err
		}
		out.Analyses = append(out.Analyses, a)
	}
	return out, nil
}

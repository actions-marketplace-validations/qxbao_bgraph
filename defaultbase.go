package bgraph

import (
	"context"
	"errors"
	"time"

	"github.com/qxbao/bgraph/internal/model"
)

// DefaultBase picks the default base for the CLI: the merge-base of HEAD with origin/HEAD, then origin/main, main, master.
// Returns an error if no ref is usable (the caller should pass an empty Base → Select returns full/no_base, or report the error).
func (ix *Index) DefaultBase(ctx context.Context) (string, error) {
	for _, ref := range []string{"origin/HEAD", "origin/main", "main", "master"} {
		if _, err := ix.repo.RevParse(ctx, ref); err != nil {
			continue
		}
		if mb, err := ix.repo.MergeBase(ctx, "HEAD", ref); err == nil {
			return mb, nil
		}
	}
	return "", errors.New("no default base found (origin/HEAD, origin/main, main, master); use --base")
}

// Unresolved analyzes the working tree and returns every unresolved import (with its widening scope), in a stable order by ecosystem.
func (ix *Index) Unresolved(ctx context.Context) ([]model.Unresolved, error) {
	st, err := ix.Analyze(ctx)
	return st.Unresolved, err
}

// AnalysisStats are statistics of the working-tree analysis.
type AnalysisStats struct {
	Files      int
	Edges      int
	Units      int
	Unresolved []model.Unresolved
	Elapsed    time.Duration
}

// Analyze runs every ecosystem's analysis on the working tree (Snapshot versus HEAD).
func (ix *Index) Analyze(ctx context.Context) (AnalysisStats, error) {
	start := time.Now()
	var st AnalysisStats
	head, err := ix.repo.RevParse(ctx, "HEAD")
	if err != nil {
		return st, err
	}
	_, wt, err := ix.repo.Snapshot(ctx, head)
	if err != nil {
		return st, err
	}
	st.Files = len(wt.Files())
	if err := ix.cache.Prefetch(ctx, wt, wt.Files(), 0); err != nil {
		return st, err
	}
	defer ix.cache.Flush()
	rf, rerr := loadRules(wt)
	if rerr != nil {
		return st, rerr
	}
	for _, e := range Ecosystems(rf) {
		a, err := e.Analyze(ctx, wt, ix.cache)
		if err != nil {
			return st, err
		}
		st.Edges += len(a.Edges)
		st.Units += len(a.Tests)
		st.Unresolved = append(st.Unresolved, a.Unresolved...)
	}
	st.Elapsed = time.Since(start)
	return st, nil
}

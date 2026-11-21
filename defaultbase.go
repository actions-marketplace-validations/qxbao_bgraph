package bgraph

import (
	"context"
	"errors"
	"time"

	"github.com/qxbao/bgraph/internal/model"
)

// DefaultBase chọn base mặc định cho CLI: merge-base của HEAD với origin/HEAD, rồi origin/main, main, master.
// Trả lỗi nếu không ref nào dùng được (người gọi nên truyền Base rỗng → Select trả full/no_base, hoặc báo lỗi).
func (ix *Index) DefaultBase(ctx context.Context) (string, error) {
	for _, ref := range []string{"origin/HEAD", "origin/main", "main", "master"} {
		if _, err := ix.repo.RevParse(ctx, ref); err != nil {
			continue
		}
		if mb, err := ix.repo.MergeBase(ctx, "HEAD", ref); err == nil {
			return mb, nil
		}
	}
	return "", errors.New("không tìm được base mặc định (origin/HEAD, origin/main, main, master); dùng --base")
}

// Unresolved phân tích cây làm việc và trả mọi import chưa phân giải (kèm phạm vi nới), thứ tự ổn định theo hệ sinh thái.
func (ix *Index) Unresolved(ctx context.Context) ([]model.Unresolved, error) {
	st, err := ix.Analyze(ctx)
	return st.Unresolved, err
}

// AnalysisStats là thống kê phân tích cây làm việc.
type AnalysisStats struct {
	Files      int
	Edges      int
	Units      int
	Unresolved []model.Unresolved
	Elapsed    time.Duration
}

// Analyze chạy phân tích của mọi hệ sinh thái trên cây làm việc (Snapshot so với HEAD).
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

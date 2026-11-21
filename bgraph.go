// Package bgraph là API thư viện ổn định (mục 7.1): chọn test bị ảnh hưởng đa ngôn ngữ, đóng khi lỗi.
//
//	idx, err := bgraph.Open(root, bgraph.Options{})
//	sel, err := idx.Select(ctx, bgraph.Change{Base: "origin/main", Worktree: true})
package bgraph

import (
	"context"
	"fmt"
	"slices"

	"bgraph/internal/eco"
	"bgraph/internal/eco/node"
	"bgraph/internal/gitx"
	"bgraph/internal/model"
	"bgraph/internal/rules"
	"bgraph/internal/scancache"
	"bgraph/internal/selector"
	"bgraph/internal/tree"
)

// Kiểu công khai (bí danh của kiểu nội bộ để giữ một định nghĩa).
type (
	Selection    = model.Selection
	SelectedTest = model.SelectedTest
	Widening     = model.Widening
	Tier         = model.Tier
)

// Options cấu hình Index.
type Options struct {
	// StateDir: thư mục chỉ mục bền (Bint truyền thư mục state của nó). Rỗng → thư mục cache của người dùng
	// (scancache.DefaultDir), không bao giờ nằm trong repo.
	StateDir string
	// MemoryOnly: không ghi cache bền (test, chạy một lần).
	MemoryOnly bool
}

// Index là chỉ mục của một repo. Giữ cache quét giữa các lần Select (server MCP sống suốt phiên).
type Index struct {
	repo  *gitx.Repo
	cache *scancache.Cache
	opt   Options
	memo  memo
}

// Open mở repo chứa root.
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

// Warnings trả cảnh báo về kho cache bền (đã hạ về bộ nhớ, tệp hỏng được thay...). Không ảnh hưởng tính đúng.
func (ix *Index) Warnings() []string { return ix.cache.Warnings() }

// Root là gốc repo.
func (ix *Index) Root() string { return ix.repo.Root }

// Close ghi cache bền và giải phóng tiến trình git sống lâu.
func (ix *Index) Close() error {
	ix.cache.Close()
	return ix.repo.Close()
}

// Change mô tả thay đổi cần chọn test.
type Change struct {
	Base string // ref hoặc OID mốc; rỗng → full (no_base)
	// Head: commit đích; rỗng hoặc Worktree=true → cây làm việc (gồm phần staged, unstaged, untracked).
	Head             string
	Worktree         bool
	PreviouslyFailed []string
}

// Select tính phần chọn. Lỗi hạ tầng (git không chạy được) trả error; mọi điều không chắc khác thành nới trong Selection.
func (ix *Index) Select(ctx context.Context, ch Change) (Selection, error) {
	in := selector.Input{Head: ch.Head, PreviouslyFailed: ch.PreviouslyFailed, Scanner: ix.cache}
	if ch.Base == "" {
		return noBase(in, "không có base"), nil
	}
	base, err := ix.repo.RevParse(ctx, ch.Base)
	if err != nil {
		msg := err.Error()
		if sh, e2 := ix.repo.IsShallow(ctx); e2 == nil && sh {
			msg = "clone nông không có base: " + msg
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
	// Quét song song phần trượt cache của cả hai cây (tệp chưa đổi trúng cache theo blob OID, không bị đọc).
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

// Ecosystems là các hệ sinh thái được hỗ trợ.
func Ecosystems(rf *rules.File) []eco.Ecosystem {
	return []eco.Ecosystem{node.New(rf)}
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

// diffTrees so hai cây commit theo blob OID và mode (không tốn spawn git).
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
			// CommitTree không lộ OID của gitlink: không chứng minh được con trỏ giữ nguyên → coi là đổi (nới).
			out = append(out, selector.Changed{Path: p, Status: 'M', OldMode: "160000", NewMode: "160000"})
		}
	}
	return out
}

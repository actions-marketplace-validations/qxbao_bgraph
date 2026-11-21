package evalkit

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// indexBuilder dựng các commit P (base + test_patch) và C (P + patch vàng) chỉ bằng git plumbing trên một tệp index tạm
// (GIT_INDEX_FILE): không checkout, không đụng cây làm việc. Commit-tree dùng môi trường git kín của tiến trình
// (tác giả/ngày cố định) nên P có cùng OID với cách `git commit` cũ trên worktree.
type indexBuilder struct {
	repo  string // thư mục repo (kho chứa đối tượng)
	idx   string // đường dẫn tệp index tạm
	env   []string
	base  string
	tree  string // cây hiện tại của index
	Head  string // commit hiện tại (base, rồi P, rồi C)
	ready bool
}

func newIndexBuilder(repo, idxFile, base string) *indexBuilder {
	return &indexBuilder{repo: repo, idx: idxFile, env: []string{"GIT_INDEX_FILE=" + idxFile}, base: base, Head: base}
}

func (b *indexBuilder) Close() { os.Remove(b.idx) }

func (b *indexBuilder) init(ctx context.Context) error {
	if b.ready {
		return nil
	}
	if _, err := gitIn(ctx, b.repo, b.env, "", "read-tree", b.base); err != nil {
		return err
	}
	b.ready = true
	return nil
}

// SetFile thay nội dung một tệp có sẵn (giữ mode) rồi ghi cây và commit mới có cha là Head hiện tại. Dùng cho T2 (đột biến).
func (b *indexBuilder) SetFile(ctx context.Context, path string, content []byte, msg string) (string, error) {
	if err := b.init(ctx); err != nil {
		return "", err
	}
	oid, err := gitIn(ctx, b.repo, nil, string(content), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	st, err := gitIn(ctx, b.repo, b.env, "", "ls-files", "-s", "--", path)
	if err != nil {
		return "", err
	}
	f := strings.Fields(st)
	if len(f) < 4 {
		return "", fmt.Errorf("tệp %s không có trong index", path)
	}
	if _, err := gitIn(ctx, b.repo, b.env, "", "update-index", "--cacheinfo", f[0]+","+strings.TrimSpace(oid)+","+path); err != nil {
		return "", err
	}
	return b.commit(ctx, msg)
}

func (b *indexBuilder) commit(ctx context.Context, msg string) (string, error) {
	tree, err := gitIn(ctx, b.repo, b.env, "", "write-tree")
	if err != nil {
		return "", err
	}
	tree = strings.TrimSpace(tree)
	c, err := gitIn(ctx, b.repo, nil, "", "commit-tree", tree, "-p", b.Head, "-m", msg)
	if err != nil {
		return "", err
	}
	b.tree = tree
	b.Head = strings.TrimSpace(c)
	return b.Head, nil
}

// Apply áp patch vào index (git apply --cached --binary) rồi ghi cây và commit mới có cha là Head hiện tại.
func (b *indexBuilder) Apply(ctx context.Context, patch, msg string) (string, error) {
	if err := b.init(ctx); err != nil {
		return "", err
	}
	if strings.TrimSpace(patch) == "" {
		return "", fmt.Errorf("patch rỗng")
	}
	if !strings.HasSuffix(patch, "\n") {
		patch += "\n"
	}
	if _, err := gitIn(ctx, b.repo, b.env, patch, "apply", "--cached", "--binary", "--whitespace=nowarn"); err != nil {
		return "", err
	}
	return b.commit(ctx, msg)
}

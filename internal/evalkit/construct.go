package evalkit

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// indexBuilder builds the P (base + test_patch) and C (P + gold patch) commits purely with git plumbing on a temporary index file
// (GIT_INDEX_FILE): no checkout, working tree untouched. Commit-tree uses the process's hermetic git environment
// (fixed author/date), so P has the same OID as the old `git commit` approach on a worktree.
type indexBuilder struct {
	repo  string // repo directory (object store)
	idx   string // path of the temporary index file
	env   []string
	base  string
	tree  string // current tree of the index
	Head  string // current commit (base, then P, then C)
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

// SetFile replaces the content of an existing file (keeping its mode), then writes the tree and a new commit whose parent is the current Head. Used by T2 (mutation).
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
		return "", fmt.Errorf("file %s is not in the index", path)
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

// Apply applies a patch to the index (git apply --cached --binary), then writes the tree and a new commit whose parent is the current Head.
func (b *indexBuilder) Apply(ctx context.Context, patch, msg string) (string, error) {
	if err := b.init(ctx); err != nil {
		return "", err
	}
	if strings.TrimSpace(patch) == "" {
		return "", fmt.Errorf("empty patch")
	}
	if !strings.HasSuffix(patch, "\n") {
		patch += "\n"
	}
	if _, err := gitIn(ctx, b.repo, b.env, patch, "apply", "--cached", "--binary", "--whitespace=nowarn"); err != nil {
		return "", err
	}
	return b.commit(ctx, msg)
}

// AddFile adds (or replaces) a regular file in the index, then writes the tree and a new commit whose parent is the current Head.
// Used for the rules overlay: bgraph.toml is injected into P, so P and C carry the identical file.
func (b *indexBuilder) AddFile(ctx context.Context, path string, content []byte, msg string) (string, error) {
	if err := b.init(ctx); err != nil {
		return "", err
	}
	oid, err := gitIn(ctx, b.repo, nil, string(content), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	if _, err := gitIn(ctx, b.repo, b.env, "", "update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(oid)+","+path); err != nil {
		return "", err
	}
	return b.commit(ctx, msg)
}

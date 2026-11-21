package evalkit

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/qxbao/bgraph/internal/tree"
)

// RustLocateRow is the locator outcome for one task.
type RustLocateRow struct {
	ID       string        `json:"instance_id"`
	Repo     string        `json:"repo"`
	Source   string        `json:"source"`
	Kind     string        `json:"f2p_kind,omitempty"`
	F2P      int           `json:"f2p_count"`
	Err      string        `json:"error,omitempty"`
	Locs     []RustTestLoc `json:"-"`
	Unique   int           `json:"unique"`
	Ambig    int           `json:"ambiguous"`
	NotFnd   int           `json:"not_found"`
	Doc      int           `json:"doc"`
	MacroHit int           `json:"not_found_macro_candidates"`
}

// RustLocateTask builds P (base + test patch, git plumbing only, no checkout) for one task in the clone repoDir and runs LocateRustTests
// over the P tree. kind is the dataset's FAIL_TO_PASS kind ("f2p" or "n2p", see evalkit/tools/fetch_rust.py).
// TODO(rust-phase-2): map the located files to Cargo target units with internal/eco/rust and feed T1.
func RustLocateTask(ctx context.Context, repoDir, idxFile string, t Task) RustLocateRow {
	row := RustLocateRow{ID: t.InstanceID, Repo: t.Repo, Source: t.Source}
	ids, err := ParseF2P(t.F2P)
	if err != nil {
		row.Err = err.Error()
		return row
	}
	row.F2P = len(ids)
	ib := newIndexBuilder(repoDir, idxFile, t.BaseCommit)
	defer ib.Close()
	p, err := ib.Apply(ctx, t.TestPatch, "eval: test patch")
	if err != nil {
		row.Err = "test_patch: " + err.Error()
		return row
	}
	gt, err := newGitTree(ctx, repoDir, p)
	if err != nil {
		row.Err = err.Error()
		return row
	}
	defer gt.Close()
	row.Locs = LocateRustTests(gt, ids)
	for _, l := range row.Locs {
		switch {
		case l.Doc:
			row.Doc++
		case l.NotFound:
			row.NotFnd++
			if len(l.MacroFiles) > 0 {
				row.MacroHit++
			}
		case l.Ambiguous:
			row.Ambig++
		default:
			row.Unique++
		}
	}
	return row
}

// RustRepoDir is the clone directory of a repo under work (<work>/repos/<owner>__<name>).
func RustRepoDir(work, repo string) string { return filepath.Join(work, "repos", repoDirName(repo)) }

// gitTree is a read-only tree.Tree of one commit, backed by a long-lived `git cat-file --batch` process. Only .rs blobs are listed
// with their content read on demand; other files are listed too (for Has/doc-test matching) but are never read.
type gitTree struct {
	files []string
	oid   map[string]string
	dirs  map[string]bool
	mu    sync.Mutex
	cmd   *exec.Cmd
	in    io.WriteCloser
	out   *bufio.Reader
}

func newGitTree(ctx context.Context, repoDir, rev string) (*gitTree, error) {
	return newGitTreeWant(ctx, repoDir, rev, func(p string) bool { return strings.HasSuffix(p, ".rs") || strings.HasSuffix(p, ".toml") })
}

// newGitTreeWant is newGitTree with a caller-chosen set of paths whose blobs are prefetched in one batch (the ones the caller will Read).
func newGitTreeWant(ctx context.Context, repoDir, rev string, want func(string) bool) (*gitTree, error) {
	ls, err := git(ctx, repoDir, "ls-tree", "-r", "-z", "--full-tree", rev)
	if err != nil {
		return nil, err
	}
	t := &gitTree{oid: map[string]string{}, dirs: map[string]bool{}}
	for _, e := range strings.Split(ls, "\x00") {
		tab := strings.IndexByte(e, '\t')
		if tab < 0 {
			continue
		}
		f := strings.Fields(e[:tab])
		if len(f) < 3 || f[1] != "blob" {
			continue
		}
		p := e[tab+1:]
		t.files = append(t.files, p)
		t.oid[p] = f[2]
		for d := filepath.ToSlash(filepath.Dir(p)); d != "." && !t.dirs[d]; d = filepath.ToSlash(filepath.Dir(d)) {
			t.dirs[d] = true
		}
	}
	sort.Strings(t.files)
	if err := prefetchBlobs(ctx, repoDir, t, want); err != nil {
		return nil, err
	}
	t.cmd = exec.CommandContext(ctx, "git", "-c", "gc.auto=0", "cat-file", "--batch")
	t.cmd.Dir = repoDir
	if t.in, err = t.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	so, err := t.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	t.out = bufio.NewReaderSize(so, 1<<20)
	if err := t.cmd.Start(); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *gitTree) Close() {
	if t.cmd != nil {
		t.in.Close()
		t.cmd.Wait()
	}
}

func (t *gitTree) Files() []string     { return t.files }
func (t *gitTree) Has(p string) bool   { _, ok := t.oid[p]; return ok }
func (t *gitTree) IsDir(p string) bool { return t.dirs[p] }
func (t *gitTree) Key(p string) string { return t.oid[p] }
func (t *gitTree) FoldLookup(p string) []string {
	var out []string
	for _, f := range t.files {
		if strings.EqualFold(f, p) {
			out = append(out, f)
		}
	}
	return out
}

func (t *gitTree) Read(p string) ([]byte, error) {
	oid, ok := t.oid[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := fmt.Fprintf(t.in, "%s\n", oid); err != nil {
		return nil, err
	}
	hdr, err := t.out.ReadString('\n')
	if err != nil {
		return nil, err
	}
	f := strings.Fields(hdr)
	if len(f) != 3 {
		return nil, fmt.Errorf("cat-file %s: %s", oid, strings.TrimSpace(hdr))
	}
	n, _ := strconv.Atoi(f[2])
	buf := make([]byte, n+1)
	if _, err := io.ReadFull(t.out, buf); err != nil {
		return nil, err
	}
	return bytes.Clone(buf[:n]), nil
}

var _ tree.Tree = (*gitTree)(nil)

// prefetchBlobs fetches the wanted blobs of the tree that a partial clone lacks in one batched round trip (the same command git itself
// uses for lazy fetches); a `cat-file --batch` would otherwise fetch them one by one.
func prefetchBlobs(ctx context.Context, repoDir string, t *gitTree, want func(string) bool) error {
	var in strings.Builder
	for _, p := range t.files {
		if want(p) {
			in.WriteString(t.oid[p] + "\n")
		}
	}
	chk, err := gitIn(ctx, repoDir, []string{"GIT_NO_LAZY_FETCH=1"}, in.String(), "cat-file", "--batch-check")
	if err != nil {
		return err
	}
	var miss strings.Builder
	n := 0
	for _, ln := range strings.Split(chk, "\n") {
		if strings.HasSuffix(ln, " missing") {
			miss.WriteString(strings.TrimSuffix(ln, " missing") + "\n")
			n++
		}
	}
	if n == 0 {
		return nil
	}
	_, err = gitIn(ctx, repoDir, nil, miss.String(), "-c", "gc.auto=0", "-c", "fetch.negotiationAlgorithm=noop", "fetch", "--no-tags",
		"--no-write-fetch-head", "--recurse-submodules=no", "--filter=blob:none", "--stdin", "origin")
	return err
}

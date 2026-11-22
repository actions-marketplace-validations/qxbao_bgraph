package evalkit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/qxbao/bgraph"
)

// Node in the Phase 6 harness (docs/dynamic.md §8.6): the hybrid T2 pass and the replay run on Node T2 repos too (Eco "" in the
// manifest: vitest, jest, mocha). Records are made with `bgraph record --eco node`; clones get a link to the main checkout's
// node_modules.

// dynEco is the dynamic-layer ecosystem of a T2 repo ("" when it has none).
func dynEco(r T2Repo) string {
	switch r.Eco {
	case "python":
		return "python"
	case "":
		return "node"
	}
	return ""
}

// openDynRepo opens an index on root for r's dynamic layer in mode (default | trust).
func openDynRepo(ctx context.Context, cfg *T2Config, r T2Repo, root, stateDir, mode string) (*bgraph.Index, error) {
	if dynEco(r) == "python" {
		return openDyn(ctx, root, stateDir, r.pyPython(cfg), mode)
	}
	return bgraph.Open(ctx, root, bgraph.Options{StateDir: stateDir, TrustNativeEnv: mode == "trust"})
}

// dynRecordEnv is the extra environment of r's recorded runs in checkout root.
func dynRecordEnv(r T2Repo, root string) []string {
	if dynEco(r) == "python" {
		return pyDynEnv(root, r.Env)
	}
	return r.Env
}

// isLink reports whether p is a symbolic link or a junction.
func isLink(p string) bool {
	_, err := os.Readlink(p)
	return err == nil
}

// dynClone is localClone for r: a Node clone's node_modules link is removed before `git clean -fdx` (which must never walk into
// the main checkout's packages) and made again afterwards.
func dynClone(ctx context.Context, r T2Repo, src, dst, rev string) error {
	if dynEco(r) != "node" {
		return localClone(ctx, src, dst, rev)
	}
	link := filepath.Join(dst, "node_modules")
	if isLink(link) {
		if err := os.Remove(link); err != nil || isLink(link) {
			return fmt.Errorf("cannot unlink %s before cleaning the clone: %v", link, err)
		}
	}
	if err := localClone(ctx, src, dst, rev); err != nil {
		return err
	}
	target := filepath.Join(src, "node_modules")
	if err := os.Symlink(target, link); err != nil {
		if out, err2 := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err2 != nil {
			return fmt.Errorf("link node_modules: %v / %v %s", err, err2, out)
		}
	}
	return nil
}

// dynSuite runs files of a Node repo in checkout root (the verification of narrowed commits) and returns their statuses.
func dynSuite(ctx context.Context, cfg *T2Config, r T2Repo, root string, files []string) (map[string]string, error) {
	tmp := filepath.Join(cfg.Dir, "tmp", r.Dir+"-dynverify")
	m, _, _, to, msg, err := r.runSuite(ctx, root, tmp, 20*time.Minute, files)
	switch {
	case err != nil:
		return nil, err
	case to:
		return nil, fmt.Errorf("timeout")
	case m == nil:
		return nil, fmt.Errorf("no result: %s", msg)
	}
	return m, nil
}

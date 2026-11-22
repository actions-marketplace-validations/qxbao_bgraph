package evalkit

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// nodeModulesDirs lists the node_modules directories (or links) of root, relative, down to a few levels (workspace packages
// have their own), without descending into any of them or into .git.
func nodeModulesDirs(root string) []string {
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		name := d.Name()
		switch {
		case name == "node_modules":
			out = append(out, rel)
			return filepath.SkipDir
		case name == ".git":
			return filepath.SkipDir
		case d.IsDir() && strings.Count(rel, string(filepath.Separator)) >= 3:
			return filepath.SkipDir
		}
		return nil
	})
	return out
}

// dynClone is localClone for r: a Node clone's node_modules links (one per node_modules directory of the main checkout: the root
// and each workspace package) are removed before `git clean -fdx`, which must never walk into the main checkout's packages, and
// made again afterwards, so that the clone sees the same installed packages (and environment fingerprint) as the checkout.
func dynClone(ctx context.Context, r T2Repo, src, dst, rev string) error {
	if dynEco(r) != "node" {
		return localClone(ctx, src, dst, rev)
	}
	if _, err := os.Stat(dst); err == nil {
		for _, rel := range nodeModulesDirs(dst) {
			link := filepath.Join(dst, rel)
			if isLink(link) {
				if err := os.Remove(link); err != nil || isLink(link) {
					return fmt.Errorf("cannot unlink %s before cleaning the clone: %v", link, err)
				}
			}
		}
	}
	if err := localClone(ctx, src, dst, rev); err != nil {
		return err
	}
	for _, rel := range nodeModulesDirs(src) {
		link, target := filepath.Join(dst, rel), filepath.Join(src, rel)
		if _, err := os.Stat(filepath.Dir(link)); err != nil {
			continue // the package does not exist at rev
		}
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink(target, link); err != nil {
			if out, err2 := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err2 != nil {
				return fmt.Errorf("link node_modules: %v / %v %s", err, err2, out)
			}
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

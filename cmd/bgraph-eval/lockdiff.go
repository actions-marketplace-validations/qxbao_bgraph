package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/qxbao/bgraph/internal/evalkit"
)

// cmdLockdiff: bgraph-eval lockdiff --format pnpm|npm|cargo --work DIR --repos a__b[:path/to/lockfile],... --out DIR
// (lockfile closures vs the package manager's own per-project dependency sets; gate L1).
func cmdLockdiff(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("lockdiff", flag.ExitOnError)
	format := fs.String("format", "", "pnpm | npm | cargo")
	evalDir := evalkit.EvalDir()
	work := fs.String("work", filepath.Join(evalDir, "work"), "work directory (repos/<owner__name>; default under $BGRAPH_EVAL_DIR)")
	tmp := fs.String("tmp", filepath.Join(evalDir, "tmp", "lockdiff"), "scratch directory (materialized trees and worktrees, removed after use)")
	var repos multi
	fs.Var(&repos, "repos", "repos owner__name[:lockfile path], comma-separated")
	commits := fs.Int("commits", 25, "last N lockfile-changing commits per repo")
	until := fs.String("until", "", "only commits before this date (e.g. 2023-06-01), to sample older lockfile versions")
	out := fs.String("out", "", "output directory (default <$BGRAPH_EVAL_DIR>/out/lockdiff-<format>)")
	jobs := fs.Int("jobs", 1, "repos in parallel")
	pnpm := fs.String("pnpm", "", "pnpm command (space-separated), e.g. \"node /path/to/pnpm/bin/pnpm.cjs\"")
	pnpmConvert := fs.String("pnpm-convert", "", "pnpm 9 command that rewrites 6.x lockfiles as 9.0 before listing, e.g. \"node /path/to/pnpm9/node_modules/pnpm/bin/pnpm.cjs\"")
	cargoHome := fs.String("cargo-home", evalkit.ToolPath("cargo-home", os.Getenv("CARGO_HOME")), "CARGO_HOME (default $BGRAPH_EVAL_TOOLS/cargo-home, else $CARGO_HOME; empty: inherited)")
	fs.Parse(args)
	if *format == "" || len(repos) == 0 {
		return fmt.Errorf("--format and --repos are required")
	}
	if *out == "" {
		*out = filepath.Join(evalDir, "out", "lockdiff-"+*format)
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return err
	}
	log := logger(*out)
	o := evalkit.LockdiffOpts{Format: *format, Work: *work, Tmp: filepath.Join(*tmp, *format), Commits: *commits, Until: *until, Log: log}
	os.MkdirAll(o.Tmp, 0o755)
	switch *format {
	case "pnpm":
		o.Pnpm = strings.Fields(*pnpm)
		o.PnpmConvert = strings.Fields(*pnpmConvert)
		if len(o.Pnpm) == 0 {
			o.Pnpm = []string{"pnpm"}
		}
		o.Env = []string{"npm_config_manage_package_manager_versions=false", "COREPACK_ENABLE_STRICT=0", "npm_config_update_notifier=false"}
		if evalkit.ToolsDir() != "" { // keep the pnpm store and caches next to the other portable tools
			o.Env = append(o.Env, "npm_config_store_dir="+evalkit.ToolPath("pnpm-store", ""), "npm_config_cache_dir="+evalkit.ToolPath("pnpm-cache", ""),
				"XDG_CACHE_HOME="+evalkit.ToolPath("pnpm-cache", ""), "PNPM_HOME="+evalkit.ToolPath("pnpm-home", ""))
		}
	case "npm":
		o.Env = []string{"npm_config_update_notifier=false", "npm_config_audit=false", "npm_config_fund=false"}
		if evalkit.ToolsDir() != "" {
			o.Env = append(o.Env, "npm_config_cache="+evalkit.ToolPath("npm-cache", ""))
		}
	case "cargo":
		o.Env = []string{"RUSTUP_TOOLCHAIN=stable", "CARGO_NET_GIT_FETCH_WITH_CLI=true"}
		if *cargoHome != "" {
			o.Env = append(o.Env, "CARGO_HOME="+*cargoHome)
		}
	default:
		return fmt.Errorf("unknown format %q", *format)
	}
	var mu sync.Mutex
	var all []evalkit.LockdiffRepo
	write := func() error {
		cp := append([]evalkit.LockdiffRepo(nil), all...)
		sort.Slice(cp, func(i, j int) bool { return cp[i].Repo < cp[j].Repo })
		b, _ := json.MarshalIndent(cp, "", " ")
		if err := os.WriteFile(filepath.Join(*out, "lockdiff.json"), b, 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(*out, "lockdiff.md"), []byte(evalkit.LockdiffSummary(*format, cp)), 0o644)
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, *jobs)
	for _, r := range repos {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := evalkit.LockdiffRun(ctx, o, r)
			mu.Lock()
			all = append(all, res)
			write()
			mu.Unlock()
			log("%s %s done (%s) skipped=%q", *format, res.Repo, res.Lockfile, res.Skipped)
		}()
	}
	wg.Wait()
	return write()
}

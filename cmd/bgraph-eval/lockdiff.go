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
	work := fs.String("work", `D:\bench\bgraph-eval\work`, "work directory (repos/<owner__name>)")
	tmp := fs.String("tmp", `D:\bench\bgraph-eval\tmp\lockdiff`, "scratch directory (materialized trees and worktrees, removed after use)")
	var repos multi
	fs.Var(&repos, "repos", "repos owner__name[:lockfile path], comma-separated")
	commits := fs.Int("commits", 25, "last N lockfile-changing commits per repo")
	until := fs.String("until", "", "only commits before this date (e.g. 2023-06-01), to sample older lockfile versions")
	out := fs.String("out", "", "output directory (default D:\\bench\\bgraph-eval\\out\\lockdiff-<format>)")
	jobs := fs.Int("jobs", 1, "repos in parallel")
	pnpm := fs.String("pnpm", "", "pnpm command (space-separated), e.g. \"node C:/.../pnpm/10.28.2/bin/pnpm.cjs\"")
	cargoHome := fs.String("cargo-home", `D:\bench\tools\cargo-home`, "CARGO_HOME")
	fs.Parse(args)
	if *format == "" || len(repos) == 0 {
		return fmt.Errorf("--format and --repos are required")
	}
	if *out == "" {
		*out = filepath.Join(`D:\bench\bgraph-eval\out`, "lockdiff-"+*format)
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
		if len(o.Pnpm) == 0 {
			o.Pnpm = []string{"pnpm"}
		}
		o.Env = []string{"npm_config_manage_package_manager_versions=false", "COREPACK_ENABLE_STRICT=0",
			`npm_config_store_dir=D:\bench\tools\pnpm-store`, `npm_config_cache_dir=D:\bench\tools\pnpm-cache`,
			`XDG_CACHE_HOME=D:\bench\tools\pnpm-cache`, `PNPM_HOME=D:\bench\tools\pnpm-home`, "npm_config_update_notifier=false"}
	case "npm":
		o.Env = []string{`npm_config_cache=D:\bench\npm-cache`, "npm_config_update_notifier=false", "npm_config_audit=false", "npm_config_fund=false"}
	case "cargo":
		o.Env = []string{"CARGO_HOME=" + *cargoHome, "RUSTUP_TOOLCHAIN=stable", "CARGO_NET_GIT_FETCH_WITH_CLI=true"}
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

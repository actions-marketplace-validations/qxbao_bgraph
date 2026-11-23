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

// cmdLockdiff: bgraph-eval lockdiff --format pnpm|npm|cargo|uv|poetry|yarn|bun --work DIR --repos a__b[:path/to/lockfile],... --out DIR
// (lockfile closures vs the package manager's own per-project dependency sets; gate L1).
func cmdLockdiff(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("lockdiff", flag.ExitOnError)
	format := fs.String("format", "", "pnpm | npm | cargo | uv | poetry | yarn | bun")
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
	yarn1 := fs.String("yarn1", "", "yarn classic command (space-separated; default node <$BGRAPH_EVAL_TOOLS>/yarn1/node_modules/yarn/bin/yarn.js, else yarn)")
	yarnBerry := fs.String("yarn-berry", "", "yarn 2+ command for commits that pin no yarnPath (default node <$BGRAPH_EVAL_TOOLS>/yarn4/node_modules/@yarnpkg/cli-dist/bin/yarn.js, else yarn)")
	bun := fs.String("bun", "", "bun command (default bun)")
	bunLinker := fs.String("bun-linker", "isolated", "bun install --linker: isolated (fast; the same resolution) or hoisted; empty: the project's own")
	cargoHome := fs.String("cargo-home", evalkit.ToolPath("cargo-home", os.Getenv("CARGO_HOME")), "CARGO_HOME (default $BGRAPH_EVAL_TOOLS/cargo-home, else $CARGO_HOME; empty: inherited)")
	uv := fs.String("uv", evalkit.ToolPath("uv-bin/uv", "uv"), "uv command (default $BGRAPH_EVAL_TOOLS/uv-bin/uv, else uv on PATH)")
	poetry := fs.String("poetry", evalkit.ToolPath("uv-bin/poetry", "poetry"), "poetry command with poetry-plugin-export (default $BGRAPH_EVAL_TOOLS/uv-bin/poetry, else poetry on PATH)")
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
	case "yarn":
		o.Yarn1 = strings.Fields(*yarn1)
		o.YarnBerry = strings.Fields(*yarnBerry)
		if len(o.Yarn1) == 0 {
			o.Yarn1 = []string{"yarn"}
			if evalkit.ToolsDir() != "" { // npm install --prefix <tools>/yarn1 yarn@1.22.22
				o.Yarn1 = []string{"node", evalkit.ToolPath("yarn1/node_modules/yarn/bin/yarn.js", "")}
			}
		}
		if len(o.YarnBerry) == 0 {
			o.YarnBerry = []string{"yarn"}
			if evalkit.ToolsDir() != "" { // npm install --prefix <tools>/yarn4 @yarnpkg/cli-dist@4
				o.YarnBerry = []string{"node", evalkit.ToolPath("yarn4/node_modules/@yarnpkg/cli-dist/bin/yarn.js", "")}
			}
		}
		// Only settings every yarn release knows: yarn 2 and 3 reject unknown ones (enableHardenedMode).
		o.Env = []string{"YARN_ENABLE_TELEMETRY=0", "COREPACK_ENABLE_STRICT=0"}
		if evalkit.ToolsDir() != "" {
			o.Env = append(o.Env, "YARN_CACHE_FOLDER="+evalkit.ToolPath("yarn1-cache", ""), "YARN_GLOBAL_FOLDER="+evalkit.ToolPath("yarn-berry-global", ""))
		}
	case "bun":
		o.Bun = strings.Fields(*bun)
		o.BunLinker = *bunLinker
		if len(o.Bun) == 0 {
			o.Bun = []string{"bun"}
		}
		o.Env = []string{"BUN_RUNTIME_TRANSPILER_CACHE_PATH=0", "DO_NOT_TRACK=1"}
		if evalkit.ToolsDir() != "" {
			o.Env = append(o.Env, "BUN_INSTALL_CACHE_DIR="+evalkit.ToolPath("bun-cache", ""), "BUN_INSTALL="+evalkit.ToolPath("bun/home", ""))
		}
	case "cargo":
		o.Env = []string{"RUSTUP_TOOLCHAIN=stable", "CARGO_NET_GIT_FETCH_WITH_CLI=true"}
		if *cargoHome != "" {
			o.Env = append(o.Env, "CARGO_HOME="+*cargoHome)
		}
	case "uv", "poetry":
		o.Uv, o.Poetry = *uv, *poetry
		// Read the lockfile only: never download an interpreter, never resolve, keep caches next to the other tools.
		o.Env = []string{"UV_PYTHON_DOWNLOADS=never", "UV_NO_PROGRESS=1", "POETRY_VIRTUALENVS_CREATE=false", "POETRY_NO_INTERACTION=1"}
		if evalkit.ToolsDir() != "" {
			o.Env = append(o.Env, "UV_CACHE_DIR="+evalkit.ToolPath("uv-cache", ""), "POETRY_CACHE_DIR="+evalkit.ToolPath("poetry-cache", ""))
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

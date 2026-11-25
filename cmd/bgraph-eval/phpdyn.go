package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qxbao/bgraph/internal/evalkit"
)

// Phase 12 evaluation of the PHP per-test-file layer (docs/dynamic-layer.md §11), run where PHP with Xdebug 3 is installed
// (the same PHP for recording and selecting):
//
//	bgraph-eval phpdyn t2 --clones DIR [--repos a,b] [--mutants id,...] [--oracle FILE]   hybrid T2 (per-file oracle from JUnit)
//	bgraph-eval phpdyn guard --repo NAME=CLONE[@REV]... [--commits 500]                   guard acceptance over first-parent commits
//	bgraph-eval phpdyn replay --repo NAME=CLONE[@REV]... [--commits 150] [--composer]    replay with fresh records at each parent
//
// Clones are working copies with their Composer install (vendor/); their working trees are checked out and restored.
func cmdPHPDyn(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 2, fmt.Errorf("phpdyn: want t2 | guard | replay")
	}
	sub := args[0]
	fs := flag.NewFlagSet("phpdyn "+sub, flag.ExitOnError)
	manifest := fs.String("manifest", "evalkit/manifest/t2-php-dyn.json", "t2: manifest of the repos")
	clones := fs.String("clones", "", "t2: directory of the working copies (<clones>/<dir>)")
	oracle := fs.String("oracle", filepath.Join(evalkit.EvalDir(), "t2", "php", "oracle.jsonl"), "t2: the cached PHP T2 oracle (killed mutants)")
	work := fs.String("work", filepath.Join(evalkit.EvalDir(), "t2", "phpdyn", "work"), "state and index directories")
	repos := fs.String("repos", "", "t2: repos (name or directory, comma-separated; default all)")
	mutants := fs.String("mutants", "", "t2: only these mutant IDs")
	jobs := fs.Int("jobs", 8, "record parallelism")
	rerecord := fs.Bool("rerecord", false, "t2: record again even if records of the current environment exist")
	var extra multiFlag
	fs.Var(&extra, "repo", "guard, replay: NAME=CLONE[@REV] (repeatable; REV default HEAD)")
	commits := fs.Int("commits", 500, "guard, replay: first-parent commits per repo")
	composer := fs.Bool("composer", false, "replay: `composer update` when composer.json at the parent differs from the installed one")
	out := fs.String("out", filepath.Join(evalkit.EvalDir(), "t2", "phpdyn", "out"), "report directory")
	fs.Parse(args[1:])
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return 1, err
	}
	lg := logger(*out)
	type spec struct{ name, clone, rev string }
	var specs []spec
	for _, e := range extra {
		name, s, ok := strings.Cut(e, "=")
		if !ok {
			return 2, fmt.Errorf("--repo %q: want NAME=CLONE[@REV]", e)
		}
		clone, rev, _ := strings.Cut(s, "@")
		if rev == "" {
			rev = "HEAD"
		}
		specs = append(specs, spec{name, clone, rev})
	}
	switch sub {
	case "t2":
		man, err := evalkit.LoadT2Manifest(*manifest)
		if err != nil {
			return 2, err
		}
		if *clones == "" {
			return 2, fmt.Errorf("--clones is required")
		}
		var only map[string]bool
		if *mutants != "" {
			only = map[string]bool{}
			for _, id := range splitList(*mutants) {
				only[id] = true
			}
		}
		want := splitList(*repos)
		var all []evalkit.PHPDynT2Result
		viol := 0
		for _, r := range man.Repos {
			if len(want) > 0 && !containsFold(want, r.Name, r.Dir) {
				continue
			}
			cfg := evalkit.PHPDynConfig{Repo: r, Clone: filepath.Join(*clones, r.Dir), Work: *work, Out: *out, Oracle: *oracle, Jobs: *jobs,
				Rerecord: *rerecord, Logf: lg}
			res := evalkit.RunPHPDynT2(ctx, cfg, only)
			for _, m := range res.Mutants {
				viol += len(m.Confirmed)
			}
			all = append(all, res)
			if err := evalkit.WriteDynJSON(*out, "t2.json", all); err != nil {
				return 1, err
			}
			if ctx.Err() != nil {
				return 1, ctx.Err()
			}
		}
		md := evalkit.PHPDynT2Markdown(all)
		os.WriteFile(filepath.Join(*out, "t2.md"), []byte(md), 0o644)
		fmt.Print(md)
		if viol > 0 {
			return 3, nil
		}
	case "guard":
		var all []evalkit.PHPGuardResult
		for _, s := range specs {
			res, err := evalkit.RunPHPGuardRate(ctx, s.name, s.clone, filepath.Join(*work, "guard-state", filepath.Base(s.clone)), s.rev, *commits, lg)
			if err != nil {
				return 1, fmt.Errorf("%s: %w", s.name, err)
			}
			all = append(all, res)
			if err := evalkit.WriteDynJSON(*out, "guard.json", all); err != nil {
				return 1, err
			}
		}
		md := evalkit.PHPGuardMarkdown(all)
		os.WriteFile(filepath.Join(*out, "guard.md"), []byte(md), 0o644)
		fmt.Print(md)
	case "replay":
		var md strings.Builder
		var all []evalkit.PHPReplayResult
		misses := 0
		for _, s := range specs {
			dir := filepath.Base(s.clone)
			cfg := evalkit.PHPReplayConfig{
				PHPDynConfig: evalkit.PHPDynConfig{Repo: evalkit.T2Repo{Name: s.name, Dir: dir}, Clone: s.clone, Work: *work, Out: *out, Jobs: *jobs, Logf: lg},
				Rev:          s.rev, Commits: *commits, Composer: *composer,
			}
			res := evalkit.RunPHPReplay(ctx, cfg)
			for _, c := range res.Commits {
				misses += len(c.Misses)
			}
			all = append(all, res)
			if err := evalkit.WriteDynJSON(*out, "replay.json", all); err != nil {
				return 1, err
			}
			md.WriteString(evalkit.PHPReplayMarkdown(res) + "\n")
			if ctx.Err() != nil {
				return 1, ctx.Err()
			}
		}
		os.WriteFile(filepath.Join(*out, "replay.md"), []byte("# PHP layer: replay\n\n"+md.String()), 0o644)
		fmt.Print(md.String())
		if misses > 0 {
			return 3, nil
		}
	default:
		return 2, fmt.Errorf("phpdyn: unknown subcommand %q (t2 | guard | replay)", sub)
	}
	return 0, nil
}

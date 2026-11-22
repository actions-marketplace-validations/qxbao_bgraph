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

// Phase 6 evaluation commands (docs/dynamic.md §7):
//
//	bgraph-eval t2dyn     [--set DIR=MANIFEST ...] [--repos a,b] [--modes default,trust] [--prepare] [--rerecord] [--jobs 8] [--out DIR]
//	bgraph-eval replaydyn --dir DIR --manifest M [--repos a,b] [--commits 150] [--sample 8] [--modes default,trust] [--out DIR]

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// cmdT2Dyn runs the hybrid pass against the cached Python T2 oracles (or, with --prepare, writes the widening-targeted mutants).
func cmdT2Dyn(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("t2dyn", flag.ExitOnError)
	var sets multiFlag
	fs.Var(&sets, "set", "T2 set DIR=MANIFEST (repeatable; default the python and python-dyn sets)")
	repos := fs.String("repos", "", "only these repos (name or directory, comma-separated)")
	modes := fs.String("modes", "default,trust", "collector settings: default, trust")
	prepare := fs.Bool("prepare", false, "write mutants-<dir>.json with mutants only in files whose edit widens, then stop")
	rerecord := fs.Bool("rerecord", false, "record again even if records of the current environment exist")
	jobs := fs.Int("jobs", 8, "record parallelism")
	out := fs.String("out", "D:/bench/bgraph-eval/out/phase6-t2dyn", "report directory")
	fs.Parse(args)
	if len(sets) == 0 {
		sets = multiFlag{"D:/bench/bgraph-eval/t2/python=evalkit/manifest/t2-python.json", "D:/bench/bgraph-eval/t2/python-dyn=evalkit/manifest/t2-python-dyn.json"}
	}
	lg := logger(*out)
	var all []evalkit.DynRepoResult
	for _, s := range sets {
		dir, manPath, ok := strings.Cut(s, "=")
		if !ok {
			return 2, fmt.Errorf("--set %q: want DIR=MANIFEST", s)
		}
		man, err := evalkit.LoadT2Manifest(manPath)
		if err != nil {
			return 2, err
		}
		if err := evalkit.SetupHermeticEnv(filepath.Join(dir, "gitcfg")); err != nil {
			return 2, err
		}
		cfg := evalkit.T2Config{Dir: dir, Manifest: man, Repos: splitList(*repos), Log: lg}
		if *prepare {
			for _, r := range man.Repos {
				if len(cfg.Repos) > 0 && !containsFold(cfg.Repos, r.Name, r.Dir) {
					continue
				}
				t, err := evalkit.PrepareDynMutants(ctx, cfg, r)
				if err != nil {
					return 1, err
				}
				lg("%s: %d target files: %s", r.Name, len(t), strings.Join(t, " "))
			}
			continue
		}
		rs, err := evalkit.RunT2Dyn(ctx, cfg, evalkit.DynOptions{Modes: splitList(*modes), Rerecord: *rerecord, Jobs: *jobs})
		all = append(all, rs...)
		if err != nil {
			return 1, err
		}
	}
	if *prepare {
		return 0, nil
	}
	if err := evalkit.WriteDynJSON(*out, "t2dyn.json", all); err != nil {
		return 1, err
	}
	md := evalkit.T2DynMarkdown(all, splitList(*modes))
	if err := os.WriteFile(filepath.Join(*out, "t2dyn.md"), []byte(md), 0o644); err != nil {
		return 1, err
	}
	fmt.Println(md)
	for _, r := range all {
		for _, c := range r.Checks {
			for _, h := range c.Hybrid {
				if len(h.Missed) > 0 {
					return 1, nil
				}
			}
		}
	}
	return 0, nil
}

func containsFold(list []string, names ...string) bool {
	for _, x := range list {
		for _, n := range names {
			if strings.EqualFold(x, n) {
				return true
			}
		}
	}
	return false
}

// cmdReplayDyn replays recent commits of Python repos with records made once at R0.
func cmdReplayDyn(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("replaydyn", flag.ExitOnError)
	var sets multiFlag
	fs.Var(&sets, "set", "T2 set DIR=MANIFEST (repeatable; default the python and python-dyn sets)")
	repos := fs.String("repos", "", "only these repos (name or directory, comma-separated)")
	modes := fs.String("modes", "default,trust", "collector settings: default, trust")
	commits := fs.Int("commits", 150, "first-parent commits per repo, ending at the manifest's R")
	sample := fs.Int("sample", 8, "commits per repo verified by running the removed units")
	rerecord := fs.Bool("rerecord", false, "record again at R0")
	jobs := fs.Int("jobs", 8, "record parallelism")
	out := fs.String("out", "D:/bench/bgraph-eval/out/phase6-replay", "report directory")
	fs.Parse(args)
	if len(sets) == 0 {
		sets = multiFlag{"D:/bench/bgraph-eval/t2/python=evalkit/manifest/t2-python.json", "D:/bench/bgraph-eval/t2/python-dyn=evalkit/manifest/t2-python-dyn.json"}
	}
	lg := logger(*out)
	var all []evalkit.DynReplayResult
	for _, s := range sets {
		dir, manPath, ok := strings.Cut(s, "=")
		if !ok {
			return 2, fmt.Errorf("--set %q: want DIR=MANIFEST", s)
		}
		man, err := evalkit.LoadT2Manifest(manPath)
		if err != nil {
			return 2, err
		}
		if err := evalkit.SetupHermeticEnv(filepath.Join(dir, "gitcfg")); err != nil {
			return 2, err
		}
		for _, r := range man.Repos {
			if *repos != "" && !containsFold(splitList(*repos), r.Name, r.Dir) {
				continue
			}
			res := evalkit.RunDynReplay(ctx, evalkit.DynReplayConfig{T2: evalkit.T2Config{Dir: dir, Manifest: man, Log: lg}, Repo: r,
				Commits: *commits, Modes: splitList(*modes), Sample: *sample, Jobs: *jobs, Rerecord: *rerecord})
			if res.Error != "" {
				lg("%s replay: %s", r.Name, res.Error)
			}
			all = append(all, res)
			if err := evalkit.WriteDynJSON(*out, "replaydyn.json", all); err != nil { // keep partial results
				return 1, err
			}
			if ctx.Err() != nil {
				return 1, ctx.Err()
			}
		}
	}
	md := evalkit.ReplayDynMarkdown(all, splitList(*modes))
	if err := os.WriteFile(filepath.Join(*out, "replaydyn.md"), []byte(md), 0o644); err != nil {
		return 1, err
	}
	fmt.Println(md)
	return 0, nil
}

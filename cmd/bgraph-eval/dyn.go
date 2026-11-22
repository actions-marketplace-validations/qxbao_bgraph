package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qxbao/bgraph/internal/evalkit"
)

// Phase 6 evaluation commands (docs/dynamic-layer.md §7):
//
//	bgraph-eval t2dyn     [--set DIR=MANIFEST ...] [--repos a,b] [--modes default,trust] [--prepare] [--rerecord] [--jobs 8] [--out DIR]
//	bgraph-eval replaydyn --dir DIR --manifest M [--repos a,b] [--commits 150] [--sample 8] [--modes default,trust] [--out DIR]

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// defaultPythonDynSets are the Python and Python-dyn T2 sets under the eval directory.
func defaultPythonDynSets() multiFlag {
	t2 := filepath.Join(evalkit.EvalDir(), "t2")
	return multiFlag{filepath.Join(t2, "python") + "=evalkit/manifest/t2-python.json", filepath.Join(t2, "python-dyn") + "=evalkit/manifest/t2-python-dyn.json"}
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// cmdT2Dyn runs the hybrid pass against the cached Python and Node T2 oracles (or, with --prepare, writes the widening-targeted
// Python mutants). Node sets: --set <eval dir>/t2=evalkit/manifest/t2.json (hono, dayjs) and
// --set <eval dir>/t2/node-dyn=evalkit/manifest/t2-node-dyn.json (express); <eval dir> is $BGRAPH_EVAL_DIR (evalkit.EvalDir).
func cmdT2Dyn(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("t2dyn", flag.ExitOnError)
	var sets multiFlag
	fs.Var(&sets, "set", "T2 set DIR=MANIFEST (repeatable; default the python and python-dyn sets)")
	repos := fs.String("repos", "", "only these repos (name or directory, comma-separated)")
	modes := fs.String("modes", "default,trust", "collector settings: default, trust")
	prepare := fs.Bool("prepare", false, "write mutants-<dir>.json with mutants only in files whose edit widens, then stop")
	rerecord := fs.Bool("rerecord", false, "record again even if records of the current environment exist")
	jobs := fs.Int("jobs", 8, "record parallelism")
	out := fs.String("out", filepath.Join(evalkit.EvalDir(), "out", "phase6-t2dyn"), "report directory (default under $BGRAPH_EVAL_DIR)")
	fs.Parse(args)
	if len(sets) == 0 {
		sets = defaultPythonDynSets()
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
	if evalkit.HybridViolations(all) > 0 {
		return 1, nil
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

// cmdReplayDyn replays recent commits of Python or Node repos with records made once at R0.
func cmdReplayDyn(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("replaydyn", flag.ExitOnError)
	var sets multiFlag
	fs.Var(&sets, "set", "T2 set DIR=MANIFEST (repeatable; default the python and python-dyn sets)")
	repos := fs.String("repos", "", "only these repos (name or directory, comma-separated)")
	modes := fs.String("modes", "default,trust", "collector settings: default, trust")
	commits := fs.Int("commits", 150, "first-parent commits per repo, ending at the manifest's R")
	sample := fs.Int("sample", 8, "commits per repo verified by running the removed units")
	rerecord := fs.Bool("rerecord", false, "record again at R0")
	fresh := fs.Bool("fresh", false, "also record at each P of a widened commit whose guards pass and select it again (mode fresh)")
	freshMax := fs.Int("fresh-max", 0, "with --fresh: at most this many fresh commits, spread over the eligible ones (0: all)")
	jobs := fs.Int("jobs", 8, "record parallelism")
	out := fs.String("out", filepath.Join(evalkit.EvalDir(), "out", "phase6-replay"), "report directory (default under $BGRAPH_EVAL_DIR)")
	render := fs.String("render", "", "only render the report from these replaydyn.json files (comma-separated; a later file's repo replaces an earlier one)")
	fs.Parse(args)
	if *render != "" {
		return renderReplayDyn(*out, splitList(*render), splitList(*modes))
	}
	if len(sets) == 0 {
		sets = defaultPythonDynSets()
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
				Commits: *commits, Modes: splitList(*modes), Sample: *sample, Jobs: *jobs, Rerecord: *rerecord, Fresh: *fresh, FreshMax: *freshMax})
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
	ms := splitList(*modes)
	if *fresh {
		ms = append(ms, "fresh")
	}
	md := evalkit.ReplayDynMarkdown(all, ms)
	if err := os.WriteFile(filepath.Join(*out, "replaydyn.md"), []byte(md), 0o644); err != nil {
		return 1, err
	}
	fmt.Println(md)
	return 0, nil
}

// renderReplayDyn merges replaydyn.json files (by repo, later wins) and writes the report to out.
func renderReplayDyn(out string, files, modes []string) (int, error) {
	var all []evalkit.DynReplayResult
	idx := map[string]int{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return 1, err
		}
		var rs []evalkit.DynReplayResult
		if err := json.Unmarshal(b, &rs); err != nil {
			return 1, fmt.Errorf("%s: %w", f, err)
		}
		for _, r := range rs {
			if i, ok := idx[r.Repo]; ok {
				all[i] = r
				continue
			}
			idx[r.Repo] = len(all)
			all = append(all, r)
		}
	}
	hasFresh := false
	for _, r := range all {
		for _, c := range r.Commits {
			if _, ok := c.Hybrid["fresh"]; ok {
				hasFresh = true
			}
		}
	}
	if hasFresh {
		modes = append(modes, "fresh")
	}
	if err := evalkit.WriteDynJSON(out, "replaydyn.json", all); err != nil {
		return 1, err
	}
	md := evalkit.ReplayDynMarkdown(all, modes)
	if err := os.WriteFile(filepath.Join(out, "replaydyn.md"), []byte(md), 0o644); err != nil {
		return 1, err
	}
	fmt.Println(md)
	return 0, nil
}

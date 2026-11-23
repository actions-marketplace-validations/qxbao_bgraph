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

// Phase 10 evaluation of the Ruby method layer (docs/dynamic-layer.md §10.7):
//
//	bgraph-eval rbdyn t2 [--repos rack,...] [--mutants id,...]       hybrid T2 on the cached Ruby oracle
//	bgraph-eval rbdyn guard [--repo NAME=CLONE[@REV]]... [--commits 500]  guard acceptance over first-parent commits
//	bgraph-eval rbdyn replay --repos rack,dry-types [--from guard.json] [--n 20]  replay of guard-accepted commits
//
// The Ruby interpreter is $BGRAPH_EVAL_RUBY_BIN (as for the Ruby T2). Clones and state live under --work (default
// $BGRAPH_EVAL_DIR/t2/ruby-dyn); the T2 set is only read.
func cmdRubyDyn(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 2, fmt.Errorf("rbdyn: want t2 | guard | replay")
	}
	sub := args[0]
	fs := flag.NewFlagSet("rbdyn "+sub, flag.ExitOnError)
	t2 := fs.String("t2", filepath.Join(evalkit.EvalDir(), "t2", "ruby"), "the Ruby T2 set (oracle.jsonl, checkouts, bundles; read only)")
	manifest := fs.String("manifest", "evalkit/manifest/t2-ruby.json", "T2 manifest")
	work := fs.String("work", filepath.Join(evalkit.EvalDir(), "t2", "ruby-dyn"), "clones and state of this evaluation")
	repos := fs.String("repos", "", "T2 repos (name or directory, comma-separated; default all for t2, rack,dry-types for replay)")
	mutants := fs.String("mutants", "", "t2: only these mutant IDs")
	jobs := fs.Int("jobs", 6, "record parallelism")
	rerecord := fs.Bool("rerecord", false, "t2: record again even if records of the current environment exist")
	var extra multiFlag
	fs.Var(&extra, "repo", "guard: extra repo NAME=CLONE[@REV] (repeatable; REV default HEAD)")
	commits := fs.Int("commits", 500, "guard: first-parent commits per repo")
	from := fs.String("from", "", "replay: guard.json whose accepted commits are replayed (default <out>/guard.json)")
	n := fs.Int("n", 20, "replay: commits per repo (the most recent accepted ones)")
	out := fs.String("out", filepath.Join(evalkit.EvalDir(), "t2", "ruby-dyn", "out"), "report directory")
	fs.Parse(args[1:])
	man, err := evalkit.LoadT2Manifest(*manifest)
	if err != nil {
		return 2, err
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return 1, err
	}
	lg := logger(*out)
	t2cfg := evalkit.T2Config{Dir: *t2, Manifest: man, Log: lg}
	pick := func(def []string) []evalkit.T2Repo {
		want := splitList(*repos)
		if len(want) == 0 {
			want = def
		}
		var rs []evalkit.T2Repo
		for _, r := range man.Repos {
			if len(want) == 0 || containsFold(want, r.Name, r.Dir) {
				rs = append(rs, r)
			}
		}
		return rs
	}
	cfgOf := func(r evalkit.T2Repo) (evalkit.RbDynConfig, error) {
		return evalkit.RbDynConfig{T2: t2cfg, Repo: r, Work: *work, Jobs: *jobs, Rerecord: *rerecord}, evalkit.RbDynSetEnv(*t2, r)
	}
	switch sub {
	case "t2":
		var only map[string]bool
		if *mutants != "" {
			only = map[string]bool{}
			for _, id := range splitList(*mutants) {
				only[id] = true
			}
		}
		var all []evalkit.RbDynT2Result
		viol := 0
		for _, r := range pick(nil) {
			cfg, err := cfgOf(r)
			if err != nil {
				return 1, err
			}
			res := evalkit.RunRubyDynT2(ctx, cfg, only)
			if res.Error != "" {
				lg("%s: %s", r.Name, res.Error)
			}
			for _, m := range res.Mutants {
				viol += len(m.Hybrid.Missed)
			}
			all = append(all, res)
			if err := evalkit.WriteDynJSON(*out, "t2.json", all); err != nil {
				return 1, err
			}
			if ctx.Err() != nil {
				return 1, ctx.Err()
			}
		}
		md := evalkit.RbDynT2Markdown(all)
		os.WriteFile(filepath.Join(*out, "t2.md"), []byte(md), 0o644)
		fmt.Print(md)
		if viol > 0 {
			return 3, nil
		}
	case "guard":
		var all []evalkit.RbGuardResult
		run := func(name, clone, rev string) error {
			res, err := evalkit.RunRubyGuardRate(ctx, name, clone, filepath.Join(*work, "guard-state", filepath.Base(clone)), rev, *commits, lg)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			all = append(all, res)
			return evalkit.WriteDynJSON(*out, "guard.json", all)
		}
		if len(extra) == 0 || *repos != "" {
			for _, r := range pick(nil) {
				cfg, err := cfgOf(r)
				if err != nil {
					return 1, err
				}
				if err := evalkit.RbDynCheckoutR(ctx, cfg); err != nil {
					return 1, err
				}
				if err := run(r.Name, filepath.Join(*work, r.Dir), r.R); err != nil {
					return 1, err
				}
			}
		}
		for _, e := range extra {
			name, spec, ok := strings.Cut(e, "=")
			if !ok {
				return 2, fmt.Errorf("--repo %q: want NAME=CLONE[@REV]", e)
			}
			clone, rev, _ := strings.Cut(spec, "@")
			if rev == "" {
				rev = "HEAD"
			}
			if err := run(name, clone, rev); err != nil {
				return 1, err
			}
		}
		md := evalkit.RbGuardMarkdown(all)
		os.WriteFile(filepath.Join(*out, "guard.md"), []byte(md), 0o644)
		fmt.Print(md)
	case "replay":
		src := *from
		if src == "" {
			src = filepath.Join(*out, "guard.json")
		}
		b, err := os.ReadFile(src)
		if err != nil {
			return 2, err
		}
		var guard []evalkit.RbGuardResult
		if err := json.Unmarshal(b, &guard); err != nil {
			return 2, err
		}
		var md strings.Builder
		all := map[string][]evalkit.RbReplayCommit{}
		misses := 0
		for _, r := range pick([]string{"rack", "dry-types"}) {
			var cs []string
			for _, g := range guard {
				if g.Repo != r.Name {
					continue
				}
				for _, c := range g.Commits { // newest first
					if c.Stage == "accepted" && len(cs) < *n {
						cs = append(cs, c.C)
					}
				}
			}
			if len(cs) == 0 {
				lg("%s: no accepted commit in %s", r.Name, src)
				continue
			}
			cfg, err := cfgOf(r)
			if err != nil {
				return 1, err
			}
			if err := evalkit.RbDynReplayBundle(*t2, *work, r); err != nil {
				return 1, err
			}
			cfg.Install = true
			res := evalkit.RunRubyReplay(ctx, cfg, cs)
			all[r.Name] = res
			for _, c := range res {
				misses += len(c.Misses)
			}
			if err := evalkit.WriteDynJSON(*out, "replay.json", all); err != nil {
				return 1, err
			}
			md.WriteString(evalkit.RbReplayMarkdown(r.Name, res) + "\n")
			if ctx.Err() != nil {
				return 1, ctx.Err()
			}
		}
		os.WriteFile(filepath.Join(*out, "replay.md"), []byte(md.String()), 0o644)
		fmt.Print(md.String())
		if misses > 0 {
			return 3, nil
		}
	default:
		return 2, fmt.Errorf("rbdyn: unknown subcommand %q (t2 | guard | replay)", sub)
	}
	return 0, nil
}

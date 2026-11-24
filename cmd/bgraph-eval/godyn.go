package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/qxbao/bgraph/internal/evalkit"
)

// Phase 13 evaluation of the Go layer (per top-level test function inside a Go package):
//
//	bgraph-eval godyn t2 [--repos hcl,...] [--mutants id,...] [--go DIR=EXE]...        hybrid T2 on the cached Go oracle
//	bgraph-eval godyn guard [--repos ...] [--repo NAME=CLONE[@REV]]... [--commits 500]   real guard over first-parent commits
//	bgraph-eval godyn replay --repos hcl,common [--commits 150] [--more N] [--from guard.json]
//
// --go DIR=EXE picks the go executable per T2 repo directory (default: go on PATH); it runs with GOTOOLCHAIN=local, first on
// PATH, CGO_ENABLED=0 and GOCACHE=<work>/gocache-<dir>, at record and at selection time alike. Clones and state live under
// --work (default $BGRAPH_EVAL_DIR/t2/go-dyn); the Go T2 set is only read.
func cmdGoDyn(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 2, fmt.Errorf("godyn: want t2 | guard | replay")
	}
	sub := args[0]
	fs := flag.NewFlagSet("godyn "+sub, flag.ExitOnError)
	t2 := fs.String("t2", filepath.Join(evalkit.EvalDir(), "t2", "go"), "the Go T2 set (oracle.jsonl, mutants, clones; read only)")
	manifest := fs.String("manifest", "evalkit/manifest/t2-go.json", "T2 manifest")
	work := fs.String("work", filepath.Join(evalkit.EvalDir(), "t2", "go-dyn"), "clones and state of this evaluation")
	repos := fs.String("repos", "", "T2 repos (name or directory, comma-separated; default all for t2/guard, hcl,common for replay)")
	mutants := fs.String("mutants", "", "t2: only these mutant IDs")
	jobs := fs.Int("jobs", 0, "record parallelism (0: the layer's default)")
	rerecord := fs.Bool("rerecord", false, "t2: record again even if usable records of the current environment exist")
	var gos, extra multiFlag
	fs.Var(&gos, "go", "go executable for a T2 repo directory, DIR=EXE (repeatable)")
	fs.Var(&extra, "repo", "guard: extra repo NAME=CLONE[@REV] (repeatable; REV default HEAD)")
	commits := fs.Int("commits", 500, "guard: first-parent commits per repo; replay: the recent window")
	from := fs.String("from", "", "replay: guard.json for --more (default <out>/guard.json)")
	more := fs.Int("more", 0, "replay: also replay up to N guard-accepted commits of guard.json older than the window")
	render := fs.Bool("render", false, "guard: only render <out>/guard.md from <out>/guard.json")
	out := fs.String("out", filepath.Join(evalkit.EvalDir(), "t2", "go-dyn", "out"), "report directory")
	fs.Parse(args[1:])
	if sub == "replay" && !flagSet(fs, "commits") {
		*commits = 150
	}
	man, err := evalkit.LoadT2Manifest(*manifest)
	if err != nil {
		return 2, err
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return 1, err
	}
	lg := logger(*out)
	t2cfg := evalkit.T2Config{Dir: *t2, Manifest: man, Log: lg}
	goExe := map[string]string{}
	for _, g := range gos {
		d, e, ok := strings.Cut(g, "=")
		if !ok {
			return 2, fmt.Errorf("--go %q: want DIR=EXE", g)
		}
		goExe[d] = e
	}
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
	cfgOf := func(r evalkit.T2Repo) (evalkit.GoDynConfig, error) {
		exe := goExe[r.Dir]
		if exe == "" {
			exe = "go"
		}
		p, err := exec.LookPath(exe)
		if err != nil {
			return evalkit.GoDynConfig{}, err
		}
		if p, err = filepath.Abs(p); err != nil {
			return evalkit.GoDynConfig{}, err
		}
		return evalkit.GoDynConfig{T2: t2cfg, Repo: r, Work: *work, Go: p, Jobs: *jobs, Rerecord: *rerecord},
			evalkit.GoDynSetEnv(*work, r.Dir, p)
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
		var all []evalkit.GoDynT2Result
		if *render { // re-render t2.md from the t2.json files given in --from (comma-separated), concatenated
			for _, f := range splitList(*from) {
				b, err := os.ReadFile(f)
				if err != nil {
					return 2, err
				}
				var part []evalkit.GoDynT2Result
				if err := json.Unmarshal(b, &part); err != nil {
					return 2, err
				}
				all = append(all, part...)
			}
			if err := evalkit.WriteDynJSON(*out, "t2.json", all); err != nil {
				return 1, err
			}
			md := evalkit.GoDynT2Markdown(all)
			os.WriteFile(filepath.Join(*out, "t2.md"), []byte(md), 0o644)
			fmt.Print(md)
			return 0, nil
		}
		viol := 0
		for _, r := range pick(nil) {
			cfg, err := cfgOf(r)
			if err != nil {
				return 1, err
			}
			res := evalkit.RunGoDynT2(ctx, cfg, only)
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
		md := evalkit.GoDynT2Markdown(all)
		os.WriteFile(filepath.Join(*out, "t2.md"), []byte(md), 0o644)
		fmt.Print(md)
		if viol > 0 {
			return 3, nil
		}
	case "guard":
		var all []evalkit.GoGuardResult
		if *render {
			b, err := os.ReadFile(filepath.Join(*out, "guard.json"))
			if err != nil {
				return 2, err
			}
			if err := json.Unmarshal(b, &all); err != nil {
				return 2, err
			}
			md := evalkit.GoGuardMarkdown(all)
			os.WriteFile(filepath.Join(*out, "guard.md"), []byte(md), 0o644)
			fmt.Print(md)
			return 0, nil
		}
		run := func(name, clone, rev string) error {
			res, err := evalkit.RunGoGuardRate(ctx, name, clone, filepath.Join(*work, "guard-state", filepath.Base(clone)), rev, *commits, lg)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			all = append(all, res)
			return evalkit.WriteDynJSON(*out, "guard.json", all)
		}
		if len(extra) == 0 || *repos != "" {
			for _, r := range pick(nil) {
				clone := filepath.Join(*work, r.Dir)
				if err := evalkit.GoDynClone(ctx, filepath.Join(*t2, r.Dir), clone, r.R); err != nil {
					return 1, err
				}
				if err := run(r.Name, clone, r.R); err != nil {
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
			if err := evalkit.GoDynClone(ctx, clone, clone, rev); err != nil {
				return 1, err
			}
			if err := run(name, clone, rev); err != nil {
				return 1, err
			}
		}
		md := evalkit.GoGuardMarkdown(all)
		os.WriteFile(filepath.Join(*out, "guard.md"), []byte(md), 0o644)
		fmt.Print(md)
	case "replay":
		var guard []evalkit.GoGuardResult
		if *more > 0 {
			src := *from
			if src == "" {
				src = filepath.Join(*out, "guard.json")
			}
			b, err := os.ReadFile(src)
			if err != nil {
				return 2, err
			}
			if err := json.Unmarshal(b, &guard); err != nil {
				return 2, err
			}
		}
		var md strings.Builder
		all := map[string][]evalkit.GoReplayCommit{}
		misses := 0
		for _, r := range pick([]string{"hcl", "common"}) {
			cfg, err := cfgOf(r)
			if err != nil {
				return 1, err
			}
			if err := evalkit.GoDynClone(ctx, filepath.Join(*t2, r.Dir), filepath.Join(*work, r.Dir), r.R); err != nil {
				return 1, err
			}
			cs, err := evalkit.FirstParent(ctx, filepath.Join(*work, r.Dir), r.R, *commits)
			if err != nil {
				return 1, err
			}
			window := len(cs)
			in := map[string]bool{}
			for _, c := range cs {
				in[c] = true
			}
			added := 0
			for _, g := range guard {
				if g.Repo != r.Name {
					continue
				}
				for _, c := range g.Commits { // newest first
					if c.Stage == "accepted" && !in[c.C] && added < *more {
						cs = append(cs, c.C)
						in[c.C] = true
						added++
					}
				}
			}
			res := evalkit.RunGoReplay(ctx, cfg, cs, window)
			all[r.Name] = res
			for _, c := range res {
				misses += len(c.Misses)
			}
			if err := evalkit.WriteDynJSON(*out, "replay.json", all); err != nil {
				return 1, err
			}
			md.WriteString(evalkit.GoReplayMarkdown(r.Name, res) + "\n")
			os.WriteFile(filepath.Join(*out, "replay.md"), []byte(md.String()), 0o644)
			if ctx.Err() != nil {
				return 1, ctx.Err()
			}
		}
		fmt.Print(md.String())
		if misses > 0 {
			return 3, nil
		}
	default:
		return 2, fmt.Errorf("godyn: unknown subcommand %q (t2 | guard | replay)", sub)
	}
	return 0, nil
}

func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

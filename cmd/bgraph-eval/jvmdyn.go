package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/evalkit"
)

// Phase 7 evaluation of the JVM class layer (docs/dynamic-layer.md §9.8), one repo per invocation (the JDK and build tool come from
// the environment, as for `bgraph record --eco jvm`, so that the fingerprints match):
//
//	bgraph-eval jvmdyn --repo gson --clone DIR --state DIR [--mutants id,...] [--replay N --units GLOB --commits 300] [--out DIR]
func cmdJVMDyn(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("jvmdyn", flag.ExitOnError)
	t2 := fs.String("t2", "D:/bench/bgraph-eval/t2/jvm", "the JVM T2 set (oracle.jsonl; read only)")
	manifest := fs.String("manifest", "evalkit/manifest/t2-jvm.json", "T2 manifest")
	repo := fs.String("repo", "", "repo (name or directory)")
	clone := fs.String("clone", "", "shared clone to run in (its working tree is modified)")
	state := fs.String("state", "", "bgraph state directory with dynamic/jvm.jsonl")
	buildArgs := fs.String("build-args", "", "extra build arguments, space-separated")
	mutants := fs.String("mutants", "", "only these mutant IDs")
	replay := fs.Int("replay", 0, "replay at most N code-only commits instead of the T2 pass (records made fresh at each parent)")
	units := fs.String("units", "", "replay: --units glob of the recordings")
	commits := fs.Int("commits", 300, "replay: first-parent commits examined")
	out := fs.String("out", "D:/bench/bgraph-eval/out/phase7-jvmdyn", "report directory")
	fs.Parse(args)
	man, err := evalkit.LoadT2Manifest(*manifest)
	if err != nil {
		return 2, err
	}
	var r *evalkit.T2Repo
	for i := range man.Repos {
		if man.Repos[i].Dir == *repo || man.Repos[i].Name == *repo {
			r = &man.Repos[i]
		}
	}
	if r == nil || *clone == "" || *state == "" {
		return 2, fmt.Errorf("--repo (in the manifest), --clone and --state are required")
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*out, "gitcfg")); err != nil {
		return 1, err
	}
	lg := logger(*out)
	cfg := evalkit.JVMDynConfig{T2Dir: *t2, Clone: *clone, StateDir: *state, Repo: *r, BuildArgs: strings.Fields(*buildArgs), Timeout: 40 * time.Minute, Logf: lg}
	if *replay > 0 {
		cs, err := evalkit.RunJVMDynReplay(ctx, evalkit.JVMReplayConfig{JVMDynConfig: cfg, Units: *units, Commits: *commits, Max: *replay})
		if err != nil {
			return 1, err
		}
		if err := evalkit.WriteDynJSON(*out, "replay-"+r.Dir+".json", cs); err != nil {
			return 1, err
		}
		md := evalkit.JVMReplayMarkdown(r.Name, cs)
		os.WriteFile(filepath.Join(*out, "replay-"+r.Dir+".md"), []byte(md), 0o644)
		fmt.Print(md)
		return 0, nil
	}
	var only map[string]bool
	if *mutants != "" {
		only = map[string]bool{}
		for _, id := range strings.Split(*mutants, ",") {
			only[id] = true
		}
	}
	res, err := evalkit.RunJVMDynT2(ctx, cfg, only)
	if err != nil {
		return 1, err
	}
	if err := evalkit.WriteDynJSON(*out, "t2-"+r.Dir+".json", res); err != nil {
		return 1, err
	}
	md := evalkit.JVMDynMarkdown([]evalkit.JVMDynResult{res})
	os.WriteFile(filepath.Join(*out, "t2-"+r.Dir+".md"), []byte(md), 0o644)
	fmt.Print(md)
	viol := 0
	for _, m := range res.Mutants {
		viol += len(m.Violations)
	}
	if viol > 0 {
		return 3, nil
	}
	return 0, nil
}

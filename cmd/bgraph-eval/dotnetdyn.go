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

// Phase 8 evaluation of the .NET class layer (docs/dynamic-layer.md §9.8), one repo per invocation (the SDK comes from the
// environment, as for `bgraph record --eco dotnet`, so that the fingerprints match):
//
//	bgraph-eval dotnetdyn --repo polly --clone DIR --state DIR [--mutants id,...] [--out DIR]
func cmdDotnetDyn(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("dotnetdyn", flag.ExitOnError)
	t2 := fs.String("t2", filepath.Join(evalkit.EvalDir(), "t2", "dotnet"), "the .NET T2 set (oracle.jsonl; read only; default under $BGRAPH_EVAL_DIR)")
	manifest := fs.String("manifest", "evalkit/manifest/t2-dotnet.json", "T2 manifest")
	repo := fs.String("repo", "", "repo (name or directory)")
	clone := fs.String("clone", "", "clone at R to run in (its working tree is modified and restored)")
	state := fs.String("state", "", "bgraph state directory with dynamic/dotnet.jsonl")
	mutants := fs.String("mutants", "", "only these mutant IDs")
	out := fs.String("out", filepath.Join(evalkit.EvalDir(), "out", "phase8-dotnetdyn"), "report directory (default under $BGRAPH_EVAL_DIR)")
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
	var only map[string]bool
	if *mutants != "" {
		only = map[string]bool{}
		for _, id := range strings.Split(*mutants, ",") {
			only[id] = true
		}
	}
	cfg := evalkit.DotnetDynConfig{T2Dir: *t2, Clone: *clone, StateDir: *state, Repo: *r, Timeout: 30 * time.Minute, Logf: logger(*out)}
	res, err := evalkit.RunDotnetDynT2(ctx, cfg, only)
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

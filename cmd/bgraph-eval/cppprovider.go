package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/qxbao/bgraph/internal/evalkit"
)

func cmdCPPProvider(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cpp-provider", flag.ExitOnError)
	work := fs.String("work", "", "evaluation workspace with repository clones")
	native := fs.String("native", "", "native metadata captured by evalkit/tools/cpp_provider.py")
	out := fs.String("out", "", "JSON results file")
	fs.Parse(args)
	if *work == "" || *native == "" {
		return fmt.Errorf("--work and --native are required")
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return err
	}
	results, err := evalkit.CompareCPPProviders(ctx, *work, *native)
	if err != nil {
		return err
	}
	for _, result := range results {
		fmt.Printf("%-24s %.12s %-5s %-16s %-24s native=%d sources=%d static=%d mismatches=%d %s\n",
			result.Repo, result.Commit, result.System, result.Project, result.Mode,
			len(result.NativeNames), result.NativeInputs, len(result.StaticNames), len(result.Mismatches), result.Error)
	}
	if *out == "" {
		return nil
	}
	data, err := json.MarshalIndent(results, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(*out, append(data, '\n'), 0o644)
}

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

func cmdSwiftProvider(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("swift-provider", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "Swift task JSONL")
	work := fs.String("work", "", "work directory (clones in <work>/repos)")
	out := fs.String("out", "", "write swift-provider.json and swift-provider.md here")
	per := fs.Int("per-repo", 5, "maximum distinct base commits per repo")
	only := fs.String("repos", "", "only these repos (comma-separated)")
	jobs := fs.Int("jobs", 4, "repositories checked in parallel")
	fs.Parse(args)
	if *tasksPath == "" || *work == "" || *per < 1 || *jobs < 1 {
		return fmt.Errorf("--tasks, --work, and positive --per-repo and --jobs are required")
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return err
	}
	tasks, err := evalkit.LoadTasks(*tasksPath)
	if err != nil {
		return err
	}
	var repos []string
	seen := map[string]bool{}
	for _, task := range tasks {
		if !seen[task.Repo] && (*only == "" || contains(*only, task.Repo)) {
			seen[task.Repo] = true
			repos = append(repos, task.Repo)
		}
	}
	sort.Strings(repos)
	byRepo := make([][]evalkit.SwiftProviderResult, len(repos))
	var wg sync.WaitGroup
	sem := make(chan struct{}, *jobs)
	for i, repo := range repos {
		wg.Add(1)
		go func(i int, repo string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			byRepo[i] = evalkit.SwiftProviderCompare(ctx, *work, repo, evalkit.CargoMetaCommits(tasks, repo, *per))
		}(i, repo)
	}
	wg.Wait()
	var results []evalkit.SwiftProviderResult
	for _, rows := range byRepo {
		results = append(results, rows...)
	}
	var report strings.Builder
	report.WriteString("# SwiftPM metadata vs bgraph\n\n| repo | commit | package | mode | native test targets | native test sources | result |\n|---|---|---|---|---:|---:|---|\n")
	for _, r := range results {
		verdict := "match"
		switch {
		case r.Error != "":
			verdict = "unavailable"
		case len(r.Mismatches) != 0:
			verdict = fmt.Sprintf("%d mismatches", len(r.Mismatches))
		}
		fmt.Fprintf(&report, "| %s | %.12s | %s | %s | %d | %d | %s |\n", r.Repo, r.Commit, r.Package, r.Mode, r.Targets, r.Files, verdict)
	}
	for _, r := range results {
		if r.Error != "" || len(r.Mismatches) != 0 {
			fmt.Fprintf(&report, "\n## %s @ %.12s (%s)\n\n", r.Repo, r.Commit, r.Package)
			if r.Error != "" {
				fmt.Fprintf(&report, "%s\n", r.Error)
			}
			for _, mismatch := range r.Mismatches {
				fmt.Fprintf(&report, "- %s\n", mismatch)
			}
		}
	}
	fmt.Print(report.String())
	if *out == "" {
		return ctx.Err()
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(results, "", " ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "swift-provider.json"), append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*out, "swift-provider.md"), []byte(report.String()), 0o644)
}

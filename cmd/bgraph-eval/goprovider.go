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

// cmdGoProvider compares bgraph's Go analysis with `go list -e -json -test ./...` at up to --per-repo base commits of --repos-max repos.
func cmdGoProvider(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("go-provider", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "Go task JSONL")
	work := fs.String("work", "", "work directory")
	out := fs.String("out", "", "write go-provider.json and go-provider.md here")
	per := fs.Int("per-repo", 5, "distinct base commits per repo")
	nrepos := fs.Int("repos-max", 25, "repos to check (multi-module / multi-package first)")
	maxGo := fs.Int("max-go-files", 3000, "skip repos with more Go files than this")
	only := fs.String("repos", "", "only these repos (comma-separated; overrides the automatic choice)")
	jobs := fs.Int("jobs", 3, "repos in parallel")
	fs.Parse(args)
	if *tasksPath == "" || *work == "" {
		return fmt.Errorf("--tasks and --work are required")
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return err
	}
	tasks, err := evalkit.LoadTasks(*tasksPath)
	if err != nil {
		return err
	}
	var repos []string
	if *only != "" {
		for _, r := range strings.Split(*only, ",") {
			repos = append(repos, strings.TrimSpace(r))
		}
	} else {
		repos = evalkit.GoProviderRepos(ctx, *work, tasks, *nrepos, *maxGo)
	}
	fmt.Println("repos:", repos)
	var mu sync.Mutex
	var all []evalkit.GoProviderResult
	var wg sync.WaitGroup
	sem := make(chan struct{}, *jobs)
	for _, r := range repos {
		wg.Add(1)
		go func(r string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			for _, res := range evalkit.GoProviderCompare(ctx, *work, r, evalkit.CargoMetaCommits(tasks, r, *per)) {
				verdict := "match"
				switch {
				case res.Error != "":
					verdict = "ERROR"
				case !res.Match:
					verdict = fmt.Sprintf("%d mismatches", len(res.Mismatches))
				}
				mu.Lock()
				all = append(all, res)
				writeGoProvider(*out, all)
				fmt.Printf("%-34s %.10s %-7s modules=%d failed=%d listed=%d units=%d edges=%d embeds=%d %s\n", res.Repo, res.Commit, res.GOOS, res.Modules, len(res.Failed), res.Listed, res.Units, res.Edges, res.Embeds, verdict)
				mu.Unlock()
			}
		}(r)
	}
	wg.Wait()
	return writeGoProvider(*out, all)
}

// writeGoProvider writes go-provider.json and go-provider.md (called after every result so a long run leaves partial output).
func writeGoProvider(out string, all []evalkit.GoProviderResult) error {
	all = append([]evalkit.GoProviderResult(nil), all...)
	sort.Slice(all, func(i, j int) bool {
		if all[i].Repo != all[j].Repo {
			return all[i].Repo < all[j].Repo
		}
		if all[i].Commit != all[j].Commit {
			return all[i].Commit < all[j].Commit
		}
		return all[i].GOOS < all[j].GOOS
	})
	var md strings.Builder
	md.WriteString("# go list vs bgraph\n\n| repo | commit | GOOS | modules | go list failed | packages | w/ tests | units | import edges | embed files | pkg errors | unlisted in-repo imports | result |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, res := range all {
		v := "match"
		switch {
		case res.Error != "":
			v = "ERROR"
		case !res.Match:
			v = fmt.Sprintf("%d mismatches", len(res.Mismatches))
		}
		fmt.Fprintf(&md, "| %s | %.10s | %s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %s |\n", res.Repo, res.Commit, res.GOOS, res.Modules, len(res.Failed), res.Listed, res.WithTst, res.Units, res.Edges, res.Embeds, res.PkgErrs, res.Unlisted, v)
	}
	md.WriteString("\n## Details\n")
	for _, res := range all {
		if res.Error == "" && res.Match && len(res.Failed) == 0 {
			continue
		}
		fmt.Fprintf(&md, "\n### %s @ %.10s (GOOS %s)\n\n", res.Repo, res.Commit, res.GOOS)
		if res.Error != "" {
			fmt.Fprintf(&md, "error: %s\n", res.Error)
		}
		for _, f := range res.Failed {
			fmt.Fprintf(&md, "- go list failed: %s\n", strings.ReplaceAll(f, "\n", " "))
		}
		for _, m := range res.Mismatches {
			fmt.Fprintf(&md, "- MISMATCH %s\n", m)
		}
	}
	if out != "" {
		if err := os.MkdirAll(out, 0o755); err != nil {
			return err
		}
		b, _ := json.MarshalIndent(all, "", " ")
		os.WriteFile(filepath.Join(out, "go-provider.json"), b, 0o644)
		return os.WriteFile(filepath.Join(out, "go-provider.md"), []byte(md.String()), 0o644)
	}
	return nil
}

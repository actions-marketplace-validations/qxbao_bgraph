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

	"github.com/qxbao/bgraph/internal/evalkit"
)

// cmdCargoMeta compares bgraph's Rust analysis with `cargo metadata --no-deps` at up to --per-repo base commits of every Rust repo.
func cmdCargoMeta(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cargometa", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "Rust task JSONL")
	work := fs.String("work", "", "work directory")
	out := fs.String("out", "", "write cargometa.json and cargometa.md here")
	per := fs.Int("per-repo", 5, "distinct base commits per repo")
	only := fs.String("repos", "", "only these repos (comma-separated)")
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
	seen := map[string]bool{}
	for _, t := range tasks {
		k := strings.ToLower(t.Repo)
		if !seen[k] && (*only == "" || contains(*only, t.Repo)) {
			seen[k] = true
			repos = append(repos, t.Repo)
		}
	}
	sort.Strings(repos)
	var all []evalkit.CargoMetaResult
	var md strings.Builder
	md.WriteString("# cargo metadata vs bgraph\n\n| repo | commit | packages | units | result |\n|---|---|---|---|---|\n")
	for _, r := range repos {
		for _, res := range evalkit.CargoMetaCompare(ctx, *work, r, evalkit.CargoMetaCommits(tasks, r, *per)) {
			all = append(all, res)
			verdict := "match"
			switch {
			case res.Error != "":
				verdict = "ERROR"
			case !res.Match:
				verdict = fmt.Sprintf("%d mismatches", len(res.Mismatches))
			}
			fmt.Fprintf(&md, "| %s | %.10s | %d | %d | %s |\n", res.Repo, res.Commit, res.Packages, res.Units, verdict)
			fmt.Printf("%-24s %.10s packages=%-3d units=%-4d %s\n", res.Repo, res.Commit, res.Packages, res.Units, verdict)
			if res.Error != "" {
				fmt.Println("    error:", res.Error)
			}
			for _, m := range res.Mismatches {
				fmt.Println("    -", m)
			}
		}
	}
	md.WriteString("\n## Details\n")
	for _, res := range all {
		if res.Error == "" && res.Match {
			continue
		}
		fmt.Fprintf(&md, "\n### %s @ %.10s\n\n", res.Repo, res.Commit)
		if res.Error != "" {
			fmt.Fprintf(&md, "error: %s\n", res.Error)
		}
		for _, m := range res.Mismatches {
			fmt.Fprintf(&md, "- %s\n", m)
		}
	}
	if *out != "" {
		if err := os.MkdirAll(*out, 0o755); err != nil {
			return err
		}
		b, _ := json.MarshalIndent(all, "", " ")
		os.WriteFile(filepath.Join(*out, "cargometa.json"), b, 0o644)
		return os.WriteFile(filepath.Join(*out, "cargometa.md"), []byte(md.String()), 0o644)
	}
	return nil
}

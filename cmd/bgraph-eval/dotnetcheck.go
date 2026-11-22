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
	"time"

	"github.com/qxbao/bgraph/internal/evalkit"
)

func dotnetToolsFlags(fs *flag.FlagSet) *evalkit.DotnetTools {
	t := &evalkit.DotnetTools{}
	fs.StringVar(&t.Dotnet, "dotnet", `D:\bench\tools\dotnet\dotnet.exe`, "portable dotnet executable")
	fs.StringVar(&t.Affected, "dotnet-affected", `D:\bench\tools\dotnet-tools\dotnet-affected.exe`, "dotnet-affected executable")
	fs.StringVar(&t.Root, "dotnet-root", `D:\bench\tools\dotnet`, "DOTNET_ROOT given to the tools")
	return t
}

// cmdDotnetAffected compares bgraph's selection with dotnet-affected (--from P, working tree at C) for every T1 task: bgraph must be a superset.
func cmdDotnetAffected(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dotnet-affected", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "task JSONL (tasks-dotnet.jsonl)")
	manPath := fs.String("manifest", "evalkit/manifest/dotnet.json", "manifest")
	work := fs.String("work", "", "work directory (clones in <work>/repos, worktrees in <work>/wt)")
	out := fs.String("out", "", "results directory")
	jobs := fs.Int("jobs", 3, "repos in parallel")
	maxPer := fs.Int("max-per-repo", 40, "")
	only := fs.String("repos", "", "only these repos")
	maxProj := fs.Int("max-projects", 800, "skip repos with more projects than this (MSBuild evaluates every project)")
	timeout := fs.Duration("timeout", 15*time.Minute, "per dotnet-affected run")
	tools := dotnetToolsFlags(fs)
	fs.Parse(args)
	if *tasksPath == "" || *work == "" || *out == "" {
		return fmt.Errorf("--tasks, --work, --out are required")
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return err
	}
	man, err := evalkit.LoadManifest(*manPath)
	if err != nil {
		return err
	}
	tasks, err := evalkit.LoadTasks(*tasksPath)
	if err != nil {
		return err
	}
	tasks = evalkit.SelectTasks(tasks, *maxPer)
	byRepo := map[string][]evalkit.Task{}
	for _, t := range tasks {
		if *only != "" && !contains(*only, t.Repo) {
			continue
		}
		byRepo[t.Repo] = append(byRepo[t.Repo], t)
	}
	lg := logger(*out)
	cfg := evalkit.DotnetAffectedConfig{Work: *work, Tools: *tools, MaxProjects: *maxProj, Timeout: *timeout, Log: lg}
	var mu sync.Mutex
	var rows []evalkit.DotnetAffectedRow
	sem := make(chan struct{}, *jobs)
	var wg sync.WaitGroup
	for repo, ts := range byRepo {
		wg.Add(1)
		sem <- struct{}{}
		go func(repo string, ts []evalkit.Task) {
			defer wg.Done()
			defer func() { <-sem }()
			mirror := evalkit.RustRepoDir(*work, repo)
			role, _ := man.Role(repo)
			for i, t := range ts {
				if ctx.Err() != nil {
					return
				}
				r := evalkit.DotnetAffectedTask(ctx, cfg, mirror, i, t, role)
				mu.Lock()
				rows = append(rows, r)
				lg("%-40s %-18s scope=%-6s sel=%d/%d affected=%d missing=%d extra=%d %dms %s", r.ID, r.Status, r.Scope, r.Selected, r.Units, r.Affected, len(r.Missing), r.Extra, r.ToolMs, firstLine(r.Detail))
				mu.Unlock()
			}
		}(repo, ts)
	}
	wg.Wait()
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	b, _ := json.MarshalIndent(rows, "", " ")
	if err := os.WriteFile(filepath.Join(*out, "dotnet-affected.json"), b, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*out, "dotnet-affected.md"), []byte(dotnetAffectedMD(rows)), 0o644)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

func dotnetAffectedMD(rows []evalkit.DotnetAffectedRow) string {
	var b strings.Builder
	status := func(r evalkit.DotnetAffectedRow) string {
		if strings.HasPrefix(r.Status, "skipped:") {
			return "skipped"
		}
		return r.Status
	}
	cnt := map[string]int{}
	by := map[string]map[string]int{}
	var repos []string
	for _, r := range rows {
		cnt[status(r)]++
		if by[r.Repo] == nil {
			by[r.Repo] = map[string]int{}
			repos = append(repos, r.Repo)
		}
		by[r.Repo][status(r)]++
		by[r.Repo]["n"]++
	}
	fmt.Fprintf(&b, "# dotnet-affected superset check\n\n%d tasks: %v\n\n", len(rows), cnt)
	fmt.Fprintf(&b, "| repo | tasks | ok | violation | full | tool_failed | skipped |\n|---|---|---|---|---|---|---|\n")
	sort.Strings(repos)
	for _, r := range repos {
		m := by[r]
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d |\n", r, m["n"], m["ok"], m["violation"], m["full"], m["tool_failed"], m["skipped"])
	}
	fmt.Fprintf(&b, "\n## Violations\n\n")
	for _, r := range rows {
		if r.Status == "violation" {
			fmt.Fprintf(&b, "- `%s` scope=%s selected %d/%d, tool affected %d, missing %v; changed %v\n", r.ID, r.Scope, r.Selected, r.Units, r.Affected, r.Missing, r.Changed)
		}
	}
	fmt.Fprintf(&b, "\n## Tool failures and skips\n\n")
	for _, r := range rows {
		if r.Status == "tool_failed" || strings.HasPrefix(r.Status, "skipped:") {
			fmt.Fprintf(&b, "- `%s` %s: %s\n", r.ID, r.Status, strings.ReplaceAll(firstLine(r.Detail), "\n", " "))
		}
	}
	return b.String()
}

// cmdDotnetProvider compares bgraph's ProjectReference edges with `dotnet msbuild -getItem:ProjectReference` at sampled base commits.
func cmdDotnetProvider(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dotnet-provider", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "task JSONL (tasks-dotnet.jsonl)")
	work := fs.String("work", "", "work directory")
	out := fs.String("out", "", "results directory")
	jobs := fs.Int("jobs", 6, "parallel msbuild evaluations")
	commits := fs.Int("commits", 5, "base commits per repo")
	bigCommits := fs.Int("big-commits", 2, "base commits for repos above --big-projects")
	maxProj := fs.Int("max-projects", 60, "projects per commit (deterministic sample)")
	bigProj := fs.Int("big-projects", 600, "a repo whose first sampled commit has more projects counts as big")
	bigMax := fs.Int("big-max-projects", 200, "projects per commit for big repos")
	only := fs.String("repos", "", "only these repos")
	timeout := fs.Duration("timeout", 3*time.Minute, "per msbuild evaluation")
	tools := dotnetToolsFlags(fs)
	fs.Parse(args)
	if *tasksPath == "" || *work == "" || *out == "" {
		return fmt.Errorf("--tasks, --work, --out are required")
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return err
	}
	tasks, err := evalkit.LoadTasks(*tasksPath)
	if err != nil {
		return err
	}
	bases := map[string][]string{}
	for _, t := range tasks {
		if *only != "" && !contains(*only, t.Repo) {
			continue
		}
		dup := false
		for _, b := range bases[t.Repo] {
			dup = dup || b == t.BaseCommit
		}
		if !dup {
			bases[t.Repo] = append(bases[t.Repo], t.BaseCommit)
		}
	}
	lg := logger(*out)
	var repos []string
	for r := range bases {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	var rows []evalkit.DotnetProviderRow
	for _, repo := range repos {
		if ctx.Err() != nil {
			break
		}
		bs := bases[repo]
		sort.Strings(bs)
		n, mp := *commits, *maxProj
		if nproj, err := evalkit.DotnetProjectCount(ctx, evalkit.RustRepoDir(*work, repo), bs[0]); err == nil && nproj > *bigProj {
			n, mp = *bigCommits, *bigMax
		}
		if len(bs) > n {
			step := float64(len(bs)) / float64(n)
			var s []string
			for i := 0; i < n; i++ {
				s = append(s, bs[int(float64(i)*step)])
			}
			bs = s
		}
		cfg := evalkit.DotnetProviderConfig{Work: *work, Tools: *tools, MaxCommits: n, MaxProjects: mp, Timeout: *timeout, Jobs: *jobs}
		r := evalkit.DotnetProviderRepo(ctx, cfg, evalkit.RustRepoDir(*work, repo), repo, bs)
		rows = append(rows, r)
		lg("%-40s commits=%d projects=%d failed=%d msbuild-edges=%d covered=%d missing=%d unresolved-covered=%d %dms", r.Repo, r.Commits, r.Projects, r.Failed, r.Edges, r.Covered, len(r.Missing), len(r.MissingUnresolved), r.ElapsedMs)
		b, _ := json.MarshalIndent(rows, "", " ")
		os.WriteFile(filepath.Join(*out, "dotnet-provider.json"), b, 0o644)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# .NET provider check (msbuild -getItem:ProjectReference subset-of bgraph edges)\n\n| repo | commits | projects | eval failed | msbuild edges | covered | missing |\n|---|---|---|---|---|---|---|\n")
	var tp, tf, te, tc int
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d |\n", r.Repo, r.Commits, r.Projects, r.Failed, r.Edges, r.Covered, len(r.Missing))
		tp, tf, te, tc = tp+r.Projects, tf+r.Failed, te+r.Edges, tc+r.Covered
	}
	fmt.Fprintf(&b, "| **total** | | %d | %d | %d | %d | |\n\n## Missing edges\n\n", tp, tf, te, tc)
	for _, r := range rows {
		for _, m := range r.Missing {
			fmt.Fprintf(&b, "- %s: %s\n", r.Repo, m)
		}
	}
	fmt.Fprintf(&b, "\n## Evaluation failures (first per repo)\n\n")
	for _, r := range rows {
		for _, e := range r.Errors {
			fmt.Fprintf(&b, "- %s: %s\n", r.Repo, strings.ReplaceAll(firstLine(e), "\n", " "))
		}
	}
	return os.WriteFile(filepath.Join(*out, "dotnet-provider.md"), []byte(b.String()), 0o644)
}

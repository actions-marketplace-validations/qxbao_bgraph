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

func jvmToolsFlags(fs *flag.FlagSet) *evalkit.JVMTools {
	d := evalkit.DefaultJVMTools()
	t := &evalkit.JVMTools{}
	fs.StringVar(&t.JDK17, "jdk17", d.JDK17, "JAVA_HOME of the default JDK (default $BGRAPH_EVAL_TOOLS/jdk17, else $JAVA_HOME)")
	fs.StringVar(&t.JDK21, "jdk21", d.JDK21, "JAVA_HOME of the fallback JDK, Gradle (default $BGRAPH_EVAL_TOOLS/jdk21, else $JAVA21_HOME, else $JAVA_HOME)")
	fs.StringVar(&t.Maven, "maven", d.Maven, "Maven home (default $BGRAPH_EVAL_TOOLS/maven, else $MAVEN_HOME; empty: mvn on PATH)")
	fs.StringVar(&t.M2, "m2", d.M2, "Maven local repository (default $BGRAPH_EVAL_TOOLS/m2, else ~/.m2/repository)")
	fs.StringVar(&t.GradleHome, "gradle-home", d.GradleHome, "GRADLE_USER_HOME (default $BGRAPH_EVAL_TOOLS/gradle-home, else $GRADLE_USER_HOME; empty: inherited)")
	fs.StringVar(&t.Tmp, "tmp", d.Tmp, "TEMP/TMP of child processes (default $BGRAPH_EVAL_TOOLS/tmp, else under the system temp directory)")
	return t
}

// cmdJVMGib compares GIB's impacted Maven modules with bgraph's selection (bgraph must be a superset) for every Maven T1 task.
func cmdJVMGib(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("jvm-gib", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "task JSONL (tasks-jvm.jsonl)")
	manPath := fs.String("manifest", "evalkit/manifest/jvm.json", "manifest")
	work := fs.String("work", "", "work directory")
	out := fs.String("out", "", "results directory")
	jobs := fs.Int("jobs", 2, "repos in parallel")
	maxPer := fs.Int("max-per-repo", 40, "")
	only := fs.String("repos", "", "only these repos")
	timeout := fs.Duration("timeout", 15*time.Minute, "per GIB run")
	tools := jvmToolsFlags(fs)
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
	os.MkdirAll(tools.Tmp, 0o755)
	lg := logger(*out)
	cfg := evalkit.JVMGibConfig{Work: *work, Tools: *tools, Timeout: *timeout}
	var mu sync.Mutex
	var rows []evalkit.JVMGibRow
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
				r := evalkit.JVMGibTask(ctx, cfg, mirror, i, t, role)
				mu.Lock()
				rows = append(rows, r)
				lg("%-40s %-18s scope=%-6s sel=%d/%d gib=%d missing=%d extra=%d %dms %s", r.ID, r.Status, r.Scope, r.Selected, r.Units, r.GIB, len(r.Missing), r.Extra, r.ToolMs, firstLine(r.Detail))
				mu.Unlock()
			}
		}(repo, ts)
	}
	wg.Wait()
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	b, _ := json.MarshalIndent(rows, "", " ")
	if err := os.WriteFile(filepath.Join(*out, "jvm-gib.json"), b, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*out, "jvm-gib.md"), []byte(jvmGibMD(rows)), 0o644)
}

func jvmGibMD(rows []evalkit.JVMGibRow) string {
	var b strings.Builder
	status := func(r evalkit.JVMGibRow) string {
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
	fmt.Fprintf(&b, "# GIB superset check (GIB impacted modules subset-of bgraph selection)\n\n%d tasks: %v\n\n", len(rows), cnt)
	fmt.Fprintf(&b, "| repo | tasks | ok | violation | full | tool_failed | skipped |\n|---|---|---|---|---|---|---|\n")
	sort.Strings(repos)
	for _, r := range repos {
		m := by[r]
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d |\n", r, m["n"], m["ok"], m["violation"], m["full"], m["tool_failed"], m["skipped"])
	}
	fmt.Fprintf(&b, "\n## Violations\n\n")
	for _, r := range rows {
		if r.Status == "violation" {
			fmt.Fprintf(&b, "- `%s` scope=%s selected %d/%d, GIB impacted %d, missing %v; changed %v\n", r.ID, r.Scope, r.Selected, r.Units, r.GIB, r.Missing, r.Changed)
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

// cmdJVMProvider compares the build tools' module lists (Maven effective-pom reactor, gradlew projects) and Maven dependency edges with
// bgraph's units and edges at sampled base commits.
func cmdJVMProvider(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("jvm-provider", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "task JSONL (tasks-jvm.jsonl)")
	work := fs.String("work", "", "work directory")
	out := fs.String("out", "", "results directory")
	commits := fs.Int("commits", 5, "base commits per repo (Maven)")
	gradleCommits := fs.Int("gradle-commits", 2, "base commits per repo (Gradle: slow)")
	only := fs.String("repos", "", "only these repos")
	timeout := fs.Duration("timeout", 15*time.Minute, "per build tool run")
	tools := jvmToolsFlags(fs)
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
	os.MkdirAll(tools.Tmp, 0o755)
	lg := logger(*out)
	var repos []string
	for r := range bases {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	pick := func(bs []string, n int) []string {
		sort.Strings(bs)
		if len(bs) <= n {
			return bs
		}
		step := float64(len(bs)) / float64(n)
		var s []string
		for i := 0; i < n; i++ {
			s = append(s, bs[int(float64(i)*step)])
		}
		return s
	}
	cfg := evalkit.JVMProviderConfig{Work: *work, Tools: *tools, Timeout: *timeout}
	var rows []evalkit.JVMProviderRow
	for _, repo := range repos {
		if ctx.Err() != nil {
			break
		}
		mirror := evalkit.RustRepoDir(*work, repo)
		bs := bases[repo]
		sort.Strings(bs)
		for _, tool := range evalkit.JVMRootTools(ctx, mirror, bs[0]) {
			n := *commits
			if tool == "gradle" {
				n = *gradleCommits
			}
			r := evalkit.JVMProviderRepo(ctx, cfg, mirror, repo, tool, pick(append([]string(nil), bs...), n))
			rows = append(rows, r)
			lg("%-36s %-6s commits=%d builds=%d failed=%d modules=%d in-units=%d edges=%d covered=%d read-ok=%d %dms", r.Repo, r.Tool, r.Commits, r.Builds, r.Failed, r.Modules, r.InUnits, r.Edges, r.Covered, r.ReadOK, r.Ms)
			b, _ := json.MarshalIndent(rows, "", " ")
			os.WriteFile(filepath.Join(*out, "jvm-provider.json"), b, 0o644)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# JVM provider check\n\n| repo | tool | commits | builds read | failed | modules | in units | build edges | edges covered | commits read OK |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d | %d | %d | %d | %d |\n", r.Repo, r.Tool, r.Commits, r.Builds, r.Failed, r.Modules, r.InUnits, r.Edges, r.Covered, r.ReadOK)
	}
	fmt.Fprintf(&b, "\n## Missing modules and edges\n\n")
	for _, r := range rows {
		for _, m := range r.Missing {
			fmt.Fprintf(&b, "- %s (%s) module %s\n", r.Repo, r.Tool, m)
		}
		for _, m := range r.MissEdg {
			fmt.Fprintf(&b, "- %s (%s) edge %s\n", r.Repo, r.Tool, m)
		}
	}
	fmt.Fprintf(&b, "\n## Build tool failures\n\n")
	for _, r := range rows {
		for _, e := range r.Errors {
			fmt.Fprintf(&b, "- %s (%s): %s\n", r.Repo, r.Tool, strings.ReplaceAll(firstLine(e), "\n", " "))
		}
	}
	return os.WriteFile(filepath.Join(*out, "jvm-provider.md"), []byte(b.String()), 0o644)
}

// Command bgraph-eval is the external evaluation suite (docs/evaluation.md).
//
//	bgraph-eval manifest --tasks tasks.jsonl [--meta meta.json] --replay zod,hono --out evalkit/manifest/node.json
//	bgraph-eval t1       --tasks tasks.jsonl --manifest evalkit/manifest/node.json --work DIR --out DIR [--jobs 4] [--max-per-repo 40]
//	bgraph-eval quick    --tasks tasks.jsonl --work DIR --out DIR [--t2-dir DIR (check T2 against the cached oracle if present)] [--manifest ...] [--regressions evalkit/manifest/node-regressions.txt] [--per-repo 4] [--commits 20] [--checkout] [--serial]
//	bgraph-eval baseline prep|score --tasks tasks.jsonl --work DIR --out DIR [--tools codegraph,graphify,gitnexus]   (0b control)
//	bgraph-eval t2       [--run-tests] [--ecosystem node|rust|python|ruby|swift|dart] [--budget 90m] [--dir DIR (default $BGRAPH_EVAL_DIR/t2)] [--manifest evalkit/manifest/t2.json] [--repos zod,hono,dayjs] [--out DIR]
//	bgraph-eval rust-locate --tasks tasks-rust.jsonl --work DIR [--out locate.json] [--examples 3]   (locate Rust FAIL_TO_PASS tests in the P trees)
//	bgraph-eval jvm-locate|dotnet-locate|go-locate|php-locate|swift-locate --tasks tasks-<eco>.jsonl --work DIR [--out locate.json] [--examples 3]   (locate FAIL_TO_PASS tests in the P trees)
//	bgraph-eval jvm-gib|jvm-provider --tasks tasks-jvm.jsonl --work DIR --out DIR   (JVM: superset check against gitflow-incremental-builder; Maven reactor and Gradle projects against bgraph units)
//	bgraph-eval dotnet-affected|dotnet-provider --tasks tasks-dotnet.jsonl --work DIR --out DIR   (.NET: superset check against dotnet-affected; ProjectReference edges against msbuild)
//	bgraph-eval dart-locate --tasks tasks-dart.jsonl --work DIR [--out dart-locate.json] [--examples 3]   (locate Dart FAIL_TO_PASS tests in the P trees)
//	bgraph-eval rb-locate --tasks tasks-ruby.jsonl --work DIR [--out rb-locate.json] [--examples 3]   (locate Ruby FAIL_TO_PASS tests in the P trees)
//	bgraph-eval py-locate --tasks tasks-python.jsonl --work DIR [--out locate.json] [--examples 3]   (locate Python FAIL_TO_PASS tests in the P trees)
//	bgraph-eval cargometa --tasks tasks-rust.jsonl --work DIR [--out DIR] [--per-repo 5] [--repos a/b,c/d]   (bgraph Rust analysis vs cargo metadata)
//	bgraph-eval lockdiff --format pnpm|npm|cargo|uv|poetry|yarn|bun --repos a__b[:lockfile],... [--commits 25] [--out DIR] [--jobs 1] [--pnpm CMD]   (lockfile closures vs the package manager; gate L1)
//	bgraph-eval replay   --repo URL [--repo URL...] --commits 100 --manifest ... --work DIR --out DIR
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qxbao/bgraph/internal/evalkit"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, strings.Split(s, ",")...); return nil }

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "t1":
		err = cmdT1(ctx, os.Args[2:])
	case "quick":
		var code int
		code, err = cmdQuick(ctx, os.Args[2:])
		if err == nil && code != 0 {
			stop()
			os.Exit(code)
		}
	case "baseline":
		err = cmdBaseline(ctx, os.Args[2:])
	case "t2":
		var code int
		code, err = cmdT2(ctx, os.Args[2:])
		if err == nil && code != 0 {
			stop()
			os.Exit(code)
		}
	case "replay":
		err = cmdReplay(ctx, os.Args[2:])
	case "t2dyn", "replaydyn", "jvmdyn", "dotnetdyn":
		cmd := cmdT2Dyn
		if os.Args[1] == "replaydyn" {
			cmd = cmdReplayDyn
		}
		if os.Args[1] == "jvmdyn" {
			cmd = cmdJVMDyn
		}
		if os.Args[1] == "dotnetdyn" {
			cmd = cmdDotnetDyn
		}
		var code int
		code, err = cmd(ctx, os.Args[2:])
		if err == nil && code != 0 {
			stop()
			os.Exit(code)
		}
	case "rbdyn":
		var code int
		code, err = cmdRubyDyn(ctx, os.Args[2:])
		if err == nil && code != 0 {
			stop()
			os.Exit(code)
		}
	case "cargometa":
		err = cmdCargoMeta(ctx, os.Args[2:])
	case "rust-locate":
		err = cmdRustLocate(ctx, os.Args[2:])
	case "py-locate":
		err = cmdPyLocate(ctx, os.Args[2:])
	case "jvm-gib":
		err = cmdJVMGib(ctx, os.Args[2:])
	case "jvm-provider":
		err = cmdJVMProvider(ctx, os.Args[2:])
	case "dotnet-affected":
		err = cmdDotnetAffected(ctx, os.Args[2:])
	case "dotnet-provider":
		err = cmdDotnetProvider(ctx, os.Args[2:])
	case "jvm-locate":
		err = cmdJVMLocate(ctx, os.Args[2:], "jvm")
	case "dotnet-locate":
		err = cmdJVMLocate(ctx, os.Args[2:], "dotnet")
	case "swift-provider":
		err = cmdSwiftProvider(ctx, os.Args[2:])
	case "cpp-provider":
		err = cmdCPPProvider(ctx, os.Args[2:])
	case "cpp-locate":
		err = cmdJVMLocate(ctx, os.Args[2:], "cpp")
	case "swift-locate":
		err = cmdJVMLocate(ctx, os.Args[2:], "swift")
	case "go-provider":
		err = cmdGoProvider(ctx, os.Args[2:])
	case "go-locate":
		err = cmdJVMLocate(ctx, os.Args[2:], "go")
	case "rb-locate":
		err = cmdJVMLocate(ctx, os.Args[2:], "ruby")
	case "php-locate":
		err = cmdJVMLocate(ctx, os.Args[2:], "php")
	case "dart-locate":
		err = cmdJVMLocate(ctx, os.Args[2:], "dart")
	case "lockdiff":
		err = cmdLockdiff(ctx, os.Args[2:])
	case "manifest":
		err = cmdManifest(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bgraph-eval:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bgraph-eval t1|t2|quick|replay|manifest [flags]  (see -h for each command)")
	fmt.Fprint(os.Stderr, `
environment:
  BGRAPH_EVAL_DIR    root of the evaluation data: T2 sets, reports, scratch (default <user cache dir>/bgraph-eval)
  BGRAPH_EVAL_TOOLS  optional directory of portable tools: jdk17, jdk21, maven, m2, gradle-home, tmp, nuget, dotnet,
                     dotnet-tools, cargo-home, pnpm-store, pnpm-cache, pnpm-home, npm-cache. When unset, the tools come
                     from JAVA_HOME, JAVA21_HOME, MAVEN_HOME, GRADLE_USER_HOME, DOTNET_ROOT, CARGO_HOME, NUGET_PACKAGES
                     and PATH. Flags such as --jdk17, --maven, --dotnet, --cargo-home override both.
`)
	os.Exit(2)
}

func logger(out string) func(string, ...any) {
	os.MkdirAll(out, 0o755)
	f, _ := os.OpenFile(filepath.Join(out, "run.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	var mu sync.Mutex
	return func(format string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		line := fmt.Sprintf(format, a...)
		fmt.Fprintln(os.Stderr, line)
		if f != nil {
			fmt.Fprintln(f, line)
		}
	}
}

func cmdT1(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("t1", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "normalized task JSONL")
	manPath := fs.String("manifest", "evalkit/manifest/node.json", "manifest repo dev/holdout")
	work := fs.String("work", "", "working directory (clones, worktrees, state)")
	out := fs.String("out", "", "results directory")
	jobs := fs.Int("jobs", 4, "number of repos to run in parallel")
	maxPer := fs.Int("max-per-repo", 40, "max tasks per repo (0 = unlimited)")
	only := fs.String("repos", "", "only these repos (comma-separated)")
	ids := fs.String("ids", "", "only these instance_ids")
	limit := fs.Int("limit", 0, "max total tasks (0 = no limit)")
	resume := fs.Bool("resume", false, "skip tasks already in t1.jsonl")
	checkout := fs.Bool("checkout", false, "build P/C via git worktree (legacy path) instead of a temporary index; a git apply --cached failure also falls back to this path")
	eco := fs.String("ecosystem", "", "node (default) | rust | python | dotnet | jvm | go | php | ruby | swift | cpp | dart; rust uses the Cargo-unit oracle, python the test-file oracle; each defaults --manifest to evalkit/manifest/<eco>.json")
	rulesDir := fs.String("rules-dir", "", "inject DIR/<owner>__<name>.toml as bgraph.toml into P and C of that repo's tasks (python [[dynamic]] declarations)")
	fs.Parse(args)
	if *eco == "rust" && *manPath == "evalkit/manifest/node.json" {
		*manPath = "evalkit/manifest/rust.json"
	}
	if *eco == "python" && *manPath == "evalkit/manifest/node.json" {
		*manPath = "evalkit/manifest/python.json"
	}
	if *eco == "dotnet" && *manPath == "evalkit/manifest/node.json" {
		*manPath = "evalkit/manifest/dotnet.json"
	}
	if *eco == "jvm" && *manPath == "evalkit/manifest/node.json" {
		*manPath = "evalkit/manifest/jvm.json"
	}
	if *eco == "go" && *manPath == "evalkit/manifest/node.json" {
		*manPath = "evalkit/manifest/go.json"
	}
	if *eco == "ruby" && *manPath == "evalkit/manifest/node.json" {
		*manPath = "evalkit/manifest/ruby.json"
	}
	if *eco == "cpp" && *manPath == "evalkit/manifest/node.json" {
		*manPath = "evalkit/manifest/cpp.json"
	}
	if *eco == "swift" && *manPath == "evalkit/manifest/node.json" {
		*manPath = "evalkit/manifest/swift.json"
	}
	if *eco == "php" && *manPath == "evalkit/manifest/node.json" {
		*manPath = "evalkit/manifest/php.json"
	}
	if *eco == "dart" && *manPath == "evalkit/manifest/node.json" {
		*manPath = "evalkit/manifest/dart.json"
	}
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
	var keep []evalkit.Task
	for _, t := range tasks {
		if _, ok := man.Role(t.Repo); !ok {
			return fmt.Errorf("repo %s is not in the manifest", t.Repo)
		}
		if *only != "" && !contains(*only, t.Repo) {
			continue
		}
		if *ids != "" && !contains(*ids, t.InstanceID) {
			continue
		}
		keep = append(keep, t)
	}
	if *limit > 0 && len(keep) > *limit {
		keep = keep[:*limit]
	}
	lg := logger(*out)
	lg("T1: %d task", len(keep))
	rs, err := evalkit.RunT1(ctx, evalkit.T1Config{Work: *work, Out: *out, Tasks: keep, Manifest: man, Jobs: *jobs, MaxPerRepo: *maxPer, Resume: *resume, Checkout: *checkout, Ecosystem: *eco, RulesDir: *rulesDir, Log: lg})
	if werr := evalkit.WriteT1(*out, rs); werr != nil {
		return werr
	}
	if *eco == "python" {
		if werr := evalkit.WritePyReport(*out, rs); werr != nil {
			return werr
		}
	}
	if *eco == "dotnet" {
		if werr := evalkit.WriteDotnetReport(*out, rs); werr != nil {
			return werr
		}
	}
	if *eco == "jvm" {
		if werr := evalkit.WriteJVMReport(*out, rs); werr != nil {
			return werr
		}
	}
	if *eco == "go" {
		if werr := evalkit.WriteGoReport(*out, rs); werr != nil {
			return werr
		}
	}
	if *eco == "ruby" {
		if werr := evalkit.WriteRubyReport(*out, rs); werr != nil {
			return werr
		}
	}
	if *eco == "php" {
		if werr := evalkit.WritePHPReport(*out, rs); werr != nil {
			return werr
		}
	}
	if *eco == "dart" {
		if werr := evalkit.WriteDartReport(*out, rs); werr != nil {
			return werr
		}
	}
	return err
}

// cmdQuick runs the quick evaluation: T1 on a stratified sample + replay; returns exit code 1 on any T1 violation or invariant violation.
func cmdQuick(ctx context.Context, args []string) (int, error) {
	start := time.Now()
	fs := flag.NewFlagSet("quick", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "normalized task JSONL")
	manPath := fs.String("manifest", "evalkit/manifest/node.json", "manifest repo dev/holdout")
	regPath := fs.String("regressions", "evalkit/manifest/node-regressions.txt", "instance_ids that once violated (always run)")
	work := fs.String("work", "", "working directory (clones, state)")
	out := fs.String("out", "", "results directory")
	jobs := fs.Int("jobs", 4, "number of repos to run in parallel")
	perRepo := fs.Int("per-repo", 4, "additional tasks per repo, chosen by sha256(instance_id)")
	maxPer := fs.Int("max-per-repo", 40, "per-repo cap of the full T1 set (the set the sample is drawn from)")
	commits := fs.Int("commits", 20, "commits to replay per repo")
	checkout := fs.Bool("checkout", false, "build P/C via git worktree (legacy path)")
	serial := fs.Bool("serial", false, "run T1 then replay sequentially (default: parallel)")
	t2Dir := fs.String("t2-dir", "", "T2 root directory (default: <work>/../t2); T2 is re-checked only if oracle.jsonl exists there")
	t2Man := fs.String("t2-manifest", "evalkit/manifest/t2.json", "manifest repo T2")
	fs.Parse(args)
	if *tasksPath == "" || *work == "" || *out == "" {
		return 2, fmt.Errorf("--tasks, --work, --out are required")
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return 2, err
	}
	man, err := evalkit.LoadManifest(*manPath)
	if err != nil {
		return 2, err
	}
	all, err := evalkit.LoadTasks(*tasksPath)
	if err != nil {
		return 2, err
	}
	reg, err := evalkit.LoadIDList(*regPath)
	if err != nil {
		return 2, err
	}
	sample, missing := evalkit.SampleQuick(all, evalkit.SelectTasks(all, *maxPer), reg, *perRepo)
	var keep []evalkit.Task
	for _, t := range sample {
		if _, ok := man.Role(t.Repo); !ok {
			return 2, fmt.Errorf("repo %s is not in the manifest", t.Repo)
		}
		keep = append(keep, t)
	}
	var rep []string
	for _, r := range man.Repos {
		if r.Replay {
			rep = append(rep, r.Name)
		}
	}
	if *t2Dir == "" {
		*t2Dir = filepath.Join(filepath.Dir(filepath.Clean(*work)), "t2")
	}
	t2m, _ := evalkit.LoadT2Manifest(*t2Man) // missing manifest → skip T2
	lg := logger(*out)
	lg("quick: %d T1 tasks (%d regressions, %d regression ids missing from the data: %v), replay %d repos x %d commits", len(keep), len(reg)-len(missing), len(missing), missing, len(rep), *commits)
	qr, err := evalkit.RunQuick(ctx, evalkit.QuickConfig{Work: *work, Out: *out, Tasks: keep, Manifest: man, Replay: rep, Commits: *commits, Jobs: *jobs, Checkout: *checkout, Serial: *serial, T2Dir: *t2Dir, T2: t2m, Log: lg})
	if err != nil {
		return 2, err
	}
	b, _ := os.ReadFile(filepath.Join(*out, "quick.md"))
	fmt.Println(string(b))
	fmt.Printf("Total time: %s\n", time.Since(start).Round(time.Second))
	if qr.Violations > 0 {
		return 1, nil
	}
	return 0, nil
}

func contains(list, v string) bool {
	for _, x := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return true
		}
	}
	return false
}

func cmdReplay(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	var repos multi
	fs.Var(&repos, "repo", "URL or owner/name (repeatable)")
	n := fs.Int("commits", 100, "most recent commits (first-parent, merges skipped)")
	manPath := fs.String("manifest", "evalkit/manifest/node.json", "manifest repo dev/holdout")
	work := fs.String("work", "", "working directory")
	out := fs.String("out", "", "results directory")
	jobs := fs.Int("jobs", 4, "number of repos in parallel")
	depth := fs.Int("depth", 0, "shallow clone depth (0 = 3*commits+20)")
	resum := fs.Bool("resummarize", false, "only re-read replay.json in --out, assign roles from the manifest and rewrite the report")
	fs.Parse(args)
	if *resum {
		man, err := evalkit.LoadManifest(*manPath)
		if err != nil {
			return err
		}
		return evalkit.Resummarize(*out, man)
	}
	if len(repos) == 0 || *work == "" || *out == "" {
		return fmt.Errorf("--repo, --work, --out are required")
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return err
	}
	man, _ := evalkit.LoadManifest(*manPath)
	lg := logger(*out)
	rs, err := evalkit.RunReplay(ctx, evalkit.ReplayConfig{Work: *work, Out: *out, Repos: repos, Commits: *n, Depth: *depth, Jobs: *jobs, Manifest: man, Log: lg})
	if werr := evalkit.WriteReplay(*out, rs); werr != nil {
		return werr
	}
	return err
}

func cmdManifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "task JSONL")
	metaPath := fs.String("meta", "", "meta.json written by fetch_datasets.py (dataset revisions)")
	var replay multi
	fs.Var(&replay, "replay", "replay repos (comma-separated)")
	maxPer := fs.Int("max-per-repo", 40, "")
	out := fs.String("out", "evalkit/manifest/node.json", "")
	eco := fs.String("ecosystem", "node", "node | rust | python | jvm | dotnet | go | php | ruby | swift | cpp | dart")
	fs.Parse(args)
	if *eco == "php" || *eco == "swift" || *eco == "cpp" || *eco == "dart" {
		outSet := false
		fs.Visit(func(f *flag.Flag) { outSet = outSet || f.Name == "out" })
		if !outSet {
			*out = "evalkit/manifest/" + *eco + ".json"
		}
	}
	tasks, err := evalkit.LoadTasks(*tasksPath)
	if err != nil {
		return err
	}
	tasks = evalkit.SelectTasks(tasks, *maxPer)
	t1 := map[string]*evalkit.ManifestRepo{}
	for _, t := range tasks {
		r := t1[strings.ToLower(t.Repo)]
		if r == nil {
			r = &evalkit.ManifestRepo{Name: t.Repo}
			t1[strings.ToLower(t.Repo)] = r
		}
		r.T1++
		if !contains(strings.Join(r.Sources, ","), t.Source) {
			r.Sources = append(r.Sources, t.Source)
		}
	}
	// Repos with the same name but different case between replay and T1: keep T1's name.
	var rep []string
	for _, r := range replay {
		if x, ok := t1[strings.ToLower(r)]; ok {
			r = x.Name
		}
		rep = append(rep, r)
	}
	sort.Strings(rep)
	ds := []evalkit.DatasetInfo{
		{Name: "SWE-bench/SWE-bench_Multilingual", URL: "https://huggingface.co/datasets/SWE-bench/SWE-bench_Multilingual", Notes: "test split; keeps JS/TS repos"},
		{Name: "ByteDance-Seed/Multi-SWE-bench", URL: "https://huggingface.co/datasets/ByteDance-Seed/Multi-SWE-bench", Notes: "js/ and ts/ directories (JSONL); FAIL_TO_PASS = f2p_tests key"},
	}
	if *metaPath != "" {
		if b, err := os.ReadFile(*metaPath); err == nil {
			// meta.json also carries non-string keys (rust_*), so read values leniently.
			var m map[string]json.RawMessage
			if json.Unmarshal(b, &m) == nil {
				for i := range ds {
					var rev string
					if json.Unmarshal(m[ds[i].Name], &rev) == nil {
						ds[i].Revision = rev
					}
				}
			}
		}
	}
	var man evalkit.Manifest
	if *eco == "jvm" || *eco == "dotnet" || *eco == "go" {
		man = evalkit.BuildJVMDotnetManifest(*eco, t1, *metaPath, *maxPer)
	} else if *eco == "ruby" {
		man = evalkit.BuildRubyManifest(t1, *metaPath, *maxPer)
	} else if *eco == "cpp" {
		ds = []evalkit.DatasetInfo{
			{Name: "SWE-bench/SWE-bench_Multilingual", Notes: "test split; CMake/CTest repositories"},
			{Name: "ByteDance-Seed/Multi-SWE-bench", Notes: "selected CMake/CTest repositories and zstd; recorded f2p or patch-linked n2p"},
			{Name: "SWE-bench-c/SWE-bench-c", Notes: "test split; zstd has both CMake/CTest and Meson projects"},
		}
		for i := range ds {
			ds[i].URL = "https://huggingface.co/datasets/" + ds[i].Name
			ds[i].Revision = metaString(*metaPath, "cpp_datasets", ds[i].Name)
		}
		for _, r := range t1 {
			r.Kind = "library"
			if strings.EqualFold(r.Name, "catchorg/Catch2") {
				r.Kind = "framework"
			}
		}
		man = evalkit.BuildCountManifest("cpp", t1, ds, *maxPer)
	} else if *eco == "dart" {
		man = evalkit.BuildDartManifest(t1, *metaPath, *maxPer)
	} else if *eco == "swift" {
		ds = []evalkit.DatasetInfo{{Name: "nebius/Longtail-SWE", URL: "https://huggingface.co/datasets/nebius/Longtail-SWE", Notes: "hard and easy splits, Swift rows with recorded FAIL_TO_PASS results"}}
		ds[0].Revision = metaString(*metaPath, "swift_datasets", ds[0].Name)
		for _, r := range t1 {
			r.Kind = evalkit.SwiftRepoKind(r.Name)
		}
		man = evalkit.BuildCountManifest("swift", t1, ds, *maxPer)
	} else if *eco == "php" {
		ds = []evalkit.DatasetInfo{{Name: "EmInReLab/bugsPHP", URL: "https://github.com/EmInReLab/bugsPHP", Notes: "BugsPHP metadata and recorded PHPUnit outcomes, with gold and test overlays reconstructed from upstream Git objects"}}
		ds[0].Revision = metaString(*metaPath, "php_datasets", ds[0].Name)
		for _, r := range t1 {
			r.Kind = evalkit.PHPRepoKind(r.Name)
		}
		man = evalkit.BuildCountManifest("php", t1, ds, *maxPer)
	} else if *eco == "python" {
		ds = []evalkit.DatasetInfo{{Name: "princeton-nlp/SWE-bench_Verified", URL: "https://huggingface.co/datasets/princeton-nlp/SWE-bench_Verified", Notes: "test split (500 tasks); all repos are libraries/frameworks"}}
		if rev := metaString(*metaPath, "python_datasets", ds[0].Name); rev != "" {
			ds[0].Revision = rev
		}
		man = evalkit.BuildCountManifest("python", t1, ds, *maxPer)
	} else if *eco == "rust" {
		ds[0].Notes = "test split; keeps Rust repos (burntsushi/ripgrep respelled BurntSushi/ripgrep)"
		ds[1].Notes = "rust/ directory (JSONL); FAIL_TO_PASS = f2p_tests keys, else n2p_tests keys (task field f2p_kind)"
		man = evalkit.BuildRustManifest(t1, ds, *maxPer)
	} else {
		man = evalkit.BuildManifest(t1, rep, ds, *maxPer)
	}
	b, _ := json.MarshalIndent(man, "", " ")
	os.MkdirAll(filepath.Dir(*out), 0o755)
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	for _, r := range man.Repos {
		fmt.Printf("%-8s t1=%-3d replay=%-5v %s\n", r.Role, r.T1, r.Replay, r.Name)
	}
	return nil
}

// cmdBaseline: control comparison of external graph tools on the T1 sample of `quick`.
//
//	prep:  build P/C, oracle, test units and bgraph's selection for each task → <out>/tasks.json
//	score: read <out>/runs/<tool>/<id>.json (written by the baseline tool-runner script), score with CheckRequired → scores.json + baseline.md
func cmdBaseline(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: baseline prep|score")
	}
	sub := args[0]
	fs := flag.NewFlagSet("baseline", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "normalized task JSONL")
	manPath := fs.String("manifest", "evalkit/manifest/node.json", "manifest")
	regPath := fs.String("regressions", "evalkit/manifest/node-regressions.txt", "regression id list")
	work := fs.String("work", "", "working directory (repo mirrors)")
	out := fs.String("out", "", "results directory")
	jobs := fs.Int("jobs", 3, "repo song song")
	perRepo := fs.Int("per-repo", 4, "")
	maxPer := fs.Int("max-per-repo", 40, "")
	tools := fs.String("tools", "codegraph,graphify,gitnexus", "")
	fs.Parse(args[1:])
	if *out == "" {
		return fmt.Errorf("--out is required")
	}
	switch sub {
	case "prep":
		if *tasksPath == "" || *work == "" {
			return fmt.Errorf("--tasks, --work are required")
		}
		if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
			return err
		}
		man, err := evalkit.LoadManifest(*manPath)
		if err != nil {
			return err
		}
		all, err := evalkit.LoadTasks(*tasksPath)
		if err != nil {
			return err
		}
		reg, err := evalkit.LoadIDList(*regPath)
		if err != nil {
			return err
		}
		sample, missing := evalkit.SampleQuick(all, evalkit.SelectTasks(all, *maxPer), reg, *perRepo)
		lg := logger(*out)
		lg("baseline prep: %d tasks (missing regression ids: %v)", len(sample), missing)
		_, err = evalkit.BaselinePrep(ctx, evalkit.T1Config{Work: *work, Out: *out, Tasks: sample, Manifest: man, Jobs: *jobs, Log: lg})
		return err
	case "score":
		tasks, err := evalkit.LoadBTasks(filepath.Join(*out, "tasks.json"))
		if err != nil {
			return err
		}
		tl := strings.Split(*tools, ",")
		ss, err := evalkit.BaselineScoreAll(*out, tasks, tl)
		if err != nil {
			return err
		}
		for _, t := range tl {
			s := evalkit.SummarizeScores(ss, t)
			fmt.Printf("%-10s tasks=%d ok=%d violation=%d timeout=%d error=%d missing=%d median=%.3f p90=%.3f idx_med=%.1fs\n", t, s.Tasks, s.OK, s.Violations, s.Timeouts, s.Errors, s.Missing, s.MedRatio, s.P90Ratio, s.MedIndexS)
		}
		return nil
	}
	return fmt.Errorf("unknown subcommand: %s", sub)
}

// cmdT2 runs the mutation oracle (docs/evaluation.md): --run-tests builds/refreshes oracle.jsonl; by default it only re-checks bgraph on the cached oracle.
func cmdT2(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("t2", flag.ExitOnError)
	dir := fs.String("dir", filepath.Join(evalkit.EvalDir(), "t2"), "T2 root directory (checkout, tools, state, oracle.jsonl; default <$BGRAPH_EVAL_DIR>/t2)")
	manPath := fs.String("manifest", "evalkit/manifest/t2.json", "manifest repo T2")
	out := fs.String("out", "", "report directory (default <dir>/out)")
	repos := fs.String("repos", "", "only these repos (name or directory, comma-separated)")
	run := fs.Bool("run-tests", false, "run tests to build/refresh the oracle (slow); default re-checks bgraph on the cached oracle")
	rebase := fs.Bool("rebaseline", false, "re-run the 3-run baseline")
	rebuild := fs.Bool("rebuild", false, "re-run every mutant, even those already in the oracle")
	reconf := fs.Bool("reconfirm", false, "re-confirm unconfirmed killed mutants by re-running only the red files (2 runs)")
	eco := fs.String("ecosystem", "node", "node | rust | python | jvm | dotnet | go | php | ruby | swift | cpp | dart (non-Node default --dir <$BGRAPH_EVAL_DIR>/t2/<eco> and --manifest evalkit/manifest/t2-<eco>.json)")
	budget := fs.Duration("budget", 0, "rust/python/swift/cpp/dart: stop starting new mutants after this long (0 = no limit)")
	fs.Parse(args)
	if *eco == "rust" || *eco == "python" || *eco == "jvm" || *eco == "dotnet" || *eco == "go" || *eco == "php" || *eco == "ruby" || *eco == "swift" || *eco == "cpp" || *eco == "dart" {
		dirSet, manSet := false, false
		fs.Visit(func(f *flag.Flag) {
			dirSet = dirSet || f.Name == "dir"
			manSet = manSet || f.Name == "manifest"
		})
		if !dirSet {
			*dir = filepath.Join(evalkit.EvalDir(), "t2", *eco)
		}
		if !manSet {
			*manPath = "evalkit/manifest/t2-" + *eco + ".json"
		}
	} else if *eco != "node" {
		return 2, fmt.Errorf("unknown --ecosystem %q", *eco)
	}
	if *reconf && (*eco == "php" || *eco == "swift" || *eco == "cpp" || *eco == "dart") {
		return 2, fmt.Errorf("%s mutants are confirmed during --run-tests", *eco)
	}
	if *out == "" {
		*out = filepath.Join(*dir, "out")
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*dir, "gitcfg")); err != nil {
		return 2, err
	}
	man, err := evalkit.LoadT2Manifest(*manPath)
	if err != nil {
		return 2, err
	}
	var sel []string
	if *repos != "" {
		sel = strings.Split(*repos, ",")
	}
	lg := logger(*out)
	var dl time.Time
	if *budget > 0 {
		dl = time.Now().Add(*budget)
	}
	qr, err := evalkit.RunT2(ctx, evalkit.T2Config{Dir: *dir, Out: *out, Manifest: man, Repos: sel, RunTests: *run, Rebaseline: *rebase, Rebuild: *rebuild, Reconfirm: *reconf, Log: lg, Deadline: dl})
	if err != nil {
		return 2, err
	}
	if err := evalkit.WriteT2(*out, qr); err != nil {
		return 2, err
	}
	b, _ := os.ReadFile(filepath.Join(*out, "t2.md"))
	fmt.Println(string(b))
	if *eco == "ruby" {
		// provider cross-check: the runner's test files (recorded by the baseline) against bgraph's units at R
		md, err := evalkit.WriteRubyUnits(*out, evalkit.RubyUnitsCheck(ctx, evalkit.T2Config{Dir: *dir, Manifest: man, Repos: sel}))
		if err != nil {
			return 2, err
		}
		fmt.Println(md)
	}
	if *eco == "swift" {
		if len(qr.Repos) == 0 {
			return 2, fmt.Errorf("no Swift repos selected")
		}
		for _, repo := range qr.Repos {
			if repo.Error != "" || repo.Errors > 0 || repo.Killed+repo.Survived == 0 {
				return 2, fmt.Errorf("incomplete Swift T2 evaluation for %s (details in %s)", repo.Repo.Name, *out)
			}
		}
	}
	if *eco == "cpp" {
		if len(qr.Repos) == 0 {
			return 2, fmt.Errorf("no C/C++ repos selected")
		}
		for _, repo := range qr.Repos {
			if repo.Error != "" || repo.Errors > 0 || repo.Killed+repo.Survived == 0 {
				return 2, fmt.Errorf("incomplete C/C++ T2 evaluation for %s (details in %s)", repo.Repo.Name, *out)
			}
		}
	}
	if *eco == "dart" {
		// provider cross-check: the suites `dart test` loaded in the baseline against bgraph's units at R
		units := evalkit.DartUnitsCheck(ctx, evalkit.T2Config{Dir: *dir, Manifest: man, Repos: sel})
		md, err := evalkit.WriteDartUnits(*out, units)
		if err != nil {
			return 2, err
		}
		fmt.Println(md)
		for _, repo := range qr.Repos {
			if repo.Error != "" || repo.Killed+repo.Survived == 0 {
				return 2, fmt.Errorf("incomplete Dart T2 evaluation for %s (details in %s)", repo.Repo.Name, *out)
			}
		}
		if !evalkit.DartUnitsOK(units) {
			return 1, fmt.Errorf("dart provider cross-check found missing or unexplained units (%s)", filepath.Join(*out, "dart-units.md"))
		}
	}
	if qr.Violations > 0 {
		return 1, nil
	}
	return 0, nil
}

// cmdRustLocate runs the Rust test-name locator over every task's P tree and prints unique / ambiguous / not-found statistics.
func cmdRustLocate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rust-locate", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "Rust task JSONL (tasks-rust.jsonl)")
	work := fs.String("work", "", "work directory (clones in <work>/repos)")
	out := fs.String("out", "", "write per-name locations as JSON here")
	examples := fs.Int("examples", 3, "examples printed per category")
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
	byRepo := map[string][]evalkit.Task{}
	for _, t := range tasks {
		k := strings.ToLower(t.Repo)
		byRepo[k] = append(byRepo[k], t)
	}
	var mu sync.Mutex
	var rows []evalkit.RustLocateRow
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, ts := range byRepo {
		wg.Add(1)
		go func(ts []evalkit.Task) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			for i, t := range ts {
				if ctx.Err() != nil {
					return
				}
				idx := filepath.Join(*work, "idx", fmt.Sprintf("rustloc_%s_%d.index", strings.ReplaceAll(t.Repo, "/", "__"), i))
				os.MkdirAll(filepath.Dir(idx), 0o755)
				r := evalkit.RustLocateTask(ctx, evalkit.RustRepoDir(*work, t.Repo), idx, t)
				r.Kind = t.F2PKind
				mu.Lock()
				rows = append(rows, r)
				fmt.Fprintf(os.Stderr, "%-44s f2p=%-4d unique=%-4d ambiguous=%-3d notfound=%-4d doc=%-3d %s\n", r.ID, r.F2P, r.Unique, r.Ambig, r.NotFnd, r.Doc, r.Err)
				mu.Unlock()
			}
		}(ts)
	}
	wg.Wait()
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	type agg struct{ tasks, names, uniq, amb, nf, doc, errs, macro int }
	by := map[string]*agg{}
	add := func(k string, r evalkit.RustLocateRow) {
		a := by[k]
		if a == nil {
			a = &agg{}
			by[k] = a
		}
		a.tasks++
		a.names += r.F2P
		a.uniq += r.Unique
		a.amb += r.Ambig
		a.nf += r.NotFnd
		a.doc += r.Doc
		a.macro += r.MacroHit
		if r.Err != "" {
			a.errs++
		}
	}
	for _, r := range rows {
		add("ALL", r)
		add("kind="+r.Kind, r)
		add("repo="+strings.ToLower(r.Repo), r)
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("%-28s %6s %7s %7s %7s %9s %5s %5s %6s\n", "group", "tasks", "names", "unique", "ambig", "notfound", "doc", "errs", "nf+mac")
	for _, k := range keys {
		a := by[k]
		fmt.Printf("%-28s %6d %7d %7d %7d %9d %5d %5d %6d\n", k, a.tasks, a.names, a.uniq, a.amb, a.nf, a.doc, a.errs, a.macro)
	}
	cat := map[string][]string{}
	for _, r := range rows {
		for _, l := range r.Locs {
			c := "unique"
			switch {
			case l.Doc:
				c = "doc"
			case l.NotFound:
				c = "notfound"
			case l.Ambiguous:
				c = "ambiguous"
			}
			if len(cat[c]) < *examples*4 && (len(cat[c]) == 0 || len(cat[c])%4 == 0 || r.Kind != "n2p") {
				cat[c] = append(cat[c], fmt.Sprintf("%s  %s -> %v", r.ID, l.Name, l.Files))
			}
		}
	}
	for _, c := range []string{"unique", "ambiguous", "notfound", "doc"} {
		fmt.Printf("\nexamples (%s):\n", c)
		for i, e := range cat[c] {
			if i >= *examples {
				break
			}
			fmt.Println("  " + e)
		}
	}
	if *out != "" {
		type rec struct {
			evalkit.RustLocateRow
			Locs []evalkit.RustTestLoc `json:"locs"`
		}
		var rs []rec
		for _, r := range rows {
			rs = append(rs, rec{r, r.Locs})
		}
		b, _ := json.MarshalIndent(rs, "", " ")
		return os.WriteFile(*out, b, 0o644)
	}
	return nil
}

// metaString reads meta[key][sub] (a string in a nested object) from meta.json; "" when absent.
func metaString(metaPath, key, sub string) string {
	b, err := os.ReadFile(metaPath)
	if err != nil {
		return ""
	}
	var m map[string]map[string]string
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return ""
	}
	m = map[string]map[string]string{}
	if v, ok := raw[key]; ok {
		var inner map[string]string
		if json.Unmarshal(v, &inner) == nil {
			m[key] = inner
		}
	}
	return m[key][sub]
}

// cmdPyLocate runs the Python test-name locator over every task's P tree and prints unique / ambiguous / not-found statistics.
func cmdPyLocate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("py-locate", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "Python task JSONL (tasks-python.jsonl)")
	work := fs.String("work", "", "work directory (clones in <work>/repos)")
	out := fs.String("out", "", "write per-name locations as JSON here")
	examples := fs.Int("examples", 3, "examples printed per category")
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
	byRepo := map[string][]evalkit.Task{}
	for _, t := range tasks {
		k := strings.ToLower(t.Repo)
		byRepo[k] = append(byRepo[k], t)
	}
	var mu sync.Mutex
	var rows []evalkit.PyLocateRow
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, ts := range byRepo {
		wg.Add(1)
		go func(ts []evalkit.Task) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			for i, t := range ts {
				if ctx.Err() != nil {
					return
				}
				idx := filepath.Join(*work, "idx", fmt.Sprintf("pyloc_%s_%d.index", strings.ReplaceAll(t.Repo, "/", "__"), i))
				os.MkdirAll(filepath.Dir(idx), 0o755)
				r := evalkit.PyLocateTask(ctx, evalkit.RustRepoDir(*work, t.Repo), idx, t)
				mu.Lock()
				rows = append(rows, r)
				fmt.Fprintf(os.Stderr, "%-44s f2p=%-4d unique=%-4d ambiguous=%-3d notfound=%-4d %s\n", r.ID, r.F2P, r.Unique, r.Ambig, r.NotFnd, r.Err)
				mu.Unlock()
			}
		}(ts)
	}
	wg.Wait()
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	type agg struct{ tasks, names, uniq, amb, nf, errs int }
	by := map[string]*agg{}
	add := func(k string, r evalkit.PyLocateRow) {
		a := by[k]
		if a == nil {
			a = &agg{}
			by[k] = a
		}
		a.tasks++
		a.names += r.F2P
		a.uniq += r.Unique
		a.amb += r.Ambig
		a.nf += r.NotFnd
		if r.Err != "" {
			a.errs++
		}
	}
	form := map[string]*agg{}
	for _, r := range rows {
		add("ALL", r)
		add("repo="+strings.ToLower(r.Repo), r)
		for _, l := range r.Locs {
			a := form[l.Form]
			if a == nil {
				a = &agg{}
				form[l.Form] = a
			}
			a.names++
			switch {
			case l.NotFound:
				a.nf++
			case l.Ambiguous:
				a.amb++
			default:
				a.uniq++
			}
		}
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("%-32s %6s %7s %7s %7s %9s %5s\n", "group", "tasks", "names", "unique", "ambig", "notfound", "errs")
	for _, k := range keys {
		a := by[k]
		fmt.Printf("%-32s %6d %7d %7d %7d %9d %5d\n", k, a.tasks, a.names, a.uniq, a.amb, a.nf, a.errs)
	}
	fmt.Println()
	for _, k := range []string{"nodeid", "django", "docstring", "bare", "other"} {
		if a := form[k]; a != nil {
			fmt.Printf("%-32s %6s %7d %7d %7d %9d\n", "form="+k, "", a.names, a.uniq, a.amb, a.nf)
		}
	}
	// One example per (outcome, form, repo), at most -examples per outcome and form.
	cat := map[string][]string{}
	seen := map[string]int{}
	for _, r := range rows {
		for _, l := range r.Locs {
			c := "unique"
			switch {
			case l.NotFound:
				c = "notfound"
			case l.Ambiguous:
				c = "ambiguous"
			}
			k := c + "|" + l.Form
			if seen[k] < *examples {
				seen[k]++
				cat[k] = append(cat[k], fmt.Sprintf("%s  %.110s -> %v", r.ID, l.Name, l.Files))
			}
		}
	}
	ckeys := make([]string, 0, len(cat))
	for k := range cat {
		ckeys = append(ckeys, k)
	}
	sort.Strings(ckeys)
	for _, k := range ckeys {
		fmt.Printf("\nexamples (%s):\n", strings.ReplaceAll(k, "|", ", form="))
		for _, e := range cat[k] {
			fmt.Println("  " + e)
		}
	}
	if *out != "" {
		type rec struct {
			evalkit.PyLocateRow
			Locs []evalkit.PyTestLoc `json:"locs"`
		}
		var rs []rec
		for _, r := range rows {
			rs = append(rs, rec{r, r.Locs})
		}
		b, _ := json.MarshalIndent(rs, "", " ")
		return os.WriteFile(*out, b, 0o644)
	}
	return nil
}

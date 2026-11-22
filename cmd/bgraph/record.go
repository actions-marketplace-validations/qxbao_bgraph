package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/qxbao/bgraph"
)

// runRecord implements `bgraph record` (docs/dynamic.md §4, §8): run every test file (Python, Node) in its own process (tree)
// with the collector and store what it loaded/opened. Exit codes: 0 done (even when some units failed or are incomplete), 1 error, 2 usage.
func runRecord(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bgraph record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "directory inside the repo")
	python := fs.String("python", "", "interpreter (overrides bgraph.toml [dynamic_layer] python; default python on PATH)")
	nodeExe := fs.String("node", "", "node executable (overrides bgraph.toml [dynamic_layer] node; default node on PATH)")
	ecoName := fs.String("eco", "", "record only this ecosystem: python, node or jvm (default: [dynamic_layer] ecosystems, else every one with test units)")
	buildArgs := fs.String("build-args", "", "jvm: extra Maven/Gradle arguments of the recording build, space-separated (e.g. -Dmaven.repo.local=DIR)")
	keepLogs := fs.Bool("keep-logs", false, "jvm: keep the collector logs (debugging)")
	trust := fs.Bool("trust-native-env", false, "as bgraph.toml [dynamic_layer] trust_native_env = true")
	units := fs.String("units", "", "only the test units (files) matching this glob")
	jobs := fs.Int("jobs", 0, "units run in parallel (default: half the CPUs for Python, a quarter for Node; jvm: parallel test JVMs, default min(4, CPUs/2))")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-unit timeout (a unit that times out gets an incomplete record)")
	allowDirty := fs.Bool("allow-dirty", false, "record a working tree that differs from HEAD")
	out := fs.String("out", "", "write the records to this JSONL file instead of the state directory (import elsewhere with `bgraph dynamic import`)")
	quiet := fs.Bool("quiet", false, "no per-unit progress")
	asJSON := fs.Bool("json", false, "print the summary in JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "record: extra arguments: %v\n", fs.Args())
		return 2
	}
	ctx := context.Background()
	ix, err := bgraph.Open(ctx, *root, bgraph.Options{Python: *python, Node: *nodeExe, TrustNativeEnv: *trust})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer ix.Close()
	opt := bgraph.RecordOptions{Eco: *ecoName, Units: *units, Jobs: *jobs, Timeout: *timeout, AllowDirty: *allowDirty, Out: *out,
		BuildArgs: strings.Fields(*buildArgs), KeepLogs: *keepLogs}
	if !*quiet {
		opt.Progress = func(done, total int, r bgraph.DynamicRecord, elapsed time.Duration, tail string) {
			state := "complete"
			switch {
			case !r.Pass && !r.Complete:
				state = "failed, incomplete: " + r.Incomplete
			case !r.Pass:
				state = "failed"
			case !r.Complete:
				state = "incomplete: " + r.Incomplete
			}
			fmt.Fprintf(stderr, "[%d/%d] %s  %s  (%d deps, %s)\n", done, total, r.Unit, state, len(r.Deps), elapsed.Round(time.Millisecond))
			if !r.Pass && tail != "" {
				lines := strings.Split(strings.TrimSpace(tail), "\n")
				if len(lines) > 5 {
					lines = lines[len(lines)-5:]
				}
				for _, l := range lines {
					fmt.Fprintf(stderr, "      | %s\n", strings.TrimRight(l, "\r"))
				}
			}
		}
	}
	sum, err := ix.Record(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	for _, w := range sum.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	if *asJSON {
		return printJSON(stdout, stderr, sum)
	}
	fmt.Fprintf(stdout, "commit: %s\nunits: %d\nrecorded: %d\ncomplete (and passed): %d\nincomplete: %d\nfailed: %d\nstore: %s\nelapsed: %s\n",
		sum.Commit, sum.Units, sum.Recorded, sum.Complete, sum.Incomplete, sum.Failed, sum.Store, sum.Elapsed.Round(time.Millisecond))
	if len(sum.Toolchain) > 0 {
		fmt.Fprintln(stdout, "native toolchain addons loaded (accepted, docs/dynamic.md §8.3):")
		for _, k := range bgraph.SortedReasons(sum.Toolchain) {
			fmt.Fprintf(stdout, "  %4d  %s\n", sum.Toolchain[k], k)
		}
	}
	if len(sum.Reasons) > 0 {
		fmt.Fprintln(stdout, "incomplete reasons:")
		for _, k := range bgraph.SortedReasons(sum.Reasons) {
			fmt.Fprintf(stdout, "  %4d  %s\n", sum.Reasons[k], k)
		}
	}
	for _, s := range sum.Skipped {
		fmt.Fprintf(stdout, "skipped: %s\n", s)
	}
	return 0
}

// runDynamic implements `bgraph dynamic import FILE` and `bgraph dynamic status`.
func runDynamic(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bgraph dynamic import [--root DIR] FILE | bgraph dynamic status [--root DIR] [--eco python|node|jvm] [--python EXE] [--node EXE] [--trust-native-env] [--json]")
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("bgraph dynamic "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "directory inside the repo")
	var python, nodeExe, ecoName *string
	var asJSON, trust *bool
	switch sub {
	case "import":
	case "status":
		python = fs.String("python", "", "interpreter (overrides bgraph.toml [dynamic_layer] python)")
		nodeExe = fs.String("node", "", "node executable (overrides bgraph.toml [dynamic_layer] node)")
		ecoName = fs.String("eco", "", "only this ecosystem (python, node or jvm); default: every one with records or test units; JSON: an object with --eco, else an array")
		trust = fs.Bool("trust-native-env", false, "as bgraph.toml [dynamic_layer] trust_native_env = true")
		asJSON = fs.Bool("json", false, "in JSON")
	default:
		fmt.Fprintf(stderr, "dynamic: unknown subcommand %q (import, status)\n", sub)
		return 2
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if (sub == "import" && fs.NArg() != 1) || (sub == "status" && fs.NArg() != 0) {
		fmt.Fprintf(stderr, "dynamic %s: wrong arguments: %v\n", sub, fs.Args())
		return 2
	}
	opt := bgraph.Options{}
	if python != nil {
		opt.Python, opt.Node, opt.TrustNativeEnv = *python, *nodeExe, *trust
	}
	ctx := context.Background()
	ix, err := bgraph.Open(ctx, *root, opt)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer ix.Close()
	if sub == "import" {
		n, warn, err := ix.ImportRecords(fs.Arg(0))
		for _, w := range warn {
			fmt.Fprintln(stderr, "warning:", w)
		}
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "imported %d records\n", n)
		return 0
	}
	ecos := []string{*ecoName}
	if *ecoName == "" {
		ecos = []string{"python", "node"}
	}
	var sts []bgraph.DynamicStatus
	for _, e := range ecos {
		st, err := ix.DynamicStatusOf(ctx, e)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if *ecoName == "" && st.Records == 0 && st.TestUnits == 0 {
			continue
		}
		for _, w := range st.Warnings {
			fmt.Fprintln(stderr, "warning:", w)
		}
		sts = append(sts, st)
	}
	if *asJSON {
		if *ecoName != "" {
			return printJSON(stdout, stderr, sts[0])
		}
		if sts == nil {
			sts = []bgraph.DynamicStatus{}
		}
		return printJSON(stdout, stderr, sts)
	}
	for i, st := range sts {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		printStatus(stdout, st)
	}
	return 0
}

func printStatus(stdout io.Writer, st bgraph.DynamicStatus) {
	fmt.Fprintf(stdout, "%s: %s\nrecords: %d (units %d of %d test units)\ncurrent environment: %d records\nusable at HEAD: %d\n",
		st.Eco, st.Store, st.Records, st.Units, st.TestUnits, st.CurrentEnv, st.Usable)
	if st.EnvError != "" {
		fmt.Fprintf(stdout, "environment: %s (no record is usable)\n", st.EnvError)
	}
	if len(st.Unusable) > 0 {
		fmt.Fprintln(stdout, "not usable at HEAD:")
		for _, k := range bgraph.SortedReasons(st.Unusable) {
			fmt.Fprintf(stdout, "  %4d  %s\n", st.Unusable[k], k)
		}
	}
	if len(st.Reasons) > 0 {
		fmt.Fprintln(stdout, "incomplete reasons:")
		for _, k := range bgraph.SortedReasons(st.Reasons) {
			fmt.Fprintf(stdout, "  %4d  %s\n", st.Reasons[k], k)
		}
	}
}

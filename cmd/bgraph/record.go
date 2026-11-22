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

// runRecord implements `bgraph record` (docs/dynamic.md §4): run every Python test file in its own process with the collector
// and store what it loaded/opened. Exit codes: 0 done (even when some units failed or are incomplete), 1 error, 2 usage.
func runRecord(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bgraph record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "directory inside the repo")
	python := fs.String("python", "", "interpreter (overrides bgraph.toml [dynamic_layer] python; default python on PATH)")
	trust := fs.Bool("trust-native-env", false, "as bgraph.toml [dynamic_layer] trust_native_env = true")
	units := fs.String("units", "", "only the test units (files) matching this glob")
	jobs := fs.Int("jobs", 0, "units run in parallel (default: half the CPUs)")
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
	ix, err := bgraph.Open(ctx, *root, bgraph.Options{Python: *python, TrustNativeEnv: *trust})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer ix.Close()
	opt := bgraph.RecordOptions{Units: *units, Jobs: *jobs, Timeout: *timeout, AllowDirty: *allowDirty, Out: *out}
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
		fmt.Fprintln(stderr, "usage: bgraph dynamic import [--root DIR] FILE | bgraph dynamic status [--root DIR] [--python EXE] [--json]")
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("bgraph dynamic "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "directory inside the repo")
	var python *string
	var asJSON, trust *bool
	switch sub {
	case "import":
	case "status":
		python = fs.String("python", "", "interpreter (overrides bgraph.toml [dynamic_layer] python)")
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
		opt.Python, opt.TrustNativeEnv = *python, *trust
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
	st, err := ix.DynamicStatus(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	for _, w := range st.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	if *asJSON {
		return printJSON(stdout, stderr, st)
	}
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
	return 0
}

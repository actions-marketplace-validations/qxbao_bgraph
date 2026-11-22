package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/audit"
)

// Exit code of `bgraph audit` when a failed test lies outside the selection (or, with --strict, is unmapped).
const exitAuditMissed = 3

// maxListed caps the failures printed per section in text mode (JSON and the log are never capped).
const maxListed = 100

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// auditLogLine is one line of the --log file.
type auditLogLine struct {
	Time     string              `json:"time"`
	Base     string              `json:"base"`
	Head     string              `json:"head"`
	Scope    bgraph.Tier         `json:"scope"`
	Selected int                 `json:"selected"`
	Failed   int                 `json:"failed"`
	Missed   []bgraph.FailedTest `json:"missed"`
	Unmapped []bgraph.FailedTest `json:"unmapped"`
	Clean    bool                `json:"clean"`
}

// runAudit implements `bgraph audit` (shadow mode): the full suite already ran; report failures the selection would
// have skipped. Exit codes: 0 clean, 3 missed failure (or unmapped with --strict), 1 error, 2 usage.
func runAudit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bgraph audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "directory inside the repo")
	var results multiFlag
	fs.Var(&results, "results", "test results file (repeatable): JUnit XML, Jest/Vitest JSON, Playwright JSON")
	format := fs.String("format", audit.FormatAuto, "results format: auto|junit|jest-json|playwright-json")
	base := fs.String("base", "", "base ref (default: merge-base with origin/HEAD, origin/main, main, master)")
	head := fs.String("head", "", "target commit (default: working tree)")
	failed := fs.String("failed", "", "ids of test units that failed in the previous run, comma-separated")
	asJSON := fs.Bool("json", false, "in JSON")
	logPath := fs.String("log", "", "append one JSON line per run to this file and print the clean-run streak")
	strict := fs.Bool("strict", false, "also exit 3 when a failure cannot be mapped to a test file")
	var dyn dynamicFlag
	dyn.register(fs)
	python := fs.String("python", "", "interpreter of the dynamic layer (overrides bgraph.toml [dynamic_layer] python)")
	nodeExe := fs.String("node", "", "node executable of the dynamic layer (overrides bgraph.toml [dynamic_layer] node)")
	trust := fs.Bool("trust-native-env", false, "as bgraph.toml [dynamic_layer] trust_native_env = true")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "audit: extra arguments: %v\n", fs.Args())
		return 2
	}
	if len(results) == 0 {
		fmt.Fprintln(stderr, "audit: --results is required")
		return 2
	}
	if !slices.Contains([]string{audit.FormatAuto, audit.FormatJUnit, audit.FormatJestJSON, audit.FormatPlaywright}, *format) {
		fmt.Fprintf(stderr, "invalid --format: %q\n", *format)
		return 2
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "error:", err); return 1 }

	var parsed []audit.Result
	for _, p := range results {
		r, err := audit.Parse(p, *format)
		if err != nil {
			return fail(err)
		}
		parsed = append(parsed, r)
	}

	ctx := context.Background()
	ix, err := bgraph.Open(ctx, *root, bgraph.Options{Python: *python, Node: *nodeExe, TrustNativeEnv: *trust})
	if err != nil {
		return fail(err)
	}
	defer ix.Close()

	b := *base
	if b == "" {
		d, err := ix.DefaultBase(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "warning:", err, "→ treating as no base (running full)")
		}
		b = d // empty → full (no_base): every failure is covered
	}
	ch := bgraph.Change{Base: b, Head: *head, Worktree: *head == "", Dynamic: dyn.mode}
	if *failed != "" {
		ch.PreviouslyFailed = strings.Split(*failed, ",")
	}
	rep, err := ix.Audit(ctx, ch, parsed)
	if err != nil {
		return fail(err)
	}
	ok := rep.Clean && (!*strict || len(rep.Unmapped) == 0)

	if *asJSON {
		if code := printJSON(stdout, stderr, rep); code != 0 {
			return code
		}
	} else {
		printAudit(stdout, rep, parsed)
	}
	if *logPath != "" {
		n, err := appendAuditLog(*logPath, auditLogLine{Time: time.Now().UTC().Format(time.RFC3339), Base: rep.Base, Head: rep.Head,
			Scope: rep.Scope, Selected: rep.Selected, Failed: rep.Failed, Missed: rep.Missed, Unmapped: rep.Unmapped, Clean: ok})
		if err != nil {
			return fail(err)
		}
		if *asJSON {
			fmt.Fprintf(stderr, "clean shadow runs in a row: %d\n", n)
		} else {
			fmt.Fprintf(stdout, "clean shadow runs in a row: %d\n", n)
		}
	}
	if !ok {
		return exitAuditMissed
	}
	return 0
}

func printAudit(w io.Writer, rep bgraph.AuditReport, parsed []audit.Result) {
	fmt.Fprintf(w, "scope: %s\n", rep.Scope)
	if rep.Base != "" {
		fmt.Fprintf(w, "base: %s\n", rep.Base)
	}
	fmt.Fprintf(w, "results files: %d\nselected test units: %d\nfailed test cases: %d\n", len(parsed), rep.Selected, rep.Failed)
	section := func(title, hint string, list []bgraph.FailedTest) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s (%d): %s\n", title, len(list), hint)
		for i, f := range list {
			if i == maxListed {
				fmt.Fprintf(w, "  ... and %d more (use --json for the full list)\n", len(list)-i)
				break
			}
			name := f.File
			if name == "" {
				name = "(unknown file)"
			}
			fmt.Fprintf(w, "  %s :: %s\n      %s\n", name, f.Test, f.Reason)
		}
	}
	section("MISSED", "failed, but the selection would not have run them (unsafe miss, flaky or pre-existing failure: judge)", rep.Missed)
	section("unmapped", "failed, but not attributable to a known test file", rep.Unmapped)
	if rep.Clean {
		fmt.Fprintln(w, "\nno missed failure")
	}
}

// appendAuditLog appends line to the log and returns the number of consecutive clean lines at its tail (including line).
func appendAuditLog(path string, line auditLogLine) (int, error) {
	if line.Missed == nil {
		line.Missed = []bgraph.FailedTest{}
	}
	if line.Unmapped == nil {
		line.Unmapped = []bgraph.FailedTest{}
	}
	b, err := json.Marshal(line)
	if err != nil {
		return 0, err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	// Make sure the new line starts on its own line even if the file lacks a trailing newline.
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, st.Size()-1); err == nil && last[0] != '\n' {
			b = append([]byte{'\n'}, b...)
		}
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return 0, err
	}
	// Streak: scan the whole file (it is one short line per CI run); unparsable lines are ignored.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	streak := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var l struct {
			Clean *bool `json:"clean"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Clean == nil {
			continue
		}
		if *l.Clean {
			streak++
		} else {
			streak = 0
		}
	}
	return streak, sc.Err()
}

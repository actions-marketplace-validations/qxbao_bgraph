// Command bgraph is the CLI that selects affected tests (docs/plan.md §7.2).
//
// Exit codes: 0 success, 1 tool error, 2 usage error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/explain"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/runner"
)

const version = "0.1.0-dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `bgraph - select affected tests from the file-level import graph (fail-closed)

Usage:
  bgraph affected  [--root DIR] [--base REF] [--head REF] [--json] [--failed ID,...] [--dynamic[=false]] [--python EXE] [--node EXE] [--trust-native-env]
  bgraph explain   [--root DIR] [--base REF] [--dynamic[=false]] [--python EXE] [--node EXE] [--trust-native-env] <test|file>
  bgraph unresolved [--root DIR] [--class unknown|dynamic|ambiguous|uncertain_scan]
  bgraph command   [--root DIR] [--base REF] [--list-dir DIR] [--json] [--dynamic[=false]] [--python EXE] [--node EXE] [--trust-native-env]
  bgraph index     [--root DIR]
  bgraph mcp       [--root DIR]        (MCP server on stdio)
  bgraph audit     --results FILE [--results FILE ...] [--format auto|junit|jest-json|playwright-json] [--root DIR] [--base REF] [--head REF] [--failed ID,...] [--json] [--log FILE] [--strict] [--dynamic[=false]] [--python EXE] [--node EXE] [--trust-native-env]
  bgraph record    [--root DIR] [--eco python|node] [--python EXE] [--node EXE] [--trust-native-env] [--units GLOB] [--jobs N] [--timeout D] [--allow-dirty] [--out FILE] [--json]
  bgraph dynamic import [--root DIR] FILE
  bgraph dynamic status [--root DIR] [--eco python|node] [--python EXE] [--node EXE] [--trust-native-env] [--json]
  bgraph doctor                (not implemented)
  bgraph version | help
`

// run is the testable entry point: it returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, "bgraph", version)
		return 0
	case "mcp":
		return runMCP(rest, stderr)
	case "audit":
		return runAudit(rest, stdout, stderr)
	case "record":
		return runRecord(rest, stdout, stderr)
	case "dynamic":
		return runDynamic(rest, stdout, stderr)
	case "doctor":
		fmt.Fprintf(stderr, "bgraph %s: not implemented (later phase)\n", cmd)
		return 2
	case "affected", "explain", "unresolved", "command", "index":
	default:
		fmt.Fprintf(stderr, "invalid command: %q\n\n%s", cmd, usage)
		return 2
	}

	fs := flag.NewFlagSet("bgraph "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "directory inside the repo")
	var base, head, failed, class, listDir, python, nodeExe string
	var asJSON bool
	var dyn dynamicFlag
	var trust bool
	switch cmd {
	case "affected", "explain", "command":
		dyn.register(fs)
		fs.StringVar(&python, "python", "", "interpreter of the dynamic layer (overrides bgraph.toml [dynamic_layer] python)")
		fs.StringVar(&nodeExe, "node", "", "node executable of the dynamic layer (overrides bgraph.toml [dynamic_layer] node)")
		fs.BoolVar(&trust, "trust-native-env", false, "as bgraph.toml [dynamic_layer] trust_native_env = true")
	}
	switch cmd {
	case "affected":
		fs.StringVar(&base, "base", "", "base ref (default: merge-base with origin/HEAD, origin/main, main, master)")
		fs.StringVar(&head, "head", "", "target commit (default: working tree)")
		fs.StringVar(&failed, "failed", "", "ids of test units that failed in the previous run, comma-separated")
		fs.BoolVar(&asJSON, "json", false, "in JSON")
	case "explain":
		fs.StringVar(&base, "base", "", "base ref")
	case "unresolved":
		fs.StringVar(&class, "class", "", "filter by class: unknown|dynamic|ambiguous|uncertain_scan")
	case "command":
		fs.StringVar(&base, "base", "", "base ref")
		fs.StringVar(&listDir, "list-dir", "", "directory to write the list file to")
		fs.BoolVar(&asJSON, "json", false, "in JSON")
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if cmd == "explain" && fs.NArg() != 1 {
		fmt.Fprintln(stderr, "explain needs exactly one argument: <test|file>")
		return 2
	}
	if cmd != "explain" && fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s: extra arguments: %v\n", cmd, fs.Args())
		return 2
	}
	if cmd == "unresolved" && class != "" && !slices.Contains([]string{"unknown", "dynamic", "ambiguous", "uncertain_scan"}, class) {
		fmt.Fprintf(stderr, "invalid --class: %q\n", class)
		return 2
	}

	ctx := context.Background()
	ix, err := bgraph.Open(ctx, *root, bgraph.Options{Python: python, Node: nodeExe, TrustNativeEnv: trust})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer ix.Close()

	fail := func(err error) int { fmt.Fprintln(stderr, "error:", err); return 1 }

	switch cmd {
	case "index":
		st, err := ix.Analyze(ctx)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "files: %d\nedges: %d\ntest units: %d\nunresolved: %d\nelapsed: %s\n",
			st.Files, st.Edges, st.Units, len(st.Unresolved), st.Elapsed.Round(1e6))
		return 0
	case "unresolved":
		list, err := ix.Unresolved(ctx)
		if err != nil {
			return fail(err)
		}
		n := 0
		for _, u := range list {
			if class != "" && string(u.Class) != class {
				continue
			}
			n++
			fmt.Fprintf(stdout, "%s:%d  %q  [%s]  widens to %s %s\n", u.File, u.Line, u.Spec, u.Class, u.ScopeTier, scopeDir(u.ScopeDir))
			if u.Detail != "" {
				fmt.Fprintf(stdout, "    %s\n", u.Detail)
			}
			if u.ScopeTier == model.TierNone && u.File == rules.FileName {
				continue // a stale [[dynamic]] declaration: the detail above says what to fix
			}
			fmt.Fprintf(stdout, "    bgraph.toml suggestion:\n%s\n", indent(suggestRule(u), "      "))
		}
		fmt.Fprintf(stdout, "total: %d\n", n)
		return 0
	}

	// affected / explain / command: need the Selection.
	if base == "" {
		b, err := ix.DefaultBase(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "warning:", err, "→ treating as no base (running full)")
		}
		base = b // empty → Select returns full (no_base)
	}
	ch := bgraph.Change{Base: base, Head: head, Worktree: head == "", Dynamic: dyn.mode}
	if failed != "" {
		ch.PreviouslyFailed = strings.Split(failed, ",")
	}
	sel, err := ix.Select(ctx, ch)
	if err != nil {
		return fail(err)
	}

	switch cmd {
	case "affected":
		if asJSON {
			return printJSON(stdout, stderr, sel)
		}
		printSelection(stdout, sel)
	case "explain":
		for _, l := range explain.Lines(sel, fs.Arg(0)) {
			fmt.Fprintln(stdout, l)
		}
	case "command":
		plan, err := runner.Commands(sel, runner.Options{ListDir: listDir})
		if err != nil {
			return fail(err)
		}
		if asJSON {
			return printJSON(stdout, stderr, plan)
		}
		fmt.Fprintf(stdout, "# scope: %s\n", plan.Scope)
		for _, n := range plan.Notes {
			fmt.Fprintf(stdout, "# %s\n", n)
		}
		for _, c := range plan.Commands {
			line := shellLine(c.Argv)
			if c.Project != "." && c.Project != "" {
				line = "cd " + shellQuote(c.Project) + " && " + line
			}
			fmt.Fprintln(stdout, line)
		}
	}
	return 0
}

func printJSON(stdout, stderr io.Writer, v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, string(b))
	return 0
}

func scopeDir(d string) string {
	if d == "" {
		return "(repo)"
	}
	return d
}

func printSelection(w io.Writer, sel model.Selection) {
	fmt.Fprintf(w, "scope: %s\n", sel.Scope)
	if sel.Base != "" {
		fmt.Fprintf(w, "base: %s\n", sel.Base)
	}
	fmt.Fprintf(w, "changed files: %d\n", len(sel.Changed))
	if len(sel.Widening) > 0 {
		fmt.Fprintln(w, "\nwidenings:")
		for _, x := range sel.Widening {
			fmt.Fprintf(w, "  - %s  %s  scope=%s %s\n", x.Reason, x.Path, x.Tier, x.Scope)
			if x.Detail != "" {
				fmt.Fprintf(w, "      %s\n", x.Detail)
			}
			if x.Suggest != "" {
				fmt.Fprintf(w, "      suggestion:\n%s\n", indent(x.Suggest, "        "))
			}
		}
	}
	if len(sel.Warnings) > 0 {
		fmt.Fprintln(w, "\nwarnings:")
		for _, x := range sel.Warnings {
			fmt.Fprintf(w, "  - %s  %s  %s\n", x.Reason, x.Path, x.Detail)
		}
	}
	fmt.Fprintf(w, "\nselected tests: %d\n", len(sel.Tests))
	for _, t := range sel.Tests {
		first := ""
		if len(t.Reasons) > 0 {
			first = t.Reasons[0]
		}
		fmt.Fprintf(w, "  %s  (%s)  %s\n", t.Unit, t.Runner, first)
	}
	if len(sel.Narrowed) > 0 {
		fmt.Fprintf(w, "\nnarrowed by dynamic records: %d\n", len(sel.Narrowed))
		for _, n := range sel.Narrowed {
			fmt.Fprintf(w, "  %s  %s\n", n.Unit, n.Reason)
		}
	}
}

// dynamicFlag is --dynamic: absent → bgraph.toml decides, --dynamic → on, --dynamic=false → off.
type dynamicFlag struct{ mode bgraph.DynamicMode }

func (d *dynamicFlag) register(fs *flag.FlagSet) {
	fs.Var(d, "dynamic", "narrow widened tests with recorded dependencies (docs/dynamic.md); =false disables it even if bgraph.toml enables it")
}
func (d *dynamicFlag) IsBoolFlag() bool { return true }
func (d *dynamicFlag) String() string   { return string(d.mode) }
func (d *dynamicFlag) Set(v string) error {
	b, err := strconv.ParseBool(v)
	if err != nil {
		return err
	}
	d.mode = bgraph.DynamicOff
	if b {
		d.mode = bgraph.DynamicOn
	}
	return nil
}

func suggestRule(u model.Unresolved) string {
	// A runner-config key computed in code: describe it with [[config]] (see docs/bgraph.example.toml).
	if strings.HasPrefix(u.Detail, model.ReasonConfigIsCode+":") {
		if file, key, ok := strings.Cut(u.Spec, ": "); ok {
			key, _, _ = strings.Cut(key, " ")
			return fmt.Sprintf("[[config]]\nfile = %q\nreplaces = [%q]\n"+
				"# alias = { ... } / alias_regex = { ... } / tests = [ ... ]: what the code computes\nreason = \"...\"", file, key)
		}
	}
	// A dynamic import or sys.path edit that bgraph cannot evaluate: describe what it loads with [[dynamic]].
	if u.Class == model.UnresolvedDynamic && u.ScopeTier != model.TierNone && strings.HasSuffix(u.File, ".py") {
		lines := ""
		if u.Line > 0 {
			lines = fmt.Sprintf("lines = [%d]\n", u.Line)
		}
		return fmt.Sprintf("[[dynamic]]\nfile = %q\n%sloads = [\"<globs of the files this site can load>\"]  # or: none = true (loads nothing from the repo)\nreason = \"...\"", u.File, lines)
	}
	dir := u.ScopeDir
	if dir == "" {
		dir = "."
	}
	return fmt.Sprintf("[[rule]]\npaths = [%q]\naffects = [%q]  # or a test glob; \"none\" if surely harmless", u.File, dir)
}

func indent(s, pre string) string {
	return pre + strings.ReplaceAll(s, "\n", "\n"+pre)
}

// shellLine joins argv into one shell line (display only; uses POSIX-style single quotes).
func shellLine(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

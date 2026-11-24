package bgraph

import (
	"strings"

	"github.com/qxbao/bgraph/internal/explain"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/runner"
)

// Run plans: the runner commands for a Selection, as `bgraph command --json` prints them.
type (
	// Plan is the run plan: the scope and one Command per runner invocation (none for scope none and full).
	Plan = runner.Plan
	// Command is one test-run command: Argv to run in Project (relative to the repository root) with Env added to the
	// caller's environment; ListFile, Batch/Batches and Note as documented in docs/cli.md (`command`).
	Command = runner.Command
	// CommandOptions configures Commands: ListDir (where list files are written; "" passes arguments directly),
	// MaxArgChars (default 8000) and FullThreshold (0 = off; a selected/total unit ratio above it makes the plan full).
	CommandOptions = runner.Options
)

// Commands builds the run plan of sel. A plan whose Scope is TierFull has no commands: the caller runs its full suite.
func Commands(sel Selection, opt CommandOptions) (Plan, error) { return runner.Commands(sel, opt) }

// Explain explains target (a test unit or a file path) against sel, one line per element, as `bgraph explain` prints it.
func Explain(sel Selection, target string) []string { return explain.Lines(sel, target) }

// Scope tiers, narrowest first (Selection.Scope, Widening.Tier).
const (
	TierNone    = model.TierNone    // no test is affected
	TierGraph   = model.TierGraph   // the selection is provable from the graph
	TierPackage = model.TierPackage // widened to a directory, package or Cargo target
	TierModule  = model.TierModule  // widened to a project (package.json, crate, Maven module, .NET project, Go module)
	TierFull    = model.TierFull    // run everything
)

// Widening reasons (Widening.Reason; stable machine-readable codes, see docs/architecture.md, "Widening reasons").
const (
	ReasonUnknownImport   = model.ReasonUnknownImport
	ReasonDynamicImport   = model.ReasonDynamicImport
	ReasonAmbiguousImport = model.ReasonAmbiguousImport
	ReasonUncertainScan   = model.ReasonUncertainScan
	ReasonConfigIsCode    = model.ReasonConfigIsCode
	ReasonManifestChanged = model.ReasonManifestChanged
	ReasonTestConfig      = model.ReasonTestConfig
	ReasonLockChanged     = model.ReasonLockChanged
	ReasonGlobalInput     = model.ReasonGlobalInput
	ReasonNoBase          = model.ReasonNoBase
	ReasonDiffTooLarge    = model.ReasonDiffTooLarge
	ReasonCaseMismatch    = model.ReasonCaseMismatch
	ReasonUserRule        = model.ReasonUserRule
	ReasonUnknownFile     = model.ReasonUnknownFile
	ReasonUnreferenced    = model.ReasonUnreferenced
	ReasonUnsupported     = model.ReasonUnsupported
	ReasonGeneratedOutput = model.ReasonGeneratedOutput
	ReasonFrameworkMagic  = model.ReasonFrameworkMagic
	ReasonSymlinkOutside  = model.ReasonSymlinkOutside
	ReasonSubmodule       = model.ReasonSubmodule
	ReasonInternalError   = model.ReasonInternalError
	ReasonRuleRejected    = model.ReasonRuleRejected   // warnings only
	ReasonDynamicSkipped  = model.ReasonDynamicSkipped // warnings only
)

// ShellLine renders c as one POSIX shell line, as `bgraph command` prints it: Env as a prefix and "cd <project> &&" when the
// project is not the repository root. Arguments are single-quoted when they contain anything but [A-Za-z0-9-_./:=@+,].
func ShellLine(c Command) string {
	q := make([]string, len(c.Argv))
	for i, a := range c.Argv {
		q[i] = shellQuote(a)
	}
	line := strings.Join(q, " ")
	for i := len(c.Env) - 1; i >= 0; i-- {
		k, v, _ := strings.Cut(c.Env[i], "=")
		line = k + "=" + shellQuote(v) + " " + line
	}
	if c.Project != "." && c.Project != "" {
		line = "cd " + shellQuote(c.Project) + " && " + line
	}
	return line
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'''`) + "'"
}

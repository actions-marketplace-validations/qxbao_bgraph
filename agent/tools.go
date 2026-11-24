// Package agent is bgraph's integration surface for coding agents, independent of any MCP SDK:
//
//   - the tools (affected_tests, test_commands, dependents_of, explain_selection) as typed Go calls, their JSON schemas and a
//     JSON dispatcher (Tools, Specs, Tools.Call), so a host can serve them from its own MCP server or call them directly;
//   - the guidance an agent reads (Instructions, Block, Skill);
//   - the Claude Code hook that points at bgraph when an agent runs a whole test suite (IsFullSuiteCommand, Hook);
//   - Install and Uninstall, which write and remove that guidance in a repository.
//
// The package github.com/qxbao/bgraph/mcpserver serves the same tools with the official Go MCP SDK (`bgraph mcp`).
//
// Output is size-bounded: the long lists (changed, tests, dependents) are capped at a per-call limit. The fail-closed sections
// (widening, warnings, unresolved) are always present, are never omitted, and are capped far more generously; every cap is
// reported through *_total counts and the truncated flag. Run commands (test_commands) are never truncated: a partial command
// list would silently skip tests.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/qxbao/bgraph"
)

const (
	// DefaultLimit caps the long lists of one result unless the call passes `limit`.
	DefaultLimit = 200
	// MaxLimit is the largest accepted `limit`.
	MaxLimit = 5000
	// FailClosedLimit is the minimum cap of the widening / warnings / unresolved sections: they are truncated last.
	FailClosedLimit = 1000
	// textLines bounds the lists inside the human-readable text summary (the structured result carries the rest).
	textLines = 40
)

// Tool names.
const (
	ToolAffected     = "affected_tests"
	ToolTestCommands = "test_commands"
	ToolDependents   = "dependents_of"
	ToolExplain      = "explain_selection"
)

// Instructions is the MCP server instructions text: when to call which tool, and how to read a fail-closed result.
const Instructions = "bgraph selects the tests a code change can affect, and widens the selection instead of guessing (fail-closed). " +
	"After editing code, call `test_commands` and run the commands it returns instead of the whole test suite. " +
	"scope=full means run the whole suite (no narrower command is safe); scope=none means no test is affected. " +
	"Before changing a file, `dependents_of` lists what depends on it. " +
	"Always read the `widening` section: it says why the selection was widened. An unresolved import MAY depend on a file."

// Tools serves the agent tools for one Index. Calls are serialized: the Index drives one long-lived git process.
// Tools does not take ownership of the Index: the caller closes it.
type Tools struct {
	ix *bgraph.Index
	mu sync.Mutex
}

// NewTools returns the tools of ix.
func NewTools(ix *bgraph.Index) *Tools { return &Tools{ix: ix} }

// AffectedInput is the input of affected_tests.
type AffectedInput struct {
	Base             string   `json:"base,omitempty" jsonschema:"base ref or commit; default: merge-base of HEAD with origin/HEAD, origin/main, main or master"`
	Head             string   `json:"head,omitempty" jsonschema:"target commit; default: the working tree (staged, unstaged and untracked changes included)"`
	PreviouslyFailed []string `json:"previously_failed,omitempty" jsonschema:"ids of test units that failed in the previous run; they are always re-selected"`
	Limit            int      `json:"limit,omitempty" jsonschema:"cap for the changed/tests lists (default 200, max 5000)"`
	Dynamic          *bool    `json:"dynamic,omitempty" jsonschema:"narrow widened tests with recorded dependencies (true) or never (false); default: bgraph.toml [dynamic_layer]"`
}

// AffectedOutput is the structured result of affected_tests.
type AffectedOutput struct {
	Scope         bgraph.Tier           `json:"scope" jsonschema:"none|graph|package|module|full; full means run the whole suite"`
	Base          string                `json:"base,omitempty"`
	Head          string                `json:"head,omitempty"`
	Changed       []string              `json:"changed"`
	ChangedTotal  int                   `json:"changed_total"`
	Tests         []bgraph.SelectedTest `json:"tests"`
	TestsTotal    int                   `json:"tests_total"`
	Widening      []bgraph.Widening     `json:"widening" jsonschema:"why the scope was widened; always present"`
	WideningTotal int                   `json:"widening_total"`
	Warnings      []bgraph.Widening     `json:"warnings" jsonschema:"problems that do not change the scope; always present"`
	WarningsTotal int                   `json:"warnings_total"`
	AlwaysRun     []string              `json:"always_run"`
	Narrowed      []bgraph.NarrowedTest `json:"narrowed" jsonschema:"widened tests removed by the dynamic layer (recorded dependencies); always present"`
	NarrowedTotal int                   `json:"narrowed_total"`
	Notes         []string              `json:"notes"`
	Truncated     bool                  `json:"truncated" jsonschema:"true when any list was cut; see the *_total counts"`
	Limit         int                   `json:"limit"`
}

// Affected selects the tests affected by the change between in.Base and in.Head.
func (t *Tools) Affected(ctx context.Context, in AffectedInput) (AffectedOutput, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sel, notes, err := t.selection(ctx, in.Base, in.Head, in.PreviouslyFailed, in.Dynamic)
	if err != nil {
		return AffectedOutput{}, err
	}
	return affectedOutput(sel, notes, clampLimit(in.Limit)), nil
}

// selection runs Select the way the CLI does, including the default base; a missing default base is a note, not an error
// (Select then returns scope=full / no_base).
func (t *Tools) selection(ctx context.Context, base, head string, failed []string, dyn *bool) (bgraph.Selection, []string, error) {
	notes := []string{}
	if base == "" {
		b, err := t.ix.DefaultBase(ctx)
		if err != nil {
			notes = append(notes, err.Error()+" → treating as no base (running full)")
		}
		base = b
	}
	mode := bgraph.DynamicDefault
	if dyn != nil {
		mode = bgraph.DynamicOff
		if *dyn {
			mode = bgraph.DynamicOn
		}
	}
	sel, err := t.ix.Select(ctx, bgraph.Change{Base: base, Head: head, Worktree: head == "", PreviouslyFailed: failed, Dynamic: mode})
	return sel, notes, err
}

func affectedOutput(sel bgraph.Selection, notes []string, limit int) AffectedOutput {
	out := AffectedOutput{Scope: sel.Scope, Base: sel.Base, Head: sel.Head, Notes: nonNil(notes), Limit: limit}
	var t1, t2, t3, t4, t5 bool
	out.Changed, out.ChangedTotal, t1 = capList(sel.Changed, limit)
	out.Tests, out.TestsTotal, t2 = capList(sel.Tests, limit)
	out.Widening, out.WideningTotal, t3 = capList(sel.Widening, failClosedLimit(limit))
	out.Warnings, out.WarningsTotal, t4 = capList(sel.Warnings, failClosedLimit(limit))
	out.Narrowed, out.NarrowedTotal, t5 = capList(sel.Narrowed, limit)
	out.AlwaysRun = nonNil(sel.AlwaysRun)
	out.Truncated = t1 || t2 || t3 || t4 || t5
	return out
}

// TestCommandsInput is the input of test_commands.
type TestCommandsInput struct {
	Base             string   `json:"base,omitempty" jsonschema:"base ref or commit; default: merge-base of HEAD with origin/HEAD, origin/main, main or master"`
	Head             string   `json:"head,omitempty" jsonschema:"target commit; default: the working tree (staged, unstaged and untracked changes included)"`
	PreviouslyFailed []string `json:"previously_failed,omitempty" jsonschema:"ids of test units that failed in the previous run; they are always re-selected"`
	Limit            int      `json:"limit,omitempty" jsonschema:"cap for the widening/warnings lists (default 200, max 5000); commands are never truncated"`
	Dynamic          *bool    `json:"dynamic,omitempty" jsonschema:"narrow widened tests with recorded dependencies (true) or never (false); default: bgraph.toml [dynamic_layer]"`
	BuildDir         string   `json:"build_dir,omitempty" jsonschema:"configured CTest or Meson build directory, relative to each project or absolute; default build"`
	BuildConfig      string   `json:"build_config,omitempty" jsonschema:"CMake build configuration for multi-config generators"`
}

// CommandLine is one command of a run plan, with the shell line to run it from the repository root.
type CommandLine struct {
	bgraph.Command
	Shell string `json:"shell" jsonschema:"the command as one POSIX shell line, to run from the repository root"`
}

// TestCommandsOutput is the structured result of test_commands.
type TestCommandsOutput struct {
	Scope         bgraph.Tier       `json:"scope" jsonschema:"none|graph|package|module|full; full means run the whole suite (no command is returned)"`
	Base          string            `json:"base,omitempty"`
	Head          string            `json:"head,omitempty"`
	Commands      []CommandLine     `json:"commands" jsonschema:"run every command, from the repository root; never truncated"`
	TestsTotal    int               `json:"tests_total" jsonschema:"number of selected test units"`
	ChangedTotal  int               `json:"changed_total"`
	Widening      []bgraph.Widening `json:"widening" jsonschema:"why the scope was widened; always present"`
	WideningTotal int               `json:"widening_total"`
	Warnings      []bgraph.Widening `json:"warnings" jsonschema:"problems that do not change the scope; always present"`
	WarningsTotal int               `json:"warnings_total"`
	Notes         []string          `json:"notes" jsonschema:"notes of the selection and of the plan (e.g. why the plan became full)"`
	Truncated     bool              `json:"truncated" jsonschema:"true when the widening or warnings list was cut; see the *_total counts"`
	Limit         int               `json:"limit"`
}

// TestCommands selects the affected tests and turns them into runner commands, as `bgraph command` does. A plan whose scope is
// full has no command: the caller runs its whole suite.
func (t *Tools) TestCommands(ctx context.Context, in TestCommandsInput) (TestCommandsOutput, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sel, notes, err := t.selection(ctx, in.Base, in.Head, in.PreviouslyFailed, in.Dynamic)
	if err != nil {
		return TestCommandsOutput{}, err
	}
	plan, err := bgraph.Commands(sel, bgraph.CommandOptions{BuildDir: in.BuildDir, BuildConfig: in.BuildConfig})
	if err != nil {
		return TestCommandsOutput{}, err
	}
	limit := clampLimit(in.Limit)
	out := TestCommandsOutput{Scope: plan.Scope, Base: sel.Base, Head: sel.Head, Commands: []CommandLine{},
		TestsTotal: len(sel.Tests), ChangedTotal: len(sel.Changed), Notes: append(nonNil(notes), plan.Notes...), Limit: limit}
	for _, c := range plan.Commands {
		out.Commands = append(out.Commands, CommandLine{Command: c, Shell: bgraph.ShellLine(c)})
	}
	var t1, t2 bool
	out.Widening, out.WideningTotal, t1 = capList(sel.Widening, failClosedLimit(limit))
	out.Warnings, out.WarningsTotal, t2 = capList(sel.Warnings, failClosedLimit(limit))
	out.Truncated = t1 || t2
	return out, nil
}

// DependentsInput is the input of dependents_of.
type DependentsInput struct {
	Files []string `json:"files" jsonschema:"repo-relative (or absolute) paths of the files to query"`
	Rev   string   `json:"rev,omitempty" jsonschema:"commit to analyze; default: the working tree"`
	Limit int      `json:"limit,omitempty" jsonschema:"cap for the dependents/tests lists (default 200, max 5000)"`
}

// DependentsOutput is the structured result of dependents_of.
type DependentsOutput struct {
	Files           []string                  `json:"files" jsonschema:"normalized queried files"`
	Missing         []string                  `json:"missing" jsonschema:"queried files that are not nodes of the graph (they may still be used outside it)"`
	Dependents      []bgraph.Dependent        `json:"dependents" jsonschema:"tests first, then by path; each with its dependency chain"`
	DependentsTotal int                       `json:"dependents_total"`
	Tests           []bgraph.SelectedTest     `json:"tests"`
	TestsTotal      int                       `json:"tests_total"`
	Unresolved      []bgraph.UnresolvedImport `json:"unresolved" jsonschema:"unresolved imports in scope of a queried file: they MAY also depend on it; always present"`
	UnresolvedTotal int                       `json:"unresolved_total"`
	Truncated       bool                      `json:"truncated"`
	Limit           int                       `json:"limit"`
}

// Dependents lists the files that depend (transitively) on in.Files.
func (t *Tools) Dependents(ctx context.Context, in DependentsInput) (DependentsOutput, error) {
	if len(in.Files) == 0 {
		return DependentsOutput{}, fmt.Errorf("files must not be empty")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	res, err := t.ix.Dependents(ctx, bgraph.DependentsQuery{Files: in.Files, Rev: in.Rev})
	if err != nil {
		return DependentsOutput{}, err
	}
	limit := clampLimit(in.Limit)
	out := DependentsOutput{Files: nonNil(res.Files), Missing: nonNil(res.Missing), Limit: limit}
	var t1, t2, t3 bool
	out.Dependents, out.DependentsTotal, t1 = capList(res.Dependents, limit)
	out.Tests, out.TestsTotal, t2 = capList(res.Tests, limit)
	out.Unresolved, out.UnresolvedTotal, t3 = capList(res.Unresolved, failClosedLimit(limit))
	out.Truncated = t1 || t2 || t3
	return out, nil
}

// ExplainInput is the input of explain_selection.
type ExplainInput struct {
	Target string `json:"target" jsonschema:"a test unit id or a file path"`
	Base   string `json:"base,omitempty" jsonschema:"base ref or commit; default: merge-base with the default branch"`
	Head   string `json:"head,omitempty" jsonschema:"target commit; default: the working tree"`
	Limit  int    `json:"limit,omitempty" jsonschema:"cap for the explanation lines (default 200, max 5000)"`
	// Dynamic: see AffectedInput.Dynamic.
	Dynamic *bool `json:"dynamic,omitempty" jsonschema:"narrow widened tests with recorded dependencies (true) or never (false); default: bgraph.toml [dynamic_layer]"`
}

// ExplainOutput is the structured result of explain_selection.
type ExplainOutput struct {
	Target        string            `json:"target"`
	Scope         bgraph.Tier       `json:"scope"`
	Lines         []string          `json:"lines" jsonschema:"the explanation, one line per element"`
	LinesTotal    int               `json:"lines_total"`
	Widening      []bgraph.Widening `json:"widening" jsonschema:"always present"`
	WideningTotal int               `json:"widening_total"`
	Warnings      []bgraph.Widening `json:"warnings" jsonschema:"always present"`
	WarningsTotal int               `json:"warnings_total"`
	// Narrowed: widened units removed by the dynamic layer.
	Narrowed      []bgraph.NarrowedTest `json:"narrowed" jsonschema:"widened tests removed by the dynamic layer (recorded dependencies); always present"`
	NarrowedTotal int                   `json:"narrowed_total"`
	Notes         []string              `json:"notes"`
	Truncated     bool                  `json:"truncated"`
	Limit         int                   `json:"limit"`
}

// Explain explains why a test unit is selected, or where a changed file leads.
func (t *Tools) Explain(ctx context.Context, in ExplainInput) (ExplainOutput, error) {
	if strings.TrimSpace(in.Target) == "" {
		return ExplainOutput{}, fmt.Errorf("target must not be empty")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	sel, notes, err := t.selection(ctx, in.Base, in.Head, nil, in.Dynamic)
	if err != nil {
		return ExplainOutput{}, err
	}
	limit := clampLimit(in.Limit)
	out := ExplainOutput{Target: in.Target, Scope: sel.Scope, Notes: nonNil(notes), Limit: limit}
	var t1, t2, t3, t4 bool
	out.Lines, out.LinesTotal, t1 = capList(bgraph.Explain(sel, in.Target), limit)
	out.Widening, out.WideningTotal, t2 = capList(sel.Widening, failClosedLimit(limit))
	out.Warnings, out.WarningsTotal, t3 = capList(sel.Warnings, failClosedLimit(limit))
	out.Narrowed, out.NarrowedTotal, t4 = capList(sel.Narrowed, limit)
	out.Truncated = t1 || t2 || t3 || t4
	return out, nil
}

// Result is the answer of Tools.Call: the structured output (one of the *Output types) and its text summary.
type Result struct {
	Structured any
	Text       string
}

// Call decodes args (a JSON object; empty means {}) into the input of the tool name, runs it and returns its result. Unknown
// fields are rejected, as the input schemas forbid them. It lets a host serve the tools from its own MCP server.
func (t *Tools) Call(ctx context.Context, name string, args json.RawMessage) (Result, error) {
	switch name {
	case ToolAffected:
		return call(ctx, args, t.Affected)
	case ToolTestCommands:
		return call(ctx, args, t.TestCommands)
	case ToolDependents:
		return call(ctx, args, t.Dependents)
	case ToolExplain:
		return call(ctx, args, t.Explain)
	}
	return Result{}, fmt.Errorf("unknown tool %q", name)
}

// texter is implemented by every *Output type.
type texter interface{ Text() string }

func call[In any, Out texter](ctx context.Context, args json.RawMessage, f func(context.Context, In) (Out, error)) (Result, error) {
	var in In
	if len(bytes.TrimSpace(args)) > 0 && !bytes.Equal(bytes.TrimSpace(args), []byte("null")) {
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return Result{}, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	out, err := f(ctx, in)
	if err != nil {
		return Result{}, err
	}
	return Result{Structured: out, Text: out.Text()}, nil
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultLimit
	case n > MaxLimit:
		return MaxLimit
	}
	return n
}

func failClosedLimit(limit int) int { return max(limit, FailClosedLimit) }

// capList returns at most n elements of s (never nil), the total count and whether it was cut.
func capList[T any](s []T, n int) ([]T, int, bool) {
	if len(s) > n {
		return append([]T{}, s[:n]...), len(s), true
	}
	return append([]T{}, s...), len(s), false
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

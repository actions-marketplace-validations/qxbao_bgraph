// Package mcpserver exposes bgraph over the Model Context Protocol (docs/cli.md, mcp): affected_tests, dependents_of and
// explain_selection. The server keeps one *bgraph.Index open for the whole session, so the scan cache and the analysis memo stay warm.
//
// Output is size-bounded: the long lists (changed, tests, dependents) are capped at a per-call limit. The fail-closed sections
// (widening, warnings, unresolved) are always present, are never omitted, and are capped far more generously; every cap is reported
// through *_total counts and the truncated flag.
package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/explain"
	"github.com/qxbao/bgraph/internal/model"
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

// Server serves one repo.
type Server struct {
	ix *bgraph.Index
	mu sync.Mutex // the Index drives one long-lived git process: serialize tool calls
}

// New builds the MCP server for ix. It does not take ownership of ix: the caller closes it.
func New(ix *bgraph.Index, version string) *mcp.Server {
	s := &Server{ix: ix}
	srv := mcp.NewServer(&mcp.Implementation{Name: "bgraph", Version: version}, &mcp.ServerOptions{
		Instructions: "bgraph selects the tests affected by a change (fail-closed). Always read the `widening` and `unresolved` sections: " +
			"scope=full means run the whole suite; a widening or unresolved import means the graph could not prove the answer.",
	})
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "affected_tests",
		Description: "Select the tests affected by the change between base and head (default: merge-base with the default branch → working tree). " +
			"Returns scope (none|graph|package|module|full), the tests with reasons, and the widening/warnings sections that explain why " +
			"the scope was widened. scope=full means run everything.",
		Annotations: ro,
	}, s.affected)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "dependents_of",
		Description: "List the files that depend (transitively) on the given files, tests first, each with the dependency chain, plus the test units " +
			"reached. `unresolved` lists imports that could not be resolved and whose scope contains a queried file: they MAY also depend on it.",
		Annotations: ro,
	}, s.dependents)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "explain_selection",
		Description: "Explain why a test unit is selected, or where a changed file leads, for the change between base and head. " +
			"Also returns the selection's scope and widening/warnings.",
		Annotations: ro,
	}, s.explain)
	return srv
}

// Run serves ix on stdio until the client disconnects or ctx ends.
func Run(ctx context.Context, ix *bgraph.Index, version string) error {
	return New(ix, version).Run(ctx, &mcp.StdioTransport{})
}

func boolPtr(b bool) *bool { return &b }

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
	Scope         model.Tier           `json:"scope" jsonschema:"none|graph|package|module|full; full means run the whole suite"`
	Base          string               `json:"base,omitempty"`
	Head          string               `json:"head,omitempty"`
	Changed       []string             `json:"changed"`
	ChangedTotal  int                  `json:"changed_total"`
	Tests         []model.SelectedTest `json:"tests"`
	TestsTotal    int                  `json:"tests_total"`
	Widening      []model.Widening     `json:"widening" jsonschema:"why the scope was widened; always present"`
	WideningTotal int                  `json:"widening_total"`
	Warnings      []model.Widening     `json:"warnings" jsonschema:"problems that do not change the scope; always present"`
	WarningsTotal int                  `json:"warnings_total"`
	AlwaysRun     []string             `json:"always_run"`
	Narrowed      []model.NarrowedTest `json:"narrowed" jsonschema:"widened tests removed by the dynamic layer (recorded dependencies); always present"`
	NarrowedTotal int                  `json:"narrowed_total"`
	Notes         []string             `json:"notes"`
	Truncated     bool                 `json:"truncated" jsonschema:"true when any list was cut; see the *_total counts"`
	Limit         int                  `json:"limit"`
}

func (s *Server) affected(ctx context.Context, _ *mcp.CallToolRequest, in AffectedInput) (*mcp.CallToolResult, AffectedOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sel, notes, err := s.selection(ctx, in.Base, in.Head, in.PreviouslyFailed, in.Dynamic)
	if err != nil {
		return nil, AffectedOutput{}, err
	}
	out := affectedOutput(sel, notes, clampLimit(in.Limit))
	return textResult(affectedText(out)), out, nil
}

// selection runs Select the way the CLI does, including the default base; a missing default base is a note, not an error
// (Select then returns scope=full / no_base).
func (s *Server) selection(ctx context.Context, base, head string, failed []string, dyn *bool) (model.Selection, []string, error) {
	notes := []string{}
	if base == "" {
		b, err := s.ix.DefaultBase(ctx)
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
	sel, err := s.ix.Select(ctx, bgraph.Change{Base: base, Head: head, Worktree: head == "", PreviouslyFailed: failed, Dynamic: mode})
	return sel, notes, err
}

func affectedOutput(sel model.Selection, notes []string, limit int) AffectedOutput {
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

// DependentsInput is the input of dependents_of.
type DependentsInput struct {
	Files []string `json:"files" jsonschema:"repo-relative (or absolute) paths of the files to query"`
	Rev   string   `json:"rev,omitempty" jsonschema:"commit to analyze; default: the working tree"`
	Limit int      `json:"limit,omitempty" jsonschema:"cap for the dependents/tests lists (default 200, max 5000)"`
}

// DependentsOutput is the structured result of dependents_of.
type DependentsOutput struct {
	Files           []string              `json:"files" jsonschema:"normalized queried files"`
	Missing         []string              `json:"missing" jsonschema:"queried files that are not nodes of the graph (they may still be used outside it)"`
	Dependents      []bgraph.Dependent    `json:"dependents" jsonschema:"tests first, then by path; each with its dependency chain"`
	DependentsTotal int                   `json:"dependents_total"`
	Tests           []bgraph.SelectedTest `json:"tests"`
	TestsTotal      int                   `json:"tests_total"`
	Unresolved      []model.Unresolved    `json:"unresolved" jsonschema:"unresolved imports in scope of a queried file: they MAY also depend on it; always present"`
	UnresolvedTotal int                   `json:"unresolved_total"`
	Truncated       bool                  `json:"truncated"`
	Limit           int                   `json:"limit"`
}

func (s *Server) dependents(ctx context.Context, _ *mcp.CallToolRequest, in DependentsInput) (*mcp.CallToolResult, DependentsOutput, error) {
	if len(in.Files) == 0 {
		return nil, DependentsOutput{}, fmt.Errorf("files must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.ix.Dependents(ctx, bgraph.DependentsQuery{Files: in.Files, Rev: in.Rev})
	if err != nil {
		return nil, DependentsOutput{}, err
	}
	limit := clampLimit(in.Limit)
	out := DependentsOutput{Files: nonNil(res.Files), Missing: nonNil(res.Missing), Limit: limit}
	var t1, t2, t3 bool
	out.Dependents, out.DependentsTotal, t1 = capList(res.Dependents, limit)
	out.Tests, out.TestsTotal, t2 = capList(res.Tests, limit)
	out.Unresolved, out.UnresolvedTotal, t3 = capList(res.Unresolved, failClosedLimit(limit))
	out.Truncated = t1 || t2 || t3
	return textResult(dependentsText(out)), out, nil
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
	Target        string           `json:"target"`
	Scope         model.Tier       `json:"scope"`
	Lines         []string         `json:"lines" jsonschema:"the explanation, one line per element"`
	LinesTotal    int              `json:"lines_total"`
	Widening      []model.Widening `json:"widening" jsonschema:"always present"`
	WideningTotal int              `json:"widening_total"`
	Warnings      []model.Widening `json:"warnings" jsonschema:"always present"`
	WarningsTotal int              `json:"warnings_total"`
	// Narrowed: widened units removed by the dynamic layer.
	Narrowed      []model.NarrowedTest `json:"narrowed" jsonschema:"widened tests removed by the dynamic layer (recorded dependencies); always present"`
	NarrowedTotal int                  `json:"narrowed_total"`
	Notes         []string             `json:"notes"`
	Truncated     bool                 `json:"truncated"`
	Limit         int                  `json:"limit"`
}

func (s *Server) explain(ctx context.Context, _ *mcp.CallToolRequest, in ExplainInput) (*mcp.CallToolResult, ExplainOutput, error) {
	if strings.TrimSpace(in.Target) == "" {
		return nil, ExplainOutput{}, fmt.Errorf("target must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sel, notes, err := s.selection(ctx, in.Base, in.Head, nil, in.Dynamic)
	if err != nil {
		return nil, ExplainOutput{}, err
	}
	limit := clampLimit(in.Limit)
	out := ExplainOutput{Target: in.Target, Scope: sel.Scope, Notes: nonNil(notes), Limit: limit}
	var t1, t2, t3, t4 bool
	out.Lines, out.LinesTotal, t1 = capList(explain.Lines(sel, in.Target), limit)
	out.Widening, out.WideningTotal, t2 = capList(sel.Widening, failClosedLimit(limit))
	out.Warnings, out.WarningsTotal, t3 = capList(sel.Warnings, failClosedLimit(limit))
	out.Narrowed, out.NarrowedTotal, t4 = capList(sel.Narrowed, limit)
	out.Truncated = t1 || t2 || t3 || t4
	return textResult(explainText(out)), out, nil
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

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func affectedText(o AffectedOutput) string {
	var b strings.Builder
	switch o.Scope {
	case model.TierFull:
		b.WriteString("SCOPE: FULL - run the ENTIRE test suite; the selection could not be narrowed (see widening below).\n")
	case model.TierNone:
		b.WriteString("scope: none - no test is affected.\n")
	default:
		fmt.Fprintf(&b, "scope: %s\n", o.Scope)
	}
	if o.Base != "" {
		fmt.Fprintf(&b, "base: %s\n", o.Base)
	}
	fmt.Fprintf(&b, "changed files: %d, selected tests: %d, widenings: %d, warnings: %d\n", o.ChangedTotal, o.TestsTotal, o.WideningTotal, o.WarningsTotal)
	for _, n := range o.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	writeWidenings(&b, "widening", o.Widening, o.WideningTotal)
	writeWidenings(&b, "warnings", o.Warnings, o.WarningsTotal)
	if len(o.Tests) > 0 {
		b.WriteString("\nselected tests:\n")
		for i, t := range o.Tests {
			if i == textLines {
				fmt.Fprintf(&b, "  ... %d more in the structured result (tests_total=%d)\n", len(o.Tests)-i, o.TestsTotal)
				break
			}
			fmt.Fprintf(&b, "  %s (%s)\n", t.Unit, t.Runner)
		}
	}
	writeNarrowed(&b, o.Narrowed, o.NarrowedTotal)
	if o.Truncated {
		fmt.Fprintf(&b, "\nTRUNCATED: some lists were cut at limit=%d; compare the *_total counts. Raise `limit` to see more.\n", o.Limit)
	}
	return b.String()
}

func dependentsText(o DependentsOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "queried: %s\ndependents: %d, tests: %d, unresolved imports in scope: %d\n", strings.Join(o.Files, ", "), o.DependentsTotal, o.TestsTotal, o.UnresolvedTotal)
	if len(o.Missing) > 0 {
		fmt.Fprintf(&b, "not in the graph (may still be used outside it): %s\n", strings.Join(o.Missing, ", "))
	}
	if len(o.Unresolved) > 0 {
		b.WriteString("\nunresolved imports that MAY also depend on the queried files:\n")
		for i, u := range o.Unresolved {
			if i == textLines {
				fmt.Fprintf(&b, "  ... %d more in the structured result (unresolved_total=%d)\n", len(o.Unresolved)-i, o.UnresolvedTotal)
				break
			}
			fmt.Fprintf(&b, "  %s:%d %q [%s] scope=%s %s\n", u.File, u.Line, u.Spec, u.Class, u.ScopeTier, u.ScopeDir)
		}
	}
	if len(o.Dependents) > 0 {
		b.WriteString("\ndependents (tests first):\n")
		for i, d := range o.Dependents {
			if i == textLines {
				fmt.Fprintf(&b, "  ... %d more in the structured result (dependents_total=%d)\n", len(o.Dependents)-i, o.DependentsTotal)
				break
			}
			kind := "file"
			if d.Test {
				kind = "test"
			}
			fmt.Fprintf(&b, "  [%s] %s\n", kind, d.File)
		}
	}
	if o.Truncated {
		fmt.Fprintf(&b, "\nTRUNCATED: some lists were cut at limit=%d; compare the *_total counts.\n", o.Limit)
	}
	return b.String()
}

func explainText(o ExplainOutput) string {
	var b strings.Builder
	if o.Scope == model.TierFull {
		b.WriteString("SCOPE: FULL - run the ENTIRE test suite (see widening below).\n")
	} else {
		fmt.Fprintf(&b, "scope: %s\n", o.Scope)
	}
	for _, n := range o.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	for _, l := range o.Lines {
		b.WriteString(l + "\n")
	}
	writeWidenings(&b, "widening", o.Widening, o.WideningTotal)
	writeWidenings(&b, "warnings", o.Warnings, o.WarningsTotal)
	writeNarrowed(&b, o.Narrowed, o.NarrowedTotal)
	if o.Truncated {
		fmt.Fprintf(&b, "\nTRUNCATED at limit=%d; compare the *_total counts.\n", o.Limit)
	}
	return b.String()
}

func writeNarrowed(b *strings.Builder, ns []model.NarrowedTest, total int) {
	if len(ns) == 0 {
		return
	}
	fmt.Fprintf(b, "\nnarrowed by dynamic records: %d (widened tests whose recorded run loaded none of the changed files)\n", total)
	for i, n := range ns {
		if i == textLines {
			fmt.Fprintf(b, "  ... %d more in the structured result (narrowed_total=%d)\n", len(ns)-i, total)
			break
		}
		fmt.Fprintf(b, "  %s\n", n.Unit)
	}
}

func writeWidenings(b *strings.Builder, title string, ws []model.Widening, total int) {
	if len(ws) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s:\n", title)
	for i, w := range ws {
		if i == textLines {
			fmt.Fprintf(b, "  ... %d more in the structured result (total=%d)\n", len(ws)-i, total)
			break
		}
		fmt.Fprintf(b, "  - %s  %s  scope=%s %s\n", w.Reason, w.Path, w.Tier, w.Scope)
		if w.Detail != "" {
			fmt.Fprintf(b, "      %s\n", w.Detail)
		}
	}
}

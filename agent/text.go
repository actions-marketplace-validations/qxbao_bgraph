package agent

import (
	"fmt"
	"strings"

	"github.com/qxbao/bgraph"
)

// Text is the human-readable summary of the result.
func (o AffectedOutput) Text() string {
	var b strings.Builder
	switch o.Scope {
	case bgraph.TierFull:
		b.WriteString("SCOPE: FULL - run the ENTIRE test suite; the selection could not be narrowed (see widening below).\n")
	case bgraph.TierNone:
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

// Text is the human-readable summary of the result.
func (o DependentsOutput) Text() string {
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

// Text is the human-readable summary of the result.
func (o ExplainOutput) Text() string {
	var b strings.Builder
	if o.Scope == bgraph.TierFull {
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

func writeNarrowed(b *strings.Builder, ns []bgraph.NarrowedTest, total int) {
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

func writeWidenings(b *strings.Builder, title string, ws []bgraph.Widening, total int) {
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

// Text is the human-readable summary of the result: the scope, then one shell line per command.
func (o TestCommandsOutput) Text() string {
	var b strings.Builder
	switch o.Scope {
	case bgraph.TierFull:
		b.WriteString("SCOPE: FULL - run the ENTIRE test suite; no narrower command can be proven safe (see the notes and widening below).\n")
	case bgraph.TierNone:
		b.WriteString("scope: none - no test is affected: nothing to run.\n")
	default:
		fmt.Fprintf(&b, "scope: %s\n", o.Scope)
	}
	if o.Base != "" {
		fmt.Fprintf(&b, "base: %s\n", o.Base)
	}
	fmt.Fprintf(&b, "changed files: %d, selected tests: %d, commands: %d\n", o.ChangedTotal, o.TestsTotal, len(o.Commands))
	for _, n := range o.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	if len(o.Commands) > 0 {
		b.WriteString("\nrun every command, from the repository root:\n")
		for _, c := range o.Commands {
			if c.Note != "" {
				fmt.Fprintf(&b, "# %s\n", c.Note)
			}
			b.WriteString(c.Shell + "\n")
		}
	}
	writeWidenings(&b, "widening", o.Widening, o.WideningTotal)
	writeWidenings(&b, "warnings", o.Warnings, o.WarningsTotal)
	if o.Truncated {
		fmt.Fprintf(&b, "\nTRUNCATED: the widening or warnings list was cut at limit=%d; compare the *_total counts. The commands are complete.\n", o.Limit)
	}
	return b.String()
}

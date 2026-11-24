package evalkit

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// T2Markdown builds the T2 report: violations first, then a per-repo table, then "native ⊄ bgraph" details.
func T2Markdown(q T2Result) string {
	var b strings.Builder
	verdict := "PASS (0 T2 violations)"
	if q.Violations > 0 {
		verdict = fmt.Sprintf("FAIL: %d T2 violations", q.Violations)
	} else if slices.ContainsFunc(q.Repos, func(r T2RepoResult) bool {
		return r.Repo.Eco == "swift" && (r.Error != "" || r.Errors > 0 || r.Killed+r.Survived == 0)
	}) {
		verdict = "INCOMPLETE (missing Swift T2 evidence)"
	}
	mode := "re-check on cached oracle"
	if q.RunTests {
		mode = "build/refresh oracle (--run-tests)"
	}
	fmt.Fprintf(&b, "# bgraph-eval t2 — %s\n\nMode: %s. Time: %s\n\n", verdict, mode, q.Wall.Round(time.Millisecond))
	b.WriteString("## Violations\n\n")
	n := 0
	for _, r := range q.Repos {
		for _, c := range r.Violations {
			n++
			fmt.Fprintf(&b, "- `%s` %s %s:%d (%s) `%s -> %s`: red files outside the selection %v; scope=%s; widenings %v\n", r.Repo.Name, c.Rec.Mutant.ID, c.Rec.Mutant.File, c.Rec.Mutant.Line, c.Rec.Mutant.Op, short(c.Rec.Mutant.Before), short(c.Rec.Mutant.After), c.Missed, c.Scope, c.Widenings)
			for _, d := range c.Diag {
				fmt.Fprintf(&b, "  - diagnosis %s: role=%s in_unit=%v units=%v imports=%d unresolved=%v reach=%d reached_changed=%v\n", d.File, d.Role, d.InUnit, d.Units, len(d.Imports), d.Unresolved, d.ReachSize, d.ReachedChanged)
			}
			for _, x := range c.Near {
				fmt.Fprintf(&b, "  - near: %s\n", x)
			}
		}
	}
	if n == 0 {
		b.WriteString("(none)\n")
	}
	var errs []string
	for _, r := range q.Repos {
		if r.Error != "" {
			errs = append(errs, fmt.Sprintf("%s: %s", r.Repo.Name, r.Error))
		}
		for _, c := range r.Checks {
			if c.Err != "" {
				errs = append(errs, fmt.Sprintf("%s %s: %s", r.Repo.Name, c.Rec.Mutant.ID, c.Err))
			}
		}
	}
	if len(errs) > 0 {
		b.WriteString("\n## Errors / warnings\n\n")
		for _, e := range errs {
			fmt.Fprintf(&b, "- %s\n", e)
		}
	}
	b.WriteString("\n## By repo\n\n| repo | R | baseline files | pre-broken | flaky | mutants | killed | survived | timeout/error | full widening | violations | native ⊄ bgraph | median sel/units bgraph | native | all | check ms |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range q.Repos {
		fmt.Fprintf(&b, "| %s | %.10s | %d | %d | %d | %d | %d | %d | %d/%d | %d | %d | %d | %.3f | %.3f | 1 | %d |\n", r.Repo.Name, r.Repo.R, r.Baseline.Files, len(r.Baseline.Failing), len(r.Baseline.Flaky),
			r.Killed+r.Survived+r.Timeout+r.Errors, r.Killed, r.Survived, r.Timeout, r.Errors, r.Full, len(r.Violations), len(r.NativeNotSubset), r.MedBgraph, r.MedNative, r.CheckMs)
	}
	rustReport(&b, q)
	pythonReport(&b, q)
	goReport(&b, q)
	swiftReport(&b, q)
	dartReport(&b, q)
	b.WriteString("\n## native⊄ bgraph\n\n")
	n = 0
	for _, r := range q.Repos {
		for _, c := range r.NativeNotSubset {
			n++
			red := map[string]bool{}
			for _, f := range c.Rec.Red {
				red[f] = true
			}
			fmt.Fprintf(&b, "- `%s` %s %s:%d: native %d files, bgraph %d/%d; native only: %v", r.Repo.Name, c.Rec.Mutant.ID, c.Rec.Mutant.File, c.Rec.Mutant.Line, len(c.Rec.Native), c.Selected, c.Units, c.NativeOnly)
			var redOnly []string
			for _, f := range c.NativeOnly {
				if red[f] {
					redOnly = append(redOnly, f)
				}
			}
			fmt.Fprintf(&b, " (of which really red: %v)\n", redOnly)
			for _, d := range c.Diag {
				fmt.Fprintf(&b, "  - %s: in_unit=%v reach=%d reached_changed=%v unresolved=%v\n", d.File, d.InUnit, d.ReachSize, d.ReachedChanged, d.Unresolved)
			}
		}
	}
	if n == 0 {
		b.WriteString("(none: native ⊆ bgraph on every mutant without full widening)\n")
	}
	b.WriteString("\n## Mutants\n\n| repo | id | operator | location | before → after | status | red | scope | sel/units | native | native-only |\n|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range q.Repos {
		for _, c := range r.Checks {
			fmt.Fprintf(&b, "| %s | %s | %s | %s:%d | `%s` → `%s` | %s | %d | %s | %d/%d | %d | %d |\n", r.Repo.Dir, c.Rec.Mutant.ID, c.Rec.Mutant.Op, c.Rec.Mutant.File, c.Rec.Mutant.Line, short(c.Rec.Mutant.Before), short(c.Rec.Mutant.After), c.Rec.Status, len(c.Rec.Red), c.Scope, c.Selected, c.Units, len(c.Rec.Native), len(c.NativeOnly))
		}
	}
	return b.String()
}

func short(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "`", "'")
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// WriteT2 writes t2.json and t2.md into dir.
func WriteT2(dir string, q T2Result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(q, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "t2.json"), b, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "t2.md"), []byte(T2Markdown(q)), 0o644)
}

// rustReport writes the per-repo mutant accounting of the Rust repos (generated / compiled / killed / survived and the selection ratios).
func rustReport(b *strings.Builder, q T2Result) {
	any := false
	for _, r := range q.Repos {
		if r.Repo.Eco != "rust" {
			continue
		}
		if !any {
			b.WriteString("\n## Rust mutants\n\n| repo | R | generated | does not compile | compiled | killed | survived | timeout | errors | selected < all | full widening | violations | median selected/units | baseline units | pre-broken | flaky | suite ms |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
			any = true
		}
		partial := 0
		for _, c := range r.Checks {
			if c.Err == "" && !c.Full() && c.Units > 0 && c.Selected < c.Units {
				partial++
			}
		}
		suite := int64(0)
		if len(r.Baseline.RunMs) > 0 {
			suite = r.Baseline.RunMs[len(r.Baseline.RunMs)-1]
		}
		fmt.Fprintf(b, "| %s | %.10s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %.3f | %d | %d | %d | %d |\n", r.Repo.Name, r.Repo.R, r.Generated, r.NoCompile, r.Killed+r.Survived+r.Timeout,
			r.Killed, r.Survived, r.Timeout, r.Errors, partial, r.Full, len(r.Violations), r.MedBgraph, r.Baseline.Files, len(r.Baseline.Failing), len(r.Baseline.Flaky), suite)
	}
}

// pythonReport writes the per-repo mutant accounting of the Python repos, with the dynamic-import widening count: a mutant's selection is
// "inflated" when units remain selected only because a dynamic_import Unresolved seeded a file (the selection without dynamic seeds is smaller).
func pythonReport(b *strings.Builder, q T2Result) {
	any := false
	for _, r := range q.Repos {
		if r.Repo.Eco != "python" {
			continue
		}
		if !any {
			b.WriteString("\n## Python mutants\n\n")
			b.WriteString("| repo | R | generated | does not compile | run | killed | survived | timeout | errors | selected < all | full | violations | median selected/units | dynamic seed fired | dynamic-inflated | test files | pre-broken | flaky | suite ms |\n")
			b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
			any = true
		}
		partial, dyn, inflated := 0, 0, 0
		for _, c := range r.Checks {
			if c.Err != "" || c.Full() {
				continue
			}
			if c.Units > 0 && c.Selected < c.Units {
				partial++
			}
			if c.Py != nil && c.Py.Dyn > 0 {
				dyn++
				if c.Py.SelNoDyn < c.Py.Sel {
					inflated++
				}
			}
		}
		suite := int64(0)
		if len(r.Baseline.RunMs) > 0 {
			suite = r.Baseline.RunMs[len(r.Baseline.RunMs)-1]
		}
		fmt.Fprintf(b, "| %s | %.10s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %.3f | %d | %d | %d | %d | %d | %d |\n", r.Repo.Name, r.Repo.R, r.Generated, r.NoCompile, r.Killed+r.Survived+r.Timeout,
			r.Killed, r.Survived, r.Timeout, r.Errors, partial, r.Full, len(r.Violations), r.MedBgraph, dyn, inflated, r.Baseline.Files, len(r.Baseline.Failing), len(r.Baseline.Flaky), suite)
	}
}

func swiftReport(b *strings.Builder, q T2Result) {
	header := false
	for _, r := range q.Repos {
		if r.Repo.Eco != "swift" {
			continue
		}
		if !header {
			b.WriteString("\n## Swift XCTest mutants\n\n| repo | baseline tests | baseline modules | planned | killed | survived | compile failures | timeouts | errors | selected < all | median selected/units | violations |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
			header = true
		}
		partial := 0
		for _, c := range r.Checks {
			if c.Err == "" && !c.Full() && c.Selected < c.Units {
				partial++
			}
		}
		fmt.Fprintf(b, "| %s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %.3f | %d |\n",
			r.Repo.Name, r.Baseline.Tests, r.Baseline.Files, r.Generated, r.Killed, r.Survived, r.NoCompile, r.Timeout, r.Errors, partial, r.MedBgraph, len(r.Violations))
	}
}

// dartReport writes the per-repo mutant accounting of the Dart repos and the widening reasons of their selections.
func dartReport(b *strings.Builder, q T2Result) {
	header := false
	for _, r := range q.Repos {
		if r.Repo.Eco != "dart" {
			continue
		}
		if !header {
			b.WriteString("\n## Dart mutants\n\n| repo | R | generated | does not compile | run | killed | survived | timeout | errors | selected < all | full | violations | median selected/units | suites | tests | pre-broken units | flaky units | suite ms |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
			header = true
		}
		partial := 0
		reasons := map[string]int{}
		for _, c := range r.Checks {
			if c.Err == "" && !c.Full() && c.Units > 0 && c.Selected < c.Units {
				partial++
			}
			for w := range c.Widenings {
				reasons[w]++
			}
		}
		suite := int64(0)
		if len(r.Baseline.RunMs) > 0 {
			suite = slices.Max(r.Baseline.RunMs)
		}
		fmt.Fprintf(b, "| %s | %.10s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %.3f | %d | %d | %d | %d | %d |\n", r.Repo.Name, r.Repo.R, r.Generated, r.NoCompile,
			r.Killed+r.Survived+r.Timeout, r.Killed, r.Survived, r.Timeout, r.Errors, partial, r.Full, len(r.Violations), r.MedBgraph, r.Baseline.Files, r.Baseline.Tests,
			len(r.Baseline.Failing), len(r.Baseline.Flaky), suite)
		if len(reasons) > 0 {
			fmt.Fprintf(b, "\nWidening reasons in %s (mutants per tier:reason):", r.Repo.Name)
			for _, k := range slices.Sorted(maps.Keys(reasons)) {
				fmt.Fprintf(b, " `%s` %d;", k, reasons[k])
			}
			b.WriteString("\n\n")
		}
	}
}

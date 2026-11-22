package bgraph

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/qxbao/bgraph/internal/audit"
	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/runner"
)

// AuditResult is the parsed content of one test-results file (see internal/audit).
type AuditResult = audit.Result

// FailedTest is one failed test case together with the test file it was mapped to.
type FailedTest struct {
	File   string `json:"file,omitempty"` // repo-relative test file; empty for unmapped failures
	Test   string `json:"test"`
	Source string `json:"source,omitempty"` // results file it came from
	Reason string `json:"reason"`           // why it is missed / unmapped
}

// AuditReport is the outcome of shadow-mode auditing (docs/plan.md §7.2, §8.7).
type AuditReport struct {
	Scope    Tier         `json:"scope"`
	Base     string       `json:"base,omitempty"`
	Head     string       `json:"head,omitempty"`
	Selected int          `json:"selected"` // number of selected test units
	Failed   int          `json:"failed"`   // failed test cases across all results
	Missed   []FailedTest `json:"missed"`   // failed, but the selection would not have run them
	Unmapped []FailedTest `json:"unmapped"` // failed, but not attributable to a known test file
	Clean    bool         `json:"clean"`    // no missed failure (unmapped failures do not affect it)
}

// Audit compares what bgraph would have selected for ch with the failures in results (a full suite run) and reports
// every failed test the selection would have skipped. A missed failure is either an unsafe miss, a flaky test or a
// failure that already existed on the base: Audit cannot tell, so it reports and the caller judges.
//
// Coverage rule (fail-closed toward reporting). A failing test file F is covered iff:
//   - the selection scope is full (the runner runs everything; nothing can be missed), or
//   - runner.Commands turns the selection into a full run (unsupported runner, or a non-empty scope with no tests), or
//   - F is in Files of some SelectedTest or below one of them (a Python directory unit: pytest collection hooks), or
//   - some SelectedTest has Whole=true and F lies within its Project (eco.Within: "." contains everything).
//
// Units removed by the dynamic layer (Selection.Narrowed) are not in Tests: their failures are missed like any other.
//
// Module/package widenings need no extra case: the selector already expands them into SelectedTests, and the runner
// executes exactly those, so SelectedTests is the ground truth. Not modelled (reported as missed, never hidden): a
// runner falling back to a whole-project command because of glob characters in a file name.
//
// Failures that cannot be attributed to a known test file (describe-name-only JUnit classnames, files unknown to the
// head tree) are returned in Unmapped and never dropped. When the scope is full nothing is mapped at all.
func (ix *Index) Audit(ctx context.Context, ch Change, results []AuditResult) (AuditReport, error) {
	sel, err := ix.Select(ctx, ch)
	if err != nil {
		return AuditReport{}, err
	}
	rep := AuditReport{Scope: sel.Scope, Base: sel.Base, Head: sel.Head, Selected: len(sel.Tests),
		Missed: []FailedTest{}, Unmapped: []FailedTest{}, Clean: true}
	for _, r := range results {
		rep.Failed += len(r.Failures)
	}
	plan, err := runner.Commands(sel, runner.Options{})
	if err != nil {
		return AuditReport{}, err
	}
	if sel.Scope == model.TierFull || plan.Scope == model.TierFull || rep.Failed == 0 {
		return rep, nil
	}

	rev := ""
	if !ch.Worktree && ch.Head != "" {
		rev = ch.Head
	}
	an, err := ix.analyzeRev(ctx, rev)
	if err != nil {
		return AuditReport{}, err
	}
	var known, projects []string
	for _, a := range an.Analyses {
		for _, u := range a.Tests {
			known = append(known, u.Files...)
			if !slices.Contains(projects, u.Project) {
				projects = append(projects, u.Project)
			}
		}
	}
	slices.Sort(known)
	known = slices.Compact(known)

	covered := func(f string) bool {
		for _, t := range sel.Tests {
			if slices.ContainsFunc(t.Files, func(tf string) bool { return eco.Within(f, tf) }) || (t.Whole && eco.Within(f, t.Project)) {
				return true
			}
		}
		return false
	}
	why := fmt.Sprintf("not reached from any changed file (scope %s, %d changed, %d test units selected)", sel.Scope, len(sel.Changed), len(sel.Tests))
	if len(sel.Changed) == 0 {
		why = "no changed file: the selection is empty"
	}
	for _, r := range results {
		m := &audit.Mapper{Root: ix.repo.Root, Known: known, Projects: projects}
		if r, err := filepath.EvalSymlinks(ix.repo.Root); err == nil && r != ix.repo.Root {
			m.AltRoots = append(m.AltRoots, r)
		}
		if r.Source != "" {
			if abs, err := filepath.Abs(filepath.Dir(r.Source)); err == nil {
				m.ResultsDir = abs
			}
		}
		for _, f := range r.Failures {
			file, out := m.Map(f.Candidates)
			ft := FailedTest{File: file, Test: f.Test, Source: r.Source}
			switch out {
			case audit.Mapped:
				if covered(file) {
					continue
				}
				ft.Reason = why
				for _, n := range sel.Narrowed {
					if n.Unit == file {
						ft.Reason = "selected by a widening but removed by the dynamic layer: " + n.Reason
					}
				}
				rep.Missed = append(rep.Missed, ft)
			case audit.Ambiguous:
				ft.Reason = "test file is ambiguous (candidate matches several known test files)"
				rep.Unmapped = append(rep.Unmapped, ft)
			default:
				ft.Reason = "cannot map to a known test file (candidates: " + fmt.Sprint(f.Candidates) + ")"
				rep.Unmapped = append(rep.Unmapped, ft)
			}
		}
	}
	rep.Clean = len(rep.Missed) == 0
	return rep, nil
}

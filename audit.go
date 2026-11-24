package bgraph

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/audit"
	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/golang"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/runner"
)

// AuditResult is the parsed content of one test-results file (see internal/audit).
type AuditResult = audit.Result

// FailedTest is one failed test case together with the test file it was mapped to.
type FailedTest struct {
	File   string `json:"file,omitempty"` // repo-relative test file; empty for unmapped failures and Go packages
	Unit   string `json:"unit,omitempty"` // test unit, for failures mapped by package (Go: go:<dir>)
	Test   string `json:"test"`
	Source string `json:"source,omitempty"` // results file it came from
	Reason string `json:"reason"`           // why it is missed / unmapped
}

// AuditReport is the outcome of shadow-mode auditing (docs/cli.md, audit).
type AuditReport struct {
	Scope    Tier         `json:"scope"`
	Base     string       `json:"base,omitempty"`
	Head     string       `json:"head,omitempty"`
	Selected int          `json:"selected"` // number of selected test units (0 for scope full)
	Total    int          `json:"total"`    // test units at head (0 when unknown)
	Full     bool         `json:"full"`     // the selection runs the whole suite (scope full, or a runner falls back to it)
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
//
// A failure carrying a Go package (go test -json) maps to the Go unit whose import path equals it, not to a file: the
// package is the unit (one unit per path; several → ambiguous, none → unmapped).
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
	rep.Full = sel.Scope == model.TierFull || plan.Scope == model.TierFull
	rev := ""
	if !ch.Worktree && ch.Head != "" {
		rev = ch.Head
	}
	if n, ok := sel.IndexStats["units"]; ok {
		rep.Total = n
	} else if an, err := ix.analyzeRev(ctx, rev); err == nil {
		// A full selection stops before counting units; the count is informational, so a failure only leaves it 0.
		for _, a := range an.Analyses {
			rep.Total += len(a.Tests)
		}
	}
	if rep.Full || rep.Failed == 0 {
		return rep, nil
	}

	an, err := ix.analyzeRev(ctx, rev)
	if err != nil {
		return AuditReport{}, err
	}
	var known, projects []string
	byPkg := map[string][]model.TestUnit{} // Go import path → units
	for _, a := range an.Analyses {
		for _, u := range a.Tests {
			known = append(known, u.Files...)
			if !slices.Contains(projects, u.Project) {
				projects = append(projects, u.Project)
			}
			if u.Eco == golang.Name && u.Package != "" {
				byPkg[u.Package] = append(byPkg[u.Package], u)
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
			ft := FailedTest{Test: f.Test, Source: r.Source}
			if f.Package != "" {
				units := byPkg[f.Package]
				switch len(units) {
				case 1:
					ft.Unit = units[0].ID
					i := slices.IndexFunc(sel.Tests, func(t SelectedTest) bool { return t.Unit == units[0].ID })
					switch {
					case i < 0 && !slices.ContainsFunc(units[0].Files, covered):
						ft.Reason = why
						rep.Missed = append(rep.Missed, ft)
					case i >= 0 && sel.Tests[i].ClassFilter:
						// The Go layer runs only the kept top-level test functions of a narrowed package.
						top, _, _ := strings.Cut(f.Test, "/")
						if !slices.Contains(sel.Tests[i].Classes, top) {
							ft.Reason = "selected package, but its test function " + top + " was removed by the dynamic layer"
							for _, n := range sel.Narrowed {
								if n.Unit == units[0].ID+"#"+top {
									ft.Reason += ": " + n.Reason
								}
							}
							rep.Missed = append(rep.Missed, ft)
						}
					}
				case 0:
					ft.Reason = "no Go test package with import path " + f.Package
					rep.Unmapped = append(rep.Unmapped, ft)
				default:
					ft.Reason = "Go import path " + f.Package + " names several test packages"
					rep.Unmapped = append(rep.Unmapped, ft)
				}
				continue
			}
			file, out := m.Map(f.Candidates)
			ft.File = file
			switch out {
			case audit.Mapped:
				if covered(file) {
					continue
				}
				ft.Reason = why
				for _, n := range sel.Narrowed {
					if n.Unit == file {
						ft.Reason = "selected but removed by the dynamic layer: " + n.Reason
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

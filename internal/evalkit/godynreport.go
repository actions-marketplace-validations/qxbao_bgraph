package evalkit

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Reports of the Go layer evaluation (t2godyn.go).

func goMedInt(v []int) string {
	if len(v) == 0 {
		return "-"
	}
	f := make([]float64, len(v))
	for i, x := range v {
		f[i] = float64(x)
	}
	return fmt.Sprintf("%.1f", Percentile(f, 50))
}

func goPct(a, b int) string {
	if b == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f %%", 100*float64(a)/float64(b))
}

func goEsc(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

// GoDynT2Markdown renders the hybrid pass.
func GoDynT2Markdown(rs []GoDynT2Result) string {
	var b strings.Builder
	b.WriteString("# Go layer: hybrid T2\n\n")
	b.WriteString("Packages: selected Go package units (hybrid: those that still run a test function + compile-only ones). Tests: top-level " +
		"test functions that run (static: every function of a selected package). Violations: an oracle-red package missing, or a red " +
		"top-level test (whole-package rerun on the mutated tree) of a selected package filtered out.\n\n")
	b.WriteString("| repo | R | go | packages / tests at R | plain suite s (warm) | records complete / units | record s | killed | narrowed | refused | kept all | errors | median packages static → hybrid (run + compile-only) | median tests static → hybrid | Σ tests static → hybrid | violations | red pkgs not reproduced |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	var details strings.Builder
	for _, r := range rs {
		narrowed, refused, keptAll, errs, viol, notRep := 0, 0, 0, 0, 0, 0
		var sp, hp, hc, stt, hyt []int
		sumS, sumH := 0, 0
		reasons, kept := map[string]int{}, map[string]int{}
		for _, m := range r.Mutants {
			if m.Error != "" {
				errs++
				continue
			}
			viol += len(m.Hybrid.Missed)
			notRep += len(m.NotReproduced)
			sp, hp, hc = append(sp, m.Static.Packages), append(hp, m.Hybrid.RunPackages), append(hc, m.Hybrid.CompileOnly)
			stt, hyt = append(stt, m.Static.Tests), append(hyt, m.Hybrid.Tests)
			sumS += m.Static.Tests
			sumH += m.Hybrid.Tests
			switch {
			case m.Hybrid.Skip != "":
				refused++
				reasons[RbReasonClass(m.Hybrid.Skip)]++
			case len(m.Hybrid.Removed) > 0:
				narrowed++
			default:
				keptAll++
			}
			for k, n := range m.Hybrid.Kept {
				kept[k] += n
			}
		}
		rec := "-"
		if s := r.Record.Summary; s != nil {
			rec = fmt.Sprintf("%d / %d", s.Complete, s.Units)
		} else if r.Record.Reused {
			rec = fmt.Sprintf("reused (%d usable of %d)", r.Status.Usable, r.Status.Records)
		}
		plain := "-"
		if len(r.PlainMs) > 0 {
			plain = fmt.Sprintf("%.1f", float64(r.PlainMs[len(r.PlainMs)-1])/1000)
		}
		red := 0
		if sumS > 0 {
			red = 100 - 100*sumH/sumS
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %d / %d | %s | %s | %d | %d | %d | %d | %d | %d | %s → %s + %s | %s → %s | %d → %d (−%d %%) | **%d** | %d |\n", r.Repo, rbShort(r.R),
			goVersionOf(r.Go), r.Packages, r.Tests, plain, rec, r.Record.Ms/1000, len(r.Mutants)-errs, narrowed, refused, keptAll, errs,
			goMedInt(sp), goMedInt(hp), goMedInt(hc), goMedInt(stt), goMedInt(hyt), sumS, sumH, red, viol, notRep)
		fmt.Fprintf(&details, "\n## %s\n\n", r.Repo)
		if r.Error != "" {
			fmt.Fprintf(&details, "Error: %s\n\n", r.Error)
		}
		if s := r.Record.Summary; s != nil {
			fmt.Fprintf(&details, "Record: %d records (packages and functions), %d complete and passed, %d incomplete, %d failed, %s (plain suite %v ms); incomplete reasons: %s; warnings: %s\n\n",
				s.Units, s.Complete, s.Incomplete, s.Failed, time.Duration(r.Record.Ms)*time.Millisecond, r.PlainMs, rbHist(s.Reasons), goEsc(strings.Join(s.Warnings, "; ")))
		}
		if len(r.PlainFail) > 0 {
			var pf []string
			for u, f := range r.PlainFail {
				pf = append(pf, u+": "+strings.Join(f, " "))
			}
			sort.Strings(pf)
			fmt.Fprintf(&details, "Failing in the plain suite at R: %s\n\n", goEsc(strings.Join(pf, "; ")))
		}
		fmt.Fprintf(&details, "Refusals: %s\n\nUnit notes (hybrid): %s\n\n", rbHist(reasons), rbHist(kept))
		details.WriteString("| mutant | file:line | op | red packages | red top-level tests | static pkgs / tests | hybrid run + compile-only pkgs / tests | removed | violations | skipped / error |\n|---|---|---|---|---|---|---|---|---|---|\n")
		for _, m := range r.Mutants {
			var fns []string
			for u, f := range m.RedFns {
				fns = append(fns, strings.TrimPrefix(u, "go:")+"#"+strings.Join(f, ","))
			}
			sort.Strings(fns)
			if len(m.NotReproduced) > 0 {
				fns = append(fns, "not reproduced: "+strings.Join(m.NotReproduced, ","))
			}
			fmt.Fprintf(&details, "| %s | %s:%d | %s | %s | %s | %d / %d | %d + %d / %d | %d | %s | %s |\n", m.ID, m.File, m.Line, m.Op, strings.Join(m.Red, " "),
				goEsc(firstN(strings.Join(fns, " "), 300)), m.Static.Packages, m.Static.Tests, m.Hybrid.RunPackages, m.Hybrid.CompileOnly, m.Hybrid.Tests,
				len(m.Hybrid.Removed), strings.Join(m.Hybrid.Missed, " "), goEsc(firstN(m.Hybrid.Skip+m.Error, 160)))
		}
	}
	return b.String() + details.String()
}

// goVersionOf names the go executable of a run: the toolchain version of a module-cache toolchain, else "go on PATH".
func goVersionOf(exe string) string {
	_, v, ok := strings.Cut(exe, "toolchain@v0.0.1-")
	if !ok {
		return "go on PATH"
	}
	v, _, _ = strings.Cut(v, "/") // go1.26.0.windows-amd64/bin/go.exe -> go1.26.0
	v, _, _ = strings.Cut(v, `\`)
	if j := strings.LastIndex(v, "."); j >= 0 && strings.Contains(v[j:], "-") {
		v = v[:j]
	}
	return v
}

// GoGuardMarkdown renders the guard rate.
func GoGuardMarkdown(rs []GoGuardResult) string {
	var b strings.Builder
	b.WriteString("# Go layer: guard over first-parent commits (real guard, no records)\n\n")
	b.WriteString("Inactive: no narrowable Go package selected (the layer does not run: documentation-only changes, or nothing selected). " +
		"Accepted: every guard passes (selection guard and body guard) and the layer stops at \"no records\". Code commits = examined − inactive.\n\n")
	b.WriteString("| repo | head | commits | inactive | refused | accepted | of which Go sources / test files only / docs only | accepted / code commits |\n|---|---|---|---|---|---|---|---|\n")
	var details strings.Builder
	for _, r := range rs {
		cnt, buckets, reasons := map[string]int{}, map[string]int{}, map[string]int{}
		for _, c := range r.Commits {
			cnt[c.Stage]++
			buckets[GoGuardBucket(c)]++
			if c.Stage == "refused" || c.Stage == "error" {
				reasons[RbReasonClass(c.Reason)]++
			}
		}
		code := len(r.Commits) - cnt["inactive"]
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d | %d / %d / %d | %s |\n", r.Repo, rbShort(r.Head), r.Examined, cnt["inactive"], cnt["refused"]+cnt["error"],
			cnt["accepted"], buckets["accepted: Go sources"], buckets["accepted: test files only"], buckets["accepted: documentation only"], goPct(cnt["accepted"], code))
		fmt.Fprintf(&details, "\n## %s\n\nBuckets: %s\n\nRefusal reasons (classes): %s\n", r.Repo, rbHist(buckets), rbHist(reasons))
	}
	return b.String() + details.String()
}

// GoReplayMarkdown renders the replay of one repo.
func GoReplayMarkdown(repo string, rs []GoReplayCommit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Go layer: replay of %s\n\n", repo)
	win, acc, accWin, replayed, removed, misses, failC, noVerd, assertFail, errs := 0, 0, 0, 0, 0, 0, 0, 0, 0, 0
	recUnits, recComplete := 0, 0
	sumSW, sumHW, sumSA, sumHA := 0, 0, 0, 0
	var recMs []int
	reasons := map[string]int{}
	for _, c := range rs {
		if c.Error != "" {
			errs++
		}
		if c.Window {
			win++
			sumSW += c.Static.Tests
			sumHW += c.Hybrid.Tests
		}
		if c.Stage == "accepted" {
			acc++
			if c.Window {
				accWin++
			}
			if c.Error == "" {
				replayed++
				sumSA += c.Static.Tests
				sumHA += c.Hybrid.Tests
			}
			if s := c.Record.Summary; s != nil {
				recUnits += s.Units
				recComplete += s.Complete
				recMs = append(recMs, int(c.Record.Ms))
				for k, n := range s.Reasons {
					reasons[k] += n
				}
			}
		} else if c.SameAs != "" && c.SameAs != "yes" {
			assertFail++
		}
		removed += len(c.Hybrid.Removed)
		misses += len(c.Misses)
		failC += len(c.FailAtC)
		noVerd += len(c.NoVerdct)
	}
	fmt.Fprintf(&b, "| commits | in the recent window | guard-accepted (in window) | replayed with records | removed functions | removed failing at C | of which misses | no verdict at C | hybrid ≠ static where not accepted | errors | complete records | median record s | tests static → hybrid (window) | tests static → hybrid (accepted, replayed) |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	fmt.Fprintf(&b, "| %d | %d | %d (%d) | %d | %d | %d | **%d** | %d | **%d** | %d | %d / %d (%s) | %s | %d → %d (−%s) | %d → %d (−%s) |\n\n", len(rs), win, acc, accWin, replayed,
		removed, failC, misses, noVerd, assertFail, errs, recComplete, recUnits, goPct(recComplete, recUnits), goMedInt(divInts(recMs, 1000)),
		sumSW, sumHW, goPct(sumSW-sumHW, sumSW), sumSA, sumHA, goPct(sumSA-sumHA, sumSA))
	fmt.Fprintf(&b, "Incomplete reasons over the replay records: %s\n\n", rbHist(reasons))
	b.WriteString("| commit | window | stage | files | static pkgs / tests | hybrid run + compile-only / tests | removed | record complete / units, s | fail at C | at P | misses | same as static / reason / error |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, c := range rs {
		rec := "-"
		if s := c.Record.Summary; s != nil {
			rec = fmt.Sprintf("%d / %d, %d", s.Complete, s.Units, c.Record.Ms/1000)
		}
		note := c.SameAs
		if c.Stage != "accepted" {
			note += " (" + RbReasonClass(c.Reason) + ")"
		}
		if c.Error != "" {
			note += " ERROR " + c.Error
		}
		fmt.Fprintf(&b, "| %s | %v | %s | %d | %d / %d | %d + %d / %d | %d | %s | %s | %s | %s | %s |\n", rbShort(c.C), c.Window, c.Stage, len(c.Files),
			c.Static.Packages, c.Static.Tests, c.Hybrid.RunPackages, c.Hybrid.CompileOnly, c.Hybrid.Tests, len(c.Hybrid.Removed), rec,
			strings.Join(c.FailAtC, " "), strings.Join(c.FailAtP, " "), strings.Join(c.Misses, " "), goEsc(firstN(note, 160)))
	}
	return b.String()
}

func divInts(v []int, d int) []int {
	out := make([]int, len(v))
	for i, x := range v {
		out[i] = x / d
	}
	return out
}

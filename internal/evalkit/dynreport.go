package evalkit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Reports of the Phase 6 evaluation (hybrid T2 and replay).

var skipPathRe = regexp.MustCompile(`^python: [^ ]+: `)

// SkipClass reduces a dynamic_skipped detail to a class for histograms.
func SkipClass(d string) string {
	if d == "" {
		return ""
	}
	first := strings.Split(d, "; ")[0]
	why := skipPathRe.ReplaceAllString(first, "")
	why = strings.TrimPrefix(why, "python: ")
	switch {
	case strings.HasPrefix(why, "status A"):
		return "file added"
	case strings.HasPrefix(why, "status D"):
		return "file deleted"
	case strings.HasPrefix(why, "status T"):
		return "type change"
	case strings.HasPrefix(why, "mode"):
		return "mode change"
	case strings.HasPrefix(why, "not a python source"):
		return "non-.py file changed"
	case strings.HasPrefix(why, "role"):
		return "changed .py is not source/test"
	case strings.HasPrefix(why, "claimed"):
		return "manifest/config/lockfile changed"
	case strings.HasPrefix(why, "named by a bgraph.toml"):
		return "declared file changed"
	case strings.HasPrefix(why, "widening"), strings.HasPrefix(why, "the selection is full"):
		return "global/full widening"
	case strings.HasPrefix(why, "no records"):
		return "no records"
	case strings.HasPrefix(why, "environment fingerprint"):
		return "interpreter"
	}
	return why
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func histo(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// T2DynMarkdown renders the hybrid T2 pass.
func T2DynMarkdown(rs []DynRepoResult, modes []string) string {
	var b strings.Builder
	b.WriteString("## Records at R\n\n| repo | mode | units | complete+pass | incomplete | failed | record time | incomplete reasons |\n|---|---|---|---|---|---|---|---|\n")
	for _, r := range rs {
		for _, rec := range r.Records {
			if rec.Error != "" {
				fmt.Fprintf(&b, "| %s | %s | error: %s |||||\n", r.Repo, rec.Mode, rec.Error)
				continue
			}
			units, comp, inc, fail, reasons := rec.Status.TestUnits, rec.Status.Usable, 0, 0, rec.Status.Reasons
			tm := "(reused)"
			if s := rec.Summary; s != nil {
				units, comp, inc, fail, reasons = s.Units, s.Complete, s.Incomplete, s.Failed, s.Reasons
				tm = fmt.Sprintf("%.1f s", float64(rec.RecordMs)/1000)
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %d (%.0f%%) | %d | %d | %s | %s |\n", r.Repo, rec.Mode, units, comp, 100*ratio(comp, units), inc, fail, tm, histo(reasons))
		}
	}
	b.WriteString("\n## Hybrid selection vs the mutation oracle\n\nsel = selected units / units (median). \"widened\" = mutants whose static selection has at least one widened unit.\n\n")
	b.WriteString("| repo | mode | mutants (killed) | widened | static viol. | hybrid viol. | hybrid-only viol. | med sel static (all / widened) | med sel hybrid (all / widened) | narrowed mutants | units removed | guard skips | kept widened units |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	totalViol := 0
	for _, r := range rs {
		if r.Error != "" {
			fmt.Fprintf(&b, "| %s | | error: %s |\n", r.Repo, r.Error)
			continue
		}
		for _, mode := range modes {
			var n, killed, wid, sv, hv, ho, nar, removed int
			var sAll, sWid, hAll, hWid []float64
			skips, kept := map[string]int{}, map[string]int{}
			for _, c := range r.Checks {
				if c.Err != "" || c.Units == 0 {
					continue
				}
				h, ok := c.Hybrid[mode]
				if !ok {
					continue
				}
				n++
				if c.Status == "killed" {
					killed++
				}
				if len(c.Static.Missed) > 0 {
					sv++
				}
				if len(h.Missed) > 0 {
					hv++
				}
				if len(hybridOnly(c.Static, h)) > 0 {
					ho++
				}
				s, hy := ratio(c.Static.Selected, c.Units), ratio(h.Selected, c.Units)
				if c.Static.Scope == "full" {
					s = 1
				}
				if h.Scope == "full" {
					hy = 1
				}
				sAll, hAll = append(sAll, s), append(hAll, hy)
				if c.Widened > 0 {
					wid++
					sWid, hWid = append(sWid, s), append(hWid, hy)
					if cl := SkipClass(h.Skip); cl != "" {
						skips[cl]++
					}
				}
				if h.Narrowed > 0 {
					nar++
					removed += h.Narrowed
				}
				for k, v := range h.Kept {
					kept[k] += v
				}
			}
			totalViol += ho
			fmt.Fprintf(&b, "| %s | %s | %d (%d) | %d | %d | %d | %d | %s / %s | %s / %s | %d | %d | %s | %s |\n", r.Repo, mode, n, killed, wid, sv, hv, ho,
				med(sAll), med(sWid), med(hAll), med(hWid), nar, removed, histo(skips), histo(kept))
		}
	}
	fmt.Fprintf(&b, "\nViolations are counted in mutants. Hybrid-only violations (a red file the static selection covers but the hybrid one does not): **%d**\n", totalViol)
	for _, r := range rs {
		for _, c := range r.Checks {
			for mode, h := range c.Hybrid {
				if ho := hybridOnly(c.Static, h); len(ho) > 0 {
					fmt.Fprintf(&b, "- HYBRID VIOLATION %s %s (%s): missed %v\n", r.Repo, c.Mutant, mode, ho)
				}
			}
		}
	}
	return b.String()
}

// ReplayDynMarkdown renders the replay.
func ReplayDynMarkdown(rs []DynReplayResult, modes []string) string {
	var b strings.Builder
	b.WriteString("## Records at R0\n\n| repo | R0 | mode | units | complete+pass | incomplete | failed | record time | incomplete reasons |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rs {
		for _, rec := range r.Records {
			units, comp, inc, fail, reasons := rec.Status.TestUnits, rec.Status.Usable, 0, 0, rec.Status.Reasons
			tm := "(reused)"
			if s := rec.Summary; s != nil {
				units, comp, inc, fail, reasons = s.Units, s.Complete, s.Incomplete, s.Failed, s.Reasons
				tm = fmt.Sprintf("%.1f s", float64(rec.RecordMs)/1000)
			}
			if rec.Error != "" {
				tm = "error: " + rec.Error
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d | %d | %s | %s |\n", r.Repo, short(r.R0), rec.Mode, units, comp, inc, fail, tm, histo(reasons))
		}
	}
	b.WriteString("\n## Replay: static vs hybrid\n\nwidened commit = static selection not full with at least one widened unit. sel = selected / units, median over widened commits.\n\n")
	b.WriteString("| repo | mode | commits | static full | widened (rate) | med sel static | med sel hybrid | narrowed commits | units removed | guard skips (widened commits) | stale-kept commits | kept widened units | assert fail | verified | violations |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	totalAssert, totalViol := 0, 0
	for _, r := range rs {
		if r.Error != "" {
			fmt.Fprintf(&b, "| %s | | error: %s |\n", r.Repo, r.Error)
			continue
		}
		for _, mode := range modes {
			var n, full, wid, nar, removed, stale, af, verified, viol int
			var sw, hw []float64
			skips, kept := map[string]int{}, map[string]int{}
			for _, c := range r.Commits {
				if c.Err != "" {
					continue
				}
				h, ok := c.Hybrid[mode]
				if !ok {
					continue
				}
				n++
				if c.Static.Scope == "full" {
					full++
					continue
				}
				if slicesContains(c.AssertFail, mode) {
					af++
				}
				if c.Widened > 0 && c.Units > 0 {
					wid++
					sw = append(sw, ratio(c.Static.Selected, c.Units))
					hw = append(hw, ratio(h.Selected, c.Units))
					if cl := SkipClass(h.Skip); cl != "" {
						skips[cl]++
					}
					if h.Kept["stale record"] > 0 {
						stale++
					}
				}
				if h.Narrowed > 0 {
					nar++
					removed += h.Narrowed
				}
				for k, v := range h.Kept {
					kept[k] += v
				}
			}
			if mode == modes[len(modes)-1] {
				for _, c := range r.Commits {
					if c.Verify != nil && c.Verify.Err == "" {
						verified++
						viol += len(c.Verify.Violations)
					}
				}
			}
			totalAssert += af
			totalViol += viol
			vs := "-"
			if mode == modes[len(modes)-1] {
				vs = fmt.Sprint(verified)
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %d (%.0f%%) | %s | %s | %d | %d | %s | %d | %s | %d | %s | %d |\n", r.Repo, mode, n, full, wid, 100*ratio(wid, n-full),
				med(sw), med(hw), nar, removed, histo(skips), stale, histo(kept), af, vs, viol)
		}
	}
	fmt.Fprintf(&b, "\nAssertion failures (hybrid != static on a commit without widened units): **%d**. Violations in verified commits: **%d**.\n\n", totalAssert, totalViol)
	b.WriteString("### Verified commits\n\n| repo | commit | removed units | fail at C | of which fail at P | violations | error |\n|---|---|---|---|---|---|---|\n")
	for _, r := range rs {
		for _, c := range r.Commits {
			if v := c.Verify; v != nil {
				fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %v | %s |\n", r.Repo, short(c.C), len(v.Removed), len(v.FailC), len(v.FailP), v.Violations, v.Err)
			}
		}
	}
	return b.String()
}

// med is the median of v with 3 decimals, "-" when v is empty.
func med(v []float64) string {
	if len(v) == 0 {
		return "-"
	}
	return fmt.Sprintf("%.3f", Percentile(v, 50))
}

func slicesContains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

// WriteDynJSON writes v as indented JSON to dir/name.
func WriteDynJSON(dir, name string, v any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), b, 0o644)
}

// hybridOnly lists the red files the hybrid selection misses although the static one covers them.
func hybridOnly(static, hybrid DynSel) []string {
	var out []string
	for _, f := range hybrid.Missed {
		if !slicesContains(static.Missed, f) {
			out = append(out, f)
		}
	}
	return out
}

// HybridViolations counts the mutant checks with a hybrid-only violation.
func HybridViolations(rs []DynRepoResult) int {
	n := 0
	for _, r := range rs {
		for _, c := range r.Checks {
			for _, h := range c.Hybrid {
				if len(hybridOnly(c.Static, h)) > 0 {
					n++
				}
			}
		}
	}
	return n
}

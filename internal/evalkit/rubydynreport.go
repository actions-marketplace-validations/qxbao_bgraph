package evalkit

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Reports of the Ruby method layer evaluation (t2rubydyn.go).

func rbHist(m map[string]int) string {
	type kv struct {
		k string
		n int
	}
	var s []kv
	for k, n := range m {
		s = append(s, kv{k, n})
	}
	sort.Slice(s, func(i, j int) bool { return s[i].n > s[j].n || s[i].n == s[j].n && s[i].k < s[j].k })
	var b []string
	for _, x := range s {
		b = append(b, fmt.Sprintf("%d× %s", x.n, x.k))
	}
	return strings.ReplaceAll(strings.Join(b, "; "), "|", "\\|")
}

func rbShort(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// RbDynT2Markdown renders the hybrid pass.
func RbDynT2Markdown(rs []RbDynT2Result) string {
	var b strings.Builder
	b.WriteString("# Ruby method layer: hybrid T2\n\n")
	b.WriteString("| repo | R | records (complete+passed / units) | record s | killed mutants | narrowed | refused by a guard | kept all | errors | median selected/units static | hybrid | mean removed (all / narrowed) | violations (hybrid) | static misses |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	var details strings.Builder
	for _, r := range rs {
		narrowed, refused, keptAll, errs, viol, smiss, removed := 0, 0, 0, 0, 0, 0, 0
		var sv, hv []float64
		reasons, kept := map[string]int{}, map[string]int{}
		for _, m := range r.Mutants {
			if m.Error != "" {
				errs++
				continue
			}
			viol += len(m.Hybrid.Missed)
			smiss += len(m.Static.Missed)
			if m.Units > 0 {
				sv = append(sv, float64(m.Static.Selected)/float64(m.Units))
				hv = append(hv, float64(m.Hybrid.Selected)/float64(m.Units))
			}
			removed += len(m.Hybrid.Removed)
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
		n := len(r.Mutants) - errs
		rec := "-"
		if s := r.Record.Summary; s != nil {
			rec = fmt.Sprintf("%d/%d", s.Complete, s.Units)
		} else if r.Record.Reused {
			rec = fmt.Sprintf("reused (%d usable)", r.Status.Usable)
		}
		meanAll, meanN := "-", "-"
		if n > 0 {
			meanAll = fmt.Sprintf("%.1f", float64(removed)/float64(n))
		}
		if narrowed > 0 {
			meanN = fmt.Sprintf("%.1f", float64(removed)/float64(narrowed))
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d | %d | %d | %d | %s | %s | %s / %s | **%d** | %d |\n", r.Repo, rbShort(r.R), rec,
			r.Record.Ms/1000, n, narrowed, refused, keptAll, errs, med(sv), med(hv), meanAll, meanN, viol, smiss)
		fmt.Fprintf(&details, "\n## %s\n\n", r.Repo)
		if r.Error != "" {
			fmt.Fprintf(&details, "Error: %s\n\n", r.Error)
		}
		if s := r.Record.Summary; s != nil {
			fmt.Fprintf(&details, "Record: %d units, %d complete and passed, %d incomplete, %d failed, %s; incomplete reasons: %s\n\n", s.Units,
				s.Complete, s.Incomplete, s.Failed, time.Duration(r.Record.Ms)*time.Millisecond, rbHist(s.Reasons))
		}
		fmt.Fprintf(&details, "Refusals: %s\n\nKept units (notes): %s\n\n", rbHist(reasons), rbHist(kept))
		details.WriteString("| mutant | file:line | op | red | static | hybrid | removed | violations | skipped / error |\n|---|---|---|---|---|---|---|---|---|\n")
		for _, m := range r.Mutants {
			fmt.Fprintf(&details, "| %s | %s:%d | %s | %d | %d | %d | %d | %s | %s |\n", m.ID, m.File, m.Line, m.Op, len(m.Red), m.Static.Selected,
				m.Hybrid.Selected, len(m.Hybrid.Removed), strings.Join(m.Hybrid.Missed, " "), strings.ReplaceAll(firstN(m.Hybrid.Skip+m.Error, 160), "|", "\\|"))
		}
	}
	return b.String() + details.String()
}

// RbGuardMarkdown renders the guard rate.
func RbGuardMarkdown(rs []RbGuardResult) string {
	var b strings.Builder
	b.WriteString("# Ruby method layer: guard over first-parent commits\n\n")
	b.WriteString("Candidates: commits whose changed files all end in .rb. Inactive: no narrowable Ruby unit selected (the layer does not run). " +
		"Accepted: the selection guard and the compile check pass.\n\n")
	b.WriteString("| repo | commits examined | .rb-only | inactive | guarded | accepted | refused by the guard | refused by the compile check | accepted / guarded |\n|---|---|---|---|---|---|---|---|---|\n")
	var details strings.Builder
	for _, r := range rs {
		cnt := map[string]int{}
		reasons := map[string]int{}
		for _, c := range r.Commits {
			cnt[c.Stage]++
			if c.Stage == "refused" || c.Stage == "parse" || c.Stage == "error" {
				reasons[c.Stage+": "+RbReasonClass(c.Reason)]++
			}
		}
		guarded := cnt["accepted"] + cnt["refused"] + cnt["parse"]
		share := "-"
		if guarded > 0 {
			share = fmt.Sprintf("%.2f", float64(cnt["accepted"])/float64(guarded))
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d | %d | %s |\n", r.Repo, r.Examined, len(r.Commits), cnt["inactive"], guarded,
			cnt["accepted"], cnt["refused"], cnt["parse"], share)
		fmt.Fprintf(&details, "\n## %s\n\n", r.Repo)
		if cnt["error"] > 0 {
			fmt.Fprintf(&details, "Errors: %d\n\n", cnt["error"])
		}
		type kv struct {
			k string
			n int
		}
		var s []kv
		for k, n := range reasons {
			s = append(s, kv{k, n})
		}
		sort.Slice(s, func(i, j int) bool { return s[i].n > s[j].n || s[i].n == s[j].n && s[i].k < s[j].k })
		for _, x := range s {
			fmt.Fprintf(&details, "- %d × %s\n", x.n, x.k)
		}
	}
	return b.String() + details.String()
}

// RbReplayMarkdown renders a replay.
func RbReplayMarkdown(repo string, cs []RbReplayCommit) string {
	var b strings.Builder
	narrowed, refused, removed, misses, errs := 0, 0, 0, 0, 0
	var sv, hv []float64
	for _, c := range cs {
		switch {
		case c.Error != "":
			errs++
		case c.Hybrid.Skip != "":
			refused++
		case len(c.Hybrid.Removed) > 0:
			narrowed++
		}
		removed += len(c.Hybrid.Removed)
		misses += len(c.Misses)
		if c.Units > 0 && c.Error == "" {
			sv = append(sv, float64(c.Static.Selected)/float64(c.Units))
			hv = append(hv, float64(c.Hybrid.Selected)/float64(c.Units))
		}
	}
	fmt.Fprintf(&b, "%s: %d commits replayed, %d narrowed, %d refused at selection, %d errors; %d removed test files run at their commit, "+
		"misses %d; median selected/units static %s, hybrid %s\n\n", repo, len(cs), narrowed, refused, errs, removed, misses, med(sv), med(hv))
	b.WriteString("| C | files | static / units | hybrid | removed | red at C | red at P too | misses | record (complete/units, s) | skipped / error |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for _, c := range cs {
		rec := "-"
		if s := c.Record.Summary; s != nil {
			rec = fmt.Sprintf("%d/%d, %d", s.Complete, s.Units, c.Record.Ms/1000)
		}
		fmt.Fprintf(&b, "| %s | %d | %d/%d | %d | %d | %d | %d | %d | %s | %s |\n", rbShort(c.C), len(c.Files), c.Static.Selected, c.Units,
			c.Hybrid.Selected, len(c.Hybrid.Removed), len(c.FailAtC), len(c.FailAtP), len(c.Misses), rec,
			strings.ReplaceAll(firstN(c.Hybrid.Skip+c.Error, 160), "|", "\\|"))
	}
	return b.String()
}

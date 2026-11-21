package evalkit

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WritePyReport writes t1-python.md next to t1.json: per repo and per kind tables, the oracle counts, the violation details and the
// selectivity breakdown by widening reason (the evidence for "is the dynamic layer needed early for Python").
func WritePyReport(dir string, rs []T1Result) error {
	var md bytes.Buffer
	fmt.Fprintf(&md, "# T1 Python: per repo, per kind, selectivity breakdown\n\n")

	evaluated := func(r T1Result) bool {
		return r.Status == "ok" || r.Status == "full" || r.Status == "violation"
	}
	group := func(title string, key func(T1Result) string) {
		fmt.Fprintf(&md, "## %s\n\n| %s | tried | skipped | ok | full | violations | full rate | median sel/units (non-full) | p90 | 95%% bound |\n|---|---|---|---|---|---|---|---|---|---|\n", title, title)
		by := map[string][]T1Result{}
		var keys []string
		for _, r := range rs {
			k := key(r)
			if _, ok := by[k]; !ok {
				keys = append(keys, k)
			}
			by[k] = append(by[k], r)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := Summarize(by[k], "")
			ub := "-"
			if s.NonFullN > 0 && s.Violations == 0 {
				ub = fmt.Sprintf("%.1f%%", 300/float64(s.NonFullN))
			}
			fmt.Fprintf(&md, "| %s | %d | %d | %d | %d | %d | %.0f%% | %.3f | %.3f | %s |\n", k, s.Attempted, s.Attempted-s.Evaluated, s.OK, s.Full, s.Violations, s.FullRate*100, s.MedianRatio, s.P90Ratio, ub)
		}
		fmt.Fprintln(&md)
	}
	group("repo", func(r T1Result) string { return r.Repo })
	group("kind", func(r T1Result) string {
		if r.Kind == "" {
			return "(none)"
		}
		return r.Kind
	})
	group("role", func(r T1Result) string { return r.Role })

	// Oracle accounting.
	var names, kept, nf, noown, amb, tasks int
	for _, r := range rs {
		if r.Py == nil {
			continue
		}
		tasks++
		names += r.Py.Names
		kept += r.Py.Kept
		nf += r.Py.NotFound
		noown += r.Py.NoOwner
		amb += r.Py.Ambiguous
	}
	fmt.Fprintf(&md, "## Oracle\n\n%d tasks; FAIL_TO_PASS names %d; kept as requirements %d (ambiguous %d); skipped: not found %d, no owning unit %d\n\n", tasks, names, kept, amb, nf, noown)
	var noOwn []string
	for _, r := range rs {
		if r.Py != nil && len(r.Py.NoOwnerFor) > 0 {
			noOwn = append(noOwn, fmt.Sprintf("%s: %v", r.ID, r.Py.NoOwnerFor))
		}
	}
	if len(noOwn) > 0 {
		fmt.Fprintf(&md, "Files located but run by no unit (names dropped):\n\n")
		for i, l := range noOwn {
			if i >= 40 {
				fmt.Fprintf(&md, "- ... %d more\n", len(noOwn)-i)
				break
			}
			fmt.Fprintf(&md, "- %s\n", l)
		}
		fmt.Fprintln(&md)
	}

	// Selectivity breakdown over non-full evaluated tasks.
	var nonFull []T1Result
	for _, r := range rs {
		if evaluated(r) && r.Status != "full" && r.PyW != nil && r.Units > 0 {
			nonFull = append(nonFull, r)
		}
	}
	fmt.Fprintf(&md, "## Selectivity breakdown (non-full tasks: %d)\n\n", len(nonFull))
	breakdown(&md, "all non-full", nonFull)
	topDynFiles(&md, nonFull)
	byKind := map[string][]T1Result{}
	byRole := map[string][]T1Result{}
	for _, r := range nonFull {
		byKind[r.Kind] = append(byKind[r.Kind], r)
		byRole[r.Role] = append(byRole[r.Role], r)
	}
	for _, k := range sortedMapKeys(byKind) {
		breakdown(&md, "kind="+k, byKind[k])
	}
	for _, k := range sortedMapKeys(byRole) {
		breakdown(&md, "role="+k, byRole[k])
	}
	byRepo := map[string][]T1Result{}
	for _, r := range nonFull {
		byRepo[r.Repo] = append(byRepo[r.Repo], r)
	}
	for _, k := range sortedMapKeys(byRepo) {
		breakdown(&md, "repo="+k, byRepo[k])
	}

	// Full reasons.
	fr := map[string]int{}
	for _, r := range rs {
		if r.Status != "full" {
			continue
		}
		for k := range r.Widenings {
			if strings.HasPrefix(k, "full:") {
				fr[k]++
			}
		}
	}
	fmt.Fprintf(&md, "## Full reasons (tasks)\n\n%s\n\n", mapStr(fr))

	// Violations.
	fmt.Fprintf(&md, "## Violations\n\n")
	n := 0
	for _, r := range rs {
		if r.Status != "violation" || r.Py == nil {
			continue
		}
		n++
		fmt.Fprintf(&md, "### %s (%s, %s) scope=%s selected %d/%d\n\n", r.ID, r.Repo, r.Role, r.Scope, r.Selected, r.Units)
		fmt.Fprintf(&md, "- changed (%d): %v\n- widenings: %v\n", len(r.Changed), r.Changed, r.Widenings)
		for _, v := range r.Py.Violations {
			fmt.Fprintf(&md, "- missing `%s`; F2P `%s`; located %v; class **%s**; reached changed %v (forward reach %d files)\n", v.Unit, v.Name, v.Files, v.Class, v.ReachedChanged, v.ReachSize)
			for _, u := range v.Unresolved {
				fmt.Fprintf(&md, "  - unresolved in reach: %s\n", u)
			}
		}
		fmt.Fprintln(&md)
	}
	if n == 0 {
		fmt.Fprintf(&md, "(none)\n")
	}
	return os.WriteFile(filepath.Join(dir, "t1-python.md"), md.Bytes(), 0o644)
}

func sortedMapKeys(m map[string][]T1Result) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// breakdown prints the widening-reason breakdown of a set of non-full tasks.
func breakdown(md *bytes.Buffer, title string, rs []T1Result) {
	if len(rs) == 0 {
		return
	}
	fmt.Fprintf(md, "### %s (%d tasks)\n\n", title, len(rs))
	cnt := func(f func(r T1Result) bool) int {
		n := 0
		for _, r := range rs {
			if f(r) {
				n++
			}
		}
		return n
	}
	hasW := func(prefix string) func(r T1Result) bool {
		return func(r T1Result) bool {
			for k := range r.PyW.WideCount {
				if strings.HasPrefix(k, prefix+"@") {
					return true
				}
			}
			return false
		}
	}
	fmt.Fprintf(md, "tasks with: dynamic_import %d (sys_path %d, nonconstant %d, static_prefix %d); unknown_import %d; ambiguous_import %d; uncertain_scan %d; unreferenced_file %d; unknown_file %d; test_config_changed %d; manifest_changed %d; lock_changed %d; whole unit selected %d; no widening at all %d\n\n",
		cnt(func(r T1Result) bool { return r.PyW.Dyn > 0 }),
		cnt(func(r T1Result) bool { return r.PyW.DynKinds["sys_path"] > 0 }),
		cnt(func(r T1Result) bool { return r.PyW.DynKinds["nonconstant"] > 0 }),
		cnt(func(r T1Result) bool { return r.PyW.DynKinds["static_prefix"] > 0 }),
		cnt(hasW("unknown_import")), cnt(hasW("ambiguous_import")), cnt(hasW("uncertain_scan")), cnt(hasW("unreferenced_file")),
		cnt(hasW("unknown_file")), cnt(hasW("test_config_changed")), cnt(hasW("manifest_changed")), cnt(hasW("lock_changed")),
		cnt(func(r T1Result) bool { return r.PyW.Whole > 0 }),
		cnt(func(r T1Result) bool { return len(r.PyW.WideCount) == 0 }))
	ratio := func(rs []T1Result, f func(r T1Result) int) (float64, float64) {
		var v []float64
		for _, r := range rs {
			v = append(v, float64(f(r))/float64(r.Units))
		}
		return Percentile(v, 50), Percentile(v, 90)
	}
	var noDyn, dyn, noWide, noUnresT []T1Result
	for _, r := range rs {
		if r.PyW.ProjectDynamic {
			dyn = append(dyn, r)
		} else {
			noDyn = append(noDyn, r)
		}
		if len(r.PyW.WideCount) == 0 {
			noWide = append(noWide, r)
		}
		if !r.PyW.ProjectUnresolved {
			noUnresT = append(noUnresT, r)
		}
	}
	fmt.Fprintf(md, "| subset | tasks | median sel/units | p90 | median selected |\n|---|---|---|---|---|\n")
	row := func(name string, sub []T1Result, f func(r T1Result) int) {
		if len(sub) == 0 {
			fmt.Fprintf(md, "| %s | 0 | - | - | - |\n", name)
			return
		}
		m, p := ratio(sub, f)
		var sv []float64
		for _, r := range sub {
			sv = append(sv, float64(f(r)))
		}
		fmt.Fprintf(md, "| %s | %d | %.3f | %.3f | %.0f |\n", name, len(sub), m, p, Percentile(sv, 50))
	}
	actual := func(r T1Result) int { return r.Selected }
	row("actual, all non-full", rs, actual)
	row("actual, tasks WITHOUT project-scope dynamic widening", noDyn, actual)
	row("actual, tasks WITH project-scope dynamic widening", dyn, actual)
	row("actual, tasks without any Unresolved-import seed", noUnresT, actual)
	row("actual, tasks with no widening of any kind", noWide, actual)
	row("counterfactual: dynamic seeds ignored, all tasks", rs, func(r T1Result) int { return r.PyW.SelNoDyn })
	row("counterfactual: sys.path-only dynamic seeds ignored", rs, func(r T1Result) int { return r.PyW.SelNoSysPath })
	row("counterfactual: non-constant-name-only dynamic seeds ignored", rs, func(r T1Result) int { return r.PyW.SelNoNonconst })
	row("counterfactual: all Unresolved seeds ignored", rs, func(r T1Result) int { return r.PyW.SelNoUnres })
	row("counterfactual: graph + Unresolved only (no scope widening, no whole units)", rs, func(r T1Result) int { return r.PyW.SelNoWiden })
	row("counterfactual: graph-proven units only", rs, func(r T1Result) int { return r.PyW.SelGraph })
	only := map[string]int{}
	for _, r := range rs {
		for k, v := range r.PyW.OnlyBy {
			only[k] += v
		}
	}
	fmt.Fprintf(md, "\nunits selected by exactly one source class (summed over tasks): %s\n\n", mapStr(only))
}

// topDynFiles lists the files whose dynamic_import widening fired in the most tasks.
func topDynFiles(md *bytes.Buffer, rs []T1Result) {
	type fc struct {
		f, cause string
		n        int
	}
	cnt := map[string]*fc{}
	var seeds []float64
	for _, r := range rs {
		seeds = append(seeds, float64(len(r.PyW.DynFiles)))
		for f, c := range r.PyW.DynFiles {
			if cnt[f] == nil {
				cnt[f] = &fc{f: f, cause: c}
			}
			cnt[f].n++
		}
	}
	var l []*fc
	for _, v := range cnt {
		l = append(l, v)
	}
	sort.Slice(l, func(i, j int) bool {
		if l[i].n != l[j].n {
			return l[i].n > l[j].n
		}
		return l[i].f < l[j].f
	})
	fmt.Fprintf(md, "Files seeded by dynamic_import per task: median %.0f, p90 %.0f, max %.0f; distinct files %d. Top seeded files (tasks, cause):\n\n", Percentile(seeds, 50), Percentile(seeds, 90), Percentile(seeds, 100), len(l))
	for i, v := range l {
		if i >= 15 {
			break
		}
		fmt.Fprintf(md, "- %s: %d tasks (%s)\n", v.f, v.n, v.cause)
	}
	fmt.Fprintln(md)
}

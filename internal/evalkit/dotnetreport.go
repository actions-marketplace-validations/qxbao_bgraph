package evalkit

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WriteDotnetReport writes dotnet.md next to t1.md: the .NET oracle statistics, the full widenings by reason per repo and every
// violation in detail (what a reviewer needs to classify it by hand).
func WriteDotnetReport(dir string, rs []T1Result) error {
	var md bytes.Buffer
	fmt.Fprintf(&md, "# .NET T1 detail\n\n")

	var di DotnetT1Info
	nt, whole := 0, 0
	for _, r := range rs {
		if r.Dotnet == nil {
			continue
		}
		nt++
		if r.Dotnet.Whole {
			whole++
		}
		di.Names += r.Dotnet.Names
		di.Kept += r.Dotnet.Kept
		di.Untouched += r.Dotnet.Untouched
		di.NotFound += r.Dotnet.NotFound
		di.NoOwner += r.Dotnet.NoOwner
		di.Ambiguous += r.Dotnet.Ambiguous
		di.AncestorsDiffer += r.Dotnet.AncestorsDiffer
	}
	fmt.Fprintf(&md, "## Oracle\n\n%d tasks reached the oracle (%d whole-suite lists, > %d names); FAIL_TO_PASS names %d; kept as requirements %d (ambiguous %d); "+
		"ancestors differ from the nearest project for %d kept names; dropped: untouched by the test patch (whole-suite lists) %d, not found %d, no owning project %d\n",
		nt, whole, dotnetWholeSuite, di.Names, di.Kept, di.Ambiguous, di.AncestorsDiffer, di.Untouched, di.NotFound, di.NoOwner)

	fmt.Fprintf(&md, "\n## Skipped / errors\n\n")
	n := 0
	for _, r := range rs {
		if strings.HasPrefix(r.Status, "skipped:") || strings.HasPrefix(r.Status, "error:") {
			n++
			d := r.Detail
			if len(d) > 240 {
				d = d[:240] + "..."
			}
			fmt.Fprintf(&md, "- `%s` %s: %s\n", r.ID, r.Status, strings.ReplaceAll(d, "\n", " "))
		}
	}
	if n == 0 {
		fmt.Fprintf(&md, "(none)\n")
	}

	fmt.Fprintf(&md, "\n## Per repo\n\n| repo | role | kind | tried | ok | full | violations | skipped | units | median sel | full reasons |\n|---|---|---|---|---|---|---|---|---|---|---|\n")
	by := map[string][]T1Result{}
	var names []string
	for _, r := range rs {
		if _, ok := by[r.Repo]; !ok {
			names = append(names, r.Repo)
		}
		by[r.Repo] = append(by[r.Repo], r)
	}
	sort.Strings(names)
	for _, nm := range names {
		var ok, full, vio, sk int
		var sel []float64
		units := 0
		reasons := map[string]int{}
		for _, r := range by[nm] {
			switch r.Status {
			case "ok":
				ok++
			case "full":
				full++
				for k := range r.Widenings {
					if strings.HasPrefix(k, "full:") {
						reasons[strings.TrimPrefix(k, "full:")]++
					}
				}
			case "violation":
				vio++
			default:
				sk++
				continue
			}
			if r.Status != "full" {
				sel = append(sel, float64(r.Selected))
			}
			units = r.Units
		}
		fmt.Fprintf(&md, "| %s | %s | %s | %d | %d | %d | %d | %d | %d | %.0f | %s |\n", nm, by[nm][0].Role, by[nm][0].Kind, len(by[nm]), ok, full, vio, sk, units, Percentile(sel, 50), mapStr(reasons))
	}

	fmt.Fprintf(&md, "\n## Violations (detail)\n\n")
	nv := 0
	for _, r := range rs {
		if r.Status != "violation" || r.Dotnet == nil {
			continue
		}
		nv++
		fmt.Fprintf(&md, "### `%s` (%s, %s) scope=%s, selected %d/%d\n\n- missing: %v\n- changed (%d): %v\n- widenings: %v\n", r.ID, r.Repo, r.Role, r.Scope, r.Selected, r.Units, r.Missing, len(r.Changed), r.Changed, r.Widenings)
		for _, v := range r.Dotnet.Violations {
			fmt.Fprintf(&md, "- unit `%s` (name `%.120s`, file %v): class **%s**, forward reach %d nodes, reached changed files %v\n", v.Unit, v.Name, v.Files, v.Class, v.ReachSize, v.ReachedChanged)
			if len(v.ReachedProjects) > 0 {
				fmt.Fprintf(&md, "  - referenced projects (transitive): %v\n", v.ReachedProjects)
			}
			for f, o := range v.ChangedOwners {
				fmt.Fprintf(&md, "  - changed `%s` owned by %v\n", f, o)
			}
		}
		md.WriteString("\n")
	}
	if nv == 0 {
		fmt.Fprintf(&md, "(none)\n")
	}
	return os.WriteFile(filepath.Join(dir, "dotnet.md"), md.Bytes(), 0o644)
}

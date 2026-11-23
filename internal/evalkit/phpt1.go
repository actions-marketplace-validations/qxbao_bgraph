package evalkit

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PHPRequirement records all PHPUnit project candidates for one failing test.
type PHPRequirement struct {
	PHPTestLoc
	Units      []string `json:"units,omitempty"`
	PatchUnits []string `json:"patch_units,omitempty"`
}

// PHPT1Info reports locator coverage and the requirements behind a T1 result.
type PHPT1Info struct {
	Names            int              `json:"names"`
	NotFound         int              `json:"not_found"`
	NoOwner          int              `json:"no_owner"`
	Untouched        int              `json:"untouched,omitempty"`
	Ambiguous        int              `json:"ambiguous"`
	Reqs             []PHPRequirement `json:"requirements"`
	PatchOnlyMissing []string         `json:"patch_only_missing,omitempty"`
}

func phpRequired(ctx context.Context, dir, p string, names []string, touched map[string]bool, kind string) ([]string, *PHPT1Info, error) {
	info := &PHPT1Info{Names: len(names)}
	gt, err := phpOracleTree(ctx, dir, p, names, true)
	if err != nil {
		return nil, info, err
	}
	defer gt.Close()
	locs, err := LocatePHPTests(gt, names, touched)
	if err != nil {
		return nil, info, err
	}
	var files []string
	for _, loc := range locs {
		files = append(files, loc.Files...)
	}
	owners, err := phpTestOwners(gt, uniqSorted(files))
	if err != nil {
		return nil, info, err
	}
	var units []string
	for _, loc := range locs {
		if len(loc.Files) == 0 {
			info.NotFound++
			continue
		}
		if kind == "n2p" && !loc.InPatch {
			info.Untouched++
			continue
		}
		if len(loc.Files) > 1 {
			info.Ambiguous++
		}
		req := PHPRequirement{PHPTestLoc: loc}
		for _, f := range loc.Files {
			if len(owners[f]) == 0 {
				info.NoOwner++
			}
			req.Units = append(req.Units, owners[f]...)
			if touched[f] {
				req.PatchUnits = append(req.PatchUnits, owners[f]...)
			}
		}
		req.Units = uniqSorted(req.Units)
		req.PatchUnits = uniqSorted(req.PatchUnits)
		info.Reqs = append(info.Reqs, req)
		units = append(units, req.Units...)
	}
	return uniqSorted(units), info, nil
}

func phpMissing(req []string, selected []SelUnit, info *PHPT1Info) []string {
	have := map[string]bool{}
	for _, s := range selected {
		have[s.Unit] = true
	}
	patchOnly := map[string]bool{}
	for _, r := range info.Reqs {
		units := r.Units
		if len(r.Files) > 1 && len(r.PatchUnits) > 0 {
			units = r.PatchUnits
		}
		for _, unit := range units {
			patchOnly[unit] = true
		}
	}
	var missing []string
	for _, unit := range req {
		if !have[unit] {
			missing = append(missing, unit)
			if patchOnly[unit] {
				info.PatchOnlyMissing = append(info.PatchOnlyMissing, unit)
			}
		}
	}
	return missing
}

// WritePHPReport reports per-repo selectivity and failing-test coverage.
func WritePHPReport(dir string, rs []T1Result) error {
	byRepo := map[string][]T1Result{}
	var repos []string
	oracle := PHPT1Info{}
	for _, r := range rs {
		if r.PHP != nil {
			oracle.Names += r.PHP.Names
			oracle.NotFound += r.PHP.NotFound
			oracle.NoOwner += r.PHP.NoOwner
			oracle.Ambiguous += r.PHP.Ambiguous
			oracle.Untouched += r.PHP.Untouched
		}
		if len(byRepo[r.Repo]) == 0 {
			repos = append(repos, r.Repo)
		}
		byRepo[r.Repo] = append(byRepo[r.Repo], r)
	}
	sort.Strings(repos)
	var md bytes.Buffer
	fmt.Fprintf(&md, "# PHP T1 detail\n\n")
	fmt.Fprintf(&md, "Names: %d. Not found: %d. Candidate files without a project: %d. Ambiguous: %d. Untouched n2p names: %d.\n\n",
		oracle.Names, oracle.NotFound, oracle.NoOwner, oracle.Ambiguous, oracle.Untouched)
	fmt.Fprint(&md, "| repo | role | kind | tried | ok | full | violations | skipped | errors | median selected/units | median select ms | full reasons |\n")
	fmt.Fprint(&md, "|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, repo := range repos {
		rows := byRepo[repo]
		summary := Summarize(rows, "")
		var times []float64
		for _, r := range rows {
			if r.Status == "ok" || r.Status == "full" || r.Status == "violation" {
				times = append(times, float64(r.Ms))
			}
		}
		fmt.Fprintf(&md, "| %s | %s | %s | %d | %d | %d | %d | %s | %d | %.3f | %.0f | %s |\n",
			repo, rows[0].Role, rows[0].Kind, len(rows), summary.OK, summary.Full, summary.Violations,
			skipStr(summary.Skipped), summary.Errors, summary.MedianRatio, Percentile(times, 50), mapStr(summary.FullReasons))
	}
	fmt.Fprint(&md, "\n## Violations, skips and errors\n\n")
	for _, r := range rs {
		switch {
		case r.Status == "violation":
			fmt.Fprintf(&md, "- `%s` missing %v, missing under patch-only policy %v, changed %v\n",
				r.ID, r.Missing, r.PHP.PatchOnlyMissing, r.Changed)
		case strings.HasPrefix(r.Status, "skipped:"), strings.HasPrefix(r.Status, "error:"):
			fmt.Fprintf(&md, "- `%s` %s %s\n", r.ID, r.Status, strings.ReplaceAll(r.Detail, "\n", " "))
		}
	}
	return os.WriteFile(filepath.Join(dir, "php.md"), md.Bytes(), 0o644)
}

// PHPRepoKind classifies non-library BugsPHP repositories for T1 reports.
func PHPRepoKind(repo string) string {
	switch strings.ToLower(repo) {
	case "cakephp/cakephp", "laravel/framework", "symfony/symfony":
		return "framework"
	case "composer/composer", "magento/magento2", "php-cs-fixer/php-cs-fixer":
		return "application"
	default:
		return "library"
	}
}

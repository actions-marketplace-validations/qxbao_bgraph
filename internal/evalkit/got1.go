package evalkit

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/golang"
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
)

// GoReq is the requirement derived from one FAIL_TO_PASS name: the units of the package directories of its candidate test files.
type GoReq struct {
	Name       string   `json:"name"`
	Form       string   `json:"form"`
	Files      []string `json:"files,omitempty"`
	Units      []string `json:"units"`
	PatchUnits []string `json:"patch_units,omitempty"` // units of the candidates the test patch touches
	Ambiguous  bool     `json:"ambiguous,omitempty"`
	Touched    bool     `json:"touched"`
}

// GoViolation is one required unit missing from the selection, with what a reviewer needs to classify it.
type GoViolation struct {
	Unit    string   `json:"unit"`
	Name    string   `json:"name"`
	Files   []string `json:"files"`
	Changed []string `json:"changed"`
	// PatchOnlyGone: the violation would not exist if ambiguous names required only the candidates the test patch touches.
	PatchOnlyGone bool `json:"patch_only_gone"`
	// Forward reach of the unit node at P.
	ReachedChanged []string `json:"reached_changed,omitempty"`
	ReachSize      int      `json:"forward_reach_size"`
	ReachedUnits   []string `json:"reached_units,omitempty"` // capped
	// ChangedPkgs: for each changed file, the package directory that owns it at P (nearest dir with a .go file), "" when none.
	ChangedPkgs map[string]string `json:"changed_pkgs,omitempty"`
	Class       string            `json:"class,omitempty"` // first pass: "real-miss?" (a path exists) | "no-path" (needs a reviewer)
}

// GoT1Info records how the Go oracle treated one task.
type GoT1Info struct {
	Kind       string        `json:"kind"`
	Names      int           `json:"names"`
	Kept       int           `json:"kept"`
	Untouched  int           `json:"untouched,omitempty"` // n2p: name in files the test patch does not touch
	NotFound   int           `json:"not_found,omitempty"` // no candidate (free-text names, logged text)
	Ginkgo     int           `json:"ginkgo,omitempty"`    // not found and the repo uses ginkgo/gocheck: skipped
	NoOwner    int           `json:"no_owner,omitempty"`  // candidate file in a directory that is not a bgraph unit
	Ambiguous  int           `json:"ambiguous,omitempty"`
	Reqs       []GoReq       `json:"reqs,omitempty"`
	Violations []GoViolation `json:"violations,omitempty"`
	// PatchOnlyMissing: required units the selection lacks under the "only the candidate touched by the test patch" policy.
	PatchOnlyMissing []string `json:"patch_only_missing,omitempty"`

	a     *eco.Analysis
	files []string
}

// goPrefetchWant: the blobs the Go analysis and the locator read, plus the sources the other ecosystems scan in mixed repos (without a
// batch prefetch a partial clone fetches them one by one).
func goPrefetchWant(f string) bool {
	b := path.Base(f)
	switch path.Ext(b) {
	case ".go", ".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx", ".vue", ".py", ".rs", ".java", ".kt", ".cs", ".toml":
		return true
	}
	switch b {
	case "go.mod", "go.work", "package.json", "pom.xml", "bgraph.toml":
		return true
	}
	return false
}

func goOracleTree(ctx context.Context, dir, p string) (*gitTree, error) {
	return newGitTreeWant(ctx, dir, p, goPrefetchWant)
}

// goRequired derives the required unit IDs of a task. dir is a repo directory holding commit p (base + test patch); tp is the set of
// files the test patch touches.
func goRequired(ctx context.Context, dir, p string, ids []string, tp map[string]bool, kind string) ([]string, *GoT1Info, error) {
	if kind == "" {
		kind = "f2p"
	}
	info := &GoT1Info{Kind: kind, Names: len(ids)}
	gt, err := goOracleTree(ctx, dir, p)
	if err != nil {
		return nil, info, err
	}
	defer gt.Close()
	a, err := golang.New(nil).Analyze(ctx, gt, scancache.New())
	if err != nil {
		return nil, info, err
	}
	info.a, info.files = a, gt.Files()
	hasUnit := map[string]bool{}
	for _, u := range a.Tests {
		hasUnit[u.ID] = true
	}
	set := map[string]bool{}
	for _, l := range LocateGoTests(gt, ids, tp) {
		if l.NotFound {
			info.NotFound++
			if l.Form == "ginkgo" || l.Form == "gocheck" {
				info.Ginkgo++
			}
			continue
		}
		if kind == "n2p" && !l.InPatch {
			info.Untouched++
			continue
		}
		req := GoReq{Name: l.Name, Form: l.Form, Files: l.Files, Ambiguous: l.Ambiguous, Touched: l.InPatch}
		for _, u := range l.Units {
			// A test file belongs to the unit of its own directory (testdata and other non-package files are never candidates).
			if d := strings.TrimPrefix(u, "go:"); hasUnit[golang.UnitID(d)] {
				req.Units = append(req.Units, golang.UnitID(d))
			}
		}
		for _, u := range l.PatchUnits {
			if d := strings.TrimPrefix(u, "go:"); hasUnit[golang.UnitID(d)] {
				req.PatchUnits = append(req.PatchUnits, golang.UnitID(d))
			}
		}
		if len(req.Units) == 0 {
			info.NoOwner++
			continue
		}
		info.Kept++
		if l.Ambiguous {
			info.Ambiguous++
		}
		for _, u := range req.Units {
			set[u] = true
		}
		info.Reqs = append(info.Reqs, req)
	}
	var out []string
	for u := range set {
		out = append(out, u)
	}
	sort.Strings(out)
	return out, info, nil
}

// goMissing returns the required unit IDs the selection lacks, records the violations (per name) in info and the units that would
// still be missing if ambiguous names only required the candidates touched by the test patch.
func goMissing(req []string, sel []SelUnit, info *GoT1Info, changed []string) []string {
	have := map[string]bool{}
	for _, u := range sel {
		have[u.Unit] = true
	}
	var miss []string
	for _, u := range req {
		if !have[u] {
			miss = append(miss, u)
		}
	}
	if info == nil {
		return miss
	}
	patchOnly := map[string]bool{}
	for _, r := range info.Reqs {
		us := r.Units
		if r.Ambiguous && len(r.PatchUnits) > 0 {
			us = r.PatchUnits
		}
		for _, u := range us {
			patchOnly[u] = true
		}
	}
	for _, u := range miss {
		if patchOnly[u] {
			info.PatchOnlyMissing = append(info.PatchOnlyMissing, u)
		}
	}
	for _, r := range info.Reqs {
		for _, u := range r.Units {
			if !have[u] {
				info.Violations = append(info.Violations, GoViolation{Unit: u, Name: r.Name, Files: r.Files, Changed: changed, PatchOnlyGone: !patchOnly[u]})
			}
		}
	}
	return miss
}

// goDiagnose fills the reach fields and Class of the violations from the Go graph at P.
func goDiagnose(info *GoT1Info) {
	if info == nil || len(info.Violations) == 0 || info.a == nil {
		return
	}
	a := info.a
	fwd := map[string][]string{}
	patsFrom := map[string][]*glob.Pattern{}
	for _, e := range a.Edges {
		if e.Kind == model.EdgePattern {
			if pt, err := glob.Compile(e.To); err == nil {
				patsFrom[e.From] = append(patsFrom[e.From], pt)
			}
			continue
		}
		fwd[e.From] = append(fwd[e.From], e.To)
	}
	pkgDirs := map[string]bool{}
	for _, f := range info.files {
		if strings.HasSuffix(f, ".go") {
			pkgDirs[path.Dir(f)] = true
		}
	}
	for i := range info.Violations {
		v := &info.Violations[i]
		d, _ := golang.ParseUnitID(v.Unit)
		start := golang.Node(d)
		seen := map[string]bool{start: true}
		q := []string{start}
		for len(q) > 0 {
			c := q[0]
			q = q[1:]
			next := fwd[c]
			for _, pt := range patsFrom[c] {
				for _, f := range info.files {
					if pt.Match(f) {
						next = append(next, f)
					}
				}
			}
			for _, n := range next {
				if !seen[n] {
					seen[n] = true
					q = append(q, n)
				}
			}
		}
		v.ReachSize = len(seen)
		for n := range seen {
			if strings.HasPrefix(n, "<go:") && n != start && len(v.ReachedUnits) < 60 {
				v.ReachedUnits = append(v.ReachedUnits, strings.TrimSuffix(strings.TrimPrefix(n, "<go:"), ">"))
			}
		}
		v.ChangedPkgs = map[string]string{}
		for _, c := range v.Changed {
			if seen[c] {
				v.ReachedChanged = append(v.ReachedChanged, c)
			}
			if len(v.ChangedPkgs) < 25 {
				pk := ""
				for dd := path.Dir(c); ; dd = path.Dir(dd) {
					if pkgDirs[dd] {
						pk = dd
						break
					}
					if dd == "." || dd == "/" || dd == "" {
						break
					}
				}
				v.ChangedPkgs[c] = pk
			}
		}
		sort.Strings(v.ReachedChanged)
		sort.Strings(v.ReachedUnits)
		v.Class = "no-path"
		if len(v.ReachedChanged) > 0 {
			v.Class = "real-miss?"
		}
	}
}

func goInfoDetail(i *GoT1Info) string {
	return fmt.Sprintf("names=%d not_found=%d (ginkgo/gocheck %d) untouched=%d no_owner=%d", i.Names, i.NotFound, i.Ginkgo, i.Untouched, i.NoOwner)
}

// WriteGoReport writes go.md next to t1.md: the Go oracle statistics, per-repo results, skipped tasks and every violation in detail.
func WriteGoReport(dir string, rs []T1Result) error {
	var md bytes.Buffer
	fmt.Fprintf(&md, "# Go T1 detail\n\n")
	var di GoT1Info
	nt := 0
	for _, r := range rs {
		if r.Go == nil {
			continue
		}
		nt++
		di.Names += r.Go.Names
		di.Kept += r.Go.Kept
		di.Untouched += r.Go.Untouched
		di.NotFound += r.Go.NotFound
		di.Ginkgo += r.Go.Ginkgo
		di.NoOwner += r.Go.NoOwner
		di.Ambiguous += r.Go.Ambiguous
	}
	fmt.Fprintf(&md, "## Oracle\n\n%d tasks reached the oracle; FAIL_TO_PASS names %d; kept as requirements %d (ambiguous %d); dropped: untouched by the test patch (n2p) %d, "+
		"not found %d (of which ginkgo/gocheck repos %d), no owning unit %d\n", nt, di.Names, di.Kept, di.Ambiguous, di.Untouched, di.NotFound, di.Ginkgo, di.NoOwner)

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

	fmt.Fprintf(&md, "\n## Per repo\n\n| repo | role | kind | tried | ok | full | violations | skipped | units | median sel | median select ms | full reasons |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
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
		var sel, ms []float64
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
			ms = append(ms, float64(r.Ms))
			units = r.Units
		}
		fmt.Fprintf(&md, "| %s | %s | %s | %d | %d | %d | %d | %d | %d | %.0f | %.0f | %s |\n", nm, by[nm][0].Role, by[nm][0].Kind, len(by[nm]), ok, full, vio, sk, units, Percentile(sel, 50), Percentile(ms, 50), mapStr(reasons))
	}

	fmt.Fprintf(&md, "\n## Violations (detail)\n\n")
	nv, gone := 0, 0
	for _, r := range rs {
		if r.Status != "violation" || r.Go == nil {
			continue
		}
		nv++
		if len(r.Go.PatchOnlyMissing) == 0 {
			gone++
		}
		fmt.Fprintf(&md, "### `%s` (%s, %s) scope=%s, selected %d/%d\n\n- missing: %v\n- still missing under the patch-only policy: %v\n- changed (%d): %v\n- widenings: %v\n", r.ID, r.Repo, r.Role, r.Scope, r.Selected, r.Units, r.Missing, r.Go.PatchOnlyMissing, len(r.Changed), r.Changed, r.Widenings)
		for _, v := range r.Go.Violations {
			fmt.Fprintf(&md, "- unit `%s` (name `%.120s`, files %v, vanishes under patch-only: %v): class **%s**, forward reach %d nodes, reached changed files %v, changed file packages %v\n", v.Unit, v.Name, v.Files, v.PatchOnlyGone, v.Class, v.ReachSize, v.ReachedChanged, v.ChangedPkgs)
		}
		md.WriteString("\n")
	}
	if nv == 0 {
		fmt.Fprintf(&md, "(none)\n")
	} else {
		fmt.Fprintf(&md, "Violating tasks that would be clean under the patch-only policy: %d of %d.\n", gone, nv)
	}
	return os.WriteFile(filepath.Join(dir, "go.md"), md.Bytes(), 0o644)
}

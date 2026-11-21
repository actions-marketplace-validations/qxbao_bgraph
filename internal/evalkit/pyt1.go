package evalkit

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco/python"
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
)

// PyReq is the requirement derived from one FAIL_TO_PASS name: the units that run the tests defined in its candidate files.
type PyReq struct {
	Name      string   `json:"name"`
	Files     []string `json:"files"` // candidate test files (all of them are required when the name is ambiguous)
	Units     []string `json:"units"`
	Ambiguous bool     `json:"ambiguous,omitempty"`
	Form      string   `json:"form,omitempty"`
}

// PyViolation is one required unit missing from the selection, with what a reviewer needs to classify it.
type PyViolation struct {
	Unit    string   `json:"unit"`
	Name    string   `json:"name"`
	Files   []string `json:"files"`
	Changed []string `json:"changed"`
	// ReachedChanged: changed files reachable from the unit's file along the graph's dependency edges at P (an import path from the
	// test to a changed file). Empty with a non-empty Changed means no static path exists (an oracle artefact, or a real miss through
	// a non-import channel such as a data file or an installed extension).
	ReachedChanged []string `json:"reached_changed,omitempty"`
	ReachSize      int      `json:"forward_reach_size"`
	// Unresolved: project-scope unresolved imports in the forward reach (they seed their own file when a changed file is in scope).
	Unresolved []string `json:"unresolved,omitempty"`
	Class      string   `json:"class,omitempty"` // first pass: "real-miss?" (an import path exists) | "no-path" (needs a reviewer)
}

// PyT1Info records how the Python oracle treated one task.
type PyT1Info struct {
	Names      int           `json:"names"`
	Kept       int           `json:"kept"`
	NotFound   int           `json:"not_found,omitempty"` // no candidate file: skipped
	NoOwner    int           `json:"no_owner,omitempty"`  // candidate file(s) run by no unit: skipped
	Ambiguous  int           `json:"ambiguous,omitempty"`
	Reqs       []PyReq       `json:"reqs,omitempty"`
	NoOwnerFor []string      `json:"no_owner_files,omitempty"`
	Violations []PyViolation `json:"violations,omitempty"`
}

// pyGitTree opens the P tree of a task with the blobs the Python analysis reads prefetched in one batch.
func pyGitTree(ctx context.Context, dir, p string) (*gitTree, error) {
	return newGitTreeWant(ctx, dir, p, func(f string) bool {
		switch path.Ext(f) {
		case ".py", ".pyi", ".toml", ".cfg", ".ini":
			return true
		}
		b := path.Base(f)
		return strings.HasPrefix(b, "requirements") && strings.HasSuffix(b, ".txt") || b == "Pipfile" || b == "MANIFEST.in"
	})
}

// pyRequired derives the required unit IDs of a task. dir is a repo directory holding commit p (base + test patch).
func pyRequired(ctx context.Context, dir, p string, ids []string, repo string) ([]string, *PyT1Info, error) {
	info := &PyT1Info{Names: len(ids)}
	gt, err := pyGitTree(ctx, dir, p)
	if err != nil {
		return nil, info, err
	}
	defer gt.Close()
	a, err := python.New(nil).Analyze(ctx, gt, scancache.New())
	if err != nil {
		return nil, info, err
	}
	byFile := map[string][]string{}
	var wholes []model.TestUnit
	for _, u := range a.Tests {
		if u.Whole {
			wholes = append(wholes, u)
			continue
		}
		for _, f := range u.Files {
			byFile[f] = append(byFile[f], u.ID)
		}
	}
	set := map[string]bool{}
	noOwn := map[string]bool{}
	for _, l := range LocatePythonTests(gt, repo, ids) {
		if l.NotFound {
			info.NotFound++
			continue
		}
		req := PyReq{Name: l.Name, Files: l.Files, Ambiguous: l.Ambiguous, Form: l.Form}
		us := map[string]bool{}
		for _, f := range l.Files {
			if uids := byFile[f]; len(uids) > 0 {
				for _, u := range uids {
					us[u] = true
				}
				continue
			}
			// No unit lists the file: a whole-project unit of a project containing it runs it.
			found := false
			for _, w := range wholes {
				if pyWithin(f, w.Project) {
					us[w.ID] = true
					found = true
				}
			}
			if !found {
				noOwn[f] = true
			}
		}
		for u := range us {
			req.Units = append(req.Units, u)
		}
		sort.Strings(req.Units)
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
	for f := range noOwn {
		info.NoOwnerFor = append(info.NoOwnerFor, f)
	}
	sort.Strings(info.NoOwnerFor)
	var out []string
	for u := range set {
		out = append(out, u)
	}
	sort.Strings(out)
	return out, info, nil
}

func pyWithin(f, dir string) bool {
	return dir == "" || dir == "." || f == dir || strings.HasPrefix(f, dir+"/")
}

// pyWholeDir returns the DIR of a "<py-all-tests:DIR>" unit ID.
func pyWholeDir(id string) (string, bool) {
	if strings.HasPrefix(id, "<py-all-tests:") && strings.HasSuffix(id, ">") {
		return strings.TrimSuffix(strings.TrimPrefix(id, "<py-all-tests:"), ">"), true
	}
	return "", false
}

// pyMissing returns the required unit IDs that the selection lacks and records the violations (per name) in info. A unit counts as
// selected when its ID is selected, or when a selected whole-project unit covers its file (SelUnit.Whole semantics, see CheckRequired).
func pyMissing(req []string, sel []SelUnit, info *PyT1Info, changed []string) []string {
	have := map[string]bool{}
	for _, u := range sel {
		have[u.Unit] = true
	}
	covered := func(id string) bool {
		if have[id] {
			return true
		}
		f := id
		if d, ok := pyWholeDir(id); ok {
			f = d
		}
		for _, u := range sel {
			if u.Whole && pyWithin(f, u.WholeProject) {
				return true
			}
		}
		return false
	}
	var miss []string
	for _, u := range req {
		if !covered(u) {
			miss = append(miss, u)
		}
	}
	if info != nil {
		for _, r := range info.Reqs {
			for _, u := range r.Units {
				if !covered(u) {
					info.Violations = append(info.Violations, PyViolation{Unit: u, Name: r.Name, Files: r.Files, Changed: changed})
				}
			}
		}
	}
	return miss
}

// pyDiagnose fills Reach*, Unresolved and Class of the violations from the Python graph at P.
func pyDiagnose(ctx context.Context, dir, p string, info *PyT1Info) {
	if info == nil || len(info.Violations) == 0 {
		return
	}
	gt, err := pyGitTree(ctx, dir, p)
	if err != nil {
		return
	}
	defer gt.Close()
	a, err := python.New(nil).Analyze(ctx, gt, scancache.New())
	if err != nil {
		return
	}
	fwd := map[string][]string{}
	type pe struct {
		from string
		pat  *glob.Pattern
	}
	var pats []pe
	for _, e := range a.Edges {
		if e.Kind == model.EdgePattern {
			if pt, err := glob.Compile(e.To); err == nil {
				pats = append(pats, pe{e.From, pt})
			}
			continue
		}
		fwd[e.From] = append(fwd[e.From], e.To)
	}
	for _, x := range pats {
		for _, f := range gt.Files() {
			if x.pat.Match(f) {
				fwd[x.from] = append(fwd[x.from], f)
			}
		}
	}
	unres := map[string][]model.Unresolved{}
	for _, u := range a.Unresolved {
		unres[u.File] = append(unres[u.File], u)
	}
	for i := range info.Violations {
		v := &info.Violations[i]
		start := v.Unit
		if d, ok := pyWholeDir(start); ok {
			start = d
		}
		seen := map[string]bool{start: true}
		q := []string{start}
		for len(q) > 0 {
			c := q[0]
			q = q[1:]
			for _, n := range fwd[c] {
				if !seen[n] {
					seen[n] = true
					q = append(q, n)
				}
			}
		}
		v.ReachSize = len(seen)
		for _, c := range v.Changed {
			if seen[c] {
				v.ReachedChanged = append(v.ReachedChanged, c)
			}
		}
		for f := range seen {
			for _, u := range unres[f] {
				if u.ScopeTier != model.TierNone && len(v.Unresolved) < 20 {
					v.Unresolved = append(v.Unresolved, fmt.Sprintf("%s: %s [%s] %s", f, u.Spec, u.Class, u.Detail))
				}
			}
		}
		sort.Strings(v.ReachedChanged)
		sort.Strings(v.Unresolved)
		v.Class = "no-path"
		if len(v.ReachedChanged) > 0 {
			v.Class = "real-miss?"
		}
	}
}

// PyWide explains why a Python selection is as big as it is. The sources of each selected unit come from its reason chains: a plain
// "changed" seed is graph-proven; "dynamic import,", "unknown import,", "ambiguous import," and "uncertain_scan import," seeds are
// Unresolved imports whose scope (the project) contained a changed file; "widen X (reason)" is a module/package widening;
// "config_is_code" is a whole-project unit.
type PyWide struct {
	Dyn, Unk, Amb, Unc int               `json:"-"`
	WideCount          map[string]int    `json:"widenings,omitempty"` // "reason@tier" -> widening count (not capped)
	DynKinds           map[string]int    `json:"dyn_kinds,omitempty"` // dynamic_import widenings by cause (sys_path, nonconstant, static_prefix)
	Sel                int               `json:"selected"`
	SelGraph           int               `json:"sel_graph"`             // units with at least one graph-proven ("changed" seed) reason
	SelNoDyn           int               `json:"sel_no_dynamic"`        // units that remain when dynamic_import seeds are ignored
	SelNoUnres         int               `json:"sel_no_unresolved"`     // ... when every Unresolved-import seed is ignored
	SelNoWiden         int               `json:"sel_no_scope_widening"` // ... when scope widenings and whole units are ignored too (graph + unresolved only)
	Whole              int               `json:"whole_units"`
	OnlyBy             map[string]int    `json:"only_by,omitempty"`   // units selected only because of one source class
	DynFiles           map[string]string `json:"dyn_files,omitempty"` // files seeded by a dynamic_import widening -> causes ("sys_path,nonconstant")
	SelNoSysPath       int               `json:"sel_no_sys_path"`     // units that remain when seeds whose only cause is a sys.path change are ignored
	SelNoNonconst      int               `json:"sel_no_nonconst"`     // ... when seeds whose only cause is a non-constant module name are ignored
	ProjectDynamic     bool              `json:"project_dynamic"`     // some dynamic_import widening fired (a project-scope Unresolved seeded a file)
	ProjectUnresolved  bool              `json:"project_unresolved"`
}

var pyWidenReason = regexp.MustCompile(`^widen .* \((\w+)\) due to `)

// pySource classifies one reason string of a selected unit.
func pySource(r string) string {
	switch {
	case strings.Contains(r, "(dynamic import, "):
		return "dynamic"
	case strings.Contains(r, "(unknown import, "):
		return "unknown"
	case strings.Contains(r, "(ambiguous import, "):
		return "ambiguous"
	case strings.Contains(r, "(uncertain_scan import, "):
		return "uncertain_scan"
	case strings.HasPrefix(r, "widen "):
		if m := pyWidenReason.FindStringSubmatch(r); m != nil {
			return "widen:" + m[1]
		}
		return "widen"
	case strings.HasPrefix(r, model.ReasonConfigIsCode+":"):
		return "whole"
	case r == "previously_failed":
		return "previously_failed"
	case strings.Contains(r, " ("):
		return "graph"
	}
	return "rule"
}

func pyIsUnres(s string) bool {
	return s == "dynamic" || s == "unknown" || s == "ambiguous" || s == "uncertain_scan"
}

func pySelStats(sel model.Selection) *PyWide {
	w := &PyWide{WideCount: map[string]int{}, DynKinds: map[string]int{}, OnlyBy: map[string]int{}, DynFiles: map[string]string{}}
	causes := map[string]map[string]bool{} // seeded file -> causes
	for _, x := range sel.Widening {
		w.WideCount[x.Reason+"@"+string(x.Tier)]++
		switch x.Reason {
		case model.ReasonDynamicImport:
			w.Dyn++
			switch {
			case strings.Contains(x.Detail, `"sys.path"`):
				w.DynKinds["sys_path"]++
				pyAddCause(causes, x.Path, "sys_path")
			case strings.Contains(x.Detail, "static prefix"):
				w.DynKinds["static_prefix"]++
				pyAddCause(causes, x.Path, "static_prefix")
			default:
				w.DynKinds["nonconstant"]++
				pyAddCause(causes, x.Path, "nonconstant")
			}
		case model.ReasonUnknownImport:
			w.Unk++
		case model.ReasonAmbiguousImport:
			w.Amb++
		case model.ReasonUncertainScan:
			w.Unc++
		}
	}
	for f, cs := range causes {
		var l []string
		for c := range cs {
			l = append(l, c)
		}
		sort.Strings(l)
		w.DynFiles[f] = strings.Join(l, ",")
	}
	only := func(f, c string) bool { cs := causes[f]; return len(cs) == 1 && cs[c] }
	w.ProjectDynamic = w.Dyn > 0
	w.ProjectUnresolved = w.Dyn+w.Unk+w.Amb+w.Unc > 0
	w.Sel = len(sel.Tests)
	for _, t := range sel.Tests {
		if t.Whole {
			w.Whole++
		}
		src := map[string]bool{}
		for _, r := range t.Reasons {
			src[pySource(r)] = true
		}
		var noDyn, noUnres, noWiden, noSys, noNon bool
		for _, r := range t.Reasons {
			s := pySource(r)
			if s != "dynamic" {
				noSys, noNon = true, true
				continue
			}
			f := pySeedFile(r)
			if !only(f, "sys_path") {
				noSys = true
			}
			if !only(f, "nonconstant") {
				noNon = true
			}
		}
		if noSys {
			w.SelNoSysPath++
		}
		if noNon {
			w.SelNoNonconst++
		}
		for s := range src {
			if s != "dynamic" {
				noDyn = true
			}
			if !pyIsUnres(s) {
				noUnres = true
			}
			if s == "graph" || pyIsUnres(s) {
				noWiden = true
			}
		}
		if src["graph"] {
			w.SelGraph++
		}
		if noDyn {
			w.SelNoDyn++
		}
		if noUnres {
			w.SelNoUnres++
		}
		if noWiden {
			w.SelNoWiden++
		}
		if len(src) == 1 {
			for s := range src {
				w.OnlyBy[s]++
			}
		}
	}
	return w
}

func pyAddCause(m map[string]map[string]bool, f, c string) {
	if m[f] == nil {
		m[f] = map[string]bool{}
	}
	m[f][c] = true
}

var pySeedRe = regexp.MustCompile(`(\S+) \((?:dynamic|unknown|ambiguous|uncertain_scan) import, `)

// pySeedFile returns the file whose Unresolved import seeded a reason chain ("" when the reason has none).
func pySeedFile(r string) string {
	if m := pySeedRe.FindStringSubmatch(r); m != nil {
		return m[1]
	}
	return ""
}

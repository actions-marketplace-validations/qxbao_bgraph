package evalkit

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/jvm"
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
)

// jvmWholeSuite: a FAIL_TO_PASS list longer than this, or one marked n2p, is a whole-suite list (every test of a module that did not
// build before runs); only names in files the test patch touches, or in a module that owns such a file, become requirements.
const jvmWholeSuite = 50

// JVMReq is the requirement derived from one FAIL_TO_PASS name: the units at the nearest module/project directory of every candidate
// test file (a Gradle task name: the units at the directories of its project).
type JVMReq struct {
	Name      string   `json:"name"`
	Files     []string `json:"files,omitempty"`
	Units     []string `json:"units"`
	Ancestors []string `json:"ancestors,omitempty"` // other units owning a candidate file by directory ancestry (informational)
	Ambiguous bool     `json:"ambiguous,omitempty"`
	Task      bool     `json:"task,omitempty"`
	Touched   bool     `json:"touched"` // some candidate file is touched by the test patch
}

// JVMViolation is one required unit missing from the selection, with what a reviewer needs to classify it.
type JVMViolation struct {
	Unit    string   `json:"unit"`
	Name    string   `json:"name"`
	Files   []string `json:"files"`
	Changed []string `json:"changed"`
	// ReachedChanged: changed files reachable from the unit's node along the dependency edges at P.
	ReachedChanged []string `json:"reached_changed,omitempty"`
	ReachSize      int      `json:"forward_reach_size"`
	// ReachedUnits: units in the forward reach (the transitive module/project dependencies), capped.
	ReachedUnits []string `json:"reached_units,omitempty"`
	// ChangedOwners: for each changed file, the units whose directory contains it at P, nearest first.
	ChangedOwners map[string][]string `json:"changed_owners,omitempty"`
	Touched       bool                `json:"touched"`
	Class         string              `json:"class,omitempty"` // first pass: "real-miss?" (a path exists) | "no-path" (needs a reviewer)
}

// JVMT1Info records how the JVM oracle treated one task.
type JVMT1Info struct {
	Names      int            `json:"names"`
	Kept       int            `json:"kept"`
	Untouched  int            `json:"untouched,omitempty"` // whole-suite list: name in a file/module the test patch does not touch
	NotFound   int            `json:"not_found,omitempty"`
	NoOwner    int            `json:"no_owner,omitempty"`   // candidate file(s) in no unit directory
	TaskBare   int            `json:"task_bare,omitempty"`  // Gradle task without a project path (whole build): skipped
	TaskNames  int            `json:"task_names,omitempty"` // Gradle task names mapped to project directories
	Ambiguous  int            `json:"ambiguous,omitempty"`
	Mixed      int            `json:"mixed_build,omitempty"` // kept names whose nearest directory holds both a Maven and a Gradle unit (all are required)
	Whole      bool           `json:"whole_suite,omitempty"`
	Reqs       []JVMReq       `json:"reqs,omitempty"`
	NoOwnerFor []string       `json:"no_owner_files,omitempty"`
	Violations []JVMViolation `json:"violations,omitempty"`

	a     *eco.Analysis
	files []string
}

// jvmGitTreeP opens the P tree of a task with the blobs the JVM analysis reads (POMs, Gradle scripts, catalogs) prefetched.
func jvmGitTreeP(ctx context.Context, dir, p string) (*gitTree, error) {
	return newGitTreeWant(ctx, dir, p, func(f string) bool {
		b := path.Base(f)
		switch {
		case b == "pom.xml", strings.HasSuffix(b, ".gradle"), strings.HasSuffix(b, ".gradle.kts"), strings.HasSuffix(b, ".toml"),
			b == "gradle.properties", strings.HasPrefix(f, ".mvn/"), strings.Contains(f, "/.mvn/"):
			return true
		}
		return false
	})
}

// jvmOwnerIndex maps a unit directory to the units there.
type jvmOwnerIndex map[string][]string

func newJVMOwnerIndex(a *eco.Analysis) jvmOwnerIndex {
	m := jvmOwnerIndex{}
	for _, u := range a.Tests {
		d := u.Project
		if d == "" {
			d = "."
		}
		m[d] = append(m[d], u.ID)
	}
	for _, v := range m {
		sort.Strings(v)
	}
	return m
}

// owners returns the units whose directory contains f, nearest directory first.
func (m jvmOwnerIndex) owners(f string) []string {
	var out []string
	for d := path.Dir(f); ; d = path.Dir(d) {
		out = append(out, m[d]...)
		if d == "." || d == "/" || d == "" {
			break
		}
	}
	return out
}

// nearest returns the units at the nearest directory that has any.
func (m jvmOwnerIndex) nearest(f string) []string {
	for d := path.Dir(f); ; d = path.Dir(d) {
		if us := m[d]; len(us) > 0 {
			return us
		}
		if d == "." || d == "/" || d == "" {
			return nil
		}
	}
}

func (m jvmOwnerIndex) unitsAt(dir string) []string { return m[strings.TrimSuffix(dir, "/")] }

// jvmRequired derives the required unit IDs of a task. dir is a repo directory holding commit p (base + test patch); tp is the set of
// files the test patch touches; n2p marks a whole-suite list by the dataset's f2p_kind.
func jvmRequired(ctx context.Context, dir, p string, ids []string, tp map[string]bool, n2p bool) ([]string, *JVMT1Info, error) {
	info := &JVMT1Info{Names: len(ids), Whole: n2p || len(ids) > jvmWholeSuite}
	gt, err := jvmGitTreeP(ctx, dir, p)
	if err != nil {
		return nil, info, err
	}
	defer gt.Close()
	a, err := jvm.New(nil).Analyze(ctx, gt, scancache.New())
	if err != nil {
		return nil, info, err
	}
	info.a, info.files = a, gt.Files()
	oi := newJVMOwnerIndex(a)
	touchedUnit := map[string]bool{} // units at the nearest directory of a file the test patch touches
	for f := range tp {
		for _, u := range oi.nearest(f) {
			touchedUnit[u] = true
		}
	}
	set := map[string]bool{}
	noOwn := map[string]bool{}
	for _, l := range LocateJVMTests(jvmGitTree{gt, ctx, dir}, ids) {
		req := JVMReq{Name: l.Name, Files: l.Files, Ambiguous: l.Ambiguous, Task: l.Task}
		var nearest, anc []string
		switch {
		case l.Task:
			if len(l.Dirs) == 0 {
				info.TaskBare++
				continue
			}
			for _, d := range l.Dirs {
				nearest = append(nearest, oi.unitsAt(d)...)
			}
			if len(nearest) == 0 {
				info.NoOwner++
				continue
			}
			info.TaskNames++
			req.Touched = true
		case l.NotFound:
			info.NotFound++
			continue
		default:
			for _, f := range l.Files {
				req.Touched = req.Touched || tp[f]
			}
			if info.Whole {
				keep := req.Touched
				for _, f := range l.Files {
					for _, u := range oi.nearest(f) {
						keep = keep || touchedUnit[u]
					}
				}
				if !keep {
					info.Untouched++
					continue
				}
			}
			for _, f := range l.Files {
				ns := oi.nearest(f)
				if len(ns) == 0 {
					noOwn[f] = true
					continue
				}
				nearest = append(nearest, ns...)
				anc = append(anc, oi.owners(f)...)
			}
			if len(nearest) == 0 {
				info.NoOwner++
				continue
			}
		}
		uniq := func(in []string) []string {
			m := map[string]bool{}
			var out []string
			for _, s := range in {
				if !m[s] {
					m[s] = true
					out = append(out, s)
				}
			}
			sort.Strings(out)
			return out
		}
		req.Units = uniq(nearest)
		isNearest := map[string]bool{}
		for _, u := range req.Units {
			isNearest[u] = true
		}
		for _, u := range uniq(anc) {
			if !isNearest[u] {
				req.Ancestors = append(req.Ancestors, u)
			}
		}
		kinds := map[string]bool{}
		for _, u := range req.Units {
			if k, _, _, ok := jvm.ParseUnitID(u); ok {
				kinds[k] = true
			}
		}
		if len(kinds) > 1 {
			info.Mixed++
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

// jvmMissing returns the required unit IDs the selection lacks and records the violations (per name) in info.
func jvmMissing(req []string, sel []SelUnit, info *JVMT1Info, changed []string) []string {
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
	if info != nil {
		for _, r := range info.Reqs {
			for _, u := range r.Units {
				if !have[u] {
					info.Violations = append(info.Violations, JVMViolation{Unit: u, Name: r.Name, Files: r.Files, Changed: changed, Touched: r.Touched})
				}
			}
		}
	}
	return miss
}

// jvmDiagnose fills the reach fields and Class of the violations from the JVM graph at P.
func jvmDiagnose(info *JVMT1Info) {
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
	oi := newJVMOwnerIndex(a)
	for i := range info.Violations {
		v := &info.Violations[i]
		start := jvm.Node(v.Unit)
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
			if strings.HasPrefix(n, "<jvm:") && n != start && len(v.ReachedUnits) < 60 {
				v.ReachedUnits = append(v.ReachedUnits, strings.TrimSuffix(strings.TrimPrefix(n, "<jvm:"), ">"))
			}
		}
		v.ChangedOwners = map[string][]string{}
		for _, c := range v.Changed {
			if seen[c] {
				v.ReachedChanged = append(v.ReachedChanged, c)
			}
			if len(v.ChangedOwners) < 25 {
				v.ChangedOwners[c] = oi.owners(c)
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

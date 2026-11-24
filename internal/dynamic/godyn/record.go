package godyn

import (
	"fmt"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// mapper turns absolute paths (forward slashes) into repo-relative paths of tree t.
type mapper struct {
	root string // forward slashes, no trailing slash
	real string // the root with symbolic links resolved, when it differs ("" otherwise)
	t    tree.Tree
}

func newMapper(root string, t tree.Tree) *mapper {
	m := &mapper{root: strings.TrimSuffix(filepath.ToSlash(root), "/"), t: t}
	if ev, err := filepath.EvalSymlinks(root); err == nil {
		if r := strings.TrimSuffix(filepath.ToSlash(ev), "/"); r != m.root {
			m.real = r
		}
	}
	return m
}

// rel returns abs relative to the repository root ("" when outside).
func (m *mapper) rel(abs string) string {
	abs = filepath.ToSlash(abs)
	for _, r := range []string{m.root, m.real} {
		if r == "" {
			continue
		}
		a := abs
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			r, a = strings.ToLower(r), strings.ToLower(a)
		}
		if strings.HasPrefix(a, r+"/") {
			return path.Clean(abs[len(r)+1:])
		}
	}
	return ""
}

// tracked returns the tracked spelling of repo-relative p.
func (m *mapper) tracked(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	if m.t.Has(p) {
		return p, true
	}
	if f := m.t.FoldLookup(p); len(f) == 1 {
		return f[0], true
	}
	return "", false
}

// DataFiles returns the files a package unit's tests may read as data, besides what its binary compiles: every tracked file
// under the package directory dir, recursively (testdata included; "." is the whole repository), and the targets of the unit's
// edges that PlainEdge rejects (fixture files, directory and embed patterns, resolved against t). A record depends on them so
// that a change between the recording and the base makes it stale.
func DataFiles(t tree.Tree, dir string, edges []model.Edge) []string {
	set := map[string]bool{}
	files := t.Files()
	for _, f := range files {
		if dir == "." || strings.HasPrefix(f, dir+"/") {
			set[f] = true
		}
	}
	for _, e := range edges {
		if PlainEdge(e) {
			continue
		}
		if e.Kind != model.EdgePattern {
			if t.Has(e.To) {
				set[e.To] = true
			}
			continue
		}
		pat, err := glob.Compile(e.To)
		if err != nil {
			continue
		}
		for _, f := range files {
			if pat.Match(f) {
				set[f] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	slices.Sort(out)
	return out
}

// goFilesIn returns the tracked non-test .go files directly in directory dir of t (any build constraint). Test files are left
// out: another package's test files never compile into a binary, and the unit's own are checked by its function-list record.
func goFilesIn(t tree.Tree, dir string) []string {
	var out []string
	prefix := dir + "/"
	if dir == "." {
		prefix = ""
	}
	for _, f := range t.Files() {
		if rest, ok := strings.CutPrefix(f, prefix); ok && !strings.Contains(rest, "/") && strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	return out
}

// RecordOf turns the outcome of a test function into its record. t is the working tree at the recorded commit (its keys are the
// blob OIDs of the content the test binary was built from); root is the repository root on disk; data are the unit's data files
// (DataFiles). Deps hold the compiled and embedded files, every .go file of the closure's package directories (Dirs) and the data
// files; those not compiled into the binary are also listed in Reads (a change to them cannot change what the binary runs).
func RecordOf(o Outcome, t tree.Tree, root, env, commit string, now time.Time, data []string) dynamic.Record {
	r := dynamic.Record{Eco: Eco, Unit: o.Unit, Collector: CollectorID, Commit: commit, Deps: map[string]string{}, Env: env,
		Complete: true, Recorded: now.UTC(), LineCov: map[string]string{}}
	var why []string
	switch {
	case o.TimedOut:
		why = append(why, "timeout")
	case o.Err != "":
		why = append(why, o.Err)
	case o.NoTests || !o.Ran:
		why = append(why, "the test function did not run (no tests to run)")
	case o.Counters == 0:
		why = append(why, "no counter file (the test process crashed or did not exit normally)")
	case !o.MainWrote:
		why = append(why, "the test process wrote no counter file")
	case o.Started < 0 && len(o.Incomplete) == 0:
		why = append(why, "cannot count the processes the test started (no coverage of the syscall package)")
	case o.Started > o.Counters-1:
		why = append(why, fmt.Sprintf("starts a process the collector does not observe (%d started, %d instrumented processes besides the test wrote coverage)",
			o.Started, o.Counters-1))
	case o.Skipped:
		// A skipped test ran only up to t.Skip; whether it skips depends on its environment, which the record cannot pin.
		why = append(why, "test skipped")
	}
	r.Pass = o.Exit == 0 && !o.TimedOut && o.Err == "" && o.Ran && !o.NoTests
	why = append(why, o.Incomplete...)
	for _, n := range o.Unmapped {
		why = append(why, "a covered file maps to no tracked repository file: "+n)
	}
	m := newMapper(root, t)
	compiled := map[string]bool{}
	if p := o.Pkg; p != nil {
		why = append(why, p.Outside...)
		for _, a := range p.Files {
			rel := m.rel(a)
			f, ok := m.tracked(rel)
			if !ok {
				if rel == "" {
					rel = a
				}
				why = append(why, "compiles untracked repository file "+rel)
				continue
			}
			if k := t.Key(f); k != "" {
				r.Deps[f] = k
				compiled[f] = true
			} else {
				why = append(why, "cannot hash "+f)
			}
		}
		extra := slices.Clone(data)
		for _, a := range p.Dirs {
			d := m.rel(a)
			if d == "" {
				if !within(a, slash(root)) {
					continue
				}
				d = "."
			}
			r.Dirs = append(r.Dirs, d)
			extra = append(extra, goFilesIn(t, d)...)
		}
		for _, f := range extra {
			if compiled[f] {
				continue
			}
			if k := t.Key(f); k != "" {
				r.Deps[f] = k
				r.Reads = append(r.Reads, f)
			} else {
				why = append(why, "cannot hash "+f)
			}
		}
		slices.Sort(r.Dirs)
		r.Dirs = slices.Compact(r.Dirs)
		slices.Sort(r.Reads)
		r.Reads = slices.Compact(r.Reads)
		canary := false
		for _, a := range p.TestFiles {
			if f, ok := m.tracked(m.rel(a)); ok {
				if _, in := r.Deps[f]; in {
					canary = true
				}
			}
		}
		if !canary {
			why = append(why, "canary: none of the package's own test files is among the dependencies")
		}
	} else {
		why = append(why, "no package")
	}
	for a, ls := range o.Lines {
		f, ok := m.tracked(m.rel(a))
		if !ok {
			why = append(why, "a covered file maps to no tracked repository file: "+a)
			continue
		}
		if !compiled[f] || path.Ext(f) != ".go" {
			if o.Pkg == nil || !o.Pkg.LineDirectives {
				why = append(why, "a covered file is not among the compiled files: "+f)
			}
			continue // a name a //line directive gave (it is not the compiled file's own)
		}
		r.LineCov[f] = ls.Format()
	}
	if len(r.LineCov) == 0 {
		r.LineCov = nil
	}
	if len(why) > 0 {
		r.Complete = false
		r.Incomplete = strings.Join(dedupe(why), "; ")
	}
	return r
}

// ListRecordOf turns the build of a package into its function-list record: Classes are the listed top-level Test, Example and
// Fuzz functions; Deps every tracked _test.go file directly in the package directory (any build constraint) and the unit's data
// files (DataFiles).
func ListRecordOf(p *Package, t tree.Tree, env, commit string, now time.Time, data []string) dynamic.Record {
	r := dynamic.Record{Eco: Eco, Unit: p.Unit.ID, Collector: CollectorID, Commit: commit, Deps: map[string]string{}, Env: env,
		Complete: true, Pass: p.Err == "", Recorded: now.UTC(), Classes: slices.Clone(p.Names)}
	var why []string
	if p.Err != "" {
		why = append(why, p.Err)
	} else if len(p.Names) == 0 {
		why = append(why, "no test function listed")
	}
	for _, f := range TestFiles(t, p.Unit.Dir) {
		if k := t.Key(f); k != "" {
			r.Deps[f] = k
		} else {
			why = append(why, "cannot hash "+f)
		}
	}
	if len(r.Deps) == 0 {
		why = append(why, "no tracked test file in the package directory")
	}
	for _, f := range data {
		if k := t.Key(f); k != "" {
			r.Deps[f] = k
		} else {
			why = append(why, "cannot hash "+f)
		}
	}
	if len(why) > 0 {
		r.Complete = false
		r.Incomplete = strings.Join(dedupe(why), "; ")
	}
	return r
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

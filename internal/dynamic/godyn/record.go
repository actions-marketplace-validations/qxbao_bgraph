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

// RecordOf turns the outcome of a test function into its record. t is the working tree at the recorded commit (its keys are the
// blob OIDs of the content the test binary was built from); root is the repository root on disk.
func RecordOf(o Outcome, t tree.Tree, root, env, commit string, now time.Time) dynamic.Record {
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
	}
	r.Pass = o.Exit == 0 && !o.TimedOut && o.Err == "" && o.Ran && !o.NoTests
	why = append(why, o.Incomplete...)
	for _, n := range o.Unmapped {
		why = append(why, "a covered file maps to no tracked repository file: "+n)
	}
	m := newMapper(root, t)
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
			} else {
				why = append(why, "cannot hash "+f)
			}
		}
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
		if _, in := r.Deps[f]; !in || path.Ext(f) != ".go" {
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
// Fuzz functions; Deps every tracked _test.go file directly in the package directory (any build constraint).
func ListRecordOf(p *Package, t tree.Tree, env, commit string, now time.Time) dynamic.Record {
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

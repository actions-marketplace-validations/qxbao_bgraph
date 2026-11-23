package dotnetdyn

import (
	"fmt"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/tree"
)

// Mapper maps the source file names of coverage reports to repository files.
type Mapper struct {
	Root string    // repository root (absolute)
	Tree tree.Tree // the recorded tree

	fold     map[string]string   // lower-case path -> tracked path (built on first use)
	projects map[string][]string // lower-case project name (file name without extension) -> project directories
}

type place int

const (
	placeTracked   place = iota // a tracked file of the tree
	placeGenerated              // under a bin/ or obj/ directory of the repository (generated sources)
	placeUntracked              // in the repository, not tracked, not generated
	placeOutside                // outside the repository (environment)
)

// rel returns the repo-relative path of a report file name and where it is. Deterministic builds write "/_/" for the
// repository root (PathMap).
func (m *Mapper) rel(name string) (string, place) {
	n := filepath.ToSlash(name)
	r, ok := strings.CutPrefix(n, "/_/")
	if ok && !m.Tree.Has(r) && m.lookupFold(r) == "" {
		if _, own := m.generatedProjects(r); !own {
			// Deterministic builds of other repositories (NuGet packages with sources) use the same path map: a "/_/" name is
			// a repository file only when the tree has it or it is generated under one of its projects.
			return n, placeOutside
		}
	}
	if !ok {
		root := strings.TrimSuffix(filepath.ToSlash(m.Root), "/") + "/"
		if len(n) <= len(root) || !(n[:len(root)] == root || runtime.GOOS == "windows" && strings.EqualFold(n[:len(root)], root)) {
			return n, placeOutside
		}
		r = n[len(root):]
	}
	if m.Tree.Has(r) {
		return r, placeTracked
	}
	if p := m.lookupFold(r); p != "" && runtime.GOOS == "windows" {
		// Report names may differ in case from the tracked path on a case-insensitive file system.
		return p, placeTracked
	}
	if buildOutput(r) {
		return r, placeGenerated
	}
	return r, placeUntracked
}

func buildOutput(r string) bool {
	q := "/" + strings.ToLower(r) + "/"
	return strings.Contains(q, "/obj/") || strings.Contains(q, "/bin/")
}

func (m *Mapper) lookupFold(r string) string {
	if m.fold == nil {
		m.fold = map[string]string{}
		for _, f := range m.Tree.Files() {
			m.fold[strings.ToLower(f)] = f
		}
	}
	return m.fold[strings.ToLower(r)]
}

// generatedProjects returns the directories of the projects a generated file belongs to: the directory before its first
// obj/ or bin/ segment when it has a project file (the default layout), else the projects named by the segment after it
// (the artifacts layout, artifacts/obj/<project>/...). ok=false when none is found.
func (m *Mapper) generatedProjects(r string) ([]string, bool) {
	if !buildOutput(r) {
		return nil, false
	}
	if m.projects == nil {
		m.projects = map[string][]string{}
		for _, f := range m.Tree.Files() {
			if projectFile(f) {
				name := strings.ToLower(strings.TrimSuffix(path.Base(f), path.Ext(f)))
				m.projects[name] = append(m.projects[name], path.Dir(f))
			}
		}
	}
	segs := strings.Split(r, "/")
	for i, s := range segs {
		if ls := strings.ToLower(s); ls != "obj" && ls != "bin" {
			continue
		}
		owner := "."
		if i > 0 {
			owner = strings.Join(segs[:i], "/")
		}
		for _, dirs := range m.projects {
			if slices.Contains(dirs, owner) {
				return []string{owner}, true
			}
		}
		if i+1 < len(segs)-1 {
			if dirs := m.projects[strings.ToLower(segs[i+1])]; len(dirs) > 0 {
				return dirs, true
			}
		}
		return nil, false
	}
	return nil, false
}

// FilesUnder lists the tracked files of t under directory dir ("." for every file).
func FilesUnder(t tree.Tree, dir string) []string {
	files := t.Files()
	if dir == "." || dir == "" {
		return slices.Clone(files)
	}
	pre := dir + "/"
	var out []string
	for i := sort.SearchStrings(files, pre); i < len(files) && strings.HasPrefix(files[i], pre); i++ {
		out = append(out, files[i])
	}
	return out
}

// deps maps the hit files of one coverage report: tracked files directly, generated sources to every tracked file of the
// project that owns them (the generator's inputs are among them; a generated file of no known project: incomplete), untracked repository files make the record incomplete,
// files outside the repository belong to the environment.
func (m *Mapper) deps(c *Coverage) (map[string]bool, []string) {
	deps := map[string]bool{}
	var why []string
	names := make([]string, 0, len(c.Hit))
	for f := range c.Hit {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		r, pl := m.rel(f)
		switch pl {
		case placeTracked:
			deps[r] = true
		case placeGenerated:
			dirs, ok := m.generatedProjects(r)
			if !ok {
				why = append(why, "executed a generated file of no known project "+r)
			}
			for _, d := range dirs {
				for _, x := range FilesUnder(m.Tree, d) {
					deps[x] = true
				}
			}
		case placeUntracked:
			why = append(why, "executed an untracked repository file "+r)
		}
	}
	return deps, why
}

func ofClass(name, c string) bool {
	return name == c || strings.HasPrefix(name, c+".") || strings.HasPrefix(name, c+"+") || strings.HasPrefix(name, c+"/")
}

// classHit reports whether coverage saw a line of class c (or of a type nested in it) executed.
func classHit(cov *Coverage, c string) bool {
	for name, hit := range cov.Classes {
		if hit && ofClass(name, c) {
			return true
		}
	}
	return false
}

// canary reports whether coverage saw an executed line of class c in a tracked repository file: the proof that the collector
// worked for this run and that its file names map to the repository (a record whose names all fall outside would otherwise
// list no dependency at all).
func (m *Mapper) canary(cov *Coverage, c string) bool {
	for name, files := range cov.ClassFiles {
		if !ofClass(name, c) {
			continue
		}
		for _, f := range files {
			if _, pl := m.rel(f); pl == placeTracked {
				return true
			}
		}
	}
	return false
}

// Records builds the class records "<unit>#<class>" and the class-list record "<unit>" of one recorded project
// (docs/dynamic-layer.md §9.1, §9.3). The class-list record also carries the instrumented lines of every tracked source file
// the reports listed (Lines), which the guard uses to require that a changed member is visible to the coverage.
func Records(m *Mapper, o Outcome, env, commit string, now time.Time) []dynamic.Record {
	u := o.Unit
	cl := dynamic.Record{Eco: Eco, Unit: u.ID, Collector: CollectorID, Commit: commit, Deps: map[string]string{}, Env: env,
		Complete: true, Pass: o.Why == "", Recorded: now.UTC()}
	for _, f := range FilesUnder(m.Tree, u.Dir) {
		cl.Deps[f] = m.Tree.Key(f)
	}
	var clWhy []string
	if o.Why != "" {
		clWhy = append(clWhy, o.Why)
	}
	fws := slices.Clone(o.Frameworks)
	sort.Strings(fws)
	for _, fw := range fws {
		if why, bad := o.Unrecorded[fw]; bad {
			clWhy = append(clWhy, "framework "+fw+" not recorded: "+why)
		}
	}
	if src, err := m.Tree.Read(u.Project); err != nil {
		clWhy = append(clWhy, "cannot read the project file")
	} else if ref := UnsafeReference(src); ref != "" {
		clWhy = append(clWhy, "references "+ref+" (reads assemblies or sources as data, or needs the profiler slot)")
	}
	if o.Why == "" && len(o.Classes) == 0 {
		clWhy = append(clWhy, "no test class reported")
	}
	for c := range o.Classes {
		cl.Classes = append(cl.Classes, c)
	}
	sort.Strings(cl.Classes)

	byClass := map[string][]ClassRun{}
	lines := map[string]map[int]bool{}
	for _, r := range o.Runs {
		byClass[r.Class] = append(byClass[r.Class], r)
		if r.Coverage == nil {
			continue
		}
		for f, ls := range r.Coverage.Lines {
			rp, pl := m.rel(f)
			if pl != placeTracked {
				continue
			}
			set := lines[rp]
			if set == nil {
				set = map[int]bool{}
				lines[rp] = set
			}
			for _, l := range ls {
				set[l] = true
			}
		}
	}
	if len(lines) > 0 {
		cl.Lines = map[string]string{}
		for f, set := range lines {
			ls := make([]int, 0, len(set))
			for l := range set {
				ls = append(ls, l)
			}
			sort.Ints(ls)
			cl.Lines[f] = m.Tree.Key(f) + "|" + Ranges(ls)
		}
	}

	var out []dynamic.Record
	if o.Why == "" {
		for _, c := range cl.Classes {
			r := dynamic.Record{Eco: Eco, Unit: u.ID + "#" + c, Collector: CollectorID, Commit: commit, Deps: map[string]string{}, Env: env,
				Complete: true, Recorded: now.UTC()}
			runs := byClass[c]
			var why []string
			r.Pass = len(runs) > 0
			on := o.On[c]
			if on == nil {
				on = fws // outcomes built without per-framework class lists: every recorded framework
			}
			for _, fw := range fws {
				if _, bad := o.Unrecorded[fw]; bad || !slices.Contains(on, fw) {
					continue // the framework could not run (the class list is incomplete), or the class does not exist there
				}
				i := slices.IndexFunc(runs, func(x ClassRun) bool { return x.Framework == fw })
				if i < 0 {
					why = append(why, "no run on "+fw)
					r.Pass = false
					continue
				}
				run := runs[i]
				if !run.Reported || run.Outcome.Tests == 0 || run.Outcome.Failed > 0 {
					r.Pass = false
				}
				switch {
				case run.Why != "":
					why = append(why, fw+": "+run.Why)
					continue
				case !run.Reported:
					why = append(why, fw+": the class was not reported in its own run")
					continue
				case !m.canary(run.Coverage, c):
					why = append(why, fw+": coverage saw no executed line of the class itself in a repository file")
				}
				d, w := m.deps(run.Coverage)
				why = append(why, w...)
				for f := range d {
					if k := m.Tree.Key(f); k != "" {
						r.Deps[f] = k
					}
				}
			}
			if len(why) > 0 {
				r.Complete, r.Incomplete = false, strings.Join(dedupe(why), "; ")
			}
			out = append(out, r)
		}
	}
	if len(clWhy) > 0 {
		cl.Complete, cl.Incomplete = false, strings.Join(clWhy, "; ")
	}
	return append(out, cl)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Summary describes one class record for progress output.
func Summary(r dynamic.Record) string {
	switch {
	case !r.Complete:
		return "incomplete: " + r.Incomplete
	case !r.Pass:
		return "failed"
	}
	return fmt.Sprintf("%d deps", len(r.Deps))
}

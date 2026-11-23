package rbdyn

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/tree"
)

// derived: directories whose files belong to the environment or are caches (an in-repo bundle, Bundler's settings, git),
// never inputs a change can reach except through tracked files.
func derived(p string) bool {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		switch s {
		case ".bundle", ".git", "node_modules":
			return true
		case "vendor":
			if i+1 < len(segs) && segs[i+1] == "bundle" {
				return true
			}
		}
	}
	return false
}

// lockfile: Bundler's lockfiles are part of the environment fingerprint (often untracked).
func lockfile(p string) bool {
	b := path.Base(p)
	return b == "Gemfile.lock" || b == "gems.locked"
}

// native: a compiled extension.
func native(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".so", ".bundle", ".dll", ".dylib", ".o":
		return true
	}
	return false
}

// mapper turns the absolute paths Ruby reports into repo-relative paths of tree t.
type mapper struct {
	root string // forward slashes, no trailing slash
	real string // the root with symbolic links resolved, when it differs ("" otherwise)
	t    tree.Tree
}

func newMapper(root string, t tree.Tree) *mapper {
	m := &mapper{root: strings.TrimSuffix(filepath.ToSlash(root), "/"), t: t}
	// The root may be reached through a symbolic link (macOS /var -> /private/var): Ruby reports real paths for some files.
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
		switch {
		case a == r:
			return "."
		case strings.HasPrefix(a, r+"/"):
			return path.Clean(abs[len(r)+1:])
		}
	}
	if ev, err := filepath.EvalSymlinks(filepath.FromSlash(abs)); err == nil && filepath.ToSlash(ev) != abs {
		return m.rel(filepath.ToSlash(ev))
	}
	return ""
}

// tracked returns the tracked spelling of repo-relative p.
func (m *mapper) tracked(p string) (string, bool) {
	if m.t.Has(p) {
		return p, true
	}
	if f := m.t.FoldLookup(p); len(f) == 1 {
		return f[0], true
	}
	return "", false
}

// trackedDir returns the tracked spelling of directory d ("." for the root).
func (m *mapper) trackedDir(d string) (string, bool) {
	if d == "." || m.t.IsDir(d) {
		return d, true
	}
	pre := strings.ToLower(d) + "/"
	for _, f := range m.t.Files() {
		if len(f) > len(pre) && strings.ToLower(f[:len(pre)]) == pre {
			return f[:len(pre)-1], true
		}
	}
	return "", false
}

// FormatMethods encodes a method table as "start-end:count" entries joined by commas, sorted.
func FormatMethods(ms []Method) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = fmt.Sprintf("%d-%d:%d", m.Start, m.End, m.Count)
	}
	return strings.Join(parts, ",")
}

// ParseMethods decodes FormatMethods; ok is false when s is malformed.
func ParseMethods(s string) (ms []Method, ok bool) {
	if s == "" {
		return nil, true
	}
	for _, e := range strings.Split(s, ",") {
		se, c, ok1 := strings.Cut(e, ":")
		a, b, ok2 := strings.Cut(se, "-")
		x, err1 := strconv.Atoi(a)
		y, err2 := strconv.Atoi(b)
		n, err3 := strconv.Atoi(c)
		if !ok1 || !ok2 || err1 != nil || err2 != nil || err3 != nil {
			return nil, false
		}
		ms = append(ms, Method{x, y, n})
	}
	return ms, true
}

// RecordOf turns an outcome into a record (docs/dynamic-layer.md §10.3). t is the working tree at the recorded commit (its keys
// are the blob OIDs of the content the test saw); root is the repository root on disk.
func RecordOf(o Outcome, u Unit, t tree.Tree, root, env, commit string, now time.Time) dynamic.Record {
	r := dynamic.Record{Eco: Eco, Unit: u.ID, Collector: CollectorID, Commit: commit, Deps: map[string]string{}, Env: env,
		Complete: true, Recorded: now.UTC(), Methods: map[string]string{}}
	var why []string
	switch {
	case o.TimedOut:
		why = append(why, "timeout")
	case o.Err != "":
		why = append(why, o.Err)
	case o.Tests == 0:
		why = append(why, "no tests ran")
	case o.Tests < 0:
		why = append(why, "no test summary in the runner's output")
	}
	r.Pass = o.Exit == 0 && !o.TimedOut && o.Err == "" && o.Tests > 0
	why = append(why, o.Incomplete...)
	m := newMapper(root, t)
	add := func(p string) {
		if k := t.Key(p); k != "" {
			r.Deps[p] = k
		} else {
			why = append(why, "cannot hash "+p)
		}
	}
	// resolve maps an absolute path to its tracked spelling; untracked existing files make the record incomplete.
	resolve := func(abs, verb string) (string, bool) {
		rel := m.rel(abs)
		if rel == "" || rel == "." {
			return "", false
		}
		if p, ok := m.tracked(rel); ok {
			return p, true
		}
		if derived(rel) || lockfile(rel) {
			return "", false
		}
		st, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || st.IsDir() {
			return "", false // absent: a negative dependency (additions are never narrowed)
		}
		why = append(why, verb+" untracked file "+rel)
		return "", false
	}
	loaded := map[string]bool{}
	for _, f := range o.Loaded {
		if rel := m.rel(f); rel != "" && native(rel) && !derived(rel) {
			why = append(why, "native extension in the repository: "+rel)
			continue
		}
		if p, ok := resolve(f, "loads"); ok {
			add(p)
			loaded[p] = true
		}
	}
	noTable := map[string]bool{}
	for f := range o.NoTable {
		if p, ok := resolve(f, "loads"); ok {
			noTable[p] = true
		}
	}
	for f, ms := range o.Methods {
		p, ok := resolve(f, "loads")
		if !ok || noTable[p] {
			continue
		}
		if cur, dup := r.Methods[p]; dup {
			// The same file under two spellings: keep both tables (a count > 0 in either keeps a unit).
			cms, _ := ParseMethods(cur)
			ms = append(cms, ms...)
			sort.Slice(ms, func(i, j int) bool {
				return ms[i].Start < ms[j].Start || ms[i].Start == ms[j].Start && ms[i].End < ms[j].End
			})
		}
		r.Methods[p] = FormatMethods(ms)
	}
	reads := map[string]bool{}
	for _, f := range o.Reads {
		if p, ok := resolve(f, "reads"); ok {
			add(p)
			reads[p] = true
		}
	}
	if len(o.Dirs) > 0 {
		files := t.Files()
		for _, d := range o.Dirs {
			rel := m.rel(d)
			if rel == "" {
				continue
			}
			dir, ok := m.trackedDir(rel)
			if !ok {
				continue
			}
			pre := dir + "/"
			i := 0
			if dir != "." {
				i = sort.SearchStrings(files, pre)
			}
			for ; i < len(files) && (dir == "." || strings.HasPrefix(files[i], pre)); i++ {
				if !derived(files[i]) {
					add(files[i])
					reads[files[i]] = true
				}
			}
		}
	}
	for p := range reads {
		r.Reads = append(r.Reads, p)
	}
	sort.Strings(r.Reads)
	if p, ok := m.tracked(u.ID); ok {
		add(p)
		if !loaded[p] {
			why = append(why, "canary: the unit's own file was not loaded")
		}
	} else {
		why = append(why, "the unit's own file "+u.ID+" is not in the tree")
	}
	if len(r.Methods) == 0 {
		r.Methods = nil
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

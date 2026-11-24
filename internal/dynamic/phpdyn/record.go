package phpdyn

import (
	"encoding/xml"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/tree"
)

// mapper turns the absolute paths PHP reports into repo-relative paths of tree t.
type mapper struct {
	root    string // forward slashes, no trailing slash
	real    string // the root with symbolic links resolved, when it differs ("" otherwise)
	t       tree.Tree
	vendors []string // repo-relative vendor directories
}

func newMapper(root string, t tree.Tree, vendors []string) *mapper {
	m := &mapper{root: strings.TrimSuffix(filepath.ToSlash(root), "/"), t: t, vendors: vendors}
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

// environment: inside a vendor directory (the fingerprint's job), git's directory or node_modules.
func (m *mapper) environment(p string) bool {
	for _, v := range m.vendors {
		if p == v || strings.HasPrefix(p, v+"/") {
			return true
		}
	}
	for _, s := range strings.Split(p, "/") {
		if s == ".git" || s == "node_modules" {
			return true
		}
	}
	return false
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

// below returns the tracked files below directory d ("." for the root), outside the environment.
func (m *mapper) below(d string) []string {
	var out []string
	for _, f := range m.t.Files() {
		if (d == "." || strings.HasPrefix(f, d+"/")) && !m.environment(f) {
			out = append(out, f)
		}
	}
	return out
}

// RecordOf turns the outcome of a test file into its record (docs/dynamic-layer.md §11.3). t is the working tree at the recorded
// commit (its keys are the blob OIDs of the content the test saw); root is the repository root on disk; vendors are the
// repository's Composer vendor directories (repo-relative).
func RecordOf(o Outcome, f FileUnit, t tree.Tree, root, env, commit string, now time.Time, vendors []string) dynamic.Record {
	r := dynamic.Record{Eco: Eco, Unit: f.ID(), Collector: CollectorID, Commit: commit, Deps: map[string]string{}, Env: env,
		Complete: true, Recorded: now.UTC(), LineCov: map[string]string{}}
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
	m := newMapper(root, t, vendors)
	add := func(p string) {
		if k := t.Key(p); k != "" {
			r.Deps[p] = k
		} else {
			why = append(why, "cannot hash "+p)
		}
	}
	onDisk := func(rel string) (exists, dir bool) {
		st, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return false, false
		}
		return true, st.IsDir()
	}
	included := map[string]bool{}
	for _, abs := range o.Included {
		rel := m.rel(abs)
		if rel == "" || rel == "." || m.environment(rel) {
			continue
		}
		if p, ok := m.tracked(rel); ok {
			add(p)
			included[p] = true
			continue
		}
		if ev, ok := o.Paths[abs]; ok && !ev.Pre && ev.Kinds["write"] {
			continue // written by the run before it was included
		}
		if exists, _ := onDisk(rel); exists {
			why = append(why, "includes untracked file "+rel)
		}
	}
	reads := map[string]bool{}
	abss := make([]string, 0, len(o.Paths))
	for abs := range o.Paths {
		abss = append(abss, abs)
	}
	sort.Strings(abss)
	for _, abs := range abss {
		ev := o.Paths[abs]
		rel := m.rel(abs)
		if rel == "" || m.environment(rel) {
			continue
		}
		data := ev.Kinds["read"] || ev.Kinds["write"] || ev.Kinds["stat"] || ev.Kinds["list"]
		if rel != "." {
			if p, ok := m.tracked(rel); ok {
				add(p)
				if data {
					reads[p] = true
				}
				continue
			}
		}
		if rel == "." || t.IsDir(rel) {
			if ev.Kinds["list"] {
				for _, p := range m.below(rel) {
					add(p)
					reads[p] = true
				}
			}
			continue
		}
		if !ev.Pre {
			continue // created by the run, or never existed (additions are refused by the guards)
		}
		if exists, dir := onDisk(rel); !exists || dir {
			continue
		}
		verb := "reads"
		if !ev.Kinds["read"] && !ev.Kinds["include"] {
			verb = "sees"
		}
		why = append(why, verb+" untracked file "+rel)
	}
	for p := range reads {
		r.Reads = append(r.Reads, p)
	}
	sort.Strings(r.Reads)
	noTable := map[string]bool{}
	for abs := range o.NoTable {
		if rel := m.rel(abs); rel != "" {
			if p, ok := m.tracked(rel); ok {
				noTable[p] = true
			}
		}
	}
	tables := map[string]LineSet{}
	for abs, ls := range o.Lines {
		rel := m.rel(abs)
		if rel == "" {
			continue
		}
		p, ok := m.tracked(rel)
		if !ok || !included[p] || noTable[p] {
			continue
		}
		tables[p] = tables[p].Union(ls)
	}
	for p, ls := range tables {
		r.LineCov[p] = ls.Format()
	}
	if p, ok := m.tracked(f.File); ok {
		add(p)
		if !included[p] {
			why = append(why, "canary: the test file was not included")
		}
	} else {
		why = append(why, "the test file "+f.File+" is not in the tree")
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

// ListRecordOf turns the listing of a unit into its file-list record (docs/dynamic-layer.md §11.3): Classes are the listed test
// files; Deps the configuration files, every tracked file under the configured test directories (TestRoots) and the listed
// files. failed names test files that could not be recorded alone: the list is then not usable.
func ListRecordOf(lo ListOutcome, u Unit, t tree.Tree, root, env, commit string, now time.Time, vendors []string,
	failed []string) dynamic.Record {
	r := dynamic.Record{Eco: Eco, Unit: u.ID, Collector: CollectorID, Commit: commit, Deps: map[string]string{}, Env: env,
		Complete: true, Pass: lo.Err == "", Recorded: now.UTC(), Runner: lo.Runner}
	var why []string
	if lo.Err != "" {
		why = append(why, lo.Err)
	}
	m := newMapper(root, t, vendors)
	add := func(p string) {
		if k := t.Key(p); k != "" {
			r.Deps[p] = k
		} else {
			why = append(why, "cannot hash "+p)
		}
	}
	roots := TestRoots(t, u)
	for _, f := range lo.Files {
		rel := m.rel(f)
		p, ok := m.tracked(rel)
		if rel == "" || !ok {
			why = append(why, "lists a test file that is not tracked: "+f)
			continue
		}
		if !underAny(p, roots) {
			why = append(why, "lists a test file outside the configured test directories: "+p)
		}
		r.Classes = append(r.Classes, p)
		add(p)
	}
	if lo.Err == "" && len(lo.Files) == 0 {
		why = append(why, "no test file listed")
	}
	sort.Strings(r.Classes)
	for _, p := range ConfigFiles(t, u) {
		add(p)
	}
	for _, p := range RootFiles(t, roots) {
		add(p)
	}
	if lo.Runner == "" && lo.Err == "" {
		why = append(why, "the runner's version is unknown")
	}
	for _, f := range failed {
		why = append(why, "test file "+f+" cannot be recorded alone")
	}
	if len(why) > 0 {
		r.Complete = false
		r.Incomplete = strings.Join(dedupe(why), "; ")
	}
	return r
}

// ConfigFiles returns the runner configuration files of unit u tracked in t: its PHPUnit XML configuration and Pest.php files.
func ConfigFiles(t tree.Tree, u Unit) []string {
	var out []string
	if u.Config != "" && t.Has(u.Config) {
		out = append(out, u.Config)
	}
	for _, n := range []string{"Pest.php", "tests/Pest.php", "test/Pest.php"} {
		if p := path.Join(u.Project, n); t.Has(p) {
			out = append(out, p)
		}
	}
	return out
}

// phpunitXML is the part of a PHPUnit XML configuration that names test locations.
type phpunitXML struct {
	Suites []struct {
		Directories []string `xml:"directory"`
		Files       []string `xml:"file"`
	} `xml:"testsuites>testsuite"`
}

// TestRoots returns the repo-relative directories (and files) where the configured test suites of unit u can find test files
// in tree t (docs/dynamic-layer.md §11.3): every <directory> and <file> of every test suite, resolved against the configuration's
// directory. A location bgraph cannot resolve statically (wildcards, placeholders, absolute paths, outside the repository),
// an unreadable configuration or one without test suites gives the project directory: every file of the project.
func TestRoots(t tree.Tree, u Unit) []string {
	project := []string{path.Clean(u.Project)}
	if u.Config == "" {
		return project
	}
	b, err := t.Read(u.Config)
	if err != nil {
		return project
	}
	var cfg phpunitXML
	if err := xml.Unmarshal(b, &cfg); err != nil {
		return project
	}
	dir := path.Dir(u.Config)
	var roots []string
	for _, s := range cfg.Suites {
		for _, e := range append(append([]string(nil), s.Directories...), s.Files...) {
			e = strings.TrimSpace(strings.ReplaceAll(e, "\\", "/"))
			if e == "" || strings.ContainsAny(e, "*?[{%$~:") || path.IsAbs(e) {
				return project
			}
			p := path.Join(dir, e)
			if p == ".." || strings.HasPrefix(p, "../") {
				return project
			}
			roots = append(roots, p)
		}
	}
	if len(roots) == 0 {
		return project
	}
	sort.Strings(roots)
	var out []string
	for _, r := range roots {
		if len(out) > 0 && (out[len(out)-1] == r || underAny(r, out)) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func underAny(p string, roots []string) bool {
	for _, r := range roots {
		if r == "." || p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// RootFiles returns the tracked files of t under roots, outside vendor, node_modules and .git directories.
func RootFiles(t tree.Tree, roots []string) []string {
	var out []string
	for _, f := range t.Files() {
		if !underAny(f, roots) {
			continue
		}
		skip := false
		for _, s := range strings.Split(f, "/") {
			if s == "vendor" || s == "node_modules" || s == ".git" {
				skip = true
			}
		}
		if !skip {
			out = append(out, f)
		}
	}
	return out
}

// RepoRel returns the repo-relative form of an absolute path PHP reported ("" when outside the repository).
func RepoRel(root, abs string) string {
	r := newMapper(root, nil, nil).rel(abs)
	if r == "." {
		return ""
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

// RunnerMajor returns the runner and major version of a file-list record's Runner ("phpunit@11.5.2" → "phpunit", 11).
func RunnerMajor(r string) (string, int) {
	name, v, ok := strings.Cut(r, "@")
	if !ok {
		return "", 0
	}
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return name, n
}

// MultiOperand reports why the runner of a file-list record cannot run several test files given as operands ("" when it
// can): PHPUnit before 10 accepts one test operand; Pest before 2 runs on such a PHPUnit.
func MultiOperand(r string) string {
	name, major := RunnerMajor(r)
	switch {
	case name == RunnerPHPUnit && major >= 10, name == RunnerPest && major >= 2:
		return ""
	case name == "":
		return "the runner's version is unknown"
	}
	return fmt.Sprintf("%s accepts one test operand", r)
}

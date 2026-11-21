package audit

import (
	"path"
	"runtime"
	"strings"
)

// Mapper maps the candidate paths of a failure to a known repo-relative test file.
type Mapper struct {
	Root       string   // absolute repo root (any slash style)
	AltRoots   []string // other spellings of Root (symlink-resolved, Windows long/short names); tried after Root
	ResultsDir string   // absolute directory of the results file ("" if unknown)
	Known      []string // every known test file, repo-relative with forward slashes
	Projects   []string // test project directories (repo-relative); "." for the repo root

	root, resDir string
	roots        []string
	set          map[string]bool
}

// Outcome of Map.
type Outcome int

const (
	Mapped    Outcome = iota
	NotFound          // no candidate matched a known test file
	Ambiguous         // a candidate is a suffix of several known test files
)

// Map returns the test file for the first candidate that resolves. Per candidate it tries: repo-relative, relative to
// the results file's directory, relative to each test project. If none resolves, a second pass accepts a candidate
// that is a suffix of exactly one known test file (several → Ambiguous).
func (m *Mapper) Map(cands []string) (string, Outcome) {
	if m.set == nil {
		m.set = make(map[string]bool, len(m.Known))
		for _, k := range m.Known {
			m.set[k] = true
		}
		m.root = normSlashes(m.Root)
		m.roots = []string{m.root}
		for _, r := range m.AltRoots {
			m.roots = append(m.roots, normSlashes(r))
		}
		m.resDir = normSlashes(m.ResultsDir)
	}
	norm := make([]string, 0, len(cands))
	for _, c := range cands {
		if n := m.normalize(c); n != "" {
			norm = append(norm, n)
		}
	}
	for _, n := range norm {
		if f, ok := m.exact(n); ok {
			return f, Mapped
		}
	}
	out := NotFound
	for _, n := range norm {
		if path.Ext(n) == "" || strings.ContainsAny(n, " \t") {
			continue
		}
		var hits []string
		for _, k := range m.Known {
			if k == n || strings.HasSuffix(k, "/"+n) {
				hits = append(hits, k)
			}
		}
		switch len(hits) {
		case 0:
		case 1:
			return hits[0], Mapped
		default:
			out = Ambiguous
		}
	}
	return "", out
}

func (m *Mapper) exact(n string) (string, bool) {
	if m.set[n] {
		return n, true
	}
	if m.resDir != "" {
		if rel, ok := m.toRepoRel(path.Join(m.resDir, n)); ok && m.set[rel] {
			return rel, true
		}
	}
	for _, p := range m.Projects {
		if p == "." || p == "" {
			continue
		}
		if c := path.Join(p, n); m.set[c] {
			return c, true
		}
	}
	return "", false
}

// normalize turns a candidate into a repo-relative slash path. Absolute paths outside the repo and paths escaping
// upwards normalize to "" (unusable).
func (m *Mapper) normalize(c string) string {
	c = normSlashes(strings.TrimPrefix(strings.TrimSpace(c), "file://"))
	if c == "" {
		return ""
	}
	if isAbs(c) {
		rel, _ := m.toRepoRel(c)
		return rel
	}
	c = path.Clean(c)
	if c == "." || strings.HasPrefix(c, "../") {
		return ""
	}
	return c
}

// toRepoRel makes the absolute slash path p relative to the repo root. The comparison is case-insensitive on Windows
// and whenever a drive letter is involved.
func (m *Mapper) toRepoRel(p string) (string, bool) {
	p = path.Clean(p)
	for _, r := range m.roots {
		if r == "" {
			continue
		}
		root := strings.TrimSuffix(path.Clean(r), "/")
		a, b := p, root
		if runtime.GOOS == "windows" || hasDrive(root) || hasDrive(p) {
			a, b = strings.ToLower(p), strings.ToLower(root)
		}
		if strings.HasPrefix(a, b+"/") {
			return p[len(root)+1:], true
		}
	}
	return "", false
}

func normSlashes(s string) string {
	s = strings.ReplaceAll(s, "\\", "/")
	for strings.HasPrefix(s, "./") {
		s = s[2:]
	}
	return s
}

func hasDrive(p string) bool {
	return len(p) >= 2 && p[1] == ':' && (p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z')
}

func isAbs(p string) bool {
	return strings.HasPrefix(p, "/") || hasDrive(p)
}

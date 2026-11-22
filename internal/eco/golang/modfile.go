package golang

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// modFile is a parsed go.mod or go.work file: for each directive verb, its entries (the tokens of one line, unquoted and joined by a
// space). Comments are dropped, so two files that differ only in comments and layout compare equal.
type modFile struct {
	entries map[string][]string
}

// Module returns the module path ("" when absent).
func (m *modFile) Module() string {
	if e := m.entries["module"]; len(e) > 0 {
		return strings.Fields(e[0])[0]
	}
	return ""
}

// replacement is a replace directive whose target is a local directory.
type replacement struct {
	old string // module path (prefix of import paths)
	dir string // target directory, relative to the file's directory, slash-separated
}

// LocalReplaces returns the replace directives that point to a directory.
func (m *modFile) LocalReplaces() []replacement {
	var out []replacement
	for _, e := range m.entries["replace"] {
		f := strings.Fields(e)
		i := slices.Index(f, "=>")
		if i < 1 || i+1 >= len(f) {
			continue
		}
		// "old [v] => new [v]": a local target has no version.
		if len(f)-(i+1) == 1 && isLocalPath(f[i+1]) {
			out = append(out, replacement{old: f[0], dir: strings.ReplaceAll(f[i+1], "\\", "/")})
		}
	}
	return out
}

// Uses returns the directories of a go.work file's use directives (slash-separated, relative to the go.work directory).
func (m *modFile) Uses() []string {
	var out []string
	for _, e := range m.entries["use"] {
		for _, f := range strings.Fields(e) {
			out = append(out, strings.ReplaceAll(f, "\\", "/"))
		}
	}
	return out
}

// isLocalPath mirrors cmd/go's rule for a replacement that is a directory: it starts with ./ or ../ (or .\ ..\), is . or .., or is
// absolute.
func isLocalPath(s string) bool {
	switch {
	case s == "." || s == "..":
		return true
	case strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || strings.HasPrefix(s, `.\`) || strings.HasPrefix(s, `..\`):
		return true
	case strings.HasPrefix(s, "/") || strings.HasPrefix(s, `\`) || (len(s) > 2 && s[1] == ':'):
		return true
	}
	return false
}

// localDir resolves a local replacement/use directory against the directory of the file declaring it. ok=false when the target is
// absolute or leaves the repository.
func localDir(base, rel string) (string, bool) {
	if strings.HasPrefix(rel, "/") || (len(rel) > 2 && rel[1] == ':') {
		return "", false
	}
	d := path.Join(base, rel)
	if d == ".." || strings.HasPrefix(d, "../") {
		return "", false
	}
	return d, true
}

var errSyntax = errors.New("syntax error")

// bom is the UTF-8 byte order mark, tolerated as white space.
var bom = string(rune(0xFEFF))

// parseModFile parses go.mod / go.work syntax: one directive per line ("verb args..."), or a block "verb (" ... ")" with one entry per
// line; "//" comments; arguments may be Go string literals.
func parseModFile(src []byte) (*modFile, error) {
	m := &modFile{entries: map[string][]string{}}
	block := ""
	for n, line := range strings.Split(string(src), "\n") {
		toks, err := lexLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		if len(toks) == 0 {
			continue
		}
		if block != "" {
			if len(toks) == 1 && toks[0] == ")" {
				block = ""
				continue
			}
			if slices.Contains(toks, "(") || slices.Contains(toks, ")") {
				return nil, fmt.Errorf("line %d: %w: parenthesis inside a block", n+1, errSyntax)
			}
			m.entries[block] = append(m.entries[block], strings.Join(toks, " "))
			continue
		}
		verb := toks[0]
		if verb == "(" || verb == ")" {
			return nil, fmt.Errorf("line %d: %w: unexpected parenthesis", n+1, errSyntax)
		}
		if len(toks) == 2 && toks[1] == "(" {
			block = verb
			if _, ok := m.entries[verb]; !ok {
				m.entries[verb] = nil
			}
			continue
		}
		if slices.Contains(toks, "(") || slices.Contains(toks, ")") {
			return nil, fmt.Errorf("line %d: %w: unexpected parenthesis", n+1, errSyntax)
		}
		if len(toks) == 1 {
			return nil, fmt.Errorf("line %d: %w: directive %q without arguments", n+1, errSyntax, verb)
		}
		m.entries[verb] = append(m.entries[verb], strings.Join(toks[1:], " "))
	}
	if block != "" {
		return nil, fmt.Errorf("%w: unterminated %s block", errSyntax, block)
	}
	if e := m.entries["module"]; len(e) > 1 {
		return nil, fmt.Errorf("%w: repeated module directive", errSyntax)
	}
	return m, nil
}

// lexLine splits one line into tokens: "(", ")", "=>", string literals (unquoted) and runs of other non-space characters.
func lexLine(line string) ([]string, error) {
	var out []string
	s := strings.TrimRight(line, "\r")
	for {
		s = strings.TrimLeft(s, " \t"+bom)
		if s == "" || strings.HasPrefix(s, "//") {
			return out, nil
		}
		switch c := s[0]; {
		case c == '(' || c == ')':
			out = append(out, s[:1])
			s = s[1:]
		case c == '"' || c == '`':
			end := -1
			for i := 1; i < len(s); i++ {
				if c == '"' && s[i] == '\\' {
					i++
					continue
				}
				if s[i] == c {
					end = i
					break
				}
			}
			if end < 0 {
				return nil, fmt.Errorf("%w: unterminated string", errSyntax)
			}
			v, err := strconv.Unquote(s[:end+1])
			if err != nil {
				return nil, fmt.Errorf("%w: bad string %s", errSyntax, s[:end+1])
			}
			out = append(out, v)
			s = s[end+1:]
		default:
			i := 0
			for i < len(s) && !strings.ContainsRune(" \t()\"`", rune(s[i])) && !strings.HasPrefix(s[i:], "//") {
				i++
			}
			out = append(out, s[:i])
			s = s[i:]
		}
	}
}

// changedVerbs compares two parsed files and returns the directive verbs whose entries differ (as multisets: the order of require
// lines does not matter).
func changedVerbs(a, b *modFile) []string {
	verbs := map[string]bool{}
	for v := range a.entries {
		verbs[v] = true
	}
	for v := range b.entries {
		verbs[v] = true
	}
	var out []string
	for v := range verbs {
		x, y := slices.Clone(a.entries[v]), slices.Clone(b.entries[v])
		slices.Sort(x)
		slices.Sort(y)
		if !slices.Equal(x, y) {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

package jvm

import (
	"bytes"
	"path"
	"strings"
	"sync"

	"github.com/qxbao/bgraph/internal/tree"
)

// Tests read files of other modules through literal relative paths (new File("../core/src/test/resources/x.json")): the
// surefire basedir and Gradle's default test workingDir are the module directory. The string literals of test sources that
// resolve to an existing file or directory the module does not own are dependencies of the module. Paths assembled at run time
// (concatenation, System.getProperty, Paths.get with variables) are not seen.

// testSourceExts are the source files scanned for path literals.
var testSourceExts = map[string]bool{".java": true, ".kt": true, ".groovy": true, ".scala": true}

// testSource: rel (relative to the unit directory) is a source file of a test source set: src/test*/**, src/*Test/** (Kotlin
// Multiplatform jvmTest, commonTest, Android androidTest), src/it/**, src/testFixtures/**.
func testSource(rel string) bool {
	if !testSourceExts[path.Ext(rel)] {
		return false
	}
	r, ok := strings.CutPrefix(rel, "src/")
	if !ok {
		return false
	}
	set, _, ok := strings.Cut(r, "/")
	if !ok {
		return false
	}
	ls := strings.ToLower(set)
	return ls == "it" || strings.Contains(ls, "test")
}

// pathLiterals returns the string literals of source file p that may be paths (they hold '/' or '\'), cached by content key.
func (e *Eco) pathLiterals(t tree.Tree, p string) []string {
	key := t.Key(p)
	if key != "" {
		key += path.Ext(p)
		e.mu.Lock()
		v, ok := e.lits[key]
		e.mu.Unlock()
		if ok {
			return v
		}
	}
	b, err := t.Read(p)
	var v []string
	if err == nil {
		v = scanPathLiterals(b)
	}
	if key != "" {
		e.mu.Lock()
		if e.lits == nil {
			e.lits = map[string][]string{}
		}
		e.lits[key] = v
		e.mu.Unlock()
	}
	return v
}

// prefetchLiterals computes pathLiterals for every test source under a unit directory in parallel (reading files dominates the
// cost of the scan), so that the ownership pass only hits the cache.
func (e *Eco) prefetchLiterals(t tree.Tree, units []*unit) {
	dirs := map[string]bool{}
	for _, u := range units {
		dirs[u.dir] = true
	}
	var files []string
	for _, f := range t.Files() {
		// The first src/ segment decides the candidate unit directory; a miss here is only scanned later, on demand.
		d, rel := ".", f
		if !strings.HasPrefix(f, "src/") {
			i := strings.Index(f, "/src/")
			if i < 0 {
				continue
			}
			d, rel = f[:i], f[i+1:]
		}
		if dirs[d] && testSource(rel) && !skipFile(f) {
			files = append(files, f)
		}
	}
	const workers = 16
	ch := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range ch {
				e.pathLiterals(t, f)
			}
		}()
	}
	for _, f := range files {
		ch <- f
	}
	close(ch)
	wg.Wait()
}

// scanPathLiterals lexes Java/Kotlin/Groovy/Scala source b (comments, "..." / '...' strings with escapes, """ text blocks) and
// returns the literal contents that hold a path separator and no line break.
func scanPathLiterals(b []byte) []string {
	if !bytes.ContainsAny(b, `"'`) {
		return nil
	}
	var out []string
	keep := func(s []byte) {
		if len(s) > 1 && len(s) < 512 && bytes.ContainsAny(s, `/\`) && !bytes.ContainsAny(s, "\n\r") {
			out = append(out, string(s))
		}
	}
	n := len(b)
	for i := 0; i < n; {
		c := b[i]
		switch {
		case c == '/' && i+1 < n && b[i+1] == '/':
			for i < n && b[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && b[i+1] == '*':
			j := bytes.Index(b[i+2:], []byte("*/"))
			if j < 0 {
				return out
			}
			i += j + 4
		case c == '"' && i+2 < n && b[i+1] == '"' && b[i+2] == '"':
			j := bytes.Index(b[i+3:], []byte(`"""`))
			if j < 0 {
				return out
			}
			keep(b[i+3 : i+3+j])
			i += j + 6
		case c == '"' || c == '\'':
			j := i + 1
			for j < n && b[j] != c && b[j] != '\n' {
				if b[j] == '\\' {
					j++
				}
				j++
			}
			if j < n && b[j] == c {
				keep(b[i+1 : j])
			}
			i = j + 1
		default:
			i++
		}
	}
	return out
}

// literalTarget resolves literal s against directory base: the repo-relative path it names, ok=false when it is absolute, a URL,
// interpolated, or leaves the repository.
func literalTarget(s, base string) (string, bool) {
	s = strings.ReplaceAll(s, `\\`, "/")
	s = strings.ReplaceAll(s, `\`, "/")
	if strings.HasPrefix(s, "/") || len(s) > 1 && s[1] == ':' || strings.Contains(s, "$") || strings.ContainsAny(s, "*?%{}<>|\"") {
		return "", false
	}
	tg := path.Join(base, s)
	if tg == ".." || strings.HasPrefix(tg, "../") {
		return "", false
	}
	return tg, true
}

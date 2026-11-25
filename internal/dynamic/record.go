// Package dynamic is bgraph's dynamic layer (docs/dynamic-layer.md): per-unit records of what each test actually loaded or opened,
// and the narrowing that removes a WIDENED unit from a selection when its usable record proves that no changed file is among
// its dependencies. It never adds units and never touches units the static graph reaches through ordinary edges.
//
// This package is pure (no process, no git): the collectors live in subpackages, the wiring in the bgraph package.
package dynamic

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Record is what one isolated run of one test unit saw (docs/dynamic-layer.md §3).
type Record struct {
	Eco       string `json:"eco"`
	Unit      string `json:"unit"`      // unit ID (Python: the test file path)
	Collector string `json:"collector"` // collector name+version, e.g. "python-audit-1"
	Commit    string `json:"commit,omitempty"`
	// Deps: repo-relative file → git blob OID of the content the test saw. The unit's own files are always in it.
	Deps       map[string]string `json:"deps"`
	Env        string            `json:"env"` // environment fingerprint (docs/dynamic-layer.md §5)
	Complete   bool              `json:"complete"`
	Incomplete string            `json:"incomplete,omitempty"` // why not complete
	Pass       bool              `json:"pass"`                 // the unit's run passed (exit 0)
	Recorded   time.Time         `json:"recorded"`
	// Toolchain: native addons of the Node toolchain allowlist the run loaded ("name@version", docs/dynamic-layer.md §8.3), for
	// transparency only.
	Toolchain []string `json:"toolchain,omitempty"`
	// Classes: the test classes the runner reported for a JVM unit (the class-list record of docs/dynamic-layer.md §9.1, whose Unit
	// is the module's unit ID and whose Deps are the files of its test roots).
	Classes []string `json:"classes,omitempty"`
	// Artifacts: JVM class records list the external archives their test JVM loaded classes from or read (absolute path →
	// "size:sha1"); a missing or changed archive makes the record unusable (docs/dynamic-layer.md §9.9).
	Artifacts map[string]string `json:"artifacts,omitempty"`
	// Lines: the .NET class-list record lists, per tracked source file the coverage reports named, "<blob OID>|<line ranges>"
	// of the instrumented lines (docs/dynamic-layer.md §9.9): a changed member none of whose lines is instrumented is invisible to
	// the records, and the guard refuses narrowing.
	Lines map[string]string `json:"lines,omitempty"`
	// Methods: Ruby records list, per repository file the unit loaded with Ruby's Coverage running, every method defined by
	// `def` as "<def line>-<end line>:<calls>" entries joined by commas (docs/dynamic-layer.md §10.3). A loaded file without an
	// entry has no method table (loaded before the collector started): no change to it can be judged.
	Methods map[string]string `json:"methods,omitempty"`
	// Reads: Ruby records list the dependencies the unit read as data (or found by listing a directory), as opposed to loading
	// them as code: any change to them keeps the unit.
	Reads []string `json:"reads,omitempty"`
	// Runner: the PHP file-list record names the installed test runner and its version, "phpunit@<version>" or
	// "pest@<version>" (docs/dynamic-layer.md §11.3); a runner that accepts one test operand only is never narrowed.
	Runner string `json:"runner,omitempty"`
	// LineCov: PHP file records list, per repository file the test file's processes included with Xdebug's coverage running,
	// "<executed line ranges>|<instrumentable but not executed line ranges>" ("3-5,9|6-8"; docs/dynamic-layer.md §11.3). An
	// included file without an entry has no line table: no change to it can be judged at function level.
	LineCov map[string]string `json:"line_cov,omitempty"`
	// Dirs: Go function records list the repository package directories of the test binary's build closure; the record is
	// usable only while each holds, at the base, no .go file that is not among Deps (a file added to a compiled package).
	Dirs []string `json:"dirs,omitempty"`
}

// valid reports whether r is well formed enough to be stored and considered at all.
func (r Record) valid() error {
	switch {
	case r.Eco == "":
		return errors.New("no eco")
	case r.Unit == "":
		return errors.New("no unit")
	case r.Collector == "":
		return errors.New("no collector")
	case r.Env == "":
		return errors.New("no env")
	}
	for p, oid := range r.Deps {
		if !cleanRel(p) {
			return fmt.Errorf("dep %q is not a clean repo-relative path", p)
		}
		if oid == "" {
			return fmt.Errorf("dep %q has no blob OID", p)
		}
	}
	for p := range r.Methods {
		if !cleanRel(p) {
			return fmt.Errorf("method table of %q: not a clean repo-relative path", p)
		}
	}
	for _, p := range r.Reads {
		if !cleanRel(p) {
			return fmt.Errorf("read %q is not a clean repo-relative path", p)
		}
	}
	for _, p := range r.Dirs {
		if p != "." && !cleanRel(p) {
			return fmt.Errorf("directory %q is not a clean repo-relative path", p)
		}
	}
	for p, v := range r.LineCov {
		if !cleanRel(p) {
			return fmt.Errorf("line table of %q: not a clean repo-relative path", p)
		}
		if !lineTable(v) {
			return fmt.Errorf("line table of %q: malformed %q", p, v)
		}
	}
	if strings.ContainsAny(r.Runner, " \t\r\n") {
		return fmt.Errorf("runner %q is malformed", r.Runner)
	}
	return nil
}

// lineTable reports whether v has the LineCov form "<ranges>|<ranges>", each ranges a comma-separated list of "n" or "n-m".
func lineTable(v string) bool {
	a, b, ok := strings.Cut(v, "|")
	if !ok {
		return false
	}
	for _, rs := range []string{a, b} {
		if rs == "" {
			continue
		}
		for _, r := range strings.Split(rs, ",") {
			x, y, two := strings.Cut(r, "-")
			if !digits(x) || two && !digits(y) {
				return false
			}
		}
	}
	return true
}

func digits(s string) bool {
	if s == "" || len(s) > 9 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func cleanRel(p string) bool {
	return p != "" && p != "." && path.Clean(p) == p && !strings.HasPrefix(p, "../") && p != ".." && !strings.HasPrefix(p, "/") &&
		!strings.Contains(p, "\\") && !(len(p) > 1 && p[1] == ':')
}

// StorePath is the record store of ecosystem eco under the state directory (never inside the repo).
func StorePath(stateDir, eco string) string {
	return filepath.Join(stateDir, "dynamic", eco+".jsonl")
}

type key struct{ unit, env string }

// newer reports whether a should replace b (same unit and env): the later recording wins; ties go to a (the later line).
func newer(a, b Record) bool { return !a.Recorded.Before(b.Recorded) }

// Merge returns the union of old and add where, per (unit, env), the latest record wins. The result is sorted by eco, unit, env.
func Merge(old, add []Record) []Record {
	m := map[string]map[key]Record{}
	put := func(r Record) {
		byKey := m[r.Eco]
		if byKey == nil {
			byKey = map[key]Record{}
			m[r.Eco] = byKey
		}
		k := key{r.Unit, r.Env}
		if cur, ok := byKey[k]; !ok || newer(r, cur) {
			byKey[k] = r
		}
	}
	for _, r := range old {
		put(r)
	}
	for _, r := range add {
		put(r)
	}
	var out []Record
	for _, byKey := range m {
		for _, r := range byKey {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Record) int {
		if c := strings.Compare(a.Eco, b.Eco); c != 0 {
			return c
		}
		if c := strings.Compare(a.Unit, b.Unit); c != 0 {
			return c
		}
		return strings.Compare(a.Env, b.Env)
	})
	return out
}

// Load reads a JSONL record file. A missing file is no records and no warning. Unreadable files, corrupt or invalid lines are
// skipped with a warning: a broken store only means fewer records (less narrowing), never an error.
func Load(file string) ([]Record, []string) {
	b, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []string{fmt.Sprintf("dynamic records %s: %v (ignored)", file, err)}
	}
	return Parse(file, b)
}

// Parse decodes JSONL records (see Load).
func Parse(name string, b []byte) ([]Record, []string) {
	var out []Record
	var warn []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<30)
	n := 0
	bad := 0
	for sc.Scan() {
		n++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r Record
		err := json.Unmarshal(line, &r)
		if err == nil {
			err = r.valid()
		}
		if err != nil {
			if bad < 5 {
				warn = append(warn, fmt.Sprintf("dynamic records %s:%d: %v (line ignored)", name, n, err))
			}
			bad++
			continue
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		warn = append(warn, fmt.Sprintf("dynamic records %s: %v (rest of the file ignored)", name, err))
	}
	if bad > 5 {
		warn = append(warn, fmt.Sprintf("dynamic records %s: %d corrupt lines ignored in total", name, bad))
	}
	return out, warn
}

// Encode writes records as JSONL.
func Encode(recs []Record) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// Save atomically replaces file with recs (write to a temporary file in the same directory, then rename).
func Save(file string, recs []Record) error {
	b, err := Encode(recs)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, file); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// Add merges add into the store file (latest per (unit, env) wins) and rewrites it atomically. Warnings about the existing
// content (corrupt lines dropped) are returned.
func Add(file string, add []Record) ([]string, error) {
	for _, r := range add {
		if err := r.valid(); err != nil {
			return nil, fmt.Errorf("record %s: %w", r.Unit, err)
		}
	}
	old, warn := Load(file)
	return warn, Save(file, Merge(old, add))
}

// Fingerprint combines the parts of an environment into the fingerprint stored in records (docs/dynamic-layer.md §5): the collector,
// the interpreter description lines (OS, arch, version, installed distributions...; order-insensitive) and a digest of each runner
// configuration file (path → digest). Callers compute it the same way at record and select time. Version 2 dropped manifests and
// lockfiles; version 3 keeps only the [pytest] section of tox.ini.
func Fingerprint(collector string, interp []string, files map[string]string) string {
	h := sha256.New()
	fmt.Fprintf(h, "bgraph-env-3\ncollector %s\n", collector)
	lines := slices.Clone(interp)
	slices.Sort(lines)
	for _, l := range lines {
		fmt.Fprintf(h, "interp %q\n", l)
	}
	ps := make([]string, 0, len(files))
	for p := range files {
		ps = append(ps, p)
	}
	slices.Sort(ps)
	for _, p := range ps {
		fmt.Fprintf(h, "file %q %s\n", p, files[p])
	}
	return hex.EncodeToString(h.Sum(nil))
}

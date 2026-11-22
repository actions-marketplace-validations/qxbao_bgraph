// Package dynamic is bgraph's dynamic layer (docs/dynamic.md): per-unit records of what each test actually loaded or opened,
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

// Record is what one isolated run of one test unit saw (docs/dynamic.md §3).
type Record struct {
	Eco       string `json:"eco"`
	Unit      string `json:"unit"`      // unit ID (Python: the test file path)
	Collector string `json:"collector"` // collector name+version, e.g. "python-audit-1"
	Commit    string `json:"commit,omitempty"`
	// Deps: repo-relative file → git blob OID of the content the test saw. The unit's own files are always in it.
	Deps       map[string]string `json:"deps"`
	Env        string            `json:"env"` // environment fingerprint (docs/dynamic.md §5)
	Complete   bool              `json:"complete"`
	Incomplete string            `json:"incomplete,omitempty"` // why not complete
	Pass       bool              `json:"pass"`                 // the unit's run passed (exit 0)
	Recorded   time.Time         `json:"recorded"`
	// Toolchain: native addons of the Node toolchain allowlist the run loaded ("name@version", docs/dynamic.md §8.3), for
	// transparency only.
	Toolchain []string `json:"toolchain,omitempty"`
	// Classes: the test classes the runner reported for a JVM unit (the class-list record of docs/dynamic.md §9.1, whose Unit
	// is the module's unit ID and whose Deps are the files of its test roots).
	Classes []string `json:"classes,omitempty"`
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
	return nil
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

// Fingerprint combines the parts of an environment into the fingerprint stored in records (docs/dynamic.md §5): the collector,
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

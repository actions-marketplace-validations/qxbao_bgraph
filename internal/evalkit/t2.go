package evalkit

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/model"
)

// T2 (docs/evaluation.md): mutation oracle. Seeds ONE bug into one line of a source file at R (commit M = R + bug, built with a temporary index,
// no checkout), runs the whole unit-test suite on the buggy working tree, and collects the set of RED test files (minus files already broken/flaky at R). Every red file must be
// in the selection of Select(R→M). The oracle does NOT depend on bgraph; results are cached (oracle.jsonl) so later runs only re-run bgraph.

//go:embed mutate.cjs
var mutateScript []byte

// T2Repo is one repo in the T2 manifest.
type T2Repo struct {
	Name       string   `json:"name"`
	Dir        string   `json:"dir"`    // checkout directory, relative to the T2 root
	Remote     string   `json:"remote"` // documentation/clone only
	R          string   `json:"r"`      // base SHA (clean tree)
	Runner     string   `json:"runner"` // vitest | jest
	Bin        string   `json:"bin"`    // the runner's JS script, relative to the checkout, run with node
	Args       []string `json:"args"`   // arguments to run the whole test suite (before the reporter flag)
	NativeArgs []string `json:"native_args"`
	NativeNote string   `json:"native_note,omitempty"`
	Install    string   `json:"install,omitempty"`
	Covers     string   `json:"covers,omitempty"`
	Include    []string `json:"include"`
	Exclude    []string `json:"exclude"`
	Mutants    int      `json:"mutants"`
	Seed       string   `json:"seed"`
	Env        []string `json:"env,omitempty"`
	Eco        string   `json:"eco,omitempty"`     // "rust" for Cargo repos, "python" for pytest repos; empty = Node
	Tries      int      `json:"tries,omitempty"`   // rust/python: how many mutants are generated at most (default 3x Mutants); Mutants is the number of killed mutants aimed at
	Targets    []string `json:"targets,omitempty"` // dotnet: test project files, relative to the checkout (one `dotnet test` each; the unit is the project)
	Jdk        string   `json:"jdk,omitempty"`     // jvm: "17" (default) or "21"
	Venv       string   `json:"venv,omitempty"`    // python: virtual environment directory, relative to the T2 root (holds the repo's dependencies and the editable install)
	Spec       []string `json:"spec,omitempty"`    // mocha: spec paths of the whole suite (Args hold the other options)
}

// T2Manifest is the T2 repo manifest.
type T2Manifest struct {
	Version int      `json:"version"`
	TS      string   `json:"ts"` // typescript directory for the mutation generator, relative to the T2 root
	Notes   string   `json:"notes,omitempty"`
	Repos   []T2Repo `json:"repos"`
}

// LoadT2Manifest reads the T2 manifest.
func LoadT2Manifest(p string) (T2Manifest, error) {
	var m T2Manifest
	b, err := os.ReadFile(p)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// T2Mutant is one mutation: replaces bytes [Start,End) of the blob at R with Repl.
type T2Mutant struct {
	ID     string `json:"id"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Op     string `json:"op"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Repl   string `json:"repl"`
	Before string `json:"before"`
	After  string `json:"after"`
	Blob   string `json:"blob"` // blob OID of the file at R (detects a stale cache)
}

// T2Record is one line of oracle.jsonl.
type T2Record struct {
	Repo   string   `json:"repo"`
	R      string   `json:"r"`
	Mutant T2Mutant `json:"mutant"`
	Commit string   `json:"commit,omitempty"` // M
	// Status: killed | survived | timeout | error
	Status         string   `json:"status"`
	Red            []string `json:"red,omitempty"`             // red test files (excluded files already removed)
	Confirmed      bool     `json:"confirmed,omitempty"`       // Red was confirmed by re-running just the red files
	RedUnconfirmed []string `json:"red_unconfirmed,omitempty"` // red in the full run but not reproduced (load-induced flakiness)
	RedExcluded    []string `json:"red_excluded,omitempty"`    // red but belongs to files already broken/flaky
	Native         []string `json:"native,omitempty"`          // the original tool's "related" set
	NativeErr      string   `json:"native_err,omitempty"`
	Detail         string   `json:"detail,omitempty"`
	TestFiles      int      `json:"test_files,omitempty"`
	Ms             int64    `json:"ms,omitempty"`
}

func (r T2Record) key() string { return r.Repo + "|" + r.R + "|" + r.Mutant.ID }

// T2Baseline is the result of 3 runs at R.
type T2Baseline struct {
	Repo    string   `json:"repo"`
	R       string   `json:"r"`
	Files   int      `json:"files"`
	Failing []string `json:"failing"` // red in all 3 runs
	Flaky   []string `json:"flaky"`   // results differ between runs (or absent)
	RunMs   []int64  `json:"run_ms"`
	// FailingTests (Rust): for a unit red in both baseline runs, the test names failing in both. A mutant run turns such a unit red only when a test outside this set fails.
	FailingTests map[string][]string `json:"failing_tests,omitempty"`
	Tests        int                 `json:"tests,omitempty"`
	Known        []string            `json:"known,omitempty"` // Python: every test file seen in the baseline runs
}

func (b T2Baseline) excluded() map[string]bool {
	m := map[string]bool{}
	for _, f := range b.Failing {
		m[f] = true
	}
	for _, f := range b.Flaky {
		m[f] = true
	}
	return m
}

// ---- running processes ----

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return
	}
	cmd.Process.Kill()
}

// runProc runs name args in dir; on timeout kills the whole process tree. Returns stdout and the stderr tail.
func runProc(ctx context.Context, dir string, env []string, timeout time.Duration, name string, args ...string) (stdout string, tail string, timedOut bool, err error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err = cmd.Start(); err != nil {
		return "", "", false, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err = <-done:
	case <-t.C:
		timedOut = true
		killTree(cmd)
		err = <-done
	case <-ctx.Done():
		killTree(cmd)
		<-done
		return "", "", false, ctx.Err()
	}
	tail = errb.String()
	if len(tail) > 1500 {
		tail = tail[len(tail)-1500:]
	}
	return out.String(), tail, timedOut, err
}

// ---- reading runner results ----

type runJSON struct {
	TestResults []struct {
		Name             string `json:"name"`
		Status           string `json:"status"`
		AssertionResults []struct {
			Meta struct {
				Typecheck bool `json:"typecheck"`
			} `json:"meta"`
		} `json:"assertionResults"`
	} `json:"testResults"`
	NumTotalTests int `json:"numTotalTests"`
}

// relPath normalises an absolute runner path to a root-relative path with / separators.
func relPath(root, p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	root = strings.TrimSuffix(strings.ReplaceAll(root, "\\", "/"), "/")
	if len(p) > len(root) && strings.EqualFold(p[:len(root)], root) && p[len(root)] == '/' {
		return p[len(root)+1:]
	}
	return p
}

// ParseRunJSON reads a vitest/jest JSON report into a map file → passed|failed|skipped. vitest's typecheck entries are dropped.
// A file appearing several times (several projects) is red if any run is red.
func ParseRunJSON(root string, b []byte) (map[string]string, int, error) {
	var r runJSON
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, 0, err
	}
	if len(r.TestResults) == 0 {
		return nil, 0, fmt.Errorf("report has no testResults")
	}
	out := map[string]string{}
	for _, t := range r.TestResults {
		tc := false
		for _, a := range t.AssertionResults {
			if a.Meta.Typecheck {
				tc = true
			}
		}
		if tc {
			continue
		}
		f := relPath(root, t.Name)
		st := t.Status
		if st != "passed" && st != "failed" {
			st = "skipped"
		}
		switch prev := out[f]; {
		case prev == "failed":
		case st == "failed", prev == "", prev == "skipped":
			out[f] = st
		}
	}
	return out, r.NumTotalTests, nil
}

// runSuite runs the whole test suite and returns each file's status.
func (r T2Repo) runSuite(ctx context.Context, root, tmp string, timeout time.Duration, only []string) (map[string]string, int, time.Duration, bool, string, error) {
	if r.Runner == "mocha" {
		return r.runMocha(ctx, root, tmp, timeout, only)
	}
	os.MkdirAll(tmp, 0o755)
	of := filepath.Join(tmp, "run.json")
	os.Remove(of)
	args := []string{r.Bin}
	args = append(args, r.Args...)
	if r.Runner == "jest" {
		args = append(args, "--json", "--outputFile="+of)
		if len(only) > 0 {
			args = append(args, "--runTestsByPath")
		}
	} else {
		args = append(args, "--reporter=json", "--outputFile="+of)
	}
	args = append(args, only...)
	t0 := time.Now()
	_, tail, to, err := runProc(ctx, root, append([]string{"CI=1"}, r.Env...), timeout, "node", args...)
	d := time.Since(t0)
	if ctx.Err() != nil {
		return nil, 0, d, false, "", ctx.Err()
	}
	if to {
		return nil, 0, d, true, "timed out after " + timeout.String(), nil
	}
	b, rerr := os.ReadFile(of)
	if rerr != nil {
		return nil, 0, d, false, "no JSON report: " + fmt.Sprint(err) + " " + tail, nil
	}
	m, n, perr := ParseRunJSON(root, b)
	if perr != nil {
		return nil, 0, d, false, perr.Error(), nil
	}
	return m, n, d, false, "", nil
}

// runNative computes the original tool's "related" set for the current change in the working tree, without running tests.
func (r T2Repo) runNative(ctx context.Context, root, tmp, file string) ([]string, error) {
	if r.Runner == "mocha" {
		return nil, fmt.Errorf("mocha has no related-tests mode")
	}
	os.MkdirAll(tmp, 0o755)
	args := []string{r.Bin}
	args = append(args, r.NativeArgs...)
	var of string
	if r.Runner == "jest" {
		args = append(args, file)
	} else {
		of = filepath.Join(tmp, "native.json")
		os.Remove(of)
		args = append(args, "--json", of)
	}
	out, tail, to, err := runProc(ctx, root, append([]string{"CI=1"}, r.Env...), 10*time.Minute, "node", args...)
	if to {
		return nil, fmt.Errorf("native timed out")
	}
	var files []string
	if r.Runner == "jest" {
		if err != nil {
			return nil, fmt.Errorf("jest: %v %s", err, tail)
		}
		sc := bufio.NewScanner(strings.NewReader(out))
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			if l != "" {
				files = append(files, relPath(root, l))
			}
		}
	} else {
		b, rerr := os.ReadFile(of)
		if rerr != nil {
			return nil, fmt.Errorf("vitest list: %v %s", err, tail)
		}
		var items []struct {
			File string `json:"file"`
		}
		if err := json.Unmarshal(b, &items); err != nil {
			return nil, err
		}
		for _, it := range items {
			files = append(files, relPath(root, it.File))
		}
	}
	sort.Strings(files)
	return dedup(files), nil
}

func dedup(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// ---- kho oracle ----

// OracleStore is oracle.jsonl: a line is appended as each mutation finishes; on read, later lines win.
type OracleStore struct {
	Path string
	mu   sync.Mutex
}

// Load reads all records (a later line overrides an earlier one with the same key), in order of first appearance.
func (s *OracleStore) Load() ([]T2Record, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	idx := map[string]int{}
	var out []T2Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var r T2Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("oracle.jsonl: %w", err)
		}
		if i, ok := idx[r.key()]; ok {
			out[i] = r
		} else {
			idx[r.key()] = len(out)
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// Append writes one more record.
func (s *OracleStore) Append(r T2Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	_, err = f.Write(append(b, '\n'))
	return err
}

// ---- building mutations ----

// ApplyEdit applies a mutation to blob content.
func ApplyEdit(blob []byte, m T2Mutant) ([]byte, error) {
	if m.Start < 0 || m.End < m.Start || m.End > len(blob) {
		return nil, fmt.Errorf("mutant %s is outside the blob range", m.ID)
	}
	out := make([]byte, 0, len(blob)+len(m.Repl))
	out = append(out, blob[:m.Start]...)
	out = append(out, m.Repl...)
	return append(out, blob[m.End:]...), nil
}

func (r T2Repo) blob(ctx context.Context, root, file string) ([]byte, string, error) {
	oid, err := git(ctx, root, "rev-parse", r.R+":"+file)
	if err != nil {
		return nil, "", err
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", strings.TrimSpace(oid))
	cmd.Dir = root
	b, err := cmd.Output()
	return b, strings.TrimSpace(oid), err
}

// MutantCommit builds commit M = R + mutation with a temporary index (no checkout). Deterministic thanks to the hermetic git environment.
func (r T2Repo) MutantCommit(ctx context.Context, root, idxDir string, m T2Mutant) (commit string, mutated []byte, err error) {
	blob, oid, err := r.blob(ctx, root, m.File)
	if err != nil {
		return "", nil, err
	}
	if oid != m.Blob {
		return "", nil, fmt.Errorf("%s: blob at R is %s, mutant was generated for %s (stale cache)", m.File, oid, m.Blob)
	}
	mutated, err = ApplyEdit(blob, m)
	if err != nil {
		return "", nil, err
	}
	os.MkdirAll(idxDir, 0o755)
	ib := newIndexBuilder(root, filepath.Join(idxDir, "mut-"+m.ID+".index"), r.R)
	defer ib.Close()
	c, err := ib.SetFile(ctx, m.File, mutated, "eval: mutant "+m.ID)
	return c, mutated, err
}

// GenMutants runs the deterministic mutation generator (node + typescript).
func (r T2Repo) GenMutants(ctx context.Context, t2Dir string, ts string, root string) ([]T2Mutant, error) {
	sdir := filepath.Join(t2Dir, "scripts")
	os.MkdirAll(sdir, 0o755)
	sp := filepath.Join(sdir, "mutate.cjs")
	if err := os.WriteFile(sp, mutateScript, 0o644); err != nil {
		return nil, err
	}
	cfg := map[string]any{"ts": filepath.ToSlash(filepath.Join(t2Dir, ts)), "repo": filepath.ToSlash(root), "rev": r.R, "include": r.Include, "exclude": r.Exclude, "count": r.Mutants, "seed": r.Seed}
	cb, _ := json.Marshal(cfg)
	cp := filepath.Join(sdir, "gen-"+r.Dir+".json")
	if err := os.WriteFile(cp, cb, 0o644); err != nil {
		return nil, err
	}
	out, tail, _, err := runProc(ctx, t2Dir, nil, 10*time.Minute, "node", sp, "@"+cp)
	if err != nil {
		return nil, fmt.Errorf("mutant generation: %v %s", err, tail)
	}
	var ms []T2Mutant
	if err := json.Unmarshal([]byte(out), &ms); err != nil {
		return nil, err
	}
	for i := range ms {
		ms[i].ID = fmt.Sprintf("%s-%02d", r.Dir, i+1)
	}
	return ms, nil
}

// ---- building the oracle ----

// T2Config configures a T2 run.
type T2Config struct {
	Dir        string // T2 root directory (checkout, tools, state, oracle.jsonl)
	Out        string // report directory
	Manifest   T2Manifest
	Repos      []string // empty = all
	RunTests   bool     // build/refresh the oracle (run tests)
	Rebaseline bool
	Rebuild    bool      // drop the repo's oracle cache, re-run every mutation
	Reconfirm  bool      // re-confirm killed mutations not yet confirmed (re-run just the red files), without re-running the full suite
	Deadline   time.Time // rust: stop starting new mutants after this time (zero = no limit)
	Log        func(format string, a ...any)
}

func (c *T2Config) logf(f string, a ...any) {
	if c.Log != nil {
		c.Log(f, a...)
	}
}

func (c *T2Config) oraclePath() string { return filepath.Join(c.Dir, "oracle.jsonl") }

func (c *T2Config) selected() []T2Repo {
	var out []T2Repo
	for _, r := range c.Manifest.Repos {
		if len(c.Repos) == 0 {
			out = append(out, r)
			continue
		}
		for _, n := range c.Repos {
			if strings.EqualFold(n, r.Name) || strings.EqualFold(n, r.Dir) {
				out = append(out, r)
			}
		}
	}
	return out
}

func medianInt64(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func (r T2Repo) ensureClean(ctx context.Context, root string) error {
	h, err := git(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(h) != r.R {
		return fmt.Errorf("%s: HEAD %s differs from R %s", r.Name, strings.TrimSpace(h), r.R)
	}
	// Restore the tree (a previous run may have been interrupted midway).
	if _, err := git(ctx, root, "checkout", "-q", "--", "."); err != nil {
		return err
	}
	st, err := git(ctx, root, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if strings.TrimSpace(st) != "" {
		return fmt.Errorf("%s: working tree is not clean: %.200s", r.Name, st)
	}
	return nil
}

// Baseline runs the test suite 3 times at R; a file red in all 3 runs = already broken, inconsistent results = flaky.
func (r T2Repo) Baseline(ctx context.Context, cfg *T2Config, root string) (T2Baseline, error) {
	bp := filepath.Join(cfg.Dir, "baseline-"+r.Dir+".json")
	if !cfg.Rebaseline {
		if b, err := os.ReadFile(bp); err == nil {
			var bl T2Baseline
			if json.Unmarshal(b, &bl) == nil && bl.R == r.R {
				return bl, nil
			}
		}
	}
	bl := T2Baseline{Repo: r.Name, R: r.R}
	var runs []map[string]string
	for i := 0; i < 3; i++ {
		m, n, d, to, msg, err := r.runSuite(ctx, root, filepath.Join(cfg.Dir, "tmp", r.Dir), 20*time.Minute, nil)
		if err != nil {
			return bl, err
		}
		if to || m == nil {
			return bl, fmt.Errorf("%s: baseline run %d failed: %s", r.Name, i+1, msg)
		}
		cfg.logf("%s: baseline %d/3 %d files, %s", r.Name, i+1, len(m), d.Round(time.Second))
		runs = append(runs, m)
		bl.RunMs = append(bl.RunMs, d.Milliseconds())
		bl.Tests = n
	}
	all := map[string]bool{}
	for _, m := range runs {
		for f := range m {
			all[f] = true
		}
	}
	for f := range all {
		nf, np, nm := 0, 0, 0
		for _, m := range runs {
			switch m[f] {
			case "failed":
				nf++
			case "passed":
				np++
			default:
				nm++
			}
		}
		switch {
		case nf == 3:
			bl.Failing = append(bl.Failing, f)
		case nf > 0 || (np > 0 && nm > 0):
			bl.Flaky = append(bl.Flaky, f)
		}
	}
	sort.Strings(bl.Failing)
	sort.Strings(bl.Flaky)
	bl.Files = len(all)
	b, _ := json.MarshalIndent(bl, "", " ")
	os.WriteFile(bp, b, 0o644)
	return bl, nil
}

// BuildOracle runs the mutations not yet in oracle.jsonl (sequential within a repo: the working tree holds only one mutation at a time).
func (r T2Repo) BuildOracle(ctx context.Context, cfg *T2Config, store *OracleStore, have map[string]T2Record) error {
	switch r.Eco {
	case "rust":
		return r.buildOracleRust(ctx, cfg, store, have)
	case "python":
		return r.buildOraclePython(ctx, cfg, store, have)
	case "jvm", "dotnet":
		return r.buildOracleJD(ctx, cfg, store, have)
	case "go":
		return r.buildOracleGo(ctx, cfg, store, have)
	case "swift":
		return r.buildOracleSwift(ctx, cfg, store, have)
	case "php":
		return r.buildOraclePHP(ctx, cfg, store, have)
	}
	root := filepath.Join(cfg.Dir, r.Dir)
	if err := r.ensureClean(ctx, root); err != nil {
		return err
	}
	bl, err := r.Baseline(ctx, cfg, root)
	if err != nil {
		return err
	}
	cfg.logf("%s: baseline %d test files, %d pre-broken, %d flaky", r.Name, bl.Files, len(bl.Failing), len(bl.Flaky))
	excl := bl.excluded()
	mp := filepath.Join(cfg.Dir, "mutants-"+r.Dir+".json")
	var muts []T2Mutant
	if b, err := os.ReadFile(mp); err == nil {
		var c struct {
			R       string     `json:"r"`
			Seed    string     `json:"seed"`
			Count   int        `json:"count"`
			Mutants []T2Mutant `json:"mutants"`
		}
		if json.Unmarshal(b, &c) == nil && c.R == r.R && c.Seed == r.Seed && c.Count == r.Mutants {
			muts = c.Mutants
		}
	}
	if muts == nil {
		muts, err = r.GenMutants(ctx, cfg.Dir, cfg.Manifest.TS, root)
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(map[string]any{"r": r.R, "seed": r.Seed, "count": r.Mutants, "mutants": muts}, "", " ")
		os.WriteFile(mp, b, 0o644)
	}
	cfg.logf("%s: %d mutants", r.Name, len(muts))
	var med []int64
	med = append(med, bl.RunMs...)
	timeout := time.Duration(medianInt64(med))*4*time.Millisecond + 2*time.Minute
	for _, m := range muts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rec := T2Record{Repo: r.Name, R: r.R, Mutant: m}
		if old, ok := have[rec.key()]; ok && !cfg.Rebuild && (old.Status == "killed" || old.Status == "survived" || old.Status == "timeout") {
			continue
		}
		t0 := time.Now()
		c, mutated, err := r.MutantCommit(ctx, root, filepath.Join(cfg.Dir, "idx"), m)
		if err != nil {
			rec.Status, rec.Detail = "error", err.Error()
			store.Append(rec)
			continue
		}
		rec.Commit = c
		wt := filepath.Join(root, filepath.FromSlash(m.File))
		orig, err := os.ReadFile(wt)
		if err != nil {
			rec.Status, rec.Detail = "error", err.Error()
			store.Append(rec)
			continue
		}
		func() {
			defer func() {
				os.WriteFile(wt, orig, 0o644)
				git(context.Background(), root, "checkout", "-q", "--", m.File)
			}()
			if err := os.WriteFile(wt, mutated, 0o644); err != nil {
				rec.Status, rec.Detail = "error", err.Error()
				return
			}
			res, _, _, to, msg, err := r.runSuite(ctx, root, filepath.Join(cfg.Dir, "tmp", r.Dir), timeout, nil)
			if err != nil {
				rec.Status, rec.Detail = "error", err.Error()
				return
			}
			if to {
				rec.Status, rec.Detail = "timeout", msg
				return
			}
			if res == nil {
				rec.Status, rec.Detail = "error", msg
				return
			}
			rec.TestFiles = len(res)
			for f, st := range res {
				if st != "failed" {
					continue
				}
				if excl[f] {
					rec.RedExcluded = append(rec.RedExcluded, f)
				} else {
					rec.Red = append(rec.Red, f)
				}
			}
			sort.Strings(rec.RedExcluded)
			if err := r.confirmRed(ctx, root, filepath.Join(cfg.Dir, "tmp", r.Dir), timeout, &rec); err != nil {
				rec.Status, rec.Detail = "error", err.Error()
				return
			}
			rec.Status = "survived"
			if len(rec.Red) > 0 {
				rec.Status = "killed"
			}
			rec.Native, err = r.runNative(ctx, root, filepath.Join(cfg.Dir, "tmp", r.Dir), m.File)
			if err != nil {
				rec.NativeErr = err.Error()
			}
		}()
		rec.Ms = time.Since(t0).Milliseconds()
		if err := store.Append(rec); err != nil {
			return err
		}
		cfg.logf("%s: %s %s %s:%d -> %s (red %d, native %d) %s", r.Name, m.ID, m.Op, m.File, m.Line, rec.Status, len(rec.Red), len(rec.Native), time.Duration(rec.Ms)*time.Millisecond)
	}
	return r.ensureClean(ctx, root)
}

// ---- checking bgraph ----

// T2Check is the result of checking one mutation against bgraph.
type T2Check struct {
	Rec        T2Record         `json:"rec"`
	Scope      string           `json:"scope,omitempty"`
	Selected   int              `json:"selected"`
	Units      int              `json:"units"`
	Missed     []string         `json:"missed,omitempty"`      // red files outside the selection (violation)
	NativeOnly []string         `json:"native_only,omitempty"` // original-tool files absent from bgraph's selection
	Widenings  map[string]int   `json:"widenings,omitempty"`
	WideList   []model.Widening `json:"widening_list,omitempty"`
	Diag       []FileDiag       `json:"diag,omitempty"`    // diagnosis of missing (violation) or native-only files
	Near       []string         `json:"near,omitempty"`    // reason chains of the tests near the missing file
	Changed    []string         `json:"changed,omitempty"` // rust/python: files bgraph saw as changed
	Py         *PyWide          `json:"py,omitempty"`      // python: where the selection came from (graph, unresolved/dynamic seeds, widenings)
	Err        string           `json:"err,omitempty"`
	Ms         int64            `json:"ms"`
}

func (c T2Check) Full() bool { return c.Scope == string(model.TierFull) }

// selCovers reports whether the selection contains test file f: it is the Files/ID of a unit, or a Whole unit whose project encloses f.
func selCovers(sel model.Selection, f string) bool {
	for _, u := range sel.Tests {
		if u.Unit == f {
			return true
		}
		for _, uf := range u.Files {
			if uf == f || under(f, uf) || uf == "." { // a directory unit (Python collection hooks) covers its files
				return true
			}
		}
		if u.Whole && (u.Project == "." || u.Project == "" || under(f, u.Project)) {
			return true
		}
	}
	return false
}

// CheckMutant runs Select(R→M) and compares with the oracle.
func (r T2Repo) CheckMutant(ctx context.Context, ix *bgraph.Index, root, idxDir string, rec T2Record) T2Check {
	ck := T2Check{Rec: rec}
	t0 := time.Now()
	defer func() { ck.Ms = time.Since(t0).Milliseconds() }()
	m, _, err := r.MutantCommit(ctx, root, idxDir, rec.Mutant)
	if err != nil {
		ck.Err = err.Error()
		return ck
	}
	sel, err := ix.Select(ctx, bgraph.Change{Base: r.R, Head: m})
	if err != nil {
		ck.Err = err.Error()
		return ck
	}
	ck.Scope = string(sel.Scope)
	ck.Selected = len(sel.Tests)
	ck.Units = sel.IndexStats["units"]
	ck.Widenings = map[string]int{}
	for _, w := range sel.Widening {
		ck.Widenings[string(w.Tier)+":"+w.Reason]++
	}
	if r.Eco == "python" {
		ck.Py = pySelStats(sel)
	}
	ck.WideList = sel.Widening
	if len(ck.WideList) > 12 {
		ck.WideList = ck.WideList[:12]
	}
	if ck.Full() {
		return ck
	}
	for _, f := range rec.Red {
		if !r.coversRed(sel, f) {
			ck.Missed = append(ck.Missed, f)
		}
	}
	for _, f := range rec.Native {
		if !selCovers(sel, f) {
			ck.NativeOnly = append(ck.NativeOnly, f)
		}
	}
	switch r.Eco {
	case "rust":
		ck.Changed = sel.Changed
		ck.Near = rustNear(sel, ck.Missed)
		return ck
	case "python":
		ck.Changed = sel.Changed
		ck.Near = pyNear(sel, ck.Missed)
		return ck
	case "jvm", "dotnet":
		ck.Changed = sel.Changed
		ck.Near = jdNear(sel, ck.Missed)
		return ck
	case "go":
		ck.Changed = sel.Changed
		ck.Near = goNear(sel, ck.Missed)
		return ck
	case "php", "swift":
		ck.Changed = sel.Changed
		return ck
	}
	diagFor := ck.Missed
	if len(diagFor) == 0 && len(ck.NativeOnly) > 0 {
		diagFor = ck.NativeOnly
		if len(diagFor) > 5 {
			diagFor = diagFor[:5]
		}
	}
	if len(diagFor) > 0 {
		ck.Diag = diagnose(ctx, root, r.R, diagFor, sel.Changed)
	}
	if len(ck.Missed) > 0 {
		for _, f := range ck.Missed {
			dir := path.Dir(f)
			n := 0
			for _, u := range sel.Tests {
				if n < 4 && (path.Dir(u.Unit) == dir || strings.HasPrefix(u.Unit, path.Dir(dir)+"/")) && len(u.Reasons) > 0 {
					ck.Near = append(ck.Near, u.Unit+": "+strings.Join(u.Reasons, " | "))
					n++
				}
			}
		}
	}
	return ck
}

// T2RepoResult is the result for one repo.
type T2RepoResult struct {
	Repo                 T2Repo     `json:"repo"`
	Baseline             T2Baseline `json:"baseline"`
	Checks               []T2Check  `json:"checks"`
	Killed               int        `json:"killed"`
	Survived             int        `json:"survived"`
	Timeout              int        `json:"timeout"`
	NoCompile            int        `json:"no_compile,omitempty"` // rust: mutants that do not compile (excluded, not counted as killed)
	Generated            int        `json:"generated,omitempty"`  // rust: mutants generated
	Errors               int        `json:"errors"`
	Full                 int        `json:"full"` // mutations widened to full (evidence not counted)
	Violations           []T2Check  `json:"violations"`
	NativeNotSubset      []T2Check  `json:"native_not_subset"`
	MedBgraph, MedNative float64
	OracleMs, CheckMs    int64
	Error                string `json:"error,omitempty"`
}

// T2Result is the result for all repos.
type T2Result struct {
	Repos      []T2RepoResult `json:"repos"`
	Wall       time.Duration  `json:"wall"`
	RunTests   bool           `json:"run_tests"`
	Violations int            `json:"violations"`
}

// RunT2 builds the oracle (if RunTests) then checks bgraph against the cached oracle. Repos run in parallel.
func RunT2(ctx context.Context, cfg T2Config) (T2Result, error) {
	start := time.Now()
	store := &OracleStore{Path: cfg.oraclePath()}
	repos := cfg.selected()
	res := T2Result{RunTests: cfg.RunTests, Repos: make([]T2RepoResult, len(repos))}
	have := map[string]T2Record{}
	if recs, err := store.Load(); err == nil {
		for _, r := range recs {
			have[r.key()] = r
		}
	}
	var wg sync.WaitGroup
	for i, r := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := &res.Repos[i]
			rr.Repo = r
			root := filepath.Join(cfg.Dir, r.Dir)
			if cfg.Reconfirm {
				if err := r.Reconfirm(ctx, &cfg, store, have); err != nil {
					rr.Error = err.Error()
					return
				}
			}
			if cfg.RunTests {
				t0 := time.Now()
				if err := r.BuildOracle(ctx, &cfg, store, have); err != nil {
					rr.Error = err.Error()
					cfg.logf("%s: oracle error: %v", r.Name, err)
					return
				}
				rr.OracleMs = time.Since(t0).Milliseconds()
			}
			if b, err := os.ReadFile(filepath.Join(cfg.Dir, "baseline-"+r.Dir+".json")); err == nil {
				json.Unmarshal(b, &rr.Baseline)
			}
			recs, err := store.Load()
			if err != nil {
				rr.Error = "no oracle yet: " + err.Error() + " (run `bgraph-eval t2 --run-tests`)"
				return
			}
			var mine []T2Record
			for _, x := range recs {
				if x.Repo == r.Name && x.R == r.R {
					mine = append(mine, x)
				}
			}
			if len(mine) == 0 {
				rr.Error = "oracle has no mutants for " + r.Name + "@" + r.R
				return
			}
			t0 := time.Now()
			r.checkAll(ctx, &cfg, root, mine, rr)
			rr.CheckMs = time.Since(t0).Milliseconds()
		}()
	}
	wg.Wait()
	for _, r := range res.Repos {
		res.Violations += len(r.Violations)
	}
	res.Wall = time.Since(start)
	return res, ctx.Err()
}

func (r T2Repo) checkAll(ctx context.Context, cfg *T2Config, root string, recs []T2Record, rr *T2RepoResult) {
	ix, err := bgraph.Open(ctx, root, bgraph.Options{StateDir: filepath.Join(cfg.Dir, "state", r.Dir)})
	if err != nil {
		rr.Error = err.Error()
		return
	}
	defer ix.Close()
	if r.Eco == "rust" || r.Eco == "python" || r.Eco == "jvm" || r.Eco == "dotnet" || r.Eco == "go" || r.Eco == "swift" {
		if b, err := os.ReadFile(filepath.Join(cfg.Dir, "mutants-"+r.Dir+".json")); err == nil {
			var c struct {
				Mutants []T2Mutant `json:"mutants"`
			}
			if json.Unmarshal(b, &c) == nil {
				rr.Generated = len(c.Mutants)
			}
		}
	}
	if r.Eco == "swift" {
		recs, err = r.swiftCachedMutants(cfg, recs)
		if err != nil {
			rr.Error = err.Error()
			return
		}
	}
	var bg, nv []float64
	for _, rec := range recs {
		switch rec.Status {
		case "killed":
			rr.Killed++
		case "survived":
			rr.Survived++
		case "timeout":
			rr.Timeout++
			continue
		case "nocompile":
			rr.NoCompile++
			continue
		default:
			rr.Errors++
			continue
		}
		c := r.CheckMutant(ctx, ix, root, filepath.Join(cfg.Dir, "idx"), rec)
		rr.Checks = append(rr.Checks, c)
		switch {
		case c.Err != "":
			rr.Errors++
		case c.Full():
			rr.Full++
		default:
			if c.Units > 0 {
				bg = append(bg, float64(c.Selected)/float64(c.Units))
				if rec.NativeErr == "" {
					nv = append(nv, float64(len(rec.Native))/float64(c.Units))
				}
			}
		}
		if len(c.Missed) > 0 {
			rr.Violations = append(rr.Violations, c)
		}
		if len(c.NativeOnly) > 0 {
			rr.NativeNotSubset = append(rr.NativeNotSubset, c)
		}
	}
	rr.MedBgraph, rr.MedNative = Percentile(bg, 50), Percentile(nv, 50)
}

// confirmRed confirms the files red in the full run: re-runs JUST those files (2 runs, on the tree with the mutation); only files red in both runs
// are red. A file red once then green (timeout from machine load, run order...) goes to RedUnconfirmed and is not used as oracle.
func (r T2Repo) confirmRed(ctx context.Context, root, tmp string, timeout time.Duration, rec *T2Record) error {
	cand := append(append([]string(nil), rec.Red...), rec.RedUnconfirmed...)
	sort.Strings(cand)
	cand = dedup(cand)
	rec.Red, rec.RedUnconfirmed, rec.Confirmed = nil, nil, true
	if len(cand) == 0 {
		return nil
	}
	still := map[string]bool{}
	for _, f := range cand {
		still[f] = true
	}
	for round := 0; round < 2; round++ {
		res, _, _, to, msg, err := r.runSuite(ctx, root, tmp, timeout, cand)
		if err != nil {
			return err
		}
		if to || res == nil {
			return fmt.Errorf("confirmation run %d failed: %s", round+1, msg)
		}
		for f := range still {
			if res[f] != "failed" {
				delete(still, f)
			}
		}
	}
	for _, f := range cand {
		if still[f] {
			rec.Red = append(rec.Red, f)
		} else {
			rec.RedUnconfirmed = append(rec.RedUnconfirmed, f)
		}
	}
	return nil
}

// Reconfirm re-confirms "killed" mutations not yet confirmed in the oracle cache (does not re-run the full suite).
func (r T2Repo) Reconfirm(ctx context.Context, cfg *T2Config, store *OracleStore, have map[string]T2Record) error {
	root := filepath.Join(cfg.Dir, r.Dir)
	if err := r.ensureClean(ctx, root); err != nil {
		return err
	}
	bl, err := r.Baseline(ctx, cfg, root)
	if err != nil {
		return err
	}
	timeout := time.Duration(medianInt64(bl.RunMs))*4*time.Millisecond + 2*time.Minute
	var recs []T2Record
	for _, x := range have {
		if x.Repo == r.Name && x.R == r.R && x.Status == "killed" && !x.Confirmed {
			recs = append(recs, x)
		}
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Mutant.ID < recs[j].Mutant.ID })
	for _, rec := range recs {
		_, mutated, err := r.MutantCommit(ctx, root, filepath.Join(cfg.Dir, "idx"), rec.Mutant)
		if err != nil {
			return err
		}
		wt := filepath.Join(root, filepath.FromSlash(rec.Mutant.File))
		orig, err := os.ReadFile(wt)
		if err != nil {
			return err
		}
		before := len(rec.Red)
		err = func() error {
			defer func() {
				os.WriteFile(wt, orig, 0o644)
				git(context.Background(), root, "checkout", "-q", "--", rec.Mutant.File)
			}()
			if err := os.WriteFile(wt, mutated, 0o644); err != nil {
				return err
			}
			return r.confirmRed(ctx, root, filepath.Join(cfg.Dir, "tmp", r.Dir), timeout, &rec)
		}()
		if err != nil {
			cfg.logf("%s: %s confirmation failed: %v", r.Name, rec.Mutant.ID, err)
			continue
		}
		rec.Status = "survived"
		if len(rec.Red) > 0 {
			rec.Status = "killed"
		}
		if err := store.Append(rec); err != nil {
			return err
		}
		cfg.logf("%s: %s confirmation: red %d -> %d (not reproduced: %d) %s", r.Name, rec.Mutant.ID, before, len(rec.Red), len(rec.RedUnconfirmed), rec.Status)
	}
	return r.ensureClean(ctx, root)
}

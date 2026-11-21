package evalkit

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/model"
)

// Python T2: the mutation oracle for pytest projects. One-token bugs are seeded into non-test Python source at R, the whole suite
// (`python -m pytest`, from the repo's own virtual environment) runs on the mutated working tree, and every red test FILE must be in
// Select(R->M). The unit is the pytest test file, exactly as bgraph models it.

// pyPython returns the interpreter of the repo's virtual environment.
func (r T2Repo) pyPython(cfg *T2Config) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(cfg.Dir, r.Venv, "Scripts", "python.exe")
	}
	return filepath.Join(cfg.Dir, r.Venv, "bin", "python")
}

type pyCase struct {
	Name      string `xml:"name,attr"`
	Classname string `xml:"classname,attr"`
	File      string `xml:"file,attr"`
	Failure   []struct {
		Message string `xml:"message,attr"`
	} `xml:"failure"`
	Error []struct {
		Message string `xml:"message,attr"`
	} `xml:"error"`
	Skipped []struct{} `xml:"skipped"`
}

type pySuiteXML struct {
	Suites []pySuiteXML `xml:"testsuite"`
	Cases  []pyCase     `xml:"testcase"`
}

// PyRun is the parsed outcome of one pytest invocation.
type PyRun struct {
	Files   map[string]string // test file -> passed | failed | skipped
	Session bool              // pytest produced no report (conftest or startup failure): every file counts as red
	Detail  string
}

// parsePyJUnit maps a junit report (junit_family=xunit1, which carries the `file` attribute) to per-file states. A file is red when any of its
// testcases failed or errored; a collection error is such an error. known is the set of test files of the project; a testcase whose file
// cannot be determined and that failed is an error (it would otherwise be silently dropped from the oracle).
func parsePyJUnit(root string, b []byte, known map[string]bool) (PyRun, error) {
	var top pySuiteXML
	if err := xml.NewDecoder(bytes.NewReader(b)).Decode(&top); err != nil {
		return PyRun{}, fmt.Errorf("junit: %w", err)
	}
	run := PyRun{Files: map[string]string{}}
	var walk func(s pySuiteXML) error
	walk = func(s pySuiteXML) error {
		for _, c := range s.Cases {
			f := pyCaseFile(root, c, known)
			st := "passed"
			switch {
			case len(c.Failure)+len(c.Error) > 0:
				st = "failed"
			case len(c.Skipped) > 0:
				st = "skipped"
			}
			if f == "" {
				if st == "failed" {
					return fmt.Errorf("failing testcase %q (classname %q) cannot be mapped to a test file", c.Name, c.Classname)
				}
				continue
			}
			switch prev := run.Files[f]; {
			case prev == "failed":
			case st == "failed", prev == "", prev == "skipped":
				run.Files[f] = st
			}
		}
		for _, x := range s.Suites {
			if err := walk(x); err != nil {
				return err
			}
		}
		return nil
	}
	return run, walk(top)
}

func pyCaseFile(root string, c pyCase, known map[string]bool) string {
	if c.File != "" {
		f := relPath(root, c.File)
		if known == nil || known[f] {
			return f
		}
		return ""
	}
	// classname "pkg.module.Class": the longest dotted prefix that is a known file
	parts := strings.Split(c.Classname, ".")
	for n := len(parts); n > 0; n-- {
		f := strings.Join(parts[:n], "/") + ".py"
		if known[f] {
			return f
		}
	}
	return ""
}

// pySuite runs the whole suite (only == nil) or just the given test files. known is the set of test files of the project (nil for the first
// baseline run, which discovers it). A run without a report is a session failure unless known is nil.
func (r T2Repo) pySuite(ctx context.Context, cfg *T2Config, root string, timeout time.Duration, only []string, known map[string]bool) (PyRun, time.Duration, bool, error) {
	tmp := filepath.Join(cfg.Dir, "tmp", r.Dir)
	os.MkdirAll(tmp, 0o755)
	of := filepath.Join(tmp, "junit.xml")
	os.Remove(of)
	args := append([]string{"-m", "pytest"}, r.Args...)
	args = append(args, "--junitxml="+of)
	args = append(args, only...)
	t0 := time.Now()
	_, tail, to, err := runProc(ctx, root, append([]string{"PYTHONDONTWRITEBYTECODE=1", "PYTHONUTF8=1"}, r.Env...), timeout, r.pyPython(cfg), args...)
	d := time.Since(t0)
	if ctx.Err() != nil {
		return PyRun{}, d, false, ctx.Err()
	}
	if to {
		return PyRun{}, d, true, nil
	}
	b, rerr := os.ReadFile(of)
	if rerr != nil {
		if known == nil {
			return PyRun{}, d, false, fmt.Errorf("pytest wrote no report: %v %s", err, tail)
		}
		run := PyRun{Files: map[string]string{}, Session: true, Detail: "no junit report: " + tail}
		if only != nil {
			for _, f := range only {
				run.Files[f] = "failed"
			}
		} else {
			for f := range known {
				run.Files[f] = "failed"
			}
		}
		return run, d, false, nil
	}
	run, perr := parsePyJUnit(root, b, known)
	if perr != nil {
		return run, d, false, perr
	}
	if len(run.Files) == 0 {
		run.Detail = "no test file ran: " + tail
	}
	return run, d, false, nil
}

// pyBaseline runs the suite twice at R; a file red in both runs is already broken, one whose result differs (or is absent in one run) is flaky.
func (r T2Repo) pyBaseline(ctx context.Context, cfg *T2Config, root string) (T2Baseline, map[string]bool, error) {
	bp := filepath.Join(cfg.Dir, "baseline-"+r.Dir+".json")
	if !cfg.Rebaseline {
		if b, err := os.ReadFile(bp); err == nil {
			var bl T2Baseline
			if json.Unmarshal(b, &bl) == nil && bl.R == r.R && len(bl.Known) > 0 {
				known := map[string]bool{}
				for _, f := range bl.Known {
					known[f] = true
				}
				return bl, known, nil
			}
		}
	}
	bl := T2Baseline{Repo: r.Name, R: r.R}
	var runs []map[string]string
	for i := 0; i < 2; i++ {
		run, d, to, err := r.pySuite(ctx, cfg, root, 30*time.Minute, nil, nil)
		if err != nil {
			return bl, nil, err
		}
		if to || len(run.Files) == 0 {
			return bl, nil, fmt.Errorf("%s: baseline run %d failed (timeout=%v): %s", r.Name, i+1, to, run.Detail)
		}
		cfg.logf("%s: baseline %d/2 %d test files, %s", r.Name, i+1, len(run.Files), d.Round(time.Second))
		runs = append(runs, run.Files)
		bl.RunMs = append(bl.RunMs, d.Milliseconds())
	}
	all := map[string]bool{}
	for _, m := range runs {
		for u := range m {
			all[u] = true
		}
	}
	for u := range all {
		a, b := runs[0][u], runs[1][u]
		switch {
		case a == "failed" && b == "failed":
			bl.Failing = append(bl.Failing, u)
		case a != b || a == "failed":
			bl.Flaky = append(bl.Flaky, u)
		}
	}
	for u := range all {
		bl.Known = append(bl.Known, u)
	}
	sort.Strings(bl.Failing)
	sort.Strings(bl.Flaky)
	sort.Strings(bl.Known)
	bl.Files = len(all)
	b, _ := json.MarshalIndent(bl, "", " ")
	os.WriteFile(bp, b, 0o644)
	return bl, all, nil
}

// pyCompiles reports whether src is valid Python (so that a mutant that breaks the syntax is not mistaken for a kill).
func (r T2Repo) pyCompiles(ctx context.Context, cfg *T2Config, src []byte) (bool, string) {
	cmd := exec.CommandContext(ctx, r.pyPython(cfg), "-c", "import sys; compile(sys.stdin.buffer.read(), 'mutant', 'exec')")
	cmd.Stdin = bytes.NewReader(src)
	var eb bytes.Buffer
	cmd.Stderr = &eb
	if err := cmd.Run(); err != nil {
		l := strings.Split(strings.TrimSpace(eb.String()), "\n")
		return false, l[len(l)-1]
	}
	return true, ""
}

// pyCandidateSources lists the mutable Python files at R.
func (r T2Repo) pyCandidateSources(ctx context.Context, root string) ([]RustMutantSource, error) {
	ls, err := git(ctx, root, "ls-tree", "-r", "--name-only", r.R)
	if err != nil {
		return nil, err
	}
	var out []RustMutantSource
	for _, f := range strings.Split(strings.TrimSpace(ls), "\n") {
		f = strings.TrimSpace(f)
		if !pyCandidateFile(f) {
			continue
		}
		b, oid, err := r.blob(ctx, root, f)
		if err != nil {
			return nil, err
		}
		out = append(out, RustMutantSource{Path: f, Blob: oid, Data: b})
	}
	return out, nil
}

// buildOraclePython runs mutants (sequentially: the working tree holds one mutation at a time) until Mutants killed ones are in the oracle,
// the try limit is reached or the budget deadline passes.
func (r T2Repo) buildOraclePython(ctx context.Context, cfg *T2Config, store *OracleStore, have map[string]T2Record) error {
	root := filepath.Join(cfg.Dir, r.Dir)
	if err := r.ensureClean(ctx, root); err != nil {
		return err
	}
	bl, known, err := r.pyBaseline(ctx, cfg, root)
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
		if json.Unmarshal(b, &c) == nil && c.R == r.R && c.Seed == r.Seed && c.Count == r.rustTries() {
			muts = c.Mutants
		}
	}
	if muts == nil {
		srcs, err := r.pyCandidateSources(ctx, root)
		if err != nil {
			return err
		}
		muts = GenPyMutants(srcs, r.Seed, r.Dir, r.rustTries())
		b, _ := json.MarshalIndent(map[string]any{"r": r.R, "seed": r.Seed, "count": r.rustTries(), "mutants": muts}, "", " ")
		os.WriteFile(mp, b, 0o644)
	}
	cfg.logf("%s: %d mutants generated", r.Name, len(muts))
	timeout := time.Duration(medianInt64(bl.RunMs))*4*time.Millisecond + time.Minute
	if timeout > 20*time.Minute {
		timeout = 20 * time.Minute
	}
	killed := 0
	for _, m := range muts {
		if x, ok := have[(T2Record{Repo: r.Name, R: r.R, Mutant: m}).key()]; ok && x.Status == "killed" {
			killed++
		}
	}
	for _, m := range muts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if killed >= r.Mutants {
			break
		}
		if !cfg.Deadline.IsZero() && time.Now().After(cfg.Deadline) {
			cfg.logf("%s: budget exhausted, stopping at %d killed", r.Name, killed)
			break
		}
		rec := T2Record{Repo: r.Name, R: r.R, Mutant: m}
		if old, ok := have[rec.key()]; ok && !cfg.Rebuild && old.Status != "error" {
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
		if ok, msg := r.pyCompiles(ctx, cfg, mutated); !ok {
			rec.Status, rec.Detail = "nocompile", msg
			store.Append(rec)
			continue
		}
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
			run, _, to, err := r.pySuite(ctx, cfg, root, timeout, nil, known)
			switch {
			case err != nil:
				rec.Status, rec.Detail = "error", err.Error()
				return
			case to:
				rec.Status, rec.Detail = "timeout", "timed out after "+timeout.String()
				return
			case len(run.Files) == 0:
				rec.Status, rec.Detail = "error", run.Detail
				return
			}
			if run.Session {
				rec.Detail = "session failure: every test file is red"
			}
			rec.TestFiles = len(run.Files)
			for f, st := range run.Files {
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
			sort.Strings(rec.Red)
			if err := r.confirmPyRed(ctx, cfg, root, known, timeout, &rec); err != nil {
				rec.Status, rec.Detail = "error", err.Error()
				return
			}
			rec.Status = "survived"
			if len(rec.Red) > 0 {
				rec.Status = "killed"
			}
		}()
		rec.Ms = time.Since(t0).Milliseconds()
		if st, err := git(ctx, root, "status", "--porcelain", "--untracked-files=no"); err != nil || strings.TrimSpace(st) != "" {
			return fmt.Errorf("%s: working tree not clean after %s: %v %.200s", r.Name, m.ID, err, st)
		}
		if err := store.Append(rec); err != nil {
			return err
		}
		if rec.Status == "killed" {
			killed++
		}
		cfg.logf("%s: %s %s %s:%d `%s` -> `%s`: %s (red %d) %s", r.Name, m.ID, m.Op, m.File, m.Line, short(m.Before), short(m.After), rec.Status, len(rec.Red), time.Duration(rec.Ms)*time.Millisecond)
	}
	return r.ensureClean(ctx, root)
}

// confirmPyRed re-runs each red file alone (the tree still holds the mutation); a file must be red again to count. The others go to
// RedUnconfirmed.
func (r T2Repo) confirmPyRed(ctx context.Context, cfg *T2Config, root string, known map[string]bool, timeout time.Duration, rec *T2Record) error {
	cand := rec.Red
	rec.Red, rec.RedUnconfirmed, rec.Confirmed = nil, nil, true
	for _, f := range cand {
		run, _, to, err := r.pySuite(ctx, cfg, root, timeout, []string{f}, known)
		if err != nil {
			return err
		}
		if !to && run.Files[f] == "failed" {
			rec.Red = append(rec.Red, f)
		} else {
			rec.RedUnconfirmed = append(rec.RedUnconfirmed, f)
		}
	}
	return nil
}

// pyNear describes, for each missed test file, the selected units of the same directory with their reason chains.
func pyNear(sel model.Selection, missed []string) []string {
	var out []string
	for _, m := range missed {
		n := 0
		for _, u := range sel.Tests {
			if path.Dir(u.Unit) == path.Dir(m) && n < 4 {
				out = append(out, m+" <- near "+u.Unit+": "+strings.Join(u.Reasons, " | "))
				n++
			}
		}
		if n == 0 {
			out = append(out, fmt.Sprintf("%s: no unit of %s is selected (%d selected overall)", m, path.Dir(m), len(sel.Tests)))
		}
	}
	return out
}

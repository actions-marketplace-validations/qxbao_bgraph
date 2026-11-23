package evalkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qxbao/bgraph/internal/eco/ruby"
	"github.com/qxbao/bgraph/internal/scancache"
)

// Ruby T2: the mutation oracle for Ruby repos (RSpec, Minitest, test-unit). One-token bugs are seeded into non-test Ruby source at R and
// EVERY test file the runner collects runs in its own Ruby process on the mutated working tree (several in parallel); every red test file
// must be in Select(R->M). The unit is the test file, as bgraph models it.
//
// One process per file gives a per-file verdict whatever the framework: the exit status of the file's process (a failed example, a load
// error of that file or a crash). RSpec aborts the whole run when one file fails to load, and Minitest/test-unit stop at the first
// require that raises, so a single suite process could not tell which files are red. The test file list is the runner's own: for RSpec
// RSpec.configuration.files_to_run after reading the project's option files (what `rspec` would load), for Minitest/test-unit the
// file_list of the project's Rake::TestTask (what `rake test` would load).
//
// The Ruby interpreter is $BGRAPH_EVAL_RUBY_BIN/ruby (a directory, prepended to PATH for the children) or ruby on PATH. Gems come from the
// repo's own bundle: BUNDLE_GEMFILE=<checkout>/Gemfile, BUNDLE_PATH=<T2 root>/<venv>.

// rbJobs is the number of test-file processes run at once per repo.
const rbJobs = 6

// rbRspecMain runs RSpec on ARGV (what the rspec executable does) and exits with its status.
const rbRspecMain = `require "rspec/core"; exit RSpec::Core::Runner.run(ARGV).to_i`

// rbRspecList prints the files RSpec would load for ARGV (the project's option files included).
const rbRspecList = `require "rspec/core"; o = RSpec::Core::ConfigurationOptions.new(ARGV); o.configure(RSpec.configuration); ` +
	`RSpec.configuration.files_to_run.each { |f| puts "FILE\t" + f }`

// rbRequireMain loads each ARGV file (Rake::TestTask's loader); the framework's autorun hook runs the tests at exit.
const rbRequireMain = `ARGV.each { |f| require File.expand_path(f) }`

// rbRakeList prints the file list and libs of the Rake::TestTask named by ARGV[0].
const rbRakeList = `require "rake"; require "rake/testtask"; $tts = []; ` +
	`Rake::TestTask.prepend(Module.new { def define; $tts << self; super; end }); ` +
	`Rake.application.init("rake", []); Rake.application.load_rakefile; ` +
	`t = $tts.find { |x| x.name.to_s == ARGV[0] } or abort("no Rake::TestTask named #{ARGV[0]}: #{$tts.map(&:name).inspect}"); ` +
	`t.file_list.to_a.each { |f| puts "FILE\t" + f }; t.libs.each { |l| puts "LIB\t" + l }`

func rubyExe() string {
	if d := os.Getenv("BGRAPH_EVAL_RUBY_BIN"); d != "" {
		if runtime.GOOS == "windows" {
			return filepath.Join(d, "ruby.exe")
		}
		return filepath.Join(d, "ruby")
	}
	if p, err := exec.LookPath("ruby"); err == nil {
		return p
	}
	return "ruby"
}

// rbEnv is the environment of the repo's Ruby processes.
func (r T2Repo) rbEnv(cfg *T2Config, root string) []string {
	env := []string{"BUNDLE_GEMFILE=" + filepath.Join(root, "Gemfile"), "BUNDLE_USER_HOME=" + filepath.Join(cfg.Dir, "bundlehome"), "NO_COLOR=1"}
	if r.Venv != "" {
		env = append(env, "BUNDLE_PATH="+filepath.Join(cfg.Dir, r.Venv))
	}
	if d := os.Getenv("BGRAPH_EVAL_RUBY_BIN"); d != "" {
		env = append(env, "PATH="+d+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return append(env, r.Env...)
}

// rbListFiles returns the test files the runner collects at the current working tree, repo-relative, and (rake) the libs of the task.
func (r T2Repo) rbListFiles(ctx context.Context, cfg *T2Config, root string) ([]string, []string, error) {
	var args []string
	switch r.Runner {
	case "rspec":
		args = append([]string{"-rbundler/setup", "-e", rbRspecList, "--"}, r.Spec...)
	case "minitest", "test-unit":
		task := r.Task
		if task == "" {
			task = "test"
		}
		args = []string{"-rbundler/setup", "-e", rbRakeList, "--", task}
	default:
		return nil, nil, fmt.Errorf("%s: unknown ruby runner %q (rspec | minitest | test-unit)", r.Name, r.Runner)
	}
	out, tail, to, err := runProc(ctx, root, r.rbEnv(cfg, root), 5*time.Minute, rubyExe(), args...)
	if to || err != nil {
		return nil, nil, fmt.Errorf("%s: listing test files: timeout=%v %v %s", r.Name, to, err, tail)
	}
	var files, libs []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if f, ok := strings.CutPrefix(l, "FILE\t"); ok {
			if filepath.IsAbs(f) {
				f = relPath(root, f)
			}
			files = append(files, strings.TrimPrefix(filepath.ToSlash(f), "./"))
		} else if lb, ok := strings.CutPrefix(l, "LIB\t"); ok {
			libs = append(libs, lb)
		}
	}
	sort.Strings(files)
	files = dedup(files)
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("%s: the runner collects no test file: %s", r.Name, tail)
	}
	return files, libs, nil
}

// rbFileResult is the outcome of one test file's process.
type rbFileResult struct {
	State  string // passed | failed | timeout
	Ms     int64
	Tail   string
	Failed []string // the failing tests the output names (Minitest, test-unit, RSpec formats)
}

var (
	reMinitestFail = regexp.MustCompile(`(?m)^\s*\d+\) (?:Failure|Error):\r?\n(.+?)(?: \[[^\]\n]*\])?:\r?$`)
	reTestUnitFail = regexp.MustCompile(`(?m)^(?:Failure|Error): (\S+\([^)\n]*\))`)
	reRspecFail    = regexp.MustCompile(`(?m)^rspec (\S+) # (.*?)\r?$`)
)

// rbFailedTests extracts the failing test names from a test process output.
func rbFailedTests(out string) []string {
	var names []string
	for _, m := range reMinitestFail.FindAllStringSubmatch(out, -1) {
		names = append(names, "minitest:"+strings.TrimSpace(m[1]))
	}
	for _, m := range reTestUnitFail.FindAllStringSubmatch(out, -1) {
		names = append(names, "test-unit:"+m[1])
	}
	for _, m := range reRspecFail.FindAllStringSubmatch(out, -1) {
		names = append(names, "rspec:"+m[2])
	}
	sort.Strings(names)
	return dedup(names)
}

// rbRunFile runs one test file in its own process.
func (r T2Repo) rbRunFile(ctx context.Context, cfg *T2Config, root string, libs []string, f string, timeout time.Duration) (rbFileResult, error) {
	args := []string{"-rbundler/setup"}
	switch r.Runner {
	case "rspec":
		args = append(args, "-e", rbRspecMain, "--")
		args = append(args, r.Args...)
		args = append(args, f)
	default:
		for _, l := range libs {
			args = append(args, "-I"+l)
		}
		args = append(args, r.Args...)
		args = append(args, "-e", rbRequireMain, "--", f)
	}
	t0 := time.Now()
	out, tail, to, err := runProc(ctx, root, r.rbEnv(cfg, root), timeout, rubyExe(), args...)
	res := rbFileResult{Ms: time.Since(t0).Milliseconds()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	var ee *exec.ExitError
	switch {
	case to:
		res.State = "timeout"
	case err == nil:
		res.State = "passed"
	case errors.As(err, &ee):
		res.State = "failed"
	default:
		return res, err // could not start ruby
	}
	if res.State != "passed" {
		res.Failed = rbFailedTests(out)
		o := strings.TrimSpace(out)
		if len(o) > 600 {
			o = o[len(o)-600:]
		}
		t := strings.TrimSpace(tail)
		if len(t) > 400 {
			t = t[len(t)-400:]
		}
		res.Tail = o + " | " + t
	}
	return res, nil
}

// rbRunAll runs the given test files rbJobs at a time.
func (r T2Repo) rbRunAll(ctx context.Context, cfg *T2Config, root string, libs, files []string, timeout time.Duration) (map[string]rbFileResult, error) {
	out := map[string]rbFileResult{}
	var mu sync.Mutex
	var first error
	ch := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < rbJobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range ch {
				res, err := r.rbRunFile(ctx, cfg, root, libs, f, timeout)
				mu.Lock()
				if err != nil && first == nil {
					first = err
				}
				out[f] = res
				mu.Unlock()
			}
		}()
	}
	for _, f := range files {
		ch <- f
	}
	close(ch)
	wg.Wait()
	return out, first
}

// rbBaseline is the Ruby baseline: the shared T2Baseline (Known = the runner's test files) plus the libs and the slowest file time.
type rbBaseline struct {
	T2Baseline
	Libs   []string         `json:"libs,omitempty"`
	FileMs map[string]int64 `json:"file_ms"`
}

// rbBaselineRun runs every test file twice at R; a file red in both runs is already broken, one whose result differs is flaky.
func (r T2Repo) rbBaselineRun(ctx context.Context, cfg *T2Config, root string) (rbBaseline, error) {
	bp := filepath.Join(cfg.Dir, "baseline-"+r.Dir+".json")
	if !cfg.Rebaseline {
		if b, err := os.ReadFile(bp); err == nil {
			var bl rbBaseline
			if json.Unmarshal(b, &bl) == nil && bl.R == r.R && len(bl.Known) > 0 {
				return bl, nil
			}
		}
	}
	files, libs, err := r.rbListFiles(ctx, cfg, root)
	if err != nil {
		return rbBaseline{}, err
	}
	bl := rbBaseline{T2Baseline: T2Baseline{Repo: r.Name, R: r.R, Known: files, Files: len(files)}, Libs: libs, FileMs: map[string]int64{}}
	var runs []map[string]rbFileResult
	for i := 0; i < 2; i++ {
		t0 := time.Now()
		res, err := r.rbRunAll(ctx, cfg, root, libs, files, 15*time.Minute)
		if err != nil {
			return bl, err
		}
		d := time.Since(t0)
		bl.RunMs = append(bl.RunMs, d.Milliseconds())
		red := 0
		for f, x := range res {
			if x.State != "passed" {
				red++
			}
			bl.FileMs[f] = max(bl.FileMs[f], x.Ms)
		}
		cfg.logf("%s: baseline %d/2 %d test files (%d red), %s", r.Name, i+1, len(files), red, d.Round(time.Second))
		if red == len(files) {
			for _, x := range res {
				return bl, fmt.Errorf("%s: every test file is red at R (environment?): %s", r.Name, x.Tail)
			}
		}
		runs = append(runs, res)
	}
	for _, f := range files {
		a, b := runs[0][f].State, runs[1][f].State
		switch {
		case a != "passed" && b != "passed":
			bl.Failing = append(bl.Failing, f)
			fa, fb := runs[0][f].Failed, runs[1][f].Failed
			if a == "failed" && b == "failed" && len(fa) > 0 && len(fb) > 0 {
				if bl.FailingTests == nil {
					bl.FailingTests = map[string][]string{}
				}
				u := append(append([]string(nil), fa...), fb...)
				sort.Strings(u)
				bl.FailingTests[f] = dedup(u)
			}
		case a != b:
			bl.Flaky = append(bl.Flaky, f)
		}
	}
	b, _ := json.MarshalIndent(bl, "", " ")
	os.WriteFile(bp, b, 0o644)
	return bl, nil
}

// rbSyntaxOK reports whether src is valid Ruby (`ruby -c`).
func (r T2Repo) rbSyntaxOK(ctx context.Context, src []byte) (bool, string) {
	cmd := exec.CommandContext(ctx, rubyExe(), "-c")
	cmd.Stdin = bytes.NewReader(src)
	var ob bytes.Buffer
	cmd.Stdout, cmd.Stderr = &ob, &ob
	if err := cmd.Run(); err != nil {
		l := strings.Split(strings.TrimSpace(ob.String()), "\n")
		return false, l[0]
	}
	return true, ""
}

// rbCandidateSources lists the mutable Ruby files at R.
func (r T2Repo) rbCandidateSources(ctx context.Context, root string) ([]RustMutantSource, error) {
	ls, err := git(ctx, root, "ls-tree", "-r", "--name-only", r.R)
	if err != nil {
		return nil, err
	}
	var out []RustMutantSource
	for _, f := range strings.Split(strings.TrimSpace(ls), "\n") {
		f = strings.TrimSpace(f)
		if !rbCandidateFile(f, r.Include) {
			continue
		}
		skip := false
		for _, x := range r.Exclude {
			skip = skip || strings.HasPrefix(f, x)
		}
		if skip {
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

// buildOracleRuby runs mutants (sequentially: the working tree holds one mutation at a time; the test files of one mutant run in parallel)
// until Mutants killed ones are in the oracle, the try limit is reached or the budget deadline passes.
func (r T2Repo) buildOracleRuby(ctx context.Context, cfg *T2Config, store *OracleStore, have map[string]T2Record) error {
	root := filepath.Join(cfg.Dir, r.Dir)
	if err := r.ensureClean(ctx, root); err != nil {
		return err
	}
	bl, err := r.rbBaselineRun(ctx, cfg, root)
	if err != nil {
		return err
	}
	cfg.logf("%s: baseline %d test files, %d pre-broken, %d flaky", r.Name, bl.Files, len(bl.Failing), len(bl.Flaky))
	var slowest int64
	for _, ms := range bl.FileMs {
		slowest = max(slowest, ms)
	}
	timeout := time.Duration(slowest)*4*time.Millisecond + time.Minute
	if timeout > 15*time.Minute {
		timeout = 15 * time.Minute
	}
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
		srcs, err := r.rbCandidateSources(ctx, root)
		if err != nil {
			return err
		}
		muts = GenRubyMutants(srcs, r.Seed, r.Dir, r.rustTries())
		b, _ := json.MarshalIndent(map[string]any{"r": r.R, "seed": r.Seed, "count": r.rustTries(), "mutants": muts}, "", " ")
		os.WriteFile(mp, b, 0o644)
	}
	cfg.logf("%s: %d mutants generated", r.Name, len(muts))
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
		synOK, synMsg := r.rbSyntaxOK(ctx, mutated)
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
			res, err := r.rbRunAll(ctx, cfg, root, bl.Libs, bl.Known, timeout)
			if err != nil {
				rec.Status, rec.Detail = "error", err.Error()
				return
			}
			rec.TestFiles = len(res)
			green := 0
			for f, x := range res {
				switch bl.verdict(f, x) {
				case "green":
					green++
				case "excluded":
					if x.State != "passed" {
						rec.RedExcluded = append(rec.RedExcluded, f)
					}
				default:
					rec.Red = append(rec.Red, f)
				}
			}
			sort.Strings(rec.RedExcluded)
			sort.Strings(rec.Red)
			if !synOK && green == 0 {
				// the mutant does not parse and the whole suite crashes: not a kill
				rec.Status, rec.Detail = "nocompile", synMsg
				return
			}
			if err := r.confirmRbRed(ctx, cfg, root, bl, timeout, &rec, res); err != nil {
				rec.Status, rec.Detail = "error", err.Error()
				return
			}
			rec.Status = "survived"
			if len(rec.Red) > 0 {
				rec.Status = "killed"
			}
			if !synOK {
				rec.Detail = "syntax error, partial kill: " + synMsg + "; " + rec.Detail
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

// confirmRbRed re-runs each red file alone (the tree still holds the mutation, no other test process runs); a file must be red again to
// count. The others go to RedUnconfirmed. Detail keeps the output tail of the first confirmed red file.
func (r T2Repo) confirmRbRed(ctx context.Context, cfg *T2Config, root string, bl rbBaseline, timeout time.Duration, rec *T2Record, first map[string]rbFileResult) error {
	cand := rec.Red
	rec.Red, rec.RedUnconfirmed, rec.Confirmed = nil, nil, true
	for _, f := range cand {
		res, err := r.rbRunFile(ctx, cfg, root, bl.Libs, f, timeout)
		if err != nil {
			return err
		}
		if bl.verdict(f, res) == "red" {
			rec.Red = append(rec.Red, f)
			if rec.Detail == "" {
				rec.Detail = f + ": " + first[f].State + ": " + short(first[f].Tail)
			}
		} else {
			rec.RedUnconfirmed = append(rec.RedUnconfirmed, f)
		}
	}
	return nil
}

// RubyUnitsResult is the provider cross-check of one T2 repo: the runner's test files against bgraph's Ruby test units at R.
type RubyUnitsResult struct {
	Repo        string   `json:"repo"`
	R           string   `json:"r"`
	Runner      string   `json:"runner"`
	RunnerFiles int      `json:"runner_files"`
	Units       int      `json:"bgraph_units"`
	Missing     []string `json:"missing,omitempty"` // collected by the runner, not a bgraph unit (a miss: bug)
	Extra       []string `json:"extra,omitempty"`   // bgraph units the runner does not collect (over-selection only)
	Whole       []string `json:"whole,omitempty"`   // whole-project units (cover every file of their project)
	Error       string   `json:"error,omitempty"`
}

// RubyUnitsCheck compares, for each selected repo, the runner's test files (recorded by the baseline) with bgraph's Ruby test units at R
// (the analysis of the commit, no selection). A runner file covered by a whole-project unit counts as present.
func RubyUnitsCheck(ctx context.Context, cfg T2Config) []RubyUnitsResult {
	var out []RubyUnitsResult
	for _, r := range cfg.selected() {
		res := RubyUnitsResult{Repo: r.Name, R: r.R, Runner: r.Runner}
		root := filepath.Join(cfg.Dir, r.Dir)
		var bl rbBaseline
		b, err := os.ReadFile(filepath.Join(cfg.Dir, "baseline-"+r.Dir+".json"))
		if err == nil {
			err = json.Unmarshal(b, &bl)
		}
		if err != nil || bl.R != r.R {
			res.Error = fmt.Sprintf("no baseline at R (run with --run-tests): %v", err)
			out = append(out, res)
			continue
		}
		gt, err := newGitTreeWant(ctx, root, r.R, func(string) bool { return true })
		if err != nil {
			res.Error = err.Error()
			out = append(out, res)
			continue
		}
		a, err := ruby.New(nil).Analyze(ctx, gt, scancache.New())
		gt.Close()
		if err != nil {
			res.Error = err.Error()
			out = append(out, res)
			continue
		}
		units := map[string]bool{}
		var wholes []string
		for _, u := range a.Tests {
			if u.Whole {
				wholes = append(wholes, u.Project)
				res.Whole = append(res.Whole, u.ID)
				continue
			}
			units[u.ID] = true
		}
		res.RunnerFiles, res.Units = len(bl.Known), len(a.Tests)
		known := map[string]bool{}
		for _, f := range bl.Known {
			known[f] = true
			if units[f] {
				continue
			}
			covered := false
			for _, p := range wholes {
				covered = covered || p == "." || p == "" || under(f, p)
			}
			if !covered {
				res.Missing = append(res.Missing, f)
			}
		}
		for u := range units {
			if !known[u] {
				res.Extra = append(res.Extra, u)
			}
		}
		sort.Strings(res.Extra)
		out = append(out, res)
	}
	return out
}

// WriteRubyUnits writes ruby-units.json and ruby-units.md into dir and returns the markdown.
func WriteRubyUnits(dir string, rs []RubyUnitsResult) (string, error) {
	var b strings.Builder
	b.WriteString("# Ruby provider cross-check: runner test files vs bgraph units at R\n\n| repo | R | runner | runner files | bgraph units | missing (bug) | extra |\n|---|---|---|---|---|---|---|\n")
	for _, r := range rs {
		if r.Error != "" {
			fmt.Fprintf(&b, "| %s | %.10s | %s | error: %s | | | |\n", r.Repo, r.R, r.Runner, r.Error)
			continue
		}
		fmt.Fprintf(&b, "| %s | %.10s | %s | %d | %d | %d | %d |\n", r.Repo, r.R, r.Runner, r.RunnerFiles, r.Units, len(r.Missing), len(r.Extra))
	}
	for _, r := range rs {
		if len(r.Missing)+len(r.Extra)+len(r.Whole) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n", r.Repo)
		for _, f := range r.Missing {
			fmt.Fprintf(&b, "- MISSING `%s`\n", f)
		}
		for _, f := range r.Extra {
			fmt.Fprintf(&b, "- extra `%s`\n", f)
		}
		for _, f := range r.Whole {
			fmt.Fprintf(&b, "- whole-project unit `%s`\n", f)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	j, _ := json.MarshalIndent(rs, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "ruby-units.json"), j, 0o644); err != nil {
		return "", err
	}
	md := b.String()
	return md, os.WriteFile(filepath.Join(dir, "ruby-units.md"), []byte(md), 0o644)
}

// verdict classifies a test file's result against the baseline: green (passed), excluded (flaky at R, or broken at R and failing only
// in tests that already failed there) or red. A file broken at R counts as red when a test outside its baseline failures fails, or when it
// now fails without naming any test (load error, crash) or times out; a broken file whose baseline failures could not be named stays
// excluded.
func (bl rbBaseline) verdict(f string, x rbFileResult) string {
	if x.State == "passed" {
		return "green"
	}
	if slices.Contains(bl.Flaky, f) {
		return "excluded"
	}
	if slices.Contains(bl.Failing, f) {
		names := bl.FailingTests[f]
		if len(names) == 0 {
			return "excluded"
		}
		if x.State == "failed" && len(x.Failed) > 0 {
			for _, n := range x.Failed {
				if !slices.Contains(names, n) {
					return "red"
				}
			}
			return "excluded"
		}
	}
	return "red"
}

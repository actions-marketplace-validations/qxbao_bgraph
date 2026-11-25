package evalkit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/doc"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/dynamic/godyn"
	"github.com/qxbao/bgraph/internal/eco/golang"
	"github.com/qxbao/bgraph/internal/model"
)

// Phase 13: the Go dynamic layer (per top-level test function inside a Go package) checked three ways, none with the collector
// as oracle.
//
//   - Hybrid T2 (RunGoDynT2): records made once at R in a clone of the Go T2 checkout, then for every killed mutant of the cached
//     package-level oracle the static selection and the selection with the Go layer on R→M. Per-function verdicts come from
//     rerunning every oracle-red package whole with `go test -json` on the mutated tree (failing top-level tests). A violation is
//     an oracle-red package missing from the hybrid selection, or a red top-level test of a selected package that the layer
//     filtered out.
//   - Guard rate (RunGoGuardRate): the selection parent→commit with the Go layer on and no records over first-parent commits; the
//     layer either refuses, does not run (no Go package selected), or passes every guard and stops at "no records".
//   - Replay (RunGoReplay): first-parent commits; where the guard passes, fresh records at the parent, the selection with the
//     layer, and every package with a removed function (or compile-only) run whole at the commit (and at the parent when a removed
//     function fails at the commit); where it does not, hybrid must equal static.
//
// The go command, the build cache and the toolchain are this process's environment (GoDynSetEnv): GOTOOLCHAIN=local, the go
// executable's directory first on PATH (tests that run `go` get the same toolchain), CGO_ENABLED=0, GOCACHE per clone, no
// GOFLAGS. The fingerprint reads the same variables at record and selection time.

// GoDynConfig configures the Go layer evaluation of one repo.
type GoDynConfig struct {
	T2       T2Config // the Go T2 set (oracle.jsonl, mutants, clones); read only
	Repo     T2Repo
	Work     string // this evaluation's clones and state (t2/go-dyn)
	Go       string // the go executable (absolute)
	Jobs     int    // recording parallelism (0: the layer's default)
	Rerecord bool
}

func (c GoDynConfig) logf(f string, a ...any) {
	c.T2.logf("%s: "+f, append([]any{c.Repo.Name}, a...)...)
}

func (c GoDynConfig) clone() string { return filepath.Join(c.Work, c.Repo.Dir) }

var goDynPath0 string

// GoDynSetEnv sets the go environment of a repo's evaluation in this process (see the package comment above).
func GoDynSetEnv(work, dir, goExe string) error {
	if goDynPath0 == "" {
		goDynPath0 = os.Getenv("PATH")
	}
	tmp := filepath.Join(work, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	kv := map[string]string{
		"GOTOOLCHAIN": "local",
		"CGO_ENABLED": "0",
		"GOCACHE":     filepath.Join(work, "gocache-"+dir),
		"GOTMPDIR":    tmp,
		"PATH":        filepath.Dir(goExe) + string(os.PathListSeparator) + goDynPath0,
	}
	for k, v := range kv {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	for _, k := range []string{"GOFLAGS", "GOWORK", "GOEXPERIMENT", "GODEBUG", "GOOS", "GOARCH", "GO111MODULE"} {
		os.Unsetenv(k)
	}
	return nil
}

// GoDynClone makes (once) the evaluation clone dst of src (a plain local clone, gc.auto=0) and checks it out clean at rev.
func GoDynClone(ctx context.Context, src, dst, rev string) error {
	if _, err := os.Stat(filepath.Join(dst, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if _, err := git(ctx, filepath.Dir(dst), "-c", "core.autocrlf=false", "clone", "-q", "--no-checkout", src, dst); err != nil {
			return err
		}
		for _, kv := range [][2]string{{"gc.auto", "0"}, {"maintenance.auto", "false"}, {"core.autocrlf", "false"}} {
			if _, err := git(ctx, dst, "config", kv[0], kv[1]); err != nil {
				return err
			}
		}
	}
	if _, err := git(ctx, dst, "checkout", "-q", "-f", "--detach", rev); err != nil {
		return err
	}
	_, err := git(ctx, dst, "clean", "-q", "-fdx")
	return err
}

func (c GoDynConfig) checkout(ctx context.Context, rev string) error {
	return GoDynClone(ctx, filepath.Join(c.T2.Dir, c.Repo.Dir), c.clone(), rev)
}

func (c GoDynConfig) open(ctx context.Context, stateDir string) (*bgraph.Index, error) {
	return bgraph.Open(ctx, c.clone(), bgraph.Options{StateDir: stateDir, Go: c.Go})
}

// GoRecordStat is one recording.
type GoRecordStat struct {
	Reused  bool                  `json:"reused,omitempty"`
	Summary *bgraph.RecordSummary `json:"summary,omitempty"`
	Ms      int64                 `json:"ms"`
	Error   string                `json:"error,omitempty"`
}

func (c GoDynConfig) record(ctx context.Context, ix *bgraph.Index, rerecord bool) GoRecordStat {
	if !rerecord {
		if cur, err := ix.DynamicStatusOf(ctx, godyn.Eco); err == nil && cur.CurrentEnv > 0 && cur.Usable > 0 {
			return GoRecordStat{Reused: true}
		}
	}
	t0 := time.Now()
	sum, err := ix.Record(ctx, bgraph.RecordOptions{Eco: godyn.Eco, Jobs: c.Jobs, Timeout: 5 * time.Minute})
	st := GoRecordStat{Ms: time.Since(t0).Milliseconds()}
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Summary = &sum
	c.logf("record: %d units, %d complete, %d incomplete, %d failed in %s %v %v", sum.Units, sum.Complete, sum.Incomplete, sum.Failed,
		time.Duration(st.Ms)*time.Millisecond, sum.Reasons, sum.Warnings)
	return st
}

// ---- go test -json with per-test verdicts ----

// goPkgRun is the outcome of one package in a `go test -json` run.
type goPkgRun struct {
	Status string            `json:"status"`          // pass | fail | skip | "" (no package event)
	Tests  map[string]string `json:"tests,omitempty"` // top-level test -> pass | fail | skip
	Build  string            `json:"build,omitempty"` // build failure detail
}

// goTestRun runs `go test -json -count=1 [-run re] args` per module for the given package units (all of the module's packages
// when units is nil), returning per-unit outcomes.
func goTestRun(ctx context.Context, gl *goLayout, units []string, runRe string, timeout time.Duration) (map[string]*goPkgRun, time.Duration, error) {
	args := map[string][]string{}
	var mods []string
	if units == nil {
		for _, m := range gl.modules {
			args[m.Dir] = []string{"./..."}
			mods = append(mods, m.Dir)
		}
	} else {
		for _, u := range units {
			d, ok := golang.ParseUnitID(u)
			m := gl.moduleOfDir(d)
			if !ok || m == nil {
				return nil, 0, fmt.Errorf("unit %s is in no module", u)
			}
			if args[m.Dir] == nil {
				mods = append(mods, m.Dir)
			}
			args[m.Dir] = append(args[m.Dir], moduleArg(m, d))
		}
	}
	sort.Strings(mods)
	res := map[string]*goPkgRun{}
	t0 := time.Now()
	for _, md := range mods {
		a := []string{"test", "-json", "-count=1", "-timeout=" + min(timeout, 10*time.Minute).String()}
		if runRe != "" {
			a = append(a, "-run", runRe)
		}
		a = append(a, args[md]...)
		so, tl, to, err := runProc(ctx, filepath.Join(gl.root, filepath.FromSlash(md)), nil, timeout+2*time.Minute, "go", a...)
		if ctx.Err() != nil {
			return res, time.Since(t0), ctx.Err()
		}
		if to {
			return res, time.Since(t0), fmt.Errorf("module %s: go test timed out after %s", md, timeout)
		}
		n := len(res)
		if perr := gl.foldTestJSON(so, res); perr != nil {
			return res, time.Since(t0), perr
		}
		if err != nil && len(res) == n && strings.Contains(tl, "matched no packages") {
			continue // a module without packages under this build context (connect-go's internal/conformance)
		}
		if err != nil && len(res) == n {
			return res, time.Since(t0), fmt.Errorf("module %s: %v: %s", md, err, firstN(strings.TrimSpace(tl+" "+so), 600))
		}
	}
	return res, time.Since(t0), nil
}

func (gl *goLayout) foldTestJSON(out string, res map[string]*goPkgRun) error {
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev goEvent
		if json.Unmarshal(line, &ev) != nil || ev.Package == "" {
			continue
		}
		d, ok := gl.dirOf(ev.Package)
		if !ok {
			return fmt.Errorf("unmapped go test package %s", ev.Package)
		}
		u := golang.UnitID(d)
		p := res[u]
		if p == nil {
			p = &goPkgRun{Tests: map[string]string{}}
			res[u] = p
		}
		if ev.Test != "" {
			if !strings.Contains(ev.Test, "/") {
				switch ev.Action {
				case "run":
					if p.Tests[ev.Test] == "" {
						p.Tests[ev.Test] = "run"
					}
				case "pass", "fail", "skip":
					p.Tests[ev.Test] = ev.Action
				}
			}
			continue
		}
		switch ev.Action {
		case "pass", "skip":
			if p.Status == "" {
				p.Status = ev.Action
			}
		case "fail":
			p.Status = "fail"
			if ev.FailedBuild != "" {
				p.Build = ev.FailedBuild
			}
		}
	}
	return nil
}

// goResolveUnfinished gives a verdict to every top-level test that started and never ended (the binary exited while it ran or
// was paused: os.Exit, log.Fatal, a fatal runtime error, a panic in a parallel sibling): it is run alone, and fails when it fails
// or ends without a verdict again.
func goResolveUnfinished(ctx context.Context, gl *goLayout, res map[string]*goPkgRun) error {
	for u, p := range res {
		for n, a := range p.Tests {
			if a != "run" {
				continue
			}
			// 2 minutes: a test that deadlocks alone under a mutant (it was cut short by a sibling's panic in the package run)
			// fails by the test timeout.
			one, _, err := goTestRun(ctx, gl, []string{u}, "^"+regexp.QuoteMeta(n)+"$", 2*time.Minute)
			if err != nil {
				return err
			}
			p.Tests[n] = "fail"
			if q := one[u]; q != nil && (q.Tests[n] == "pass" || q.Tests[n] == "skip") {
				p.Tests[n] = q.Tests[n] + " (alone; unfinished in the package run)"
			}
		}
	}
	return nil
}

func (p *goPkgRun) failed() []string {
	var out []string
	if p == nil {
		return nil
	}
	for n, a := range p.Tests {
		if a == "fail" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// ---- top-level test functions, read from the sources ----

var goTestBuild = func() build.Context {
	c := build.Default
	c.CgoEnabled = false
	return c
}()

func isGoTestName(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	r := rest[0]
	return !(r >= 'a' && r <= 'z')
}

// goCountTests returns the top-level test functions `go test -list` would print for one _test.go file under this host's build
// context (cgo off): Test*, Fuzz* and Example* with an output comment; TestMain excluded. 0 when the file is excluded by a build
// constraint or does not parse.
func goCountTests(name string, src []byte) int {
	bc := goTestBuild
	bc.OpenFile = func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(src)), nil }
	if ok, err := bc.MatchFile("x", path.Base(name)); err != nil || !ok {
		return 0
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		return 0
	}
	n := 0
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name != "TestMain" &&
			(isGoTestName(fd.Name.Name, "Test") || isGoTestName(fd.Name.Name, "Fuzz")) {
			n++
		}
	}
	for _, ex := range doc.Examples(f) {
		if ex.Output != "" || ex.EmptyOutput {
			n++
		}
	}
	return n
}

// catFiles reads files at rev with one `git cat-file --batch`.
func catFiles(ctx context.Context, root, rev string, files []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	if len(files) == 0 {
		return out, nil
	}
	var in strings.Builder
	for _, f := range files {
		in.WriteString(rev + ":" + f + "\n")
	}
	raw, err := gitIn(ctx, root, nil, in.String(), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		i := strings.IndexByte(raw, '\n')
		if i < 0 {
			return nil, fmt.Errorf("cat-file: short output at %s", f)
		}
		hdr := strings.Fields(raw[:i])
		raw = raw[i+1:]
		if len(hdr) != 3 {
			continue // missing
		}
		n, err := strconv.Atoi(hdr[2])
		if err != nil || n > len(raw) {
			return nil, fmt.Errorf("cat-file: bad header %q", hdr)
		}
		out[f] = []byte(raw[:n])
		raw = strings.TrimPrefix(raw[n:], "\n")
	}
	return out, nil
}

// goTestCounts returns, per Go package unit with test files at rev, its number of top-level test functions (from the sources).
func goTestCounts(ctx context.Context, root, rev string) (map[string]int, error) {
	ls, err := git(ctx, root, "ls-tree", "-r", "--name-only", rev)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(strings.TrimSpace(ls), "\n") {
		if strings.HasSuffix(f, "_test.go") && !t2GoIgnoredDir(path.Dir(f)) {
			files = append(files, f)
		}
	}
	blobs, err := catFiles(ctx, root, rev, files)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, f := range files {
		out[golang.UnitID(path.Dir(f))] += goCountTests(f, blobs[f])
	}
	return out, nil
}

// ---- selection summary ----

// GoSel summarizes one selection's Go part.
type GoSel struct {
	Scope       string         `json:"scope"`
	Packages    int            `json:"packages"`     // selected Go package units
	RunPackages int            `json:"run_packages"` // of which at least one test function runs
	CompileOnly int            `json:"compile_only,omitempty"`
	Tests       int            `json:"tests"` // top-level test functions that run
	Removed     []string       `json:"removed,omitempty"`
	Skip        string         `json:"skip,omitempty"` // the Go layer's dynamic_skipped reason
	Kept        map[string]int `json:"kept,omitempty"` // note class -> units
	Missed      []string       `json:"missed,omitempty"`
	Units       []string       `json:"units,omitempty"`
	Filter      map[string]int `json:"filter,omitempty"` // filtered unit -> kept functions
}

var reGoNum = regexp.MustCompile(`[0-9]+`)

// goKeptClass reduces a Go unit's dynamic note to a class.
func goKeptClass(note string) string {
	switch {
	case strings.HasPrefix(note, "not narrowed by the Go layer"):
		return "layer skipped"
	case strings.HasPrefix(note, "Go layer kept the whole package: "):
		why := strings.TrimPrefix(note, "Go layer kept the whole package: ")
		switch {
		case strings.HasPrefix(why, "its test file"):
			return "whole: a test file of the package changed"
		case strings.HasPrefix(why, "the static graph reaches it"):
			return "whole: fixture/embed/cgo edge"
		case strings.HasPrefix(why, "its tests may read the package's Go sources"):
			return "whole: tests may read the package's Go sources"
		case strings.HasPrefix(why, "function list not usable"):
			return "whole: function list not usable"
		}
		return "whole: " + reGoNum.ReplaceAllString(why, "N")
	case strings.HasPrefix(note, "Go layer: all "):
		return "all functions kept"
	case strings.Contains(note, "none of"):
		return "compile only"
	case strings.Contains(note, " test functions run"):
		return "narrowed"
	}
	note = reGoNum.ReplaceAllString(note, "N")
	if len(note) > 80 {
		note = note[:80]
	}
	return note
}

// goSummarize summarizes sel; counts are the top-level test functions per unit (static), red the oracle's red package units and
// redFns their failing top-level tests.
func goSummarize(sel model.Selection, counts map[string]int, red []string, redFns map[string][]string) GoSel {
	d := GoSel{Scope: string(sel.Scope), Skip: ecoSkipReason(sel, godyn.Eco)}
	for _, n := range sel.Narrowed {
		if strings.HasPrefix(n.Unit, "go:") {
			d.Removed = append(d.Removed, n.Unit)
		}
	}
	sort.Strings(d.Removed)
	byUnit := map[string]model.SelectedTest{}
	for _, t := range sel.Tests {
		if _, ok := golang.ParseUnitID(t.Unit); !ok || t.Whole {
			continue
		}
		byUnit[t.Unit] = t
		d.Packages++
		d.Units = append(d.Units, t.Unit)
		n := counts[t.Unit]
		if t.ClassFilter {
			n = len(t.Classes)
			if d.Filter == nil {
				d.Filter = map[string]int{}
			}
			d.Filter[t.Unit] = n
		}
		d.Tests += n
		if t.ClassFilter && len(t.Classes) == 0 {
			d.CompileOnly++
		} else {
			d.RunPackages++
		}
		if t.Dynamic != "" {
			if d.Kept == nil {
				d.Kept = map[string]int{}
			}
			d.Kept[goKeptClass(t.Dynamic)]++
		}
	}
	sort.Strings(d.Units)
	for _, u := range red {
		t, ok := byUnit[u]
		if !ok {
			d.Missed = append(d.Missed, u)
			continue
		}
		if !t.ClassFilter {
			continue
		}
		for _, fn := range redFns[u] {
			if !slicesContains(t.Classes, fn) {
				d.Missed = append(d.Missed, godyn.SubID(u, fn))
			}
		}
	}
	return d
}

// ---- hybrid T2 ----

// GoFnVerdict is the per-function oracle of one killed mutant: every oracle-red package rerun whole on the mutated tree.
type GoFnVerdict struct {
	Units map[string]*goPkgRun `json:"units"`
	Ms    int64                `json:"ms"`
	Error string               `json:"error,omitempty"`
}

// GoDynMutant is one killed mutant.
type GoDynMutant struct {
	ID     string              `json:"id"`
	File   string              `json:"file"`
	Line   int                 `json:"line"`
	Op     string              `json:"op"`
	Red    []string            `json:"red"`
	RedFns map[string][]string `json:"red_fns,omitempty"` // red unit -> failing top-level tests in the whole-package rerun
	// NotReproduced: oracle-red units that passed in the rerun (kept as package-level red, with no function verdicts).
	NotReproduced []string `json:"not_reproduced,omitempty"`
	// Unattributed: red units without any failing top-level test (build failure, TestMain, a failure after the last test) that
	// the hybrid selection filters: not a violation by definition, checked by hand.
	Unattributed []string `json:"unattributed,omitempty"`
	Static       GoSel    `json:"static"`
	Hybrid       GoSel    `json:"hybrid"`
	Error        string   `json:"error,omitempty"`
}

// GoDynT2Result is the hybrid pass over one repo.
type GoDynT2Result struct {
	Repo      string               `json:"repo"`
	R         string               `json:"r"`
	Go        string               `json:"go"`
	PlainMs   []int64              `json:"plain_ms"`             // whole suite at R, cold-ish then warm
	PlainFail map[string][]string  `json:"plain_fail,omitempty"` // failing top-level tests of the plain suite at R
	Record    GoRecordStat         `json:"record"`
	Status    bgraph.DynamicStatus `json:"status"`
	Packages  int                  `json:"packages"` // Go package units with tests at R
	Tests     int                  `json:"tests"`    // top-level test functions at R
	Mutants   []GoDynMutant        `json:"mutants"`
	Error     string               `json:"error,omitempty"`
}

func (c GoDynConfig) fnOraclePath() string {
	return filepath.Join(c.Work, "fnoracle-"+c.Repo.Dir+".json")
}

// RunGoDynT2 runs the hybrid pass on one repo against the cached oracle.
func RunGoDynT2(ctx context.Context, cfg GoDynConfig, only map[string]bool) GoDynT2Result {
	r := cfg.Repo
	res := GoDynT2Result{Repo: r.Name, R: r.R, Go: cfg.Go}
	recs, err := loadOracle(cfg.T2.oraclePath())
	if err != nil {
		res.Error = err.Error()
		return res
	}
	latest := map[string]T2Record{}
	var keys []string
	for _, x := range recs {
		if x.Repo != r.Name || x.R != r.R {
			continue
		}
		if _, ok := latest[x.key()]; !ok {
			keys = append(keys, x.key())
		}
		latest[x.key()] = x
	}
	if err := cfg.checkout(ctx, r.R); err != nil {
		res.Error = err.Error()
		return res
	}
	gl, err := loadGoLayout(ctx, r, cfg.clone())
	if err != nil {
		res.Error = err.Error()
		return res
	}
	counts, err := goTestCounts(ctx, cfg.clone(), r.R)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	for u, n := range counts {
		if gl.units[u] {
			res.Packages++
			res.Tests += n
		}
	}
	// The plain suite twice (the second with a warm build cache): the recording-time comparison, and which tests fail at R here.
	for i := 0; i < 2; i++ {
		run, d, err := goTestRun(ctx, gl, nil, "", 30*time.Minute)
		if err != nil {
			res.Error = "plain suite: " + err.Error()
			return res
		}
		res.PlainMs = append(res.PlainMs, d.Milliseconds())
		if i == 1 {
			for u, p := range run {
				if f := p.failed(); len(f) > 0 || p.Status == "fail" {
					if res.PlainFail == nil {
						res.PlainFail = map[string][]string{}
					}
					res.PlainFail[u] = append(f, "(package "+p.Status+")")
				}
			}
		}
		cfg.logf("plain suite %d/2: %s", i+1, d.Round(time.Second))
	}
	if err := cfg.checkout(ctx, r.R); err != nil {
		res.Error = err.Error()
		return res
	}
	// per-function oracle (cached)
	fo := map[string]GoFnVerdict{}
	if b, err := os.ReadFile(cfg.fnOraclePath()); err == nil {
		json.Unmarshal(b, &fo)
	}
	idx := filepath.Join(cfg.Work, "idx")
	for _, k := range keys {
		o := latest[k]
		if o.Status != "killed" || only != nil && !only[o.Mutant.ID] {
			continue
		}
		if v, ok := fo[o.Mutant.ID]; ok && v.Error == "" {
			continue
		}
		if ctx.Err() != nil {
			res.Error = ctx.Err().Error()
			return res
		}
		v := cfg.fnVerdict(ctx, gl, idx, o)
		fo[o.Mutant.ID] = v
		cfg.logf("%s: per-function oracle %s", o.Mutant.ID, map[bool]string{true: "error " + v.Error, false: "ok"}[v.Error != ""])
		b, _ := json.MarshalIndent(fo, "", " ")
		if err := os.WriteFile(cfg.fnOraclePath(), b, 0o644); err != nil {
			res.Error = err.Error()
			return res
		}
	}
	if err := cfg.checkout(ctx, r.R); err != nil {
		res.Error = err.Error()
		return res
	}
	state := filepath.Join(cfg.Work, "state", r.Dir)
	ix, err := cfg.open(ctx, state)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer ix.Close()
	res.Record = cfg.record(ctx, ix, cfg.Rerecord)
	if res.Record.Error != "" {
		res.Error = "record: " + res.Record.Error
		return res
	}
	res.Status, _ = ix.DynamicStatusOf(ctx, godyn.Eco)
	for _, k := range keys {
		o := latest[k]
		if o.Status != "killed" || only != nil && !only[o.Mutant.ID] {
			continue
		}
		if ctx.Err() != nil {
			res.Error = ctx.Err().Error()
			return res
		}
		m := GoDynMutant{ID: o.Mutant.ID, File: o.Mutant.File, Line: o.Mutant.Line, Op: o.Mutant.Op, Red: o.Red}
		v := fo[o.Mutant.ID]
		if v.Error != "" {
			m.Error = "per-function oracle: " + v.Error
		} else {
			for _, u := range o.Red {
				p := v.Units[u]
				if p == nil || p.Status != "fail" {
					m.NotReproduced = append(m.NotReproduced, u)
					continue
				}
				if f := p.failed(); len(f) > 0 {
					if m.RedFns == nil {
						m.RedFns = map[string][]string{}
					}
					m.RedFns[u] = f
				}
			}
			commit, _, err := r.MutantCommit(ctx, cfg.clone(), idx, o.Mutant)
			if err == nil {
				var st, hy model.Selection
				st, err = ix.Select(ctx, bgraph.Change{Base: r.R, Head: commit, Dynamic: bgraph.DynamicOff})
				if err == nil {
					hy, err = ix.Select(ctx, bgraph.Change{Base: r.R, Head: commit, Dynamic: bgraph.DynamicOn})
				}
				if err == nil {
					m.Static, m.Hybrid = goSummarize(st, counts, o.Red, m.RedFns), goSummarize(hy, counts, o.Red, m.RedFns)
					for _, u := range o.Red {
						if _, ok := m.Hybrid.Filter[u]; ok && len(m.RedFns[u]) == 0 && !slicesContains(m.NotReproduced, u) {
							m.Unattributed = append(m.Unattributed, u)
						}
					}
				}
			}
			if err != nil {
				m.Error = err.Error()
			}
		}
		cfg.logf("%s %s:%d %s: red %v fns %v; static %d pkgs/%d tests, hybrid %d run+%d compile-only pkgs/%d tests (removed %d)%s%s", m.ID, m.File,
			m.Line, m.Op, m.Red, m.RedFns, m.Static.Packages, m.Static.Tests, m.Hybrid.RunPackages, m.Hybrid.CompileOnly, m.Hybrid.Tests,
			len(m.Hybrid.Removed), map[bool]string{true: " VIOLATION " + strings.Join(m.Hybrid.Missed, ","), false: ""}[len(m.Hybrid.Missed) > 0]+
				map[bool]string{true: " UNATTRIBUTED " + strings.Join(m.Unattributed, ","), false: ""}[len(m.Unattributed) > 0],
			map[bool]string{true: "; skipped: " + m.Hybrid.Skip, false: ""}[m.Hybrid.Skip != ""]+m.Error)
		res.Mutants = append(res.Mutants, m)
	}
	return res
}

// fnVerdict applies the mutant to the clone's working tree (at R), reruns the oracle-red packages whole and restores the tree.
func (cfg GoDynConfig) fnVerdict(ctx context.Context, gl *goLayout, idx string, o T2Record) (v GoFnVerdict) {
	t0 := time.Now()
	defer func() { v.Ms = time.Since(t0).Milliseconds() }()
	_, mutated, err := cfg.Repo.MutantCommit(ctx, cfg.clone(), idx, o.Mutant)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	wt := filepath.Join(cfg.clone(), filepath.FromSlash(o.Mutant.File))
	orig, err := os.ReadFile(wt)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	defer func() {
		os.WriteFile(wt, orig, 0o644)
		git(context.Background(), cfg.clone(), "checkout", "-q", "-f", "--", ".")
		if st, err := git(context.Background(), cfg.clone(), "status", "--porcelain", "--untracked-files=no"); err != nil || strings.TrimSpace(st) != "" {
			v.Error = fmt.Sprintf("working tree not clean after the rerun: %v %.200s", err, st)
		}
	}()
	if err := os.WriteFile(wt, mutated, 0o644); err != nil {
		v.Error = err.Error()
		return v
	}
	run, _, err := goTestRun(ctx, gl, o.Red, "", 20*time.Minute)
	if err == nil {
		err = goResolveUnfinished(ctx, gl, run)
	}
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Units = run
	return v
}

// ---- guard rate ----

// GoGuardCommit is one first-parent commit.
type GoGuardCommit struct {
	C        string   `json:"c"`
	P        string   `json:"p"`
	Files    []string `json:"files"`
	Scope    string   `json:"scope"`
	Packages int      `json:"packages"` // selected Go package units
	Stage    string   `json:"stage"`    // accepted | refused | inactive (no narrowable Go unit selected) | error
	Reason   string   `json:"reason,omitempty"`
}

// GoGuardResult is the guard rate over one repo's history.
type GoGuardResult struct {
	Repo     string          `json:"repo"`
	Head     string          `json:"head"`
	Examined int             `json:"examined"`
	Commits  []GoGuardCommit `json:"commits"`
}

// RunGoGuardRate examines the n first-parent commits ending at rev in clone: the selection parent→commit with the Go layer on and
// no records (an index opened on the clone; nothing is checked out after rev).
func RunGoGuardRate(ctx context.Context, name, clone, stateDir, rev string, n int, logf func(string, ...any)) (GoGuardResult, error) {
	res := GoGuardResult{Repo: name, Head: rev}
	out, err := git(ctx, clone, "rev-list", "--first-parent", fmt.Sprintf("-n%d", n), rev)
	if err != nil {
		return res, err
	}
	os.RemoveAll(stateDir) // no records
	ix, err := bgraph.Open(ctx, clone, bgraph.Options{StateDir: stateDir})
	if err != nil {
		return res, err
	}
	defer ix.Close()
	for _, c := range strings.Fields(out) {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.Examined++
		gc := goGuardOne(ctx, ix, clone, c)
		if gc.P == "" {
			continue // root commit
		}
		if logf != nil {
			logf("%s guard %s: %d files: %s %s", name, c[:10], len(gc.Files), gc.Stage, gc.Reason)
		}
		res.Commits = append(res.Commits, gc)
	}
	return res, nil
}

func goGuardOne(ctx context.Context, ix *bgraph.Index, clone, c string) GoGuardCommit {
	gc := GoGuardCommit{C: c}
	p, err := git(ctx, clone, "rev-parse", c+"^1")
	if err != nil {
		return gc
	}
	gc.P = strings.TrimSpace(p)
	raw, err := git(ctx, clone, "diff", "--name-only", "--no-renames", gc.P, c)
	if err != nil {
		gc.Stage, gc.Reason = "error", err.Error()
		return gc
	}
	gc.Files = strings.Split(strings.TrimSpace(raw), "\n")
	sel, err := ix.Select(ctx, bgraph.Change{Base: gc.P, Head: c, Dynamic: bgraph.DynamicOn})
	if err != nil {
		gc.Stage, gc.Reason = "error", err.Error()
		return gc
	}
	gc.Scope = string(sel.Scope)
	for _, t := range sel.Tests {
		if godyn.Narrowable(t, godyn.Eco) {
			gc.Packages++
		}
	}
	why := ecoSkipReason(sel, godyn.Eco)
	switch {
	case why == "":
		gc.Stage, gc.Reason = "inactive", fmt.Sprintf("scope %s, %d units selected", sel.Scope, len(sel.Tests))
	case strings.HasPrefix(why, "no records"):
		gc.Stage = "accepted"
	default:
		gc.Stage, gc.Reason = "refused", why
	}
	return gc
}

// GoGuardBucket reduces a guard outcome to the buckets of the lexical estimate (worth-it check).
func GoGuardBucket(c GoGuardCommit) string {
	switch c.Stage {
	case "accepted":
		tests, src := 0, 0
		for _, f := range c.Files {
			switch {
			case strings.HasSuffix(f, "_test.go"):
				tests++
			case strings.HasSuffix(f, ".go"):
				src++
			}
		}
		switch {
		case src == 0 && tests > 0:
			return "accepted: test files only"
		case src == 0:
			return "accepted: documentation only"
		}
		return "accepted: Go sources"
	case "inactive":
		if c.Scope == string(model.TierFull) {
			return "inactive: full selection, no Go package"
		}
		return "inactive: no Go package selected"
	case "error":
		return "error"
	}
	w := c.Reason
	file, rest, ok := strings.Cut(w, ": ")
	if !ok || strings.Contains(file, " ") {
		file, rest = "", w
	}
	b := path.Base(file)
	switch {
	case strings.HasPrefix(w, "the selection is full"):
		return "refused: full selection"
	case strings.HasPrefix(w, "widening"):
		if strings.Contains(w, "manifest") && (strings.Contains(w, "go.mod") || strings.Contains(w, "go.sum") || strings.Contains(w, "go.work")) {
			return "refused: widening (go.mod / go.sum)"
		}
		return "refused: widening (other)"
	case b == "go.mod" || b == "go.sum" || b == "go.work" || b == "go.work.sum":
		return "refused: go.mod / go.sum / go.work"
	case strings.HasPrefix(rest, "status "):
		return "refused: not a modification (" + rest[:8] + ")"
	case strings.HasPrefix(rest, "mode"):
		return "refused: mode"
	case strings.HasPrefix(rest, "claimed"):
		return "refused: claimed build input"
	case strings.HasPrefix(rest, "a .go file under testdata"):
		return "refused: .go under testdata/vendor/./_"
	case strings.HasPrefix(rest, "not a Go source file"):
		return "refused: non-Go, non-doc file"
	case strings.HasPrefix(rest, "role "):
		return "refused: role (generated / non-source .go)"
	case strings.Contains(rest, "a change outside function bodies"), strings.Contains(rest, "does not pair with"):
		return "refused: declaration (change outside function bodies)"
	case strings.Contains(rest, "generated"):
		return "refused: generated file"
	case strings.Contains(rest, "cgo") || strings.Contains(rest, `"C"`):
		return "refused: cgo"
	case strings.Contains(rest, "build constraint"):
		return "refused: build constraint"
	case strings.Contains(rest, "//line directive"):
		return "refused: //line directive"
	case strings.Contains(rest, "parse"):
		return "refused: does not parse"
	}
	return "refused: " + RbReasonClass(w)
}

// ---- replay ----

// GoReplayCommit is one replayed commit.
type GoReplayCommit struct {
	C        string       `json:"c"`
	P        string       `json:"p"`
	Files    []string     `json:"files"`
	Stage    string       `json:"stage"` // the guard (no records): accepted | refused | inactive | error
	Reason   string       `json:"reason,omitempty"`
	Window   bool         `json:"window"` // in the n most recent commits (otherwise an extra guard-accepted commit)
	Static   GoSel        `json:"static"`
	Hybrid   GoSel        `json:"hybrid"`
	Record   GoRecordStat `json:"record"`
	SameAs   string       `json:"same_as_static,omitempty"` // not accepted: "yes" or what differs
	Verified []string     `json:"verified,omitempty"`       // packages run whole at C
	FailAtC  []string     `json:"fail_at_c,omitempty"`      // removed functions failing at C
	FailAtP  []string     `json:"fail_at_p,omitempty"`
	NoVerdct []string     `json:"no_verdict,omitempty"` // removed functions without a verdict at C (package did not build or crashed)
	Misses   []string     `json:"misses,omitempty"`
	VerifyMs int64        `json:"verify_ms"`
	Error    string       `json:"error,omitempty"`
}

// RunGoReplay replays commits (newest first). window: how many of them are the recent first-parent window (the rest are extra
// guard-accepted commits).
func RunGoReplay(ctx context.Context, cfg GoDynConfig, commits []string, window int) []GoReplayCommit {
	var out []GoReplayCommit
	gstate := filepath.Join(cfg.Work, "replay-guard-state", cfg.Repo.Dir)
	os.RemoveAll(gstate)
	if err := cfg.checkout(ctx, cfg.Repo.R); err != nil {
		return []GoReplayCommit{{Error: err.Error()}}
	}
	gix, err := cfg.open(ctx, gstate)
	if err != nil {
		return []GoReplayCommit{{Error: err.Error()}}
	}
	defer gix.Close()
	for i, c := range commits {
		if ctx.Err() != nil {
			break
		}
		rc := cfg.replayOne(ctx, gix, c)
		rc.Window = i < window
		cfg.logf("replay %s [%s]: %d files, static %d pkgs/%d tests, hybrid %d+%d pkgs/%d tests, removed %d, fail at C %v, at P %v, misses %v %s %s",
			c[:10], rc.Stage, len(rc.Files), rc.Static.Packages, rc.Static.Tests, rc.Hybrid.RunPackages, rc.Hybrid.CompileOnly, rc.Hybrid.Tests,
			len(rc.Hybrid.Removed), rc.FailAtC, rc.FailAtP, rc.Misses, rc.SameAs, rc.Error)
		out = append(out, rc)
	}
	if err := cfg.checkout(ctx, cfg.Repo.R); err != nil {
		cfg.logf("restore the clone at R: %v", err)
	}
	return out
}

func sameGoSel(a, b GoSel) string {
	if strings.Join(a.Units, " ") != strings.Join(b.Units, " ") {
		return "units differ"
	}
	if len(b.Removed) > 0 || len(b.Filter) > 0 {
		return "the hybrid filters functions"
	}
	if a.Tests != b.Tests {
		return "tests differ"
	}
	return "yes"
}

func (cfg GoDynConfig) replayOne(ctx context.Context, gix *bgraph.Index, c string) (rc GoReplayCommit) {
	g := goGuardOne(ctx, gix, cfg.clone(), c)
	rc.C, rc.P, rc.Files, rc.Stage, rc.Reason = c, g.P, g.Files, g.Stage, g.Reason
	if rc.P == "" {
		rc.Error = "root commit"
		return rc
	}
	counts, err := goTestCounts(ctx, cfg.clone(), c)
	if err != nil {
		rc.Error = err.Error()
		return rc
	}
	if g.Stage != "accepted" {
		st, err := gix.Select(ctx, bgraph.Change{Base: rc.P, Head: c, Dynamic: bgraph.DynamicOff})
		if err != nil {
			rc.Error = err.Error()
			return rc
		}
		hy, err := gix.Select(ctx, bgraph.Change{Base: rc.P, Head: c, Dynamic: bgraph.DynamicOn})
		if err != nil {
			rc.Error = err.Error()
			return rc
		}
		rc.Static, rc.Hybrid = goSummarize(st, counts, nil, nil), goSummarize(hy, counts, nil, nil)
		rc.SameAs = sameGoSel(rc.Static, rc.Hybrid)
		return rc
	}
	if err := cfg.checkout(ctx, rc.P); err != nil {
		rc.Error = err.Error()
		return rc
	}
	state := filepath.Join(cfg.Work, "replay-state", cfg.Repo.Dir, c[:12])
	os.RemoveAll(state)
	ix, err := cfg.open(ctx, state)
	if err != nil {
		rc.Error = err.Error()
		return rc
	}
	defer ix.Close()
	rc.Record = cfg.record(ctx, ix, true)
	if rc.Record.Error != "" {
		rc.Error = "record: " + rc.Record.Error
		return rc
	}
	st, err := ix.Select(ctx, bgraph.Change{Base: rc.P, Head: c, Dynamic: bgraph.DynamicOff})
	if err != nil {
		rc.Error = err.Error()
		return rc
	}
	hy, err := ix.Select(ctx, bgraph.Change{Base: rc.P, Head: c, Dynamic: bgraph.DynamicOn})
	if err != nil {
		rc.Error = err.Error()
		return rc
	}
	rc.Static, rc.Hybrid = goSummarize(st, counts, nil, nil), goSummarize(hy, counts, nil, nil)
	if len(rc.Hybrid.Removed) == 0 && rc.Hybrid.CompileOnly == 0 {
		return rc
	}
	removed := map[string][]string{} // unit -> removed function names
	for _, s := range rc.Hybrid.Removed {
		u, fn, _ := strings.Cut(s, "#")
		removed[u] = append(removed[u], fn)
	}
	for u, n := range rc.Hybrid.Filter {
		if n == 0 && removed[u] == nil {
			removed[u] = []string{}
		}
	}
	var units []string
	for u := range removed {
		units = append(units, u)
	}
	sort.Strings(units)
	rc.Verified = units
	t0 := time.Now()
	defer func() { rc.VerifyMs = time.Since(t0).Milliseconds() }()
	run := func(rev string, units []string) (map[string]*goPkgRun, error) {
		if err := cfg.checkout(ctx, rev); err != nil {
			return nil, err
		}
		gl, err := goLayoutAt(ctx, cfg.clone(), rev)
		if err != nil {
			return nil, err
		}
		res, _, err := goTestRun(ctx, gl, units, "", 30*time.Minute)
		if err == nil {
			err = goResolveUnfinished(ctx, gl, res)
		}
		return res, err
	}
	atC, err := run(c, units)
	if err != nil {
		rc.Error = "verify at C: " + err.Error()
		return rc
	}
	var redUnits []string
	for _, u := range units {
		p := atC[u]
		hit := false
		for _, fn := range removed[u] {
			switch {
			case p != nil && p.Tests[fn] == "fail":
				rc.FailAtC = append(rc.FailAtC, godyn.SubID(u, fn))
				hit = true
			case p == nil || p.Tests[fn] == "":
				rc.NoVerdct = append(rc.NoVerdct, godyn.SubID(u, fn))
				hit = true
			}
		}
		if hit {
			redUnits = append(redUnits, u)
		}
	}
	if len(redUnits) == 0 {
		return rc
	}
	atP, err := run(rc.P, redUnits)
	if err != nil {
		rc.Error = "verify at P: " + err.Error()
		return rc
	}
	for _, s := range append(append([]string(nil), rc.FailAtC...), rc.NoVerdct...) {
		u, fn, _ := strings.Cut(s, "#")
		if p := atP[u]; p != nil && p.Tests[fn] == "fail" {
			rc.FailAtP = append(rc.FailAtP, s)
		} else if slicesContains(rc.FailAtC, s) {
			rc.Misses = append(rc.Misses, s)
		}
	}
	return rc
}

// FirstParent lists the n first-parent commits ending at rev in clone, newest first.
func FirstParent(ctx context.Context, clone, rev string, n int) ([]string, error) {
	out, err := git(ctx, clone, "rev-list", "--first-parent", fmt.Sprintf("-n%d", n), rev)
	return strings.Fields(out), err
}

// goLayoutAt is loadGoLayout for an arbitrary revision of root (the module layout may differ from R's).
func goLayoutAt(ctx context.Context, root, rev string) (*goLayout, error) {
	return loadGoLayout(ctx, T2Repo{R: rev}, root)
}

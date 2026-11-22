package evalkit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"go/build"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/eco/golang"
	"github.com/qxbao/bgraph/internal/model"
)

// Go T2: the mutation oracle for Go repos. One-token bugs are seeded into non-test Go code at R, the whole suite (`go test -json -count=1
// ./...` in every module of the repo) runs on the mutated working tree, and every red package unit (`go:<dir>`) must be in Select(R->M).
// A package is red when its package-level event is `fail` (a test failed, the test binary crashed or the package / its tests failed to
// build). A mutant is "nocompile" when the mutated package itself does not build (`go build` of that package fails); then no suite runs.

// goModule is one go.mod of the repo at R.
type goModule struct {
	Dir  string // repo-relative, "." for the root
	Path string // module path
}

// goLayout maps `go test -json` package import paths back to repo directories.
type goLayout struct {
	root    string
	modules []goModule // sorted by descending module-path length (longest prefix first)
	units   map[string]bool
}

var reModuleLine = regexp.MustCompile(`(?m)^\s*module\s+("?)([^\s"]+)("?)\s*(//.*)?$`)

// t2GoIgnoredDir reports whether a repo directory is one the go tool and bgraph's module scan skip.
func t2GoIgnoredDir(d string) bool {
	if d == "." {
		return false
	}
	for _, seg := range strings.Split(d, "/") {
		if seg == "vendor" || seg == "testdata" || seg == "node_modules" || strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, "_") {
			return true
		}
	}
	return false
}

func loadGoLayout(ctx context.Context, r T2Repo, root string) (*goLayout, error) {
	ls, err := git(ctx, root, "ls-tree", "-r", "--name-only", r.R)
	if err != nil {
		return nil, err
	}
	gl := &goLayout{root: root, units: map[string]bool{}}
	files := strings.Split(strings.TrimSpace(ls), "\n")
	for _, f := range files {
		if path.Base(f) != "go.mod" || t2GoIgnoredDir(path.Dir(f)) {
			continue
		}
		b, _, err := r.blob(ctx, root, f)
		if err != nil {
			return nil, err
		}
		m := reModuleLine.FindSubmatch(b)
		if m == nil {
			return nil, fmt.Errorf("%s: no module line", f)
		}
		gl.modules = append(gl.modules, goModule{Dir: path.Dir(f), Path: string(m[2])})
	}
	if len(gl.modules) == 0 {
		return nil, fmt.Errorf("%s: no go.mod at R", r.Name)
	}
	sort.Slice(gl.modules, func(i, j int) bool { return len(gl.modules[i].Path) > len(gl.modules[j].Path) })
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") && !t2GoIgnoredDir(path.Dir(f)) && gl.moduleOfDir(path.Dir(f)) != nil {
			gl.units[golang.UnitID(path.Dir(f))] = true
		}
	}
	return gl, nil
}

// moduleOfDir returns the innermost module containing repo directory d.
func (gl *goLayout) moduleOfDir(d string) *goModule {
	var best *goModule
	for i := range gl.modules {
		m := &gl.modules[i]
		if m.Dir == "." || d == m.Dir || strings.HasPrefix(d, m.Dir+"/") {
			if best == nil || len(m.Dir) > len(best.Dir) || best.Dir == "." {
				best = m
			}
		}
	}
	return best
}

// dirOf maps an import path to its repo directory.
func (gl *goLayout) dirOf(imp string) (string, bool) {
	for _, m := range gl.modules {
		if imp == m.Path {
			return m.Dir, true
		}
		if rest, ok := strings.CutPrefix(imp, m.Path+"/"); ok {
			return path.Join(m.Dir, rest), true
		}
	}
	return "", false
}

// moduleArg returns the `go test` package argument for repo directory d relative to module m.
func moduleArg(m *goModule, d string) string {
	if d == m.Dir {
		return "."
	}
	rel := d
	if m.Dir != "." {
		rel = strings.TrimPrefix(d, m.Dir+"/")
	}
	return "./" + rel
}

func (r T2Repo) goEnv(cfg *T2Config) []string {
	tmp := filepath.Join(cfg.Dir, "tmp")
	os.MkdirAll(tmp, 0o755)
	return []string{"GOCACHE=" + filepath.Join(cfg.Dir, "gocache-"+r.Dir), "GOMODCACHE=D:\\cache\\go-mod", "GOFLAGS=-mod=mod", "GOTMPDIR=" + tmp,
		"TEMP=" + tmp, "TMP=" + tmp, "CGO_ENABLED=0", "GOTOOLCHAIN=auto", "GOPROXY=https://proxy.golang.org,direct"}
}

// goEvent is one line of `go test -json`.
type goEvent struct {
	Action      string `json:"Action"`
	Package     string `json:"Package"`
	Test        string `json:"Test"`
	FailedBuild string `json:"FailedBuild"`
	ImportPath  string `json:"ImportPath"`
	Output      string `json:"Output"`
}

// GoRun is the parsed outcome of one suite run (all modules).
type GoRun struct {
	Units    map[string]string   // unit -> passed | failed (only packages with test files)
	Names    map[string][]string // unit -> failed top-level test names
	Detail   string
	Unmapped []string
}

// parseGoTestJSON folds `go test -json` output into run. Non-JSON lines (go command errors) are kept in Detail.
func (gl *goLayout) parseGoTestJSON(out string, run *GoRun) {
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			if t := strings.TrimSpace(string(line)); t != "" && run.Detail == "" {
				run.Detail = t
			}
			continue
		}
		var ev goEvent
		if json.Unmarshal(line, &ev) != nil || ev.Package == "" {
			continue
		}
		d, ok := gl.dirOf(ev.Package)
		if !ok {
			run.Unmapped = append(run.Unmapped, ev.Package)
			continue
		}
		u := golang.UnitID(d)
		if ev.Test != "" {
			if ev.Action == "fail" && !strings.Contains(ev.Test, "/") {
				run.Names[u] = append(run.Names[u], ev.Test)
			}
			continue
		}
		switch ev.Action {
		case "pass":
			if run.Units[u] == "" {
				run.Units[u] = "passed"
			}
		case "fail":
			run.Units[u] = "failed"
			if ev.FailedBuild != "" && run.Detail == "" {
				run.Detail = "build failed: " + ev.FailedBuild
			}
		}
	}
}

// goSuite runs `go test -json -count=1` per module: the whole module (only == nil) or the given unit dirs of it.
func (r T2Repo) goSuite(ctx context.Context, cfg *T2Config, gl *goLayout, timeout time.Duration, only []string) (GoRun, time.Duration, bool, error) {
	run := GoRun{Units: map[string]string{}, Names: map[string][]string{}}
	args := map[string][]string{} // module dir -> package args
	var mods []string
	if only == nil {
		for _, m := range gl.modules {
			args[m.Dir] = []string{"./..."}
			mods = append(mods, m.Dir)
		}
	} else {
		for _, u := range only {
			d, ok := golang.ParseUnitID(u)
			m := gl.moduleOfDir(d)
			if !ok || m == nil {
				return run, 0, false, fmt.Errorf("unit %s is in no module", u)
			}
			if args[m.Dir] == nil {
				mods = append(mods, m.Dir)
			}
			args[m.Dir] = append(args[m.Dir], moduleArg(m, d))
		}
	}
	sort.Strings(mods)
	t0 := time.Now()
	deadline := t0.Add(timeout)
	for _, md := range mods {
		left := time.Until(deadline)
		if left <= 0 {
			return run, time.Since(t0), true, nil
		}
		a := append([]string{"test", "-json", "-count=1", "-timeout=15m"}, args[md]...)
		so, tail, to, err := runProc(ctx, filepath.Join(gl.root, filepath.FromSlash(md)), r.goEnv(cfg), left, "go", a...)
		if ctx.Err() != nil {
			return run, time.Since(t0), false, ctx.Err()
		}
		if to {
			return run, time.Since(t0), true, nil
		}
		before := len(run.Units)
		gl.parseGoTestJSON(so, &run)
		if err != nil && len(run.Units) == before {
			// the go command failed without reporting any package (bad go.mod, missing toolchain...)
			run.Detail = fmt.Sprintf("module %s: %v: %s %s", md, err, strings.TrimSpace(tail), run.Detail)
		}
	}
	// keep only packages that have test files at R (a package without tests reports `skip` or nothing)
	for u := range run.Units {
		if !gl.units[u] {
			delete(run.Units, u)
		}
	}
	for _, n := range run.Names {
		sort.Strings(n)
	}
	if len(run.Unmapped) > 0 {
		sort.Strings(run.Unmapped)
		run.Unmapped = dedup(run.Unmapped)
		return run, time.Since(t0), false, fmt.Errorf("unmapped go test packages: %v", run.Unmapped)
	}
	return run, time.Since(t0), false, nil
}

// goBuildsPkg reports whether the package in repo directory d builds (non-test files only); detail is the compiler's first lines otherwise.
func (r T2Repo) goBuildsPkg(ctx context.Context, cfg *T2Config, gl *goLayout, d string) (bool, string, error) {
	m := gl.moduleOfDir(d)
	if m == nil {
		return false, "", fmt.Errorf("directory %s is in no module", d)
	}
	// -o names a file: a single package (main or not) is written there and nothing lands in the checkout
	outFile := filepath.Join(cfg.Dir, "tmp", "build-"+r.Dir+".out")
	os.MkdirAll(filepath.Dir(outFile), 0o755)
	_, tail, to, err := runProc(ctx, filepath.Join(gl.root, filepath.FromSlash(m.Dir)), r.goEnv(cfg), 10*time.Minute, "go", "build", "-o", outFile, moduleArg(m, d))
	if ctx.Err() != nil {
		return false, "", ctx.Err()
	}
	if to {
		return false, "", fmt.Errorf("go build %s timed out", d)
	}
	if err != nil {
		t := strings.TrimSpace(tail)
		if len(t) > 400 {
			t = t[:400]
		}
		return false, t, nil
	}
	return true, "", nil
}

// goBaseline runs the suite twice at R; a unit red in both runs is pre-broken, one whose result differs (or is absent in one run) is flaky.
func (r T2Repo) goBaseline(ctx context.Context, cfg *T2Config, gl *goLayout) (T2Baseline, error) {
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
	var names []map[string][]string
	for i := 0; i < 2; i++ {
		run, d, to, err := r.goSuite(ctx, cfg, gl, 30*time.Minute, nil)
		if err != nil {
			return bl, err
		}
		if to || len(run.Units) == 0 {
			return bl, fmt.Errorf("%s: baseline run %d failed (timeout=%v): %s", r.Name, i+1, to, run.Detail)
		}
		cfg.logf("%s: baseline %d/2 %d units, %s", r.Name, i+1, len(run.Units), d.Round(time.Second))
		runs = append(runs, run.Units)
		names = append(names, run.Names)
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
			if both := intersectStr(names[0][u], names[1][u]); len(both) > 0 {
				if bl.FailingTests == nil {
					bl.FailingTests = map[string][]string{}
				}
				bl.FailingTests[u] = both
			}
		case a != b:
			bl.Flaky = append(bl.Flaky, u)
		}
	}
	sort.Strings(bl.Failing)
	sort.Strings(bl.Flaky)
	bl.Files = len(all)
	b, _ := json.MarshalIndent(bl, "", " ")
	os.WriteFile(bp, b, 0o644)
	return bl, nil
}

// goCandidateFiles lists the mutable Go files at R (see goMutableFile; build context = this host with cgo off, as the suite runs).
func (r T2Repo) goCandidateFiles(ctx context.Context, root string, gl *goLayout) ([]RustMutantSource, error) {
	ls, err := git(ctx, root, "ls-tree", "-r", "--name-only", r.R)
	if err != nil {
		return nil, err
	}
	bctx := build.Default
	bctx.CgoEnabled = false
	var out []RustMutantSource
	for _, f := range strings.Split(strings.TrimSpace(ls), "\n") {
		if !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") || gl.moduleOfDir(path.Dir(f)) == nil {
			continue
		}
		b, oid, err := r.blob(ctx, root, f)
		if err != nil {
			return nil, err
		}
		if !goMutableFile(f, b, &bctx) {
			continue
		}
		out = append(out, RustMutantSource{Path: f, Blob: oid, Data: b})
	}
	return out, nil
}

// goRedUnits splits the failed units of run into red (counted) and excluded (pre-broken / flaky at R). A unit failing at R stays red when a
// top-level test outside the baseline's failing set fails.
func goRedUnits(run GoRun, bl T2Baseline, excl map[string]bool) (red, excluded []string) {
	for u, st := range run.Units {
		if st != "failed" {
			continue
		}
		if base, refined := bl.FailingTests[u]; excl[u] && !(refined && len(newFailures(run.Names[u], base)) > 0) {
			excluded = append(excluded, u)
		} else {
			red = append(red, u)
		}
	}
	sort.Strings(red)
	sort.Strings(excluded)
	return red, excluded
}

// buildOracleGo runs mutants (sequentially: the working tree holds one mutation at a time) until Mutants killed ones are in the oracle,
// the try limit is reached or the budget deadline passes.
func (r T2Repo) buildOracleGo(ctx context.Context, cfg *T2Config, store *OracleStore, have map[string]T2Record) error {
	root := filepath.Join(cfg.Dir, r.Dir)
	if err := r.ensureClean(ctx, root); err != nil {
		return err
	}
	gl, err := loadGoLayout(ctx, r, root)
	if err != nil {
		return err
	}
	bl, err := r.goBaseline(ctx, cfg, gl)
	if err != nil {
		return err
	}
	if err := r.ensureClean(ctx, root); err != nil {
		return fmt.Errorf("after the baseline: %w", err)
	}
	cfg.logf("%s: baseline %d units, %d pre-broken, %d flaky", r.Name, bl.Files, len(bl.Failing), len(bl.Flaky))
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
		srcs, err := r.goCandidateFiles(ctx, root, gl)
		if err != nil {
			return err
		}
		muts = GenGoMutants(srcs, r.Seed, r.Dir, r.rustTries())
		b, _ := json.MarshalIndent(map[string]any{"r": r.R, "seed": r.Seed, "count": r.rustTries(), "mutants": muts}, "", " ")
		os.WriteFile(mp, b, 0o644)
	}
	cfg.logf("%s: %d mutants generated", r.Name, len(muts))
	timeout := time.Duration(medianInt64(bl.RunMs))*4*time.Millisecond + 3*time.Minute
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
				// the whole tracked tree: with -mod=mod the go command may add go.sum lines (e.g. go.mod hashes of a sub-module)
				git(context.Background(), root, "checkout", "-q", "--", ".")
			}()
			if err := os.WriteFile(wt, mutated, 0o644); err != nil {
				rec.Status, rec.Detail = "error", err.Error()
				return
			}
			okb, detail, err := r.goBuildsPkg(ctx, cfg, gl, path.Dir(m.File))
			switch {
			case err != nil:
				rec.Status, rec.Detail = "error", err.Error()
				return
			case !okb:
				rec.Status, rec.Detail = "nocompile", detail
				return
			}
			run, _, to, err := r.goSuite(ctx, cfg, gl, timeout, nil)
			switch {
			case err != nil:
				rec.Status, rec.Detail = "error", err.Error()
				return
			case to:
				rec.Status, rec.Detail = "timeout", "timed out after "+timeout.String()
				return
			case len(run.Units) == 0:
				rec.Status, rec.Detail = "error", "no unit ran: "+run.Detail
				return
			}
			rec.TestFiles = len(run.Units)
			rec.Red, rec.RedExcluded = goRedUnits(run, bl, excl)
			if err := r.confirmGoRed(ctx, cfg, gl, bl, excl, timeout, &rec); err != nil {
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
		cfg.logf("%s: %s %s %s:%d `%s` -> `%s`: %s (red %v) %s", r.Name, m.ID, m.Op, m.File, m.Line, short(m.Before), short(m.After), rec.Status, rec.Red, time.Duration(rec.Ms)*time.Millisecond)
	}
	return r.ensureClean(ctx, root)
}

// confirmGoRed re-runs the red units once (the tree still holds the mutation); a unit must be red again to count. The others go to
// RedUnconfirmed.
func (r T2Repo) confirmGoRed(ctx context.Context, cfg *T2Config, gl *goLayout, bl T2Baseline, excl map[string]bool, timeout time.Duration, rec *T2Record) error {
	cand := rec.Red
	rec.Red, rec.RedUnconfirmed, rec.Confirmed = nil, nil, true
	if len(cand) == 0 {
		return nil
	}
	run, _, to, err := r.goSuite(ctx, cfg, gl, timeout, cand)
	if err != nil {
		return err
	}
	again := map[string]bool{}
	if !to {
		red, _ := goRedUnits(run, bl, excl)
		for _, u := range red {
			again[u] = true
		}
	}
	for _, u := range cand {
		if again[u] {
			rec.Red = append(rec.Red, u)
		} else {
			rec.RedUnconfirmed = append(rec.RedUnconfirmed, u)
		}
	}
	return nil
}

// goNear describes, for each missed unit, the selected units in the same module directory tree (with their reason chains).
func goNear(sel model.Selection, missed []string) []string {
	var out []string
	for _, m := range missed {
		md, _ := golang.ParseUnitID(m)
		n := 0
		for _, u := range sel.Tests {
			ud, ok := golang.ParseUnitID(u.Unit)
			if ok && n < 4 && (path.Dir(ud) == path.Dir(md) || ud == path.Dir(md) || path.Dir(ud) == md) {
				out = append(out, m+" <- near "+u.Unit+": "+strings.Join(u.Reasons, " | "))
				n++
			}
		}
		if n == 0 {
			out = append(out, fmt.Sprintf("%s: no unit near it is selected (%d selected overall)", m, len(sel.Tests)))
		}
	}
	return out
}

// goReport writes the per-repo mutant accounting of the Go repos.
func goReport(b *strings.Builder, q T2Result) {
	any := false
	for _, r := range q.Repos {
		if r.Repo.Eco != "go" {
			continue
		}
		if !any {
			b.WriteString("\n## Go mutants\n\n| repo | R | units w/ tests | generated | nocompile | killed | survived | timeout | errors | selected < all | full | violations | median sel/units | multi-unit kills | pre-broken | flaky | suite ms |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
			any = true
		}
		partial, multi := 0, 0
		for _, c := range r.Checks {
			if c.Err == "" && !c.Full() && c.Units > 0 && c.Selected < c.Units {
				partial++
			}
			if c.Rec.Status == "killed" && len(c.Rec.Red) > 1 {
				multi++
			}
		}
		suite := int64(0)
		if len(r.Baseline.RunMs) > 0 {
			suite = r.Baseline.RunMs[len(r.Baseline.RunMs)-1]
		}
		fmt.Fprintf(b, "| %s | %.10s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %.3f | %d | %d | %d | %d |\n", r.Repo.Name, r.Repo.R, r.Baseline.Files, r.Generated, r.NoCompile,
			r.Killed, r.Survived, r.Timeout, r.Errors, partial, r.Full, len(r.Violations), r.MedBgraph, multi, len(r.Baseline.Failing), len(r.Baseline.Flaky), suite)
	}
}

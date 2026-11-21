package evalkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/eco/rust"
	"github.com/qxbao/bgraph/internal/model"
)

// Rust T2: the mutation oracle for Cargo workspaces. One-token bugs are seeded into non-test Rust code at R, the whole suite
// (`cargo test --workspace --no-fail-fast`, doc-tests included, benches not run) runs on the mutated working tree, and every red Cargo unit
// must be in Select(R->M). The units are the ones bgraph models: `<pkgdir>::lib|doc|bin:<n>|test:<n>`.

// rustTarget is one cargo target that `cargo test` runs.
type rustTarget struct {
	Unit string // unit ID
	Kind string // rust.Kind*
	Name string // target name
	Src  string // normalised absolute src_path
}

// rustMap maps cargo's test output back to unit IDs. It comes from `cargo metadata --no-deps` at R (the same source the cargometa command
// uses to validate bgraph's analysis).
type rustMap struct {
	root     string
	bySrc    map[string][]rustTarget // normalised src path -> targets
	docUnit  map[string]string       // crate name (underscored) -> doc unit
	pkgName  map[string]string       // pkg dir -> package name
	pkgDir   map[string]string       // package name -> pkg dir
	pkgDirs  []string
	allUnits map[string]bool
}

func normPath(p string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.ToSlash(p), "//?/"))
}

func crateName(n string) string { return strings.ReplaceAll(n, "-", "_") }

func loadRustMap(ctx context.Context, root string) (*rustMap, error) {
	cmd := exec.CommandContext(ctx, "cargo", "metadata", "--no-deps", "--format-version", "1")
	cmd.Dir = root
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("cargo metadata: %w: %.400s", err, se.String())
	}
	var m cmMeta
	if err := json.Unmarshal(so.Bytes(), &m); err != nil {
		return nil, err
	}
	rm := &rustMap{root: root, bySrc: map[string][]rustTarget{}, docUnit: map[string]string{}, pkgName: map[string]string{}, pkgDir: map[string]string{}, allUnits: map[string]bool{}}
	for _, p := range m.Packages {
		pdir := strings.TrimSuffix(strings.TrimSuffix(relToRoot(root, p.ManifestPath), "Cargo.toml"), "/")
		if pdir == "" {
			pdir = "."
		}
		rm.pkgName[pdir] = p.Name
		rm.pkgDir[p.Name] = pdir
		rm.pkgDirs = append(rm.pkgDirs, pdir)
		for _, t := range p.Targets {
			kind, ok := cargoKind(t.Kind)
			if !ok {
				continue
			}
			add := func(id, k string) {
				rm.allUnits[id] = true
				s := normPath(t.SrcPath)
				rm.bySrc[s] = append(rm.bySrc[s], rustTarget{Unit: id, Kind: k, Name: t.Name, Src: s})
			}
			switch kind {
			case rust.KindLib:
				if t.Test {
					add(rust.UnitID(pdir, kind, ""), kind)
				}
				if t.Doctest {
					id := rust.UnitID(pdir, rust.KindDoc, "")
					rm.docUnit[crateName(t.Name)] = id
					rm.allUnits[id] = true
				}
			case rust.KindBin, rust.KindTest:
				if t.Test {
					add(rust.UnitID(pdir, kind, t.Name), kind)
				}
			}
		}
	}
	sort.Strings(rm.pkgDirs)
	return rm, nil
}

var (
	reRunning  = regexp.MustCompile(`^\s*Running (?:(unittests) )?(.+?) \((.+)\)\s*$`)
	reDoctests = regexp.MustCompile(`^\s*Doc-tests (\S+)`)
	reFailedTg = regexp.MustCompile("^\\s*`(?:-p (\\S+) )?(--lib|--doc|--bin \\S+|--test \\S+)`")
	reTestFail = regexp.MustCompile(`^test (.+) \.\.\. FAILED$`)
	reExeStem  = regexp.MustCompile(`-[0-9a-f]{16}$`)
)

// RustRun is the parsed outcome of one `cargo test` invocation.
type RustRun struct {
	Units      map[string]string   // unit -> passed | failed
	Names      map[string][]string // unit -> sorted names of the tests that failed in it (as printed by libtest)
	CompileErr bool
	Detail     string   // first error line for a compile error / fatal failure
	Unmapped   []string // headers that could not be mapped to a unit
}

// parseCargoTest maps the output of `cargo test` to unit states. A unit is red when its section holds `test result: FAILED`, or when cargo's
// final summary lists it as a failed target (this also catches a test binary that crashed before printing a result).
func (rm *rustMap) parseCargoTest(out string) RustRun {
	run := RustRun{Units: map[string]string{}, Names: map[string][]string{}}
	cur := ""
	defer func() {
		for _, n := range run.Names {
			sort.Strings(n)
		}
	}()
	mark := func(u, st string) {
		if u == "" {
			return
		}
		if st == "failed" || run.Units[u] == "" {
			run.Units[u] = st
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := reRunning.FindStringSubmatch(line); m != nil {
			cur = ""
			unit := m[1] == "unittests"
			p := m[2]
			var cands []rustTarget
			if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
				cands = rm.bySrc[normPath(p)]
			} else {
				// cargo prints the path relative to the workspace root for the root package and relative to the package for members
				cands = append(cands, rm.bySrc[normPath(filepath.Join(rm.root, p))]...)
				for _, pd := range rm.pkgDirs {
					if pd != "." {
						cands = append(cands, rm.bySrc[normPath(filepath.Join(rm.root, filepath.FromSlash(pd), p))]...)
					}
				}
			}
			stem := strings.TrimSuffix(path.Base(filepath.ToSlash(m[3])), ".exe")
			stem = crateName(reExeStem.ReplaceAllString(stem, ""))
			var pick []rustTarget
			for _, c := range cands {
				if unit == (c.Kind != rust.KindTest) {
					pick = append(pick, c)
				}
			}
			if len(pick) > 1 {
				var by []rustTarget
				for _, c := range pick {
					if crateName(c.Name) == stem {
						by = append(by, c)
					}
				}
				if len(by) > 0 {
					pick = by
				}
			}
			if len(pick) == 0 {
				run.Unmapped = append(run.Unmapped, strings.TrimSpace(line))
				continue
			}
			cur = pick[0].Unit
			mark(cur, "passed")
			continue
		}
		if m := reDoctests.FindStringSubmatch(line); m != nil {
			cur = rm.docUnit[crateName(m[1])]
			if cur == "" {
				run.Unmapped = append(run.Unmapped, strings.TrimSpace(line))
				continue
			}
			mark(cur, "passed")
			continue
		}
		if cur != "" && strings.HasPrefix(line, "test result: FAILED") {
			mark(cur, "failed")
		}
		if m := reTestFail.FindStringSubmatch(line); m != nil && cur != "" {
			run.Names[cur] = append(run.Names[cur], m[1])
		}
		if m := reFailedTg.FindStringSubmatch(line); m != nil {
			pd, ok := rm.pkgDir[m[1]]
			if m[1] == "" && len(rm.pkgDirs) == 1 { // single-package workspace: cargo omits `-p`
				pd, ok = rm.pkgDirs[0], true
			}
			if ok {
				f := strings.Fields(m[2])
				switch f[0] {
				case "--lib":
					mark(rust.UnitID(pd, rust.KindLib, ""), "failed")
				case "--doc":
					mark(rust.UnitID(pd, rust.KindDoc, ""), "failed")
				case "--bin":
					mark(rust.UnitID(pd, rust.KindBin, f[1]), "failed")
				case "--test":
					mark(rust.UnitID(pd, rust.KindTest, f[1]), "failed")
				}
			}
		}
		if run.Detail == "" && strings.HasPrefix(line, "error") && !strings.HasPrefix(line, "error: test failed") && !strings.HasPrefix(line, "error: doctest failed") &&
			!strings.Contains(line, "target failed") && !strings.Contains(line, "targets failed") {
			run.Detail = line
		}
		if strings.HasPrefix(line, "error: could not compile") || strings.HasPrefix(line, "error: failed to run custom build command") {
			run.CompileErr = true
		}
	}
	// A compile failure of a test target can also be a doc-test run failing to build; those show up as `test result: FAILED` and are not
	// compile errors here. Only a cargo-level compile error (no unit ran) is.
	if run.CompileErr && len(run.Units) > 0 {
		anyRan := false
		for _, st := range run.Units {
			if st == "passed" || st == "failed" {
				anyRan = true
			}
		}
		if anyRan {
			run.CompileErr = false
		}
	}
	return run
}

// runCombined runs a command with stdout and stderr on the same pipe so the order of cargo's header lines and the test output is kept.
func runCombined(ctx context.Context, dir string, env []string, timeout time.Duration, name string, args ...string) (out string, timedOut bool, err error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err = cmd.Start(); err != nil {
		return "", false, err
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
		return "", false, ctx.Err()
	}
	return buf.String(), timedOut, err
}

func (r T2Repo) rustEnv(cfg *T2Config) []string {
	return []string{"CARGO_TARGET_DIR=" + filepath.Join(cfg.Dir, "target-"+r.Dir), "CARGO_TERM_COLOR=never", "CARGO_TERM_PROGRESS_WHEN=never", "RUST_BACKTRACE=0"}
}

// rustSuite runs the whole suite (only == nil) or the given cargo test selector args.
func (r T2Repo) rustSuite(ctx context.Context, cfg *T2Config, rm *rustMap, timeout time.Duration, only []string) (RustRun, time.Duration, bool, error) {
	args := []string{"test"}
	if only == nil {
		args = append(args, "--workspace")
	} else {
		args = append(args, only...)
	}
	args = append(args, "--no-fail-fast")
	t0 := time.Now()
	out, to, err := runCombined(ctx, rm.root, r.rustEnv(cfg), timeout, "cargo", args...)
	d := time.Since(t0)
	if ctx.Err() != nil {
		return RustRun{}, d, false, ctx.Err()
	}
	if to {
		return RustRun{}, d, true, nil
	}
	_ = err // a non-zero exit is the normal "some test failed" outcome; the output decides
	run := rm.parseCargoTest(out)
	if len(run.Unmapped) > 0 {
		return run, d, false, fmt.Errorf("unmapped cargo output headers: %v", run.Unmapped)
	}
	if !run.CompileErr && len(run.Units) == 0 {
		tail := out
		if len(tail) > 600 {
			tail = tail[len(tail)-600:]
		}
		run.Detail = "no test unit ran: " + tail
	}
	return run, d, false, nil
}

// rustSelector returns the `cargo test` arguments that run exactly one unit.
func (rm *rustMap) rustSelector(unit string) ([]string, bool) {
	pd, kind, name, ok := rust.ParseUnitID(unit)
	if !ok {
		return nil, false
	}
	pn := rm.pkgName[pd]
	switch kind {
	case rust.KindLib:
		return []string{"-p", pn, "--lib"}, true
	case rust.KindDoc:
		return []string{"-p", pn, "--doc"}, true
	case rust.KindBin:
		return []string{"-p", pn, "--bin", name}, true
	case rust.KindTest:
		return []string{"-p", pn, "--test", name}, true
	}
	return nil, false
}

// rustBaseline runs the suite twice at R; a unit red in both runs is already broken, one whose result differs (or is absent in one run) is flaky.
func (r T2Repo) rustBaseline(ctx context.Context, cfg *T2Config, rm *rustMap) (T2Baseline, error) {
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
		run, d, to, err := r.rustSuite(ctx, cfg, rm, 30*time.Minute, nil)
		if err != nil {
			return bl, err
		}
		if to || run.CompileErr || len(run.Units) == 0 {
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
		case a != b || a == "failed":
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

// rustCandidateFiles lists the Rust files at R that belong to lib/bin targets: under a package directory, not under tests/, benches/, examples/,
// not build.rs.
func (r T2Repo) rustCandidateFiles(ctx context.Context, root string, rm *rustMap) ([]RustMutantSource, error) {
	ls, err := git(ctx, root, "ls-tree", "-r", "--name-only", r.R)
	if err != nil {
		return nil, err
	}
	var out []RustMutantSource
	for _, f := range strings.Split(strings.TrimSpace(ls), "\n") {
		if !strings.HasSuffix(f, ".rs") || path.Base(f) == "build.rs" {
			continue
		}
		bad := false
		for _, seg := range strings.Split(path.Dir(f), "/") {
			switch seg {
			case "tests", "benches", "examples", "target", "fuzz", "vendor", "testdata":
				bad = true
			}
		}
		in := false
		for _, pd := range rm.pkgDirs {
			if pd == "." || strings.HasPrefix(f, pd+"/") {
				in = true
			}
		}
		if bad || !in {
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

func (r T2Repo) rustTries() int {
	if r.Tries > 0 {
		return r.Tries
	}
	return 3 * r.Mutants
}

// buildOracleRust runs mutants (sequentially: the working tree holds one mutation at a time) until Mutants killed ones are in the oracle,
// the try limit is reached or the budget deadline passes.
func (r T2Repo) buildOracleRust(ctx context.Context, cfg *T2Config, store *OracleStore, have map[string]T2Record) error {
	root := filepath.Join(cfg.Dir, r.Dir)
	if err := r.ensureClean(ctx, root); err != nil {
		return err
	}
	rm, err := loadRustMap(ctx, root)
	if err != nil {
		return err
	}
	bl, err := r.rustBaseline(ctx, cfg, rm)
	if err != nil {
		return err
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
		srcs, err := r.rustCandidateFiles(ctx, root, rm)
		if err != nil {
			return err
		}
		muts = GenRustMutants(srcs, r.Seed, r.Dir, r.rustTries())
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
				git(context.Background(), root, "checkout", "-q", "--", m.File)
			}()
			if err := os.WriteFile(wt, mutated, 0o644); err != nil {
				rec.Status, rec.Detail = "error", err.Error()
				return
			}
			run, _, to, err := r.rustSuite(ctx, cfg, rm, timeout, nil)
			switch {
			case err != nil:
				rec.Status, rec.Detail = "error", err.Error()
				return
			case to:
				rec.Status, rec.Detail = "timeout", "timed out after "+timeout.String()
				return
			case run.CompileErr:
				rec.Status, rec.Detail = "nocompile", run.Detail
				return
			case len(run.Units) == 0:
				rec.Status, rec.Detail = "error", run.Detail
				return
			}
			rec.TestFiles = len(run.Units)
			for u, st := range run.Units {
				if st != "failed" {
					continue
				}
				if base, refined := bl.FailingTests[u]; excl[u] && !(refined && len(newFailures(run.Names[u], base)) > 0) {
					rec.RedExcluded = append(rec.RedExcluded, u)
				} else {
					rec.Red = append(rec.Red, u)
				}
			}
			sort.Strings(rec.RedExcluded)
			if err := r.confirmRustRed(ctx, cfg, rm, bl, timeout, &rec); err != nil {
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

// confirmRustRed re-runs each red unit alone (the tree still holds the mutation); a unit must be red again to count. The others go to
// RedUnconfirmed.
func (r T2Repo) confirmRustRed(ctx context.Context, cfg *T2Config, rm *rustMap, bl T2Baseline, timeout time.Duration, rec *T2Record) error {
	cand := rec.Red
	rec.Red, rec.RedUnconfirmed, rec.Confirmed = nil, nil, true
	for _, u := range cand {
		sel, ok := rm.rustSelector(u)
		if !ok {
			return fmt.Errorf("no cargo selector for unit %s", u)
		}
		run, _, to, err := r.rustSuite(ctx, cfg, rm, timeout, sel)
		if err != nil {
			return err
		}
		base, refined := bl.FailingTests[u]
		if !to && !run.CompileErr && run.Units[u] == "failed" && (!refined || len(newFailures(run.Names[u], base)) > 0) {
			rec.Red = append(rec.Red, u)
		} else {
			rec.RedUnconfirmed = append(rec.RedUnconfirmed, u)
		}
	}
	return nil
}

func intersectStr(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if in[x] {
			out = append(out, x)
		}
	}
	return out
}

// newFailures returns the names in cur that are not in base.
func newFailures(cur, base []string) []string {
	in := map[string]bool{}
	for _, x := range base {
		in[x] = true
	}
	var out []string
	for _, x := range cur {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}

// rustNear describes, for each missed unit, the selected units of the same package (with their reason chains), which is what a reviewer
// needs to see why the missed one was not reached.
func rustNear(sel model.Selection, missed []string) []string {
	var out []string
	for _, m := range missed {
		pd, _, _, _ := rust.ParseUnitID(m)
		n := 0
		for _, u := range sel.Tests {
			if upd, _, _, ok := rust.ParseUnitID(u.Unit); ok && upd == pd && n < 4 {
				out = append(out, m+" <- near "+u.Unit+": "+strings.Join(u.Reasons, " | "))
				n++
			}
		}
		if n == 0 {
			out = append(out, fmt.Sprintf("%s: no unit of package %s is selected (%d selected overall)", m, pd, len(sel.Tests)))
		}
	}
	return out
}

package evalkit

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/eco/dotnet"
	"github.com/qxbao/bgraph/internal/eco/jvm"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
)

// JVM and .NET T2: the mutation oracle for Maven, Gradle and dotnet test builds. One-token bugs are seeded into non-test Java/Kotlin/C#
// source at R, the whole suite runs on the mutated working tree, and every red UNIT (Maven module, Gradle project, .NET test project) must
// be in Select(R->M). A unit is red when any of its tests failed or errored in the run; the red units are re-run alone to confirm.

// jvmT2Tools are the portable tools of the evaluation drive (never PATH, never the registry).
var jvmT2Tools = JVMTools{JDK17: `D:\bench\tools\jdk17`, JDK21: `D:\bench\tools\jdk21`, Maven: `D:\bench\tools\maven`, M2: `D:\bench\tools\m2`,
	GradleHome: `D:\bench\tools\gradle-home`, Tmp: `D:\bench\tools\tmp`}

const dotnetRoot = `D:\bench\tools\dotnet`

func (r T2Repo) jdJDK() string {
	if r.Jdk == "21" {
		return jvmT2Tools.JDK21
	}
	return jvmT2Tools.JDK17
}

func (r T2Repo) jdEnv() []string {
	t := jvmT2Tools
	return []string{"JAVA_HOME=" + r.jdJDK(), "GRADLE_USER_HOME=" + t.GradleHome, "TEMP=" + t.Tmp, "TMP=" + t.Tmp,
		"MAVEN_OPTS=-Djava.io.tmpdir=" + t.Tmp, "DOTNET_ROOT=" + dotnetRoot, "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1",
		"DOTNET_ROLL_FORWARD=Major", "DOTNET_MULTILEVEL_LOOKUP=0", "NUGET_PACKAGES=" + filepath.Join(filepath.Dir(t.Maven), "nuget"),
		"MSBUILDDISABLENODEREUSE=1", "CI=true"}
}

// JDRun is the outcome of one suite run.
type JDRun struct {
	Units     map[string]string // unit ID -> passed | failed
	NoCompile bool              // the build failed to compile (no verdict about the tests)
	Detail    string
}

type jdSuite struct {
	XMLName  xml.Name `xml:"testsuite"`
	Failures int      `xml:"failures,attr"`
	Errors   int      `xml:"errors,attr"`
	Tests    int      `xml:"tests,attr"`
	Cases    []struct {
		Failure []struct{} `xml:"failure"`
		Error   []struct{} `xml:"error"`
	} `xml:"testcase"`
}

// jdUnitDirs maps the directory of every Maven module / Gradle project at R to its unit IDs.
func (r T2Repo) jdUnitDirs(ctx context.Context, root string) (map[string][]string, error) {
	gt, err := jvmGitTreeP(ctx, root, r.R)
	if err != nil {
		return nil, err
	}
	defer gt.Close()
	a, err := jvm.New(nil).Analyze(ctx, gt, scancache.New())
	if err != nil {
		return nil, err
	}
	m := map[string][]string{}
	for _, u := range a.Tests {
		if (r.Runner == "maven") == strings.HasPrefix(u.ID, "maven:") {
			d := u.Project
			if d == "" {
				d = "."
			}
			m[d] = append(m[d], u.ID)
		}
	}
	return m, nil
}

// jdReports parses the JUnit XML reports below root (Maven surefire, Gradle test results) into per-unit verdicts.
func (r T2Repo) jdReports(root string, dirs map[string][]string) (map[string]string, error) {
	units := map[string]string{}
	marker := "/target/surefire-reports/"
	if r.Runner == "gradle" {
		marker = "/build/test-results/"
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".gradle":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasPrefix(d.Name(), "TEST-") || !strings.HasSuffix(d.Name(), ".xml") {
			return nil
		}
		rel := "/" + filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
		i := strings.Index(rel, marker)
		if i < 0 {
			return nil
		}
		dir := strings.TrimPrefix(rel[:i], "/")
		if dir == "" {
			dir = "."
		}
		ids := dirs[dir]
		if len(ids) == 0 {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var s jdSuite
		if xml.Unmarshal(b, &s) != nil {
			return nil
		}
		red := s.Failures+s.Errors > 0
		for _, c := range s.Cases {
			red = red || len(c.Failure)+len(c.Error) > 0
		}
		for _, id := range ids {
			if red {
				units[id] = "failed"
			} else if units[id] == "" {
				units[id] = "passed"
			}
		}
		return nil
	})
	return units, err
}

func removeReports(root, marker string) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".gradle":
				return filepath.SkipDir
			}
			if strings.HasSuffix(filepath.ToSlash(p)+"/", marker) {
				os.RemoveAll(p)
				return filepath.SkipDir
			}
		}
		return nil
	})
}

var compileErrMarkers = []string{"COMPILATION ERROR", "Compilation failure", "Compilation failed", "error CS", "error: ", "e: file:///", "Could not compile"}

func looksLikeCompileError(out string) bool {
	for _, m := range compileErrMarkers {
		if strings.Contains(out, m) {
			return true
		}
	}
	return false
}

// jdSuite runs the whole suite (only == nil) or only the given units. dirs is jdUnitDirs.
func (r T2Repo) jdRunSuite(ctx context.Context, cfg *T2Config, root string, dirs map[string][]string, timeout time.Duration, only []string) (JDRun, time.Duration, bool, error) {
	t0 := time.Now()
	env := r.jdEnv()
	switch r.Runner {
	case "maven":
		removeReports(root, "/target/surefire-reports/")
		args := []string{"-B", "-ntp", "-Dmaven.repo.local=" + jvmT2Tools.M2, "-Dmaven.test.failure.ignore=true", "-Dsurefire.failIfNoSpecifiedTests=false"}
		args = append(args, r.Args...)
		if only == nil && len(r.Targets) > 0 {
			args = append(args, "-pl", strings.Join(r.Targets, ","))
		}
		if only != nil {
			var pl []string
			for _, u := range only {
				if _, d, _, ok := jvm.ParseUnitID(u); ok {
					pl = append(pl, d)
				}
			}
			args = append(args, "-pl", strings.Join(pl, ","), "-am")
		}
		args = append(args, "test")
		out, tail, to, err := runProc(ctx, root, env, timeout, filepath.Join(jvmT2Tools.Maven, "bin", "mvn.cmd"), args...)
		d := time.Since(t0)
		if ctx.Err() != nil {
			return JDRun{}, d, false, ctx.Err()
		}
		if to {
			return JDRun{}, d, true, nil
		}
		units, rerr := r.jdReports(root, dirs)
		if rerr != nil {
			return JDRun{}, d, false, rerr
		}
		run := JDRun{Units: units}
		if err != nil || len(units) == 0 {
			if looksLikeCompileError(out + tail) {
				run.NoCompile, run.Detail = true, firstN(mvnError(out+"\n"+tail), 300)
			} else if len(units) == 0 {
				return run, d, false, fmt.Errorf("maven produced no test report: %s", firstN(mvnError(out+"\n"+tail), 300))
			}
		}
		return run, d, false, nil
	case "gradle":
		args := []string{"--console=plain", "--continue", "-q"}
		args = append(args, r.Args...)
		if only != nil {
			args = append(args, "cleanTest")
			for _, u := range only {
				if _, _, pp, ok := jvm.ParseUnitID(u); ok {
					args = append(args, strings.TrimSuffix(pp, ":")+":test")
				}
			}
		} else {
			args = append(args, "cleanTest", "test")
		}
		out, tail, to, err := runProc(ctx, root, env, timeout, filepath.Join(root, "gradlew.bat"), args...)
		d := time.Since(t0)
		if ctx.Err() != nil {
			return JDRun{}, d, false, ctx.Err()
		}
		if to {
			return JDRun{}, d, true, nil
		}
		units, rerr := r.jdReports(root, dirs)
		if rerr != nil {
			return JDRun{}, d, false, rerr
		}
		run := JDRun{Units: units}
		if err != nil && looksLikeCompileError(out+tail) {
			// a compile error anywhere stops the dependants; --continue keeps the independent projects running, but the run has no full verdict
			run.NoCompile, run.Detail = true, firstN(gradleErr(out+"\n"+tail), 300)
		} else if len(units) == 0 {
			return run, d, false, fmt.Errorf("gradle produced no test report: %s", firstN(gradleErr(out+"\n"+tail), 300))
		}
		return run, d, false, nil
	}
	// dotnet: one `dotnet test` per target project; the unit is the project file.
	targets := r.Targets
	if only != nil {
		targets = nil
		for _, u := range only {
			if p, ok := dotnet.ParseUnitID(u); ok {
				targets = append(targets, p)
			}
		}
	}
	run := JDRun{Units: map[string]string{}}
	trx := filepath.Join(cfg.Dir, "tmp", r.Dir, "trx")
	os.RemoveAll(trx)
	for i, tp := range targets {
		name := fmt.Sprintf("u%d", i)
		args := []string{"test", filepath.FromSlash(tp), "--nologo", "-v", "q", "--logger", "trx;LogFileName=" + name + ".trx", "--results-directory", filepath.Join(trx, name)}
		args = append(args, r.Args...)
		out, tl, to, err := runProc(ctx, root, env, timeout, filepath.Join(dotnetRoot, "dotnet.exe"), args...)
		if ctx.Err() != nil {
			return JDRun{}, time.Since(t0), false, ctx.Err()
		}
		if to {
			return JDRun{}, time.Since(t0), true, nil
		}
		id := dotnet.UnitID(tp)
		b, rerr := os.ReadFile(filepath.Join(trx, name, name+".trx"))
		if rerr != nil {
			if looksLikeCompileError(out + tl) {
				run.NoCompile, run.Detail = true, firstN(strings.TrimSpace(tail(out+"\n"+tl, 300)), 300)
				continue
			}
			return run, time.Since(t0), false, fmt.Errorf("%s: no trx report (%v): %s", tp, err, firstN(strings.TrimSpace(tl), 300))
		}
		var res struct {
			Results struct {
				R []struct {
					Outcome string `xml:"outcome,attr"`
				} `xml:"UnitTestResult"`
			} `xml:"Results"`
		}
		if xml.NewDecoder(bytes.NewReader(b)).Decode(&res) != nil {
			return run, time.Since(t0), false, fmt.Errorf("%s: unreadable trx", tp)
		}
		st := "passed"
		for _, x := range res.Results.R {
			if x.Outcome == "Failed" || x.Outcome == "Error" || x.Outcome == "Timeout" || x.Outcome == "Aborted" {
				st = "failed"
			}
		}
		run.Units[id] = st
	}
	return run, time.Since(t0), false, nil
}

// jdBaseline runs the suite twice at R; a unit red in both runs is already broken, one whose result differs (or is absent in one) is flaky.
func (r T2Repo) jdBaseline(ctx context.Context, cfg *T2Config, root string, dirs map[string][]string) (T2Baseline, map[string]bool, error) {
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
		run, d, to, err := r.jdRunSuite(ctx, cfg, root, dirs, 30*time.Minute, nil)
		if err != nil {
			return bl, nil, err
		}
		if to || run.NoCompile || len(run.Units) == 0 {
			return bl, nil, fmt.Errorf("%s: baseline run %d failed (timeout=%v, nocompile=%v): %s", r.Name, i+1, to, run.NoCompile, run.Detail)
		}
		cfg.logf("%s: baseline %d/2 %d units, %s", r.Name, i+1, len(run.Units), d.Round(time.Second))
		runs = append(runs, run.Units)
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

func (r T2Repo) jdCandidateSources(ctx context.Context, root string) ([]RustMutantSource, error) {
	ls, err := git(ctx, root, "ls-tree", "-r", "--name-only", r.R)
	if err != nil {
		return nil, err
	}
	var out []RustMutantSource
	for _, f := range strings.Split(strings.TrimSpace(ls), "\n") {
		f = strings.TrimSpace(f)
		ok := jvmCandidateFile(f)
		if r.Eco == "dotnet" {
			ok = dotnetCandidateFile(f)
		}
		if !ok {
			continue
		}
		if len(r.Include) > 0 {
			in := false
			for _, p := range r.Include {
				in = in || strings.HasPrefix(f, p)
			}
			if !in {
				continue
			}
		}
		b, oid, err := r.blob(ctx, root, f)
		if err != nil {
			return nil, err
		}
		out = append(out, RustMutantSource{Path: f, Blob: oid, Data: b})
	}
	return out, nil
}

// buildOracleJD runs mutants (sequentially: the working tree holds one mutation at a time) until Mutants killed ones are in the oracle, the
// try limit is reached or the budget deadline passes.
func (r T2Repo) buildOracleJD(ctx context.Context, cfg *T2Config, store *OracleStore, have map[string]T2Record) error {
	root := filepath.Join(cfg.Dir, r.Dir)
	if err := r.ensureClean(ctx, root); err != nil {
		return err
	}
	var dirs map[string][]string
	if r.Eco == "jvm" {
		var err error
		if dirs, err = r.jdUnitDirs(ctx, root); err != nil {
			return err
		}
	}
	bl, known, err := r.jdBaseline(ctx, cfg, root, dirs)
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
		srcs, err := r.jdCandidateSources(ctx, root)
		if err != nil {
			return err
		}
		muts = GenCFamilyMutants(srcs, r.Eco == "dotnet", r.Seed, r.Dir, r.rustTries())
		b, _ := json.MarshalIndent(map[string]any{"r": r.R, "seed": r.Seed, "count": r.rustTries(), "mutants": muts}, "", " ")
		os.WriteFile(mp, b, 0o644)
	}
	cfg.logf("%s: %d mutants generated", r.Name, len(muts))
	timeout := time.Duration(medianInt64(bl.RunMs))*4*time.Millisecond + 2*time.Minute
	if timeout > 25*time.Minute {
		timeout = 25 * time.Minute
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
			run, _, to, err := r.jdRunSuite(ctx, cfg, root, dirs, timeout, nil)
			switch {
			case err != nil:
				rec.Status, rec.Detail = "error", err.Error()
				return
			case to:
				rec.Status, rec.Detail = "timeout", "timed out after "+timeout.String()
				return
			case run.NoCompile:
				rec.Status, rec.Detail = "nocompile", run.Detail
				return
			}
			rec.TestFiles = len(run.Units)
			for u, st := range run.Units {
				if st != "failed" {
					continue
				}
				if excl[u] {
					rec.RedExcluded = append(rec.RedExcluded, u)
				} else {
					rec.Red = append(rec.Red, u)
				}
			}
			sort.Strings(rec.RedExcluded)
			sort.Strings(rec.Red)
			if err := r.confirmJDRed(ctx, cfg, root, dirs, known, timeout, &rec); err != nil {
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

// confirmJDRed re-runs the red units alone (the tree still holds the mutation); a unit must be red again to count. The others go to
// RedUnconfirmed.
func (r T2Repo) confirmJDRed(ctx context.Context, cfg *T2Config, root string, dirs map[string][]string, known map[string]bool, timeout time.Duration, rec *T2Record) error {
	cand := rec.Red
	rec.Red, rec.RedUnconfirmed, rec.Confirmed = nil, nil, true
	if len(cand) == 0 {
		return nil
	}
	run, _, to, err := r.jdRunSuite(ctx, cfg, root, dirs, timeout, cand)
	if err != nil {
		return err
	}
	for _, u := range cand {
		if !to && !run.NoCompile && run.Units[u] == "failed" {
			rec.Red = append(rec.Red, u)
		} else {
			rec.RedUnconfirmed = append(rec.RedUnconfirmed, u)
		}
	}
	return nil
}

// jdNear describes, for each missed unit, the selected units of the same directory with their reason chains.
func jdNear(sel model.Selection, missed []string) []string {
	var out []string
	unitDir := func(id string) string {
		if p, ok := dotnet.ParseUnitID(id); ok {
			return path.Dir(p)
		}
		if _, d, _, ok := jvm.ParseUnitID(id); ok {
			return d
		}
		return id
	}
	for _, m := range missed {
		n := 0
		for _, u := range sel.Tests {
			if unitDir(u.Unit) == unitDir(m) && n < 4 {
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

package evalkit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qxbao/bgraph/internal/eco/dart"
	"github.com/qxbao/bgraph/internal/scancache"
)

// Dart T2: the mutation oracle for Dart repos. One-token bugs are seeded into package code (lib/, bin/) at R and the whole
// `dart test` suite of every package listed in the manifest runs on the mutated working tree, one `dart test --reporter json`
// process per package (packages run concurrently). The unit is the test file (bgraph's Dart unit ID is the repo-relative
// path), so the verdict is per suite: a suite is red when one of its tests fails or errors (an error reported after the
// test completed included) or when the suite fails to load. A test red in either baseline run at R is excluded, red units
// are confirmed by re-running the suites of their packages, and a mutant that makes a suite fail to load with a compile
// error ("Failed to load ...: <file>.dart:L:C: Error: ...") is recorded as "nocompile" and excluded from the check.
//
// The toolchain runs in Docker: one long-lived container per repo (docker exec per run) with the checkout bind-mounted at
// /repo and the pub cache in a named volume, so `dart pub get` runs once per container and mutants reuse the cache.
// The image is the manifest's "image" or $BGRAPH_DART_IMAGE.

// dartPubCacheVolume holds the pub cache shared by the Dart T2 containers.
const dartPubCacheVolume = "bgraph-dart-t2-pubcache"

// dartCompileError matches a compile error of the front end in a load failure.
var dartCompileError = regexp.MustCompile(`\.dart:[0-9]+:[0-9]+: Error: `)

// dartTest is one test of a `dart test` run.
type dartTest struct {
	File string // repo-relative suite path (the unit)
	Red  bool
}

// dartRun is one `dart test --reporter json` run (or several packages' runs merged).
type dartRun struct {
	Suites  []string            // repo-relative suite paths, sorted
	Tests   map[string]dartTest // key: suite path NUL test name (NUL #n for the n-th repeat of a name)
	Compile []string            // compile errors reported by load failures
	Done    bool                // the run reached its "done" event
}

// dartSuitePath turns a suite path of `dart test` run in package directory pkg into a repo-relative path.
func dartSuitePath(pkg, p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if rest, ok := strings.CutPrefix(p, "/repo/"); ok {
		return rest
	}
	if pkg == "" || pkg == "." {
		return path.Clean(p)
	}
	return path.Join(pkg, p)
}

// parseDartTestJSON reads the JSON reporter's event stream of a run in package directory pkg. Lines that are not JSON
// events (output of child processes) are skipped.
func parseDartTestJSON(pkg string, data []byte) (dartRun, error) {
	run := dartRun{Tests: map[string]dartTest{}}
	suites := map[int]string{}
	keys := map[int]string{}
	seen := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 1<<28)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Suite *struct {
				ID   int     `json:"id"`
				Path *string `json:"path"`
			} `json:"suite"`
			Test *struct {
				ID      int    `json:"id"`
				Name    string `json:"name"`
				SuiteID int    `json:"suiteID"`
			} `json:"test"`
			TestID  int    `json:"testID"`
			Result  string `json:"result"`
			Error   string `json:"error"`
			Success *bool  `json:"success"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "suite":
			if ev.Suite == nil || ev.Suite.Path == nil {
				return run, fmt.Errorf("suite event without a path: %s", line)
			}
			suites[ev.Suite.ID] = dartSuitePath(pkg, *ev.Suite.Path)
		case "testStart":
			if ev.Test == nil {
				continue
			}
			file, ok := suites[ev.Test.SuiteID]
			if !ok {
				return run, fmt.Errorf("test %d of unknown suite %d", ev.Test.ID, ev.Test.SuiteID)
			}
			key := file + "\x00" + ev.Test.Name
			seen[key]++
			if n := seen[key]; n > 1 {
				key += "\x00#" + strconv.Itoa(n)
			}
			keys[ev.Test.ID] = key
			run.Tests[key] = dartTest{File: file}
		case "testDone":
			key, ok := keys[ev.TestID]
			if !ok {
				return run, fmt.Errorf("testDone of unknown test %d", ev.TestID)
			}
			if ev.Result != "success" {
				t := run.Tests[key]
				t.Red = true
				run.Tests[key] = t
			}
		case "error":
			key, ok := keys[ev.TestID]
			if !ok {
				return run, fmt.Errorf("error of unknown test %d", ev.TestID)
			}
			t := run.Tests[key]
			t.Red = true
			run.Tests[key] = t
			if strings.HasPrefix(ev.Error, "Failed to load") && dartCompileError.MatchString(ev.Error) {
				msg := ev.Error
				if len(msg) > 400 {
					msg = msg[:400]
				}
				run.Compile = append(run.Compile, msg)
			}
		case "done":
			run.Done = true
		}
	}
	if err := sc.Err(); err != nil {
		return run, err
	}
	run.Suites = slices.Sorted(maps.Values(suites))
	run.Suites = slices.Compact(run.Suites)
	return run, nil
}

// merge adds the results of another package's run.
func (d *dartRun) merge(o dartRun) {
	d.Suites = append(d.Suites, o.Suites...)
	slices.Sort(d.Suites)
	d.Suites = slices.Compact(d.Suites)
	maps.Copy(d.Tests, o.Tests)
	d.Compile = append(d.Compile, o.Compile...)
}

// redUnits returns the units with a red test outside excluded, and those with a red test in excluded only.
func (d dartRun) redUnits(excluded map[string]bool) (red, onlyExcluded []string) {
	r, x := map[string]bool{}, map[string]bool{}
	for key, t := range d.Tests {
		if !t.Red {
			continue
		}
		if excluded[key] {
			x[t.File] = true
		} else {
			r[t.File] = true
		}
	}
	for f := range r {
		delete(x, f)
	}
	return slices.Sorted(maps.Keys(r)), slices.Sorted(maps.Keys(x))
}

// dartContainer is the long-lived container of one repo.
type dartContainer struct {
	name  string
	image string
	env   []string
	args  []string
}

func (r T2Repo) dartImage() string {
	if img := os.Getenv("BGRAPH_DART_IMAGE"); img != "" {
		return img
	}
	return r.Image
}

func (r T2Repo) dartPackages() []string {
	if len(r.Packages) == 0 {
		return []string{"."}
	}
	return r.Packages
}

func startDartContainer(ctx context.Context, r T2Repo, root string) (*dartContainer, error) {
	img := r.dartImage()
	if img == "" {
		return nil, fmt.Errorf("%s: no Dart image (manifest \"image\" or $BGRAPH_DART_IMAGE)", r.Name)
	}
	c := &dartContainer{name: "bgraph-dart-t2-" + strings.NewReplacer("/", "-", "_", "-", ".", "-").Replace(r.Dir), image: img, env: r.Env, args: r.Args}
	exec.CommandContext(ctx, "docker", "rm", "-f", c.name).Run()
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", c.name, "-v", root+":/repo", "-v", dartPubCacheVolume+":/root/.pub-cache",
		"-w", "/repo", img, "sleep", "infinity").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run: %v: %s", err, out)
	}
	return c, nil
}

func (c *dartContainer) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exec.CommandContext(ctx, "docker", "rm", "-f", c.name).Run()
}

func (c *dartContainer) execArgs(pkg string, cmd ...string) []string {
	args := []string{"exec", "-w", path.Join("/repo", pkg)}
	for _, e := range c.env {
		args = append(args, "-e", e)
	}
	return append(append(args, c.name), cmd...)
}

// pubGet resolves every package once (a workspace member resolves its workspace).
func (c *dartContainer) pubGet(ctx context.Context, pkgs []string) error {
	for _, p := range pkgs {
		out, tail, to, err := runProc(ctx, "", nil, 15*time.Minute, "docker", c.execArgs(p, "dart", "pub", "get")...)
		if err != nil || to {
			return fmt.Errorf("dart pub get in %s: %v (timeout %v): %s %s", p, err, to, lastBytes(out, 1500), tail)
		}
	}
	return nil
}

func lastBytes(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// runPackages runs the whole suite of each package concurrently and merges the runs. status is "" on success, "timeout"
// when a package's run was killed, "error" when a run did not complete.
func (c *dartContainer) runPackages(ctx context.Context, pkgs []string, timeout time.Duration, logDir, label string) (dartRun, time.Duration, string, error) {
	merged := dartRun{Tests: map[string]dartTest{}, Done: true}
	type res struct {
		run    dartRun
		status string
		err    error
	}
	results := make([]res, len(pkgs))
	start := time.Now()
	var wg sync.WaitGroup
	for i, p := range pkgs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			secs := strconv.Itoa(int(timeout.Seconds()))
			cmd := append([]string{"timeout", "-s", "KILL", secs, "dart", "test", "--reporter", "json"}, c.args...)
			out, tail, to, err := runProc(ctx, "", nil, timeout+time.Minute, "docker", c.execArgs(p, cmd...)...)
			if logDir != "" {
				os.MkdirAll(logDir, 0o755)
				os.WriteFile(filepath.Join(logDir, label+"-"+strings.ReplaceAll(p, "/", "_")+".jsonl"), []byte(out), 0o644)
			}
			if ctx.Err() != nil {
				results[i] = res{status: "error", err: ctx.Err()}
				return
			}
			run, perr := parseDartTestJSON(p, []byte(out))
			switch {
			case perr != nil:
				results[i] = res{status: "error", err: fmt.Errorf("%s: %v", p, perr)}
			case !run.Done && (to || exitCode(err) == 137 || exitCode(err) == 124):
				results[i] = res{status: "timeout", err: fmt.Errorf("%s: timed out after %s", p, timeout)}
			case !run.Done && dartCompileError.MatchString(tail+out):
				// The runner itself did not compile (the package under test is one of package:test's own dependencies).
				results[i] = res{status: "nocompile", err: fmt.Errorf("%s: the test runner did not compile: %s", p, lastBytes(tail, 600))}
			case !run.Done:
				results[i] = res{status: "error", err: fmt.Errorf("%s: run did not complete (%v): %s", p, err, lastBytes(tail, 600))}
			default:
				results[i] = res{run: run}
			}
		}()
	}
	wg.Wait()
	took := time.Since(start)
	for _, r := range results {
		if r.err != nil {
			return merged, took, r.status, r.err
		}
		merged.merge(r.run)
	}
	return merged, took, "", nil
}

func exitCode(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 0
}

// dartBaseline runs every package's suite twice at R.
func (r T2Repo) dartBaseline(ctx context.Context, cfg *T2Config, c *dartContainer) (T2Baseline, error) {
	cache := filepath.Join(cfg.Dir, "baseline-"+r.Dir+".json")
	if !cfg.Rebaseline {
		data, err := os.ReadFile(cache)
		var b T2Baseline
		if err == nil && json.Unmarshal(data, &b) == nil && b.R == r.R && len(b.RunMs) == 2 && b.Files == len(b.Known) && b.Files > 0 &&
			r.dartSuitesMatchPackages(b.Known) {
			return b, nil
		}
	}
	result := T2Baseline{Repo: r.Name, R: r.R}
	logDir := filepath.Join(cfg.Dir, "runs", r.Dir)
	var runs []dartRun
	for i := 1; i <= 2; i++ {
		run, took, _, err := c.runPackages(ctx, r.dartPackages(), 20*time.Minute, logDir, "baseline-"+strconv.Itoa(i))
		if err != nil {
			return result, fmt.Errorf("baseline run %d: %w", i, err)
		}
		if len(run.Compile) > 0 {
			return result, fmt.Errorf("baseline run %d: compile error at R: %s", i, run.Compile[0])
		}
		cfg.logf("%s: baseline %d/2 %d suites, %d tests, %s", r.Name, i, len(run.Suites), len(run.Tests), took.Round(time.Second))
		runs = append(runs, run)
		result.RunMs = append(result.RunMs, took.Milliseconds())
	}
	if !slices.Equal(runs[0].Suites, runs[1].Suites) {
		return result, fmt.Errorf("baseline suites changed between runs: %d vs %d", len(runs[0].Suites), len(runs[1].Suites))
	}
	result.Known = runs[0].Suites
	result.Files = len(result.Known)
	result.Tests = len(runs[0].Tests)
	result.FailingTests = map[string][]string{}
	failing, flaky := map[string]bool{}, map[string]bool{}
	keys := map[string]bool{}
	for k := range runs[0].Tests {
		keys[k] = true
	}
	for k := range runs[1].Tests {
		keys[k] = true
	}
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		a, inA := runs[0].Tests[k]
		b, inB := runs[1].Tests[k]
		file := a.File
		if !inA {
			file = b.File
		}
		redA, redB := inA && a.Red, inB && b.Red
		if redA || redB || inA != inB {
			result.FailingTests[file] = append(result.FailingTests[file], k)
		}
		switch {
		case redA && redB:
			failing[file] = true
		case redA != redB || inA != inB:
			flaky[file] = true
		}
	}
	result.Failing, result.Flaky = slices.Sorted(maps.Keys(failing)), slices.Sorted(maps.Keys(flaky))
	return result, writeSwiftT2JSON(cache, result)
}

// dartSuitesMatchPackages: every suite lies in a listed package and every listed package has a suite (a cached baseline
// of another package list is stale).
func (r T2Repo) dartSuitesMatchPackages(suites []string) bool {
	seen := map[string]bool{}
	for _, s := range suites {
		p := dartPackageOf(s, r.dartPackages())
		if p == "" {
			return false
		}
		seen[p] = true
	}
	return len(seen) == len(r.dartPackages())
}

// dartMutants generates (or reads the cached) mutant list.
func (r T2Repo) dartMutants(ctx context.Context, cfg *T2Config, root string) ([]T2Mutant, error) {
	file := filepath.Join(cfg.Dir, "mutants-"+r.Dir+".json")
	var cached struct {
		R        string     `json:"r"`
		Seed     string     `json:"seed"`
		Tries    int        `json:"tries"`
		Packages []string   `json:"packages"`
		Mutants  []T2Mutant `json:"mutants"`
	}
	if data, err := os.ReadFile(file); err == nil && json.Unmarshal(data, &cached) == nil && cached.R == r.R && cached.Seed == r.Seed &&
		cached.Tries == r.Tries && slices.Equal(cached.Packages, r.dartPackages()) && len(cached.Mutants) > 0 {
		return cached.Mutants, nil
	}
	tracked, err := git(ctx, root, "ls-tree", "-r", "-z", "--name-only", r.R)
	if err != nil {
		return nil, err
	}
	var files []RustMutantSource
	for f := range strings.SplitSeq(tracked, "\x00") {
		if !dartCandidateFile(f, r.dartPackages()) {
			continue
		}
		if len(r.Include) > 0 && !slices.ContainsFunc(r.Include, func(p string) bool { return strings.HasPrefix(f, p) }) {
			continue
		}
		if slices.ContainsFunc(r.Exclude, func(p string) bool { return strings.HasPrefix(f, p) }) {
			continue
		}
		data, oid, err := r.blob(ctx, root, f)
		if err != nil {
			return nil, err
		}
		files = append(files, RustMutantSource{Path: f, Blob: oid, Data: data})
	}
	mutants := GenDartMutants(files, r.dartPackages(), r.Seed, "dart-"+r.Dir, r.Tries)
	if len(mutants) == 0 {
		return nil, fmt.Errorf("no Dart mutation candidates in %s", root)
	}
	return mutants, writeSwiftT2JSON(file, map[string]any{"r": r.R, "seed": r.Seed, "tries": r.Tries, "packages": r.dartPackages(), "mutants": mutants})
}

func (r T2Repo) buildOracleDart(ctx context.Context, cfg *T2Config, store *OracleStore, have map[string]T2Record) error {
	root, err := filepath.Abs(filepath.Join(cfg.Dir, r.Dir))
	if err != nil {
		return err
	}
	if err := r.ensureClean(ctx, root); err != nil {
		return err
	}
	mutants, err := r.dartMutants(ctx, cfg, root)
	if err != nil {
		return err
	}
	cfg.logf("%s: %d Dart mutants", r.Name, len(mutants))
	c, err := startDartContainer(ctx, r, root)
	if err != nil {
		return err
	}
	defer c.stop()
	if err := c.pubGet(ctx, r.dartPackages()); err != nil {
		return err
	}
	baseline, err := r.dartBaseline(ctx, cfg, c)
	if err != nil {
		return err
	}
	cfg.logf("%s: Dart baseline %d suites, %d tests, %d broken, %d flaky", r.Name, baseline.Files, baseline.Tests, len(baseline.Failing), len(baseline.Flaky))
	excluded := map[string]bool{}
	for _, keys := range baseline.FailingTests {
		for _, k := range keys {
			excluded[k] = true
		}
	}
	timeout := max(3*time.Minute, 4*time.Duration(slices.Max(baseline.RunMs))*time.Millisecond)
	logDir := filepath.Join(cfg.Dir, "runs", r.Dir)
	for _, m := range mutants {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !cfg.Deadline.IsZero() && time.Now().After(cfg.Deadline) {
			break
		}
		rec := T2Record{Repo: r.Name, R: r.R, Mutant: m, NativeErr: "dart test has no related-test mode"}
		if old, ok := have[rec.key()]; ok && old.Mutant == m && !cfg.Rebuild && !cfg.Rebaseline && old.Status != "error" {
			continue
		}
		commit, edited, err := r.MutantCommit(ctx, root, filepath.Join(cfg.Dir, "idx"), m)
		if err != nil {
			return err
		}
		rec.Commit = commit
		if err := r.dartMutantVerdict(ctx, c, root, timeout, logDir, baseline, excluded, edited, &rec); err != nil {
			return err
		}
		// A test may write into the checkout (a mutated `melos init` targets the working directory): restore tracked files and
		// remove new untracked ones (ignored files such as .dart_tool/ stay), and record the run as an error.
		if dirty, err := r.dartRestoreTree(ctx, root); err != nil {
			return err
		} else if dirty != "" {
			rec.Status, rec.Red, rec.RedUnconfirmed = "error", nil, nil
			rec.Detail = "the mutant's tests wrote into the checkout (restored): " + dirty
		}
		if err := store.Append(rec); err != nil {
			return err
		}
		cfg.logf("%s: %s %s:%d %s %s (red %v) %s %s", r.Name, m.ID, m.File, m.Line, m.Op, rec.Status, rec.Red, time.Duration(rec.Ms)*time.Millisecond, short(rec.Detail))
	}
	return r.ensureClean(ctx, root)
}

func (r T2Repo) dartMutantVerdict(ctx context.Context, c *dartContainer, root string, timeout time.Duration, logDir string, baseline T2Baseline,
	excluded map[string]bool, edited []byte, rec *T2Record) (err error) {
	file := filepath.Join(root, filepath.FromSlash(rec.Mutant.File))
	original, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	defer func() {
		if restoreErr := os.WriteFile(file, original, 0o644); restoreErr != nil && err == nil {
			err = fmt.Errorf("restore Dart source %s: %w", file, restoreErr)
		}
	}()
	if err := os.WriteFile(file, edited, 0o644); err != nil {
		return err
	}
	start := time.Now()
	defer func() { rec.Ms = time.Since(start).Milliseconds() }()
	run, _, status, runErr := c.runPackages(ctx, r.dartPackages(), timeout, logDir, rec.Mutant.ID)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if runErr != nil {
		rec.Status, rec.Detail = status, runErr.Error()
		return nil
	}
	rec.TestFiles = baseline.Files
	red, onlyExcluded := run.redUnits(excluded)
	rec.RedExcluded = onlyExcluded
	if len(run.Compile) > 0 {
		rec.Status, rec.Red, rec.Detail = "nocompile", red, run.Compile[0]
		return nil
	}
	if missing := dartMissingSuites(baseline.Known, run.Suites); len(missing) > 0 {
		rec.Status, rec.Detail = "error", fmt.Sprintf("suites of the baseline not run: %v", missing)
		return nil
	}
	rec.Status = "survived"
	if len(red) == 0 {
		return nil
	}
	// Confirm: re-run the packages that hold a red unit.
	var pkgs []string
	for _, u := range red {
		p := dartPackageOf(u, r.dartPackages())
		if !slices.Contains(pkgs, p) {
			pkgs = append(pkgs, p)
		}
	}
	again, _, status, runErr := c.runPackages(ctx, pkgs, timeout, logDir, rec.Mutant.ID+"-confirm")
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if runErr != nil {
		rec.Status, rec.Detail = status, "confirmation: "+runErr.Error()
		return nil
	}
	red2, _ := again.redUnits(excluded)
	for _, u := range red {
		if slices.Contains(red2, u) {
			rec.Red = append(rec.Red, u)
		} else {
			rec.RedUnconfirmed = append(rec.RedUnconfirmed, u)
		}
	}
	rec.Confirmed = true
	if len(rec.Red) > 0 {
		rec.Status = "killed"
	}
	return nil
}

// dartRestoreTree checks the checkout after a mutant run (the mutated file is already restored). It returns the
// porcelain status when tests changed it, after restoring tracked files and removing untracked, non-ignored files.
func (r T2Repo) dartRestoreTree(ctx context.Context, root string) (string, error) {
	st, err := git(ctx, root, "status", "--porcelain")
	if err != nil || strings.TrimSpace(st) == "" {
		return "", err
	}
	if _, err := git(ctx, root, "checkout", "-q", "--", "."); err != nil {
		return "", err
	}
	if _, err := git(ctx, root, "clean", "-q", "-f", "-d"); err != nil {
		return "", err
	}
	if after, err := git(ctx, root, "status", "--porcelain"); err != nil || strings.TrimSpace(after) != "" {
		return "", fmt.Errorf("could not restore %s: %v %s", root, err, after)
	}
	return lastBytes(strings.TrimSpace(st), 300), nil
}

// dartMissingSuites returns the baseline suites absent from a run.
func dartMissingSuites(known, got []string) []string {
	var out []string
	for _, s := range known {
		if _, found := slices.BinarySearch(got, s); !found {
			out = append(out, s)
		}
	}
	return out
}

// dartCachedMutants keeps the oracle records of the cached mutant list (in its order).
func (r T2Repo) dartCachedMutants(cfg *T2Config, recs []T2Record) ([]T2Record, error) {
	var list struct {
		R       string     `json:"r"`
		Seed    string     `json:"seed"`
		Mutants []T2Mutant `json:"mutants"`
	}
	file := filepath.Join(cfg.Dir, "mutants-"+r.Dir+".json")
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(data, &list) != nil || list.R != r.R || list.Seed != r.Seed {
		return nil, fmt.Errorf("stale Dart mutant list %s (run with --run-tests)", file)
	}
	byMutant := map[T2Mutant]T2Record{}
	for _, rec := range recs {
		byMutant[rec.Mutant] = rec
	}
	var out []T2Record
	for _, m := range list.Mutants {
		if rec, ok := byMutant[m]; ok {
			out = append(out, rec)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no Dart oracle results match the mutant list %s", file)
	}
	return out, nil
}

// ---- provider cross-check ----

// DartUnitsResult compares, for one package, the suites `dart test` loaded in the baseline run with bgraph's units at R.
type DartUnitsResult struct {
	Repo    string            `json:"repo"`
	R       string            `json:"r"`
	Package string            `json:"package"`
	Suites  int               `json:"suites"`       // suites loaded by `dart test` at R
	Units   int               `json:"bgraph_units"` // bgraph test units of the package
	Missing []string          `json:"missing,omitempty"`
	Extra   map[string]string `json:"extra,omitempty"` // bgraph unit → why `dart test` did not load it ("" = unexplained)
	Whole   []string          `json:"whole,omitempty"`
	Error   string            `json:"error,omitempty"`
}

var dartTestOn = regexp.MustCompile(`(?m)^\s*@TestOn\(\s*(['"][^'"]*['"])\s*\)`)

// DartUnitsCheck runs the provider cross-check of every selected Dart repo from its cached baseline.
func DartUnitsCheck(ctx context.Context, cfg T2Config) []DartUnitsResult {
	var out []DartUnitsResult
	for _, r := range cfg.selected() {
		if r.Eco != "dart" {
			continue
		}
		fail := func(err error) {
			out = append(out, DartUnitsResult{Repo: r.Name, R: r.R, Error: err.Error()})
		}
		root := filepath.Join(cfg.Dir, r.Dir)
		var bl T2Baseline
		data, err := os.ReadFile(filepath.Join(cfg.Dir, "baseline-"+r.Dir+".json"))
		if err == nil {
			err = json.Unmarshal(data, &bl)
		}
		if err != nil || bl.R != r.R {
			fail(fmt.Errorf("no baseline at R (run with --run-tests): %v", err))
			continue
		}
		gt, err := newGitTreeWant(ctx, root, r.R, func(string) bool { return true })
		if err != nil {
			fail(err)
			continue
		}
		a, err := dart.New(nil).Analyze(ctx, gt, scancache.New())
		if err != nil {
			gt.Close()
			fail(err)
			continue
		}
		for _, p := range r.dartPackages() {
			res := DartUnitsResult{Repo: r.Name, R: r.R, Package: p, Extra: map[string]string{}}
			units := map[string]bool{}
			whole := false
			for _, u := range a.Tests {
				if u.Project != p {
					continue
				}
				res.Units++
				if u.Whole {
					whole = true
					res.Whole = append(res.Whole, u.ID)
					continue
				}
				units[u.ID] = true
			}
			suites := map[string]bool{}
			for _, s := range bl.Known {
				if dartPackageOf(s, r.dartPackages()) != p {
					continue
				}
				suites[s] = true
				res.Suites++
				if !units[s] && !whole {
					res.Missing = append(res.Missing, s)
				}
			}
			for u := range units {
				if suites[u] {
					continue
				}
				why := ""
				if src, err := gt.Read(u); err == nil {
					if m := dartTestOn.FindSubmatch(src); m != nil {
						why = "@TestOn(" + string(m[1]) + "): not run on the VM platform"
					}
				}
				res.Extra[u] = why
			}
			out = append(out, res)
		}
		gt.Close()
	}
	return out
}

// WriteDartUnits writes dart-units.json and dart-units.md into dir and returns the markdown.
func WriteDartUnits(dir string, rs []DartUnitsResult) (string, error) {
	var b strings.Builder
	b.WriteString("# Dart provider cross-check: suites loaded by `dart test` at R vs bgraph units\n\n| repo | R | package | dart test suites | bgraph units | missing (bug) | extra (explained) | extra (unexplained) |\n|---|---|---|---|---|---|---|---|\n")
	for _, r := range rs {
		if r.Error != "" {
			fmt.Fprintf(&b, "| %s | %.10s | | error: %s | | | | |\n", r.Repo, r.R, r.Error)
			continue
		}
		expl, unexpl := 0, 0
		for _, why := range r.Extra {
			if why != "" {
				expl++
			} else {
				unexpl++
			}
		}
		fmt.Fprintf(&b, "| %s | %.10s | %s | %d | %d | %d | %d | %d |\n", r.Repo, r.R, r.Package, r.Suites, r.Units, len(r.Missing), expl, unexpl)
	}
	for _, r := range rs {
		if len(r.Missing)+len(r.Extra)+len(r.Whole) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s %s\n\n", r.Repo, r.Package)
		for _, f := range r.Missing {
			fmt.Fprintf(&b, "- MISSING `%s`\n", f)
		}
		for _, f := range slices.Sorted(maps.Keys(r.Extra)) {
			why := r.Extra[f]
			if why == "" {
				why = "UNEXPLAINED"
			}
			fmt.Fprintf(&b, "- extra `%s`: %s\n", f, why)
		}
		for _, f := range r.Whole {
			fmt.Fprintf(&b, "- whole-package unit `%s`\n", f)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	j, _ := json.MarshalIndent(rs, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "dart-units.json"), j, 0o644); err != nil {
		return "", err
	}
	md := b.String()
	return md, os.WriteFile(filepath.Join(dir, "dart-units.md"), []byte(md), 0o644)
}

// DartUnitsOK reports whether the cross-check found no missing unit, no unexplained extra unit and no error.
func DartUnitsOK(rs []DartUnitsResult) bool {
	for _, r := range rs {
		if r.Error != "" || len(r.Missing) > 0 {
			return false
		}
		for _, why := range r.Extra {
			if why == "" {
				return false
			}
		}
	}
	return len(rs) > 0
}

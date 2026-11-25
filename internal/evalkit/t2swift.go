package evalkit

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/eco/swift"
	"github.com/qxbao/bgraph/internal/model"
	swiftlex "github.com/qxbao/bgraph/internal/scan/swift"
)

func (r T2Repo) coversRed(sel model.Selection, red string) bool {
	if r.Eco == "cpp" {
		return cppCoversRed(sel, red)
	}
	if r.Eco != "swift" {
		return selCovers(sel, red)
	}
	dir, _, ok := swift.ParseUnitID(red)
	if !ok {
		return false
	}
	for _, u := range sel.Tests {
		if u.Unit == red || (u.Whole && u.Unit == swift.UnitID(dir, "")) {
			return true
		}
	}
	return false
}

// Swift's parallel XCTest xUnit output qualifies each class with its module.
type swiftTest struct {
	unit string
	red  bool
}

func parseSwiftXunit(data []byte) (map[string]swiftTest, error) {
	var report struct {
		XMLName xml.Name `xml:"testsuites"`
		Suites  []struct {
			Tests int `xml:"tests,attr"`
			Cases []struct {
				Class   string    `xml:"classname,attr"`
				Name    string    `xml:"name,attr"`
				Failure *struct{} `xml:"failure"`
				Error   *struct{} `xml:"error"`
				Skipped *struct{} `xml:"skipped"`
			} `xml:"testcase"`
		} `xml:"testsuite"`
	}
	if err := xml.Unmarshal(data, &report); err != nil {
		return nil, err
	}
	tests := map[string]swiftTest{}
	for _, suite := range report.Suites {
		if suite.Tests != len(suite.Cases) {
			return nil, fmt.Errorf("incomplete Swift XCTest suite (%d of %d tests)", len(suite.Cases), suite.Tests)
		}
		for _, c := range suite.Cases {
			module, class, ok := strings.Cut(c.Class, ".")
			if !ok || module == "" || class == "" || c.Name == "" || c.Skipped != nil {
				return nil, fmt.Errorf("missing XCTest module or unexecuted test %q/%q", c.Class, c.Name)
			}
			id := c.Class + "/" + c.Name
			if _, duplicate := tests[id]; duplicate {
				return nil, fmt.Errorf("duplicate XCTest result for %q", id)
			}
			tests[id] = swiftTest{unit: swift.UnitID(".", module), red: c.Failure != nil || c.Error != nil}
		}
	}
	if len(tests) == 0 {
		return nil, fmt.Errorf("empty Swift XCTest report")
	}
	return tests, nil
}

func swiftCensus(tests map[string]swiftTest) []string {
	return slices.Sorted(maps.Keys(tests))
}

func swiftRed(tests map[string]swiftTest, excluded map[string]bool) []string {
	units := map[string]bool{}
	for name, test := range tests {
		if test.red && !excluded[name] {
			units[test.unit] = true
		}
	}
	return slices.Sorted(maps.Keys(units))
}

var swiftCompileError = regexp.MustCompile(`(?m)^.*\.swift:[0-9]+:[0-9]+: error: |^error: (compile command|emit-module command|link command) failed`)

func (r T2Repo) swiftSuite(ctx context.Context, root, scratch, label string, timeout time.Duration) (map[string]swiftTest, time.Duration, string, error) {
	runDir, err := os.MkdirTemp(scratch, label+"-")
	if err != nil {
		return nil, 0, "error", err
	}
	logPath := filepath.Join(runDir, "swift.log")
	log, err := os.Create(logPath)
	if err != nil {
		return nil, 0, "error", err
	}
	defer log.Close()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	scratchPath, reportPath := filepath.Join(scratch, "build"), filepath.Join(runDir, "xunit.xml")
	suiteArgs := []string{"test", "--scratch-path", scratchPath, "--parallel", "--disable-swift-testing", "--xunit-output", reportPath}
	name, args := "swift", suiteArgs
	if image := os.Getenv("BGRAPH_SWIFT_IMAGE"); image != "" {
		container := "bgraph-swift-t2-" + filepath.Base(scratch) + "-" + filepath.Base(runDir)
		defer func() {
			if ctx.Err() != nil {
				cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
				defer done()
				exec.CommandContext(cleanup, "docker", "rm", "-f", container).Run()
			}
		}()
		suiteArgs[2] = "/scratch/build"
		suiteArgs[6] = "/scratch/" + filepath.Base(runDir) + "/xunit.xml"
		args = []string{"run", "--rm", "--init", "--name", container, "-v", root + ":/repo", "-v", scratch + ":/scratch", "-w", "/repo"}
		for _, env := range r.Env {
			args = append(args, "-e", env)
		}
		name, args = "docker", append(append(args, image, "swift"), suiteArgs...)
	}
	cmd := exec.CommandContext(ctx, name, append(args, r.Args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), r.Env...)
	cmd.Stdout, cmd.Stderr = log, log
	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)
	if ctx.Err() != nil {
		return nil, elapsed, "timeout", fmt.Errorf("%w (%s)", ctx.Err(), logPath)
	}
	data, readErr := os.ReadFile(reportPath)
	if readErr == nil {
		tests, parseErr := parseSwiftXunit(data)
		if parseErr != nil {
			return nil, elapsed, "error", fmt.Errorf("%w (%s)", parseErr, reportPath)
		}
		hasFailures := len(swiftRed(tests, nil)) > 0
		if hasFailures != (runErr != nil) {
			return nil, elapsed, "error", fmt.Errorf("exit status %v from Swift disagrees with XCTest report (%s)", runErr, logPath)
		}
		return tests, elapsed, "", nil
	}
	if runErr != nil {
		output, _ := os.ReadFile(logPath)
		if swiftCompileError.Match(output) {
			return nil, elapsed, "nocompile", fmt.Errorf("compiling Swift failed (%s)", logPath)
		}
	}
	return nil, elapsed, "error", fmt.Errorf("missing Swift XCTest report (%v, %v; %s)", runErr, readErr, logPath)
}

func (r T2Repo) swiftBaseline(ctx context.Context, cfg *T2Config, root, scratch string) (T2Baseline, error) {
	cache := filepath.Join(cfg.Dir, "baseline-"+r.Dir+".json")
	if !cfg.Rebaseline {
		data, err := os.ReadFile(cache)
		var b T2Baseline
		if err == nil && json.Unmarshal(data, &b) == nil && b.R == r.R && len(b.RunMs) == 2 && b.Tests == len(b.Known) && b.Tests > 0 {
			return b, nil
		}
	}
	result := T2Baseline{Repo: r.Name, R: r.R}
	first, took, _, err := r.swiftSuite(ctx, root, scratch, "baseline-1", 12*time.Minute)
	if err != nil {
		return result, err
	}
	result.RunMs = append(result.RunMs, took.Milliseconds())
	second, took, _, err := r.swiftSuite(ctx, root, scratch, "baseline-2", 12*time.Minute)
	if err != nil {
		return result, err
	}
	result.RunMs = append(result.RunMs, took.Milliseconds())
	result.Known = swiftCensus(first)
	if !slices.Equal(result.Known, swiftCensus(second)) {
		return result, fmt.Errorf("baseline XCTest census changed between Swift runs")
	}
	result.Tests = len(first)
	result.FailingTests = map[string][]string{}
	units, failing, flaky := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, id := range result.Known {
		a, b := first[id], second[id]
		units[a.unit] = true
		if a.red || b.red {
			result.FailingTests[a.unit] = append(result.FailingTests[a.unit], id)
		}
		if a.red && b.red {
			failing[a.unit] = true
		}
		if a.red != b.red {
			flaky[a.unit] = true
		}
	}
	result.Files = len(units)
	result.Failing, result.Flaky = slices.Sorted(maps.Keys(failing)), slices.Sorted(maps.Keys(flaky))
	return result, writeSwiftT2JSON(cache, result)
}

func writeSwiftT2JSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (r T2Repo) swiftMutants(ctx context.Context, root string) ([]T2Mutant, error) {
	tracked, err := git(ctx, root, "ls-tree", "-r", "-z", "--name-only", r.R)
	if err != nil {
		return nil, err
	}
	var files []RustMutantSource
	for f := range strings.SplitSeq(tracked, "\x00") {
		if !strings.HasPrefix(f, "Sources/") || !strings.HasSuffix(f, ".swift") {
			continue
		}
		if len(r.Include) > 0 && !slices.ContainsFunc(r.Include, func(prefix string) bool { return strings.HasPrefix(f, prefix) }) {
			continue
		}
		if slices.ContainsFunc(r.Exclude, func(prefix string) bool { return strings.HasPrefix(f, prefix) }) {
			continue
		}
		data, oid, err := r.blob(ctx, root, f)
		if err != nil {
			return nil, err
		}
		files = append(files, RustMutantSource{Path: f, Blob: oid, Data: data})
	}

	// The position-bearing C-family scanner proposes edits. Require an exact
	// one-token change under the Swift lexer to reject edits inside literals,
	// comments, and longer Swift-specific operators.
	candidates := map[string][]rustCand{}
	flips := map[string]string{"==": "!=", "!=": "==", "<=": ">", ">=": "<", "<": ">=", ">": "<=", "&&": "||", "||": "&&"}
	for _, f := range files {
		original, err := swiftlex.Lex(f.Data)
		if err != nil {
			continue
		}
		for _, cand := range cfScan(f.Data, false) {
			op := string(f.Data[cand.Start:cand.End])
			if flips[op] != cand.Repl {
				continue
			}
			edit, err := ApplyEdit(f.Data, T2Mutant{Start: cand.Start, End: cand.End, Repl: cand.Repl})
			if err != nil {
				return nil, err
			}
			mutated, err := swiftlex.Lex(edit)
			if err == nil && swiftOperatorFlip(original, mutated, op, cand.Repl) {
				candidates[f.Path] = append(candidates[f.Path], cand)
			}
		}
	}
	limit := r.Tries
	if limit == 0 {
		limit = r.Mutants * 2
	}
	return spreadMutants(files, candidates, nil, r.Seed, "swift-"+filepath.Base(root)+"-"+r.Seed, limit), nil
}

func swiftOperatorFlip(original, mutated []swiftlex.Token, before, after string) bool {
	if len(original) != len(mutated) {
		return false
	}
	diff := false
	for i, token := range original {
		if mutated[i] == token {
			continue
		}
		if diff || token != (swiftlex.Token{Kind: swiftlex.Punctuation, Text: before}) ||
			mutated[i] != (swiftlex.Token{Kind: swiftlex.Punctuation, Text: after}) {
			return false
		}
		diff = true
	}
	return diff
}

func (r T2Repo) buildOracleSwift(ctx context.Context, cfg *T2Config, store *OracleStore, have map[string]T2Record) error {
	root, err := filepath.Abs(filepath.Join(cfg.Dir, r.Dir))
	if err != nil {
		return err
	}
	head, err := git(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	status, err := git(ctx, root, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != r.R || status != "" {
		return fmt.Errorf("expected a clean tracked tree at %s in %s for Swift T2", r.R, root)
	}
	scratch, err := filepath.Abs(filepath.Join(cfg.Dir, "swift-runs", filepath.Base(r.Dir)))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return err
	}
	baseline, err := r.swiftBaseline(ctx, cfg, root, scratch)
	if err != nil {
		return err
	}
	cfg.logf("%s: Swift baseline %d tests, %d modules", r.Name, baseline.Tests, baseline.Files)
	excluded := map[string]bool{}
	for _, names := range baseline.FailingTests {
		for _, name := range names {
			excluded[name] = true
		}
	}
	mutants, err := r.swiftMutants(ctx, root)
	if err != nil {
		return err
	}
	if len(mutants) == 0 {
		return fmt.Errorf("no Swift mutation candidates in %s", root)
	}
	if err := writeSwiftT2JSON(filepath.Join(cfg.Dir, "mutants-"+r.Dir+".json"), map[string]any{"r": r.R, "seed": r.Seed, "mutants": mutants}); err != nil {
		return err
	}
	timeout := max(3*time.Minute, 4*time.Duration(baseline.RunMs[1])*time.Millisecond)
	for _, mutant := range mutants {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !cfg.Deadline.IsZero() && time.Now().After(cfg.Deadline) {
			break
		}
		rec := T2Record{Repo: r.Name, R: r.R, Mutant: mutant, NativeErr: "SwiftPM has no related-test command"}
		if old, ok := have[rec.key()]; ok && old.Mutant == mutant && !cfg.Rebuild && !cfg.Rebaseline && old.Status != "error" {
			continue
		}
		commit, edited, err := r.MutantCommit(ctx, root, filepath.Join(cfg.Dir, "idx"), mutant)
		if err != nil {
			return err
		}
		rec.Commit = commit
		if err := r.swiftMutantVerdict(ctx, root, scratch, timeout, baseline, excluded, edited, &rec); err != nil {
			return err
		}
		if err := store.Append(rec); err != nil {
			return err
		}
		cfg.logf("%s: %s %s %s (%v) %s", r.Name, mutant.ID, mutant.File, rec.Status, rec.Red, rec.Detail)
	}
	return nil
}

func (r T2Repo) swiftMutantVerdict(ctx context.Context, root, scratch string, timeout time.Duration, baseline T2Baseline, excluded map[string]bool, edited []byte, rec *T2Record) (err error) {
	file := filepath.Join(root, filepath.FromSlash(rec.Mutant.File))
	original, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	defer func() {
		if restoreErr := os.WriteFile(file, original, 0o644); restoreErr != nil {
			err = fmt.Errorf("restore Swift source %s: %w", file, restoreErr)
		}
	}()
	if err := os.WriteFile(file, edited, 0o644); err != nil {
		return err
	}
	tests, took, status, err := r.swiftSuite(ctx, root, scratch, rec.Mutant.ID, timeout)
	rec.Ms = took.Milliseconds()
	if err != nil {
		rec.Status, rec.Detail = status, err.Error()
		return nil
	}
	if !slices.Equal(baseline.Known, swiftCensus(tests)) {
		rec.Status, rec.Detail = "error", "mutant XCTest census differs from baseline"
		return nil
	}
	rec.TestFiles = baseline.Files
	red := swiftRed(tests, excluded)
	rec.Status = "survived"
	for _, unit := range swiftRed(tests, nil) {
		if !slices.Contains(red, unit) {
			rec.RedExcluded = append(rec.RedExcluded, unit)
		}
	}
	if len(red) == 0 {
		return nil
	}
	repeat, _, status, runErr := r.swiftSuite(ctx, root, scratch, rec.Mutant.ID+"-confirm", timeout)
	if runErr != nil {
		rec.Status, rec.Detail = status, runErr.Error()
		return nil
	}
	if !slices.Equal(baseline.Known, swiftCensus(repeat)) {
		rec.Status, rec.Detail = "error", "confirmation XCTest census differs from baseline"
		return nil
	}
	again := swiftRed(repeat, excluded)
	for _, unit := range red {
		if slices.Contains(again, unit) {
			rec.Red = append(rec.Red, unit)
		} else {
			rec.RedUnconfirmed = append(rec.RedUnconfirmed, unit)
		}
	}
	if len(rec.Red) > 0 {
		rec.Status, rec.Confirmed = "killed", true
	}
	return nil
}

func (r T2Repo) swiftCachedMutants(cfg *T2Config, recs []T2Record) ([]T2Record, error) {
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
		return nil, fmt.Errorf("stale Swift mutant list %s (run with --run-tests)", file)
	}
	records := map[T2Mutant]T2Record{}
	for _, rec := range recs {
		records[rec.Mutant] = rec
	}
	var out []T2Record
	for _, mutant := range list.Mutants {
		if rec, exists := records[mutant]; exists {
			out = append(out, rec)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no Swift oracle results match the mutant list %s", file)
	}
	return out, nil
}

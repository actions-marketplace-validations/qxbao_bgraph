package evalkit

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/eco/cpp"
	"github.com/qxbao/bgraph/internal/model"
)

const cppT2Image = "bgraph-cpp-eval:cmake3.31-meson1.8"

func cppCoversRed(sel model.Selection, red string) bool {
	system, dir, name, ok := cpp.ParseUnitID(red)
	if !ok || name == "" {
		return false
	}
	for _, test := range sel.Tests {
		if test.Unit == red || (test.Whole && test.Unit == cpp.UnitID(system, dir, "")) {
			return true
		}
	}
	return false
}

func (r T2Repo) cppMutants(ctx context.Context, root string) ([]T2Mutant, error) {
	files, err := git(ctx, root, "ls-tree", "-r", "-z", "--name-only", r.R)
	if err != nil {
		return nil, err
	}
	if len(r.Include) == 0 {
		return nil, fmt.Errorf("%s: specify non-test source prefixes in include", r.Name)
	}
	var sources []RustMutantSource
	candidates := map[string][]rustCand{}
	for file := range strings.SplitSeq(files, "\x00") {
		switch path.Ext(file) {
		case ".c", ".cc", ".cpp", ".cxx":
		default:
			continue
		}
		if !slices.ContainsFunc(r.Include, func(prefix string) bool { return strings.HasPrefix(file, prefix) }) ||
			slices.ContainsFunc(r.Exclude, func(prefix string) bool { return strings.HasPrefix(file, prefix) }) {
			continue
		}
		data, oid, err := r.blob(ctx, root, file)
		if err != nil {
			return nil, err
		}
		sources = append(sources, RustMutantSource{Path: file, Blob: oid, Data: data})
		candidates[file] = cfScan(data, false)
	}
	return spreadMutants(sources, candidates, nil, r.Seed, filepath.Base(root), r.rustTries()), nil
}

type cppNative struct {
	root, cache, reports string
}

func (n cppNative) run(ctx context.Context, label, tool string, args ...string) (bool, error) {
	container := "bgraph-cpp-t2-" + filepath.Base(n.reports) + "-" + label
	dockerArgs := []string{"run", "--rm", "--init", "--network", "none", "--name", container,
		"--volume", n.root + ":/src", "--volume", n.cache + ":/out", "--workdir", "/src", cppT2Image, tool}
	out, tail, timeout, err := runProc(ctx, n.root, nil, 5*time.Minute, "docker", append(dockerArgs, args...)...)
	if timeout || ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanup, "docker", "rm", "-f", container).Run()
	}
	if logErr := os.WriteFile(filepath.Join(n.reports, label+".log"), []byte(out+"\n"+tail), 0o644); logErr != nil {
		return false, logErr
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return timeout, err
}

func (n cppNative) suite(ctx context.Context, label string) (map[string]string, time.Duration, bool, error) {
	file := label + ".xml"
	t0 := time.Now()
	timeout, runErr := n.run(ctx, label, "ctest", "--test-dir", "/out/build", "--parallel", "4",
		"--timeout", "90", "--output-on-failure", "--output-junit", "/out/"+filepath.Base(n.reports)+"/"+file)
	elapsed := time.Since(t0)
	if timeout || ctx.Err() != nil {
		return nil, elapsed, timeout, runErr
	}
	data, err := os.ReadFile(filepath.Join(n.reports, file))
	if err != nil {
		return nil, elapsed, false, fmt.Errorf("missing CTest JUnit report (%v): %w", runErr, err)
	}
	tests, err := parseCppCTest(data)
	if err != nil {
		return nil, elapsed, false, err
	}
	outcomes := slices.Collect(maps.Values(tests))
	if slices.Contains(outcomes, "timeout") {
		return nil, elapsed, true, fmt.Errorf("CTest test timed out (see %s)", filepath.Join(n.reports, file))
	}
	if runErr != nil && !slices.Contains(outcomes, "failed") {
		return nil, elapsed, false, fmt.Errorf("CTest failed without a red unit: %w", runErr)
	}
	return tests, elapsed, false, nil
}

func parseCppCTest(data []byte) (map[string]string, error) {
	var report struct {
		Cases []struct {
			Name string `xml:"name,attr"`
			Stat string `xml:"status,attr"`
			Fail *struct {
				Message string `xml:"message,attr"`
			} `xml:"failure"`
			Err  *struct{} `xml:"error"`
			Skip *struct{} `xml:"skipped"`
		} `xml:"testcase"`
	}
	if err := xml.Unmarshal(data, &report); err != nil {
		return nil, err
	}
	units := map[string]string{}
	for _, tc := range report.Cases {
		id := cpp.UnitID(cpp.CMake, ".", tc.Name)
		if tc.Name == "" || units[id] != "" {
			return nil, fmt.Errorf("empty or duplicate CTest unit %q", tc.Name)
		}
		switch {
		case tc.Fail != nil && strings.EqualFold(tc.Fail.Message, "Timeout"):
			units[id] = "timeout"
		case tc.Fail != nil || tc.Err != nil || tc.Stat == "fail":
			units[id] = "failed"
		case tc.Skip != nil || tc.Stat == "notrun" || tc.Stat == "disabled":
			units[id] = "skipped"
		case tc.Stat == "run":
			units[id] = "passed"
		default:
			return nil, fmt.Errorf("unknown CTest status %q for %s", tc.Stat, id)
		}
	}
	if len(units) == 0 {
		return nil, fmt.Errorf("empty CTest census")
	}
	return units, nil
}

func (r T2Repo) cppBaseline(ctx context.Context, cfg *T2Config, native cppNative) (T2Baseline, error) {
	baseline := T2Baseline{Repo: r.Name, R: r.R}
	file := filepath.Join(cfg.Dir, "baseline-"+r.Dir+".json")
	if !cfg.Rebaseline {
		if data, err := os.ReadFile(file); err == nil {
			if json.Unmarshal(data, &baseline) == nil && baseline.R == r.R && len(baseline.Known) >= 5 {
				return baseline, nil
			}
			baseline = T2Baseline{Repo: r.Name, R: r.R}
		}
	}

	var outcomes [2]map[string]string
	for i := range outcomes {
		run, elapsed, timeout, err := native.suite(ctx, fmt.Sprintf("baseline-%d", i+1))
		if err != nil || timeout {
			return baseline, fmt.Errorf("baseline %d failed (timeout=%v): %v", i+1, timeout, err)
		}
		outcomes[i] = run
		baseline.RunMs = append(baseline.RunMs, elapsed.Milliseconds())
	}
	all := maps.Clone(outcomes[0])
	maps.Copy(all, outcomes[1])
	baseline.Known = slices.Sorted(maps.Keys(all))
	baseline.Files = len(all)
	for _, id := range baseline.Known {
		first, second := outcomes[0][id], outcomes[1][id]
		switch {
		case first != second || first == "skipped":
			baseline.Flaky = append(baseline.Flaky, id)
		case first == "failed":
			baseline.Failing = append(baseline.Failing, id)
		}
	}
	if baseline.Files-len(baseline.Flaky)-len(baseline.Failing) < 5 {
		return baseline, fmt.Errorf("%s: fewer than five consistently passing CTest units", r.Name)
	}
	return baseline, writeCppJSON(file, baseline)
}

func writeCppJSON(file string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, append(data, '\n'), 0o644)
}

func (r T2Repo) buildOracleCPP(ctx context.Context, cfg *T2Config, store *OracleStore, have map[string]T2Record) error {
	if r.Runner != "ctest" {
		return fmt.Errorf("unsupported C++ T2 runner %q", r.Runner)
	}
	workDir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return err
	}
	root := filepath.Join(workDir, r.Dir)
	if err := r.ensureClean(ctx, root); err != nil {
		return err
	}
	cache := filepath.Join(workDir, "native", r.Dir)
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	reports, err := os.MkdirTemp(cache, "run-")
	if err != nil {
		return err
	}
	native := cppNative{root: root, cache: cache, reports: reports}
	configure := append([]string{"-S", "/src", "-B", "/out/build", "-G", "Ninja"}, r.Args...)
	if to, err := native.run(ctx, "configure", "cmake", configure...); to || err != nil {
		return fmt.Errorf("%s configure failed (timeout=%v): %v; see %s", r.Name, to, err, reports)
	}
	if to, err := native.build(ctx, "build-R"); to || err != nil {
		return fmt.Errorf("%s baseline build failed (timeout=%v): %v; see %s", r.Name, to, err, reports)
	}
	baseline, err := r.cppBaseline(ctx, cfg, native)
	if err != nil {
		return fmt.Errorf("%w; see %s", err, reports)
	}
	cfg.logf("%s: baseline %d units, %d failing, %d flaky/skipped", r.Name, baseline.Files, len(baseline.Failing), len(baseline.Flaky))
	mutants, err := r.cppMutants(ctx, root)
	if err != nil {
		return err
	}
	if len(mutants) == 0 {
		return fmt.Errorf("%s: no non-test C/C++ mutation candidates", r.Name)
	}
	file := filepath.Join(cfg.Dir, "mutants-"+r.Dir+".json")
	if err := writeCppJSON(file, map[string]any{"r": r.R, "seed": r.Seed, "mutants": mutants}); err != nil {
		return err
	}
	for _, mutant := range mutants {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !cfg.Deadline.IsZero() && time.Now().After(cfg.Deadline) {
			break
		}
		rec := T2Record{Repo: r.Name, R: r.R, Mutant: mutant}
		if old, ok := have[rec.key()]; ok && old.Mutant == mutant && !cfg.Rebuild && !cfg.Rebaseline && old.Status != "error" {
			continue
		}
		t0 := time.Now()
		commit, edited, err := r.MutantCommit(ctx, root, filepath.Join(cfg.Dir, "idx"), mutant)
		if err != nil {
			return err
		}
		rec.Commit = commit
		if err := native.mutant(ctx, edited, baseline, &rec); err != nil {
			return err
		}
		rec.Ms = time.Since(t0).Milliseconds()
		if err := store.Append(rec); err != nil {
			return err
		}
		cfg.logf("%s: %s %s:%d %s (%v) %s", r.Name, mutant.ID, mutant.File, mutant.Line, rec.Status, rec.Red, rec.Detail)
	}
	return r.ensureClean(ctx, root)
}

func (n cppNative) build(ctx context.Context, label string) (bool, error) {
	return n.run(ctx, label, "cmake", "--build", "/out/build", "--parallel", "4")
}

func (n cppNative) mutant(ctx context.Context, edited []byte, baseline T2Baseline, rec *T2Record) (err error) {
	file := filepath.Join(n.root, filepath.FromSlash(rec.Mutant.File))
	original, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	defer func() {
		if restore := os.WriteFile(file, original, info.Mode().Perm()); restore != nil {
			err = fmt.Errorf("restore %s: %w", file, restore)
		}
	}()
	if err := os.WriteFile(file, edited, info.Mode().Perm()); err != nil {
		return err
	}
	if timeout, buildErr := n.build(ctx, rec.Mutant.ID+"-build"); timeout || buildErr != nil {
		rec.Status, rec.Detail = "nocompile", fmt.Sprint(buildErr)
		if timeout {
			rec.Status, rec.Detail = "timeout", "native build timed out"
		}
		if status, ok := buildErr.(*exec.ExitError); !timeout && (!ok || status.ExitCode() >= 125) {
			rec.Status = "error"
		}
		return ctx.Err()
	}

	excluded := baseline.excluded()
	first, _, timeout, runErr := n.suite(ctx, rec.Mutant.ID+"-suite")
	if timeout || runErr != nil {
		rec.Status, rec.Detail = "error", fmt.Sprint(runErr)
		if timeout {
			rec.Status, rec.Detail = "timeout", "CTest suite timed out"
		}
		return ctx.Err()
	}
	if !slices.Equal(slices.Sorted(maps.Keys(first)), baseline.Known) {
		rec.Status, rec.Detail = "error", "CTest census changed"
		return nil
	}
	rec.TestFiles, rec.Status = len(first), "survived"
	var red []string
	for _, id := range baseline.Known {
		if first[id] == "failed" {
			if excluded[id] {
				rec.RedExcluded = append(rec.RedExcluded, id)
			} else {
				red = append(red, id)
			}
		}
	}
	if len(red) == 0 {
		return nil
	}
	second, _, timeout, runErr := n.suite(ctx, rec.Mutant.ID+"-confirm")
	if timeout || runErr != nil {
		rec.Status, rec.Detail = "error", fmt.Sprint(runErr)
		if timeout {
			rec.Status, rec.Detail = "timeout", "CTest confirmation timed out"
		}
		return ctx.Err()
	}
	if !slices.Equal(slices.Sorted(maps.Keys(second)), baseline.Known) {
		rec.Status, rec.Detail = "error", "CTest census changed on confirmation"
		return nil
	}
	rec.Confirmed = true
	for _, id := range red {
		if second[id] == "failed" {
			rec.Red = append(rec.Red, id)
		} else {
			rec.RedUnconfirmed = append(rec.RedUnconfirmed, id)
		}
	}
	if len(rec.Red) > 0 {
		rec.Status = "killed"
	}
	return nil
}

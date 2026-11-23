package evalkit

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/eco/php"
	"github.com/qxbao/bgraph/internal/glob"
)

const phpTimeout = 2 * time.Minute

type phpJUnitSuite struct {
	Cases []struct {
		Failure *struct{} `xml:"failure"`
		Error   *struct{} `xml:"error"`
	} `xml:"testcase"`
	Suites []phpJUnitSuite `xml:"testsuite"`
}

func phpJUnitVerdict(r io.Reader) (int, bool, error) {
	var report struct {
		XMLName xml.Name
		phpJUnitSuite
	}
	if err := xml.NewDecoder(r).Decode(&report); err != nil {
		return 0, false, err
	}
	if report.XMLName.Local != "testsuite" && report.XMLName.Local != "testsuites" {
		return 0, false, fmt.Errorf("unexpected JUnit root %q", report.XMLName.Local)
	}
	tests, red := report.phpJUnitSuite.verdict()
	return tests, red, nil
}

func (s phpJUnitSuite) verdict() (int, bool) {
	tests, red := len(s.Cases), false
	for _, c := range s.Cases {
		red = red || c.Failure != nil || c.Error != nil
	}
	for _, suite := range s.Suites {
		n, failed := suite.verdict()
		tests += n
		red = red || failed
	}
	return tests, red
}

func (r T2Repo) phpSuite(ctx context.Context, root, suite, report string) (int, bool, bool, error) {
	if err := os.Remove(report); err != nil && !os.IsNotExist(err) {
		return 0, false, false, err
	}
	args := append([]string{r.Bin, "--do-not-cache-result", "--log-junit", report}, r.Args...)
	out, tail, timeout, runErr := runProc(ctx, filepath.Join(root, filepath.FromSlash(suite)), r.Env, phpTimeout, "php", args...)
	if timeout {
		return 0, false, true, nil
	}
	f, err := os.Open(report)
	if err != nil {
		return 0, false, false, fmt.Errorf("no JUnit report for %s (%v): %.500s %.500s", suite, runErr, out, tail)
	}
	defer f.Close()
	tests, red, err := phpJUnitVerdict(f)
	if err != nil {
		return 0, false, false, err
	}
	if tests == 0 || (runErr != nil && !red) {
		return 0, false, false, fmt.Errorf("unusable suite result for %s (%v): %.500s %.500s", suite, runErr, out, tail)
	}
	return tests, red, false, nil
}

// Invoke the runtime tokenizer once for all candidate source files. Offsets are bytes, like Git blobs.
const phpMutationTokens = `
$flips = ['===' => '!==', '!==' => '===', '==' => '!=', '!=' => '==',
          '>=' => '<', '<=' => '>', '>' => '<=', '<' => '>=', '&&' => '||', '||' => '&&'];
foreach (array_slice($argv, 1) as $file) {
    $offset = 0;
    $line = 1;
    foreach (token_get_all(file_get_contents($file)) as $token) {
        $text = is_array($token) ? $token[1] : $token;
        if (isset($flips[$text]) && (!is_array($token) || in_array($token[0], [T_IS_IDENTICAL,
            T_IS_NOT_IDENTICAL, T_IS_EQUAL, T_IS_NOT_EQUAL, T_IS_GREATER_OR_EQUAL, T_IS_SMALLER_OR_EQUAL,
            T_BOOLEAN_AND, T_BOOLEAN_OR], true))) {
            echo json_encode(['file' => $file, 'line' => $line, 'start' => $offset,
                'end' => $offset + strlen($text), 'op' => $text, 'repl' => $flips[$text]], JSON_THROW_ON_ERROR), "\n";
        }
        $offset += strlen($text);
        $line += substr_count($text, "\n");
    }
}`

func (r T2Repo) phpMutants(ctx context.Context, root string) ([]T2Mutant, error) {
	files, err := git(ctx, root, "ls-tree", "-r", "--name-only", "-z", r.R)
	if err != nil {
		return nil, err
	}
	args := []string{"-r", phpMutationTokens, "--"}
	for _, f := range strings.Split(files, "\x00") {
		matches := func(pattern string) bool { return glob.Match(pattern, f) }
		if strings.HasSuffix(f, ".php") && slices.ContainsFunc(r.Include, matches) && !slices.ContainsFunc(r.Exclude, matches) {
			args = append(args, f)
		}
	}
	if len(args) == 3 || len(r.Include) == 0 {
		return nil, fmt.Errorf("%s: no mutable PHP source files (set include in the manifest)", r.Name)
	}
	out, tail, _, err := runProc(ctx, root, r.Env, phpTimeout, "php", args...)
	if err != nil {
		return nil, fmt.Errorf("PHP tokenizer: %w: %s", err, tail)
	}
	var candidates []T2Mutant
	dec := json.NewDecoder(strings.NewReader(out))
	for dec.More() {
		var m T2Mutant
		if err := dec.Decode(&m); err != nil {
			return nil, err
		}
		candidates = append(candidates, m)
	}
	seed := int64(0)
	for _, char := range r.Seed {
		seed = 31*seed + int64(char)
	}
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	var mutants []T2Mutant
	seen := map[string]bool{}
	for _, m := range candidates {
		key := fmt.Sprintf("%s:%d", m.File, m.Line)
		if seen[key] {
			continue
		}
		seen[key] = true
		blob, oid, err := r.blob(ctx, root, m.File)
		if err != nil {
			return nil, err
		}
		if m.Start < 0 || m.End < m.Start || m.End > len(blob) || string(blob[m.Start:m.End]) != m.Op {
			return nil, fmt.Errorf("PHP tokenizer offsets do not match Git blob for %s:%d", m.File, m.Line)
		}
		m.ID, m.Blob = fmt.Sprintf("%s-%03d", r.Dir, len(mutants)+1), oid
		lineStart := strings.LastIndexByte(string(blob[:m.Start]), '\n') + 1
		lineTail, _, _ := strings.Cut(string(blob[m.End:]), "\n")
		lineHead := string(blob[lineStart:m.Start])
		m.Before = strings.TrimSpace(lineHead + m.Op + lineTail)
		m.After = strings.TrimSpace(lineHead + m.Repl + lineTail)
		mutants = append(mutants, m)
		if len(mutants) == r.rustTries() {
			break
		}
	}
	return mutants, nil
}

func (r T2Repo) buildOraclePHP(ctx context.Context, cfg *T2Config, store *OracleStore, have map[string]T2Record) error {
	root := filepath.Join(cfg.Dir, r.Dir)
	if err := r.ensureClean(ctx, root); err != nil {
		return err
	}
	if r.Bin == "" || r.Mutants <= 0 {
		return fmt.Errorf("%s: bin and a positive mutants count are required", r.Name)
	}
	suites := r.Targets
	if len(suites) == 0 {
		suites = []string{"."}
	}
	report, err := filepath.Abs(filepath.Join(cfg.Dir, "php-junit-"+r.Dir+".xml"))
	if err != nil {
		return err
	}
	baseline := T2Baseline{Repo: r.Name, R: r.R, Files: len(suites)}
	for run := 0; run < 2; run++ {
		start := time.Now()
		tests := 0
		for _, suite := range suites {
			n, red, timeout, err := r.phpSuite(ctx, root, suite, report)
			if err != nil || red || timeout {
				return fmt.Errorf("%s: unreliable baseline in %s (red=%t, timeout=%t): %v", r.Name, suite, red, timeout, err)
			}
			tests += n
		}
		if run > 0 && tests != baseline.Tests {
			return fmt.Errorf("%s: baseline test count changed from %d to %d", r.Name, baseline.Tests, tests)
		}
		baseline.Tests = tests
		baseline.RunMs = append(baseline.RunMs, time.Since(start).Milliseconds())
	}
	b, err := json.Marshal(baseline)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cfg.Dir, "baseline-"+r.Dir+".json"), b, 0o644); err != nil {
		return err
	}
	mutants, err := r.phpMutants(ctx, root)
	if err != nil {
		return err
	}
	killed := 0
	for _, m := range mutants {
		if err := ctx.Err(); err != nil {
			return err
		}
		if killed >= r.Mutants || (!cfg.Deadline.IsZero() && time.Now().After(cfg.Deadline)) {
			break
		}
		rec := T2Record{Repo: r.Name, R: r.R, Mutant: m, NativeErr: "no independent static selector"}
		if old, ok := have[rec.key()]; ok && old.Mutant == m && !cfg.Rebuild && old.Status != "error" {
			if old.Status == "killed" {
				killed++
			}
			continue
		}
		start := time.Now()
		if err := r.phpMutantVerdict(ctx, cfg, root, suites, report, &rec); err != nil {
			rec.Status, rec.Detail = "error", err.Error()
		}
		rec.Ms = time.Since(start).Milliseconds()
		if err := store.Append(rec); err != nil {
			return err
		}
		if rec.Status == "killed" {
			killed++
		}
		cfg.logf("%s: %s %s:%d %s %v", r.Name, m.ID, m.File, m.Line, rec.Status, rec.Red)
	}
	return r.ensureClean(ctx, root)
}

func (r T2Repo) phpMutantVerdict(ctx context.Context, cfg *T2Config, root string, suites []string, report string, rec *T2Record) (err error) {
	m := rec.Mutant
	commit, mutated, err := r.MutantCommit(ctx, root, filepath.Join(cfg.Dir, "idx"), m)
	if err != nil {
		return err
	}
	rec.Commit = commit
	file := filepath.Join(root, filepath.FromSlash(m.File))
	original, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	defer func() {
		if restore := os.WriteFile(file, original, 0o644); restore != nil {
			err = restore
		}
	}()
	if err := os.WriteFile(file, mutated, 0o644); err != nil {
		return err
	}
	out, tail, timeout, err := runProc(ctx, root, r.Env, phpTimeout, "php", "-l", m.File)
	if timeout {
		rec.Status = "timeout"
		return nil
	}
	if err != nil {
		if !strings.Contains(out+tail, "Parse error") && !strings.Contains(out+tail, "Fatal error") {
			return fmt.Errorf("PHP lint: %w: %s", err, tail)
		}
		rec.Status, rec.Detail = "nocompile", strings.TrimSpace(out+tail)
		return nil
	}
	var redSuites []string
	for _, suite := range suites {
		_, red, timeout, err := r.phpSuite(ctx, root, suite, report)
		if timeout {
			rec.Status = "timeout"
			return nil
		}
		if err != nil {
			return err
		}
		if red {
			redSuites = append(redSuites, suite)
		}
	}
	for _, suite := range redSuites {
		_, red, timeout, err := r.phpSuite(ctx, root, suite, report)
		if err != nil || timeout {
			return fmt.Errorf("confirming %s (timeout=%t): %v", suite, timeout, err)
		}
		id := php.UnitID(filepath.ToSlash(suite))
		if red {
			rec.Red = append(rec.Red, id)
		} else {
			rec.RedUnconfirmed = append(rec.RedUnconfirmed, id)
		}
	}
	rec.TestFiles, rec.Confirmed, rec.Status = len(suites), true, "survived"
	if len(rec.Red) > 0 {
		rec.Status = "killed"
	}
	return nil
}

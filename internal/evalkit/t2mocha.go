package evalkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Mocha support of the Node T2 harness (Phase 6, docs/dynamic-layer.md §8.6): the whole suite or a list of files with mocha's JSON
// reporter written to a file. T2Repo.Spec are the spec paths of the whole suite (mocha's default is ./test, not recursive);
// Args are the remaining options (--require, --check-leaks...).

type mochaJSON struct {
	Stats struct {
		Tests int `json:"tests"`
	} `json:"stats"`
	Tests []struct {
		File string `json:"file"`
	} `json:"tests"`
	Passes []struct {
		File string `json:"file"`
	} `json:"passes"`
	Failures []struct {
		File      string `json:"file"`
		FullTitle string `json:"fullTitle"`
	} `json:"failures"`
	Pending []struct {
		File string `json:"file"`
	} `json:"pending"`
}

// ParseMochaJSON reads mocha's JSON report into a map file → passed|failed|skipped. A failure without a file (a root hook)
// is an error: it cannot be attributed.
func ParseMochaJSON(root string, b []byte) (map[string]string, int, error) {
	var r mochaJSON
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, 0, err
	}
	out := map[string]string{}
	set := func(f, st string) {
		f = relPath(root, f)
		switch prev := out[f]; {
		case prev == "failed":
		case st == "failed", prev == "", prev == "skipped":
			out[f] = st
		}
	}
	for _, t := range r.Pending {
		set(t.File, "skipped")
	}
	for _, t := range r.Passes {
		set(t.File, "passed")
	}
	for _, t := range r.Failures {
		if t.File == "" {
			return nil, 0, fmt.Errorf("failure without a file: %s", t.FullTitle)
		}
		set(t.File, "failed")
	}
	if len(out) == 0 {
		return nil, 0, fmt.Errorf("report has no tests")
	}
	return out, r.Stats.Tests, nil
}

// runMocha runs the suite (only: these files instead of Spec) with the JSON reporter.
func (r T2Repo) runMocha(ctx context.Context, root, tmp string, timeout time.Duration, only []string) (map[string]string, int, time.Duration, bool, string, error) {
	os.MkdirAll(tmp, 0o755)
	of := filepath.Join(tmp, "run.json")
	os.Remove(of)
	args := append([]string{r.Bin}, r.Args...)
	args = append(args, "--reporter", "json", "--reporter-option", "output="+of)
	if len(only) > 0 {
		args = append(args, only...)
	} else {
		args = append(args, r.Spec...)
	}
	t0 := time.Now()
	_, tail, to, err := runProc(ctx, root, append([]string{"CI=1"}, r.Env...), timeout, "node", args...)
	d := time.Since(t0)
	if ctx.Err() != nil {
		return nil, 0, d, false, "", ctx.Err()
	}
	if to {
		return nil, 0, d, true, "timed out after " + timeout.String(), nil
	}
	b, rerr := os.ReadFile(of)
	if rerr != nil {
		return nil, 0, d, false, "no JSON report: " + fmt.Sprint(err) + " " + tail, nil
	}
	m, n, perr := ParseMochaJSON(root, b)
	if perr != nil {
		return nil, 0, d, false, perr.Error(), nil
	}
	return m, n, d, false, "", nil
}

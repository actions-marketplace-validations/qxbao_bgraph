package nodedyn

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed bgraph_collect.cjs
var collectSrc []byte

//go:embed bgraph_hooks.mjs
var hooksSrc []byte

// Unit is one Node test unit to record (model.TestUnit fields).
type Unit struct {
	ID      string
	Project string // the directory the runner runs from (repo-relative)
	Runner  string // vitest, jest, mocha, node-test, ava
	Files   []string
	// Incomplete: reasons known before running (docs/dynamic.md §8.3 rule 4, jest hasteImpl); the unit still runs.
	Incomplete []string
}

// RunOptions configures Collect.
type RunOptions struct {
	Node    string // node executable
	Root    string // repository root (absolute)
	TmpDir  string // where the collector and the per-unit logs are written (a fresh subdirectory is used)
	Timeout time.Duration
	Jobs    int
	Trust   bool     // trust_native_env
	Env     []string // extra environment (KEY=VALUE); not fingerprinted
	// Progress is called after each unit (serialized).
	Progress func(done, total int, o Outcome)
}

// Outcome is what one isolated run of a unit produced.
type Outcome struct {
	Unit     string
	Analysis Analysis
	Exit     int // process exit code (-1: did not exit normally)
	TimedOut bool
	Err      string // the unit could not be run, or the collector produced no usable output
	Elapsed  time.Duration
	Tail     string // last lines of the output
}

// Collect runs every unit in its own runner invocation with the collector and returns the outcomes in unit order.
func Collect(ctx context.Context, opt RunOptions, units []Unit) ([]Outcome, error) {
	root, err := filepath.EvalSymlinks(opt.Root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	opt.Root = root
	dir, err := os.MkdirTemp(opt.TmpDir, "bgraph-record-node-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if opt.Timeout <= 0 {
		opt.Timeout = 10 * time.Minute
	}
	jobs := max(1, opt.Jobs)
	out := make([]Outcome, len(units))
	var mu sync.Mutex
	done := 0
	idx := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				o := runUnit(ctx, opt, filepath.Join(dir, strconv.Itoa(i)), units[i])
				mu.Lock()
				out[i] = o
				done++
				if opt.Progress != nil {
					opt.Progress(done, len(units), o)
				}
				mu.Unlock()
			}
		}()
	}
	for i := range units {
		if ctx.Err() != nil {
			break
		}
		idx <- i
	}
	close(idx)
	wg.Wait()
	return out, ctx.Err()
}

// runnerPackage is the npm package whose bin runs each runner.
var runnerPackage = map[string]string{"vitest": "vitest", "jest": "jest", "mocha": "mocha", "ava": "ava"}

// ResolveBin finds the JS entry point of package pkg's bin from directory dir upwards (not above root), and the package version.
func ResolveBin(root, dir, pkg string) (bin, version string, err error) {
	for d := dir; ; d = filepath.Dir(d) {
		pj := filepath.Join(d, "node_modules", filepath.FromSlash(pkg), "package.json")
		if b, err := os.ReadFile(pj); err == nil {
			var p struct {
				Version string          `json:"version"`
				Bin     json.RawMessage `json:"bin"`
			}
			if err := json.Unmarshal(b, &p); err != nil {
				return "", "", fmt.Errorf("%s: %w", pj, err)
			}
			var one string
			var many map[string]string
			switch {
			case json.Unmarshal(p.Bin, &one) == nil && one != "":
			case json.Unmarshal(p.Bin, &many) == nil && len(many) > 0:
				one = many[path.Base(pkg)]
				if one == "" && pkg == "jest" {
					one = many["jest"]
				}
				if one == "" {
					for _, v := range many {
						one = v
						break
					}
				}
			}
			if one == "" {
				return "", "", fmt.Errorf("%s has no bin", pj)
			}
			return filepath.Join(filepath.Dir(pj), filepath.FromSlash(one)), p.Version, nil
		}
		if rel, ok := relIn(root, d); !ok || rel == "." {
			break
		}
	}
	return "", "", fmt.Errorf("%s is not installed (no node_modules/%s from %s up to the repository root)", pkg, pkg, dir)
}

func relIn(root, d string) (string, bool) { return rel(root, d) }

// versionAtLeast compares a semver-ish version with major.minor.
func versionAtLeast(v string, major, minor int) bool {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) < 2 {
		return false
	}
	ma, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(strings.TrimLeft(parts[1], "^~"))
	if err1 != nil || err2 != nil {
		return false
	}
	return ma > major || (ma == major && mi >= minor)
}

// Args returns the arguments after the node executable that run unit u alone (docs/dynamic.md §8.1). bin is the runner's JS
// entry point and version its package version (unused for node-test). file is the unit's file relative to the project.
func Args(runner, bin, version, file string) ([]string, error) {
	if strings.HasPrefix(file, "-") {
		file = "./" + file
	}
	switch runner {
	case "vitest":
		a := []string{bin, "run", file, "--coverage.enabled=false", "--reporter=dot"}
		if versionAtLeast(version, 4, 1) {
			a = append(a, "--update=none")
		}
		switch {
		case versionAtLeast(version, 5, 0):
			a = append(a, "--fsModuleCache=false")
		case versionAtLeast(version, 4, 1):
			a = append(a, "--experimental.fsModuleCache=false")
		}
		return a, nil
	case "jest":
		return []string{bin, "--runTestsByPath", file, "-i", "--ci", "--no-cache", "--no-watchman", "--coverage=false"}, nil
	case "mocha", "ava":
		return []string{bin, file}, nil
	case "node-test":
		return []string{"--test", file}, nil
	}
	return nil, fmt.Errorf("runner %s is not supported by the Node collector", runner)
}

// nodeOptionsToken is the NODE_OPTIONS entry that loads the collector (quoted when the path has spaces).
func nodeOptionsToken(preload string) string {
	p := filepath.ToSlash(preload)
	if strings.ContainsAny(p, " \t") {
		return `--require="` + p + `"`
	}
	return "--require=" + p
}

func runUnit(ctx context.Context, opt RunOptions, dir string, u Unit) (o Outcome) {
	o = Outcome{Unit: u.ID, Exit: -1}
	t0 := time.Now()
	defer func() { o.Elapsed = time.Since(t0) }()
	projDir := filepath.Join(opt.Root, filepath.FromSlash(u.Project))
	bin, version := "", ""
	if pkg, ok := runnerPackage[u.Runner]; ok {
		var err error
		if bin, version, err = ResolveBin(opt.Root, projDir, pkg); err != nil {
			o.Err = err.Error()
			return o
		}
	}
	file := u.ID
	if u.Project != "." && u.Project != "" {
		r, ok := strings.CutPrefix(u.ID, strings.TrimSuffix(u.Project, "/")+"/")
		if !ok {
			o.Err = "test file outside its project"
			return o
		}
		file = r
	}
	args, err := Args(u.Runner, bin, version, file)
	if err != nil {
		o.Err = err.Error()
		return o
	}
	colDir, logDir := filepath.Join(dir, "collector"), filepath.Join(dir, "logs")
	for _, d := range []string{colDir, logDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			o.Err = err.Error()
			return o
		}
	}
	preload := filepath.Join(colDir, "bgraph_collect.cjs")
	token := nodeOptionsToken(preload)
	cfg, _ := json.Marshal(map[string]string{"root": opt.Root, "out": logDir, "preload": token})
	for name, b := range map[string][]byte{"bgraph_collect.cjs": collectSrc, "bgraph_hooks.mjs": hooksSrc, "config.json": cfg} {
		if err := os.WriteFile(filepath.Join(colDir, name), b, 0o644); err != nil {
			o.Err = err.Error()
			return o
		}
	}
	env := environ(opt.Env, token)
	cmd := exec.Command(opt.Node, args...)
	cmd.Dir = projDir
	cmd.Env = env
	var buf tailBuffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		o.Err = err.Error()
		return o
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	timer := time.NewTimer(opt.Timeout)
	defer timer.Stop()
	select {
	case err = <-wait:
	case <-timer.C:
		o.TimedOut = true
		killTree(cmd)
		<-wait
	case <-ctx.Done():
		killTree(cmd)
		<-wait
		o.Err = ctx.Err().Error()
		return o
	}
	o.Tail = buf.String()
	if o.TimedOut {
		return o
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		o.Exit = 0
	case errors.As(err, &ee) && ee.Exited():
		o.Exit = ee.ExitCode()
	default:
		o.Err = err.Error()
		return o
	}
	o.Analysis = Analyze(logDir, opt.Root, cmd.Process.Pid, opt.Trust)
	o.Analysis.Incomplete = append(o.Analysis.Incomplete, u.Incomplete...)
	return o
}

// environ is the process environment of a unit's run: the collector appended to NODE_OPTIONS, no inherited spawn token.
func environ(extra []string, token string) []string {
	var env []string
	nodeOpts := ""
	add := func(kv string) {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case strings.EqualFold(k, "NODE_OPTIONS"):
			nodeOpts = strings.TrimSpace(v)
		case strings.EqualFold(k, "BGRAPH_COLLECT_PARENT"):
		default:
			env = append(env, kv)
		}
	}
	for _, kv := range os.Environ() {
		add(kv)
	}
	for _, kv := range extra {
		add(kv)
	}
	if nodeOpts != "" {
		nodeOpts += " "
	}
	return append(env, "NODE_OPTIONS="+nodeOpts+token)
}

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return
	}
	cmd.Process.Kill()
}

// tailBuffer keeps the last 4 KiB written.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 8192 {
		t.b = append([]byte(nil), t.b[len(t.b)-4096:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.b
	if len(b) > 4096 {
		b = b[len(b)-4096:]
	}
	return string(bytes.ToValidUTF8(b, []byte("?")))
}

// DefaultJobs is the default parallelism of `bgraph record` for Node units (runner processes are heavier than pytest ones).
func DefaultJobs() int { return max(1, runtime.NumCPU()/4) }

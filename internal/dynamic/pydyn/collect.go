package pydyn

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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/tree"
)

//go:embed bgraph_collect.py
var pluginSrc []byte

const bootSrc = "import sys\nimport bgraph_collect\nbgraph_collect.bootstrap(sys.argv[1:])\n"

// Unit is one Python test unit to record (model.TestUnit fields).
type Unit struct {
	ID      string
	Project string // pytest rootdir candidate: the directory the unit's command runs from
	Runner  string // "pytest" or "django"
	Config  string // pytest config file, or the Django driver script
	Files   []string
}

// RunOptions configures Collect.
type RunOptions struct {
	Python      string // interpreter executable
	Root        string // repository root (absolute)
	TmpDir      string // where the plugin and the per-unit outputs are written (a fresh subdirectory is used)
	Timeout     time.Duration
	Jobs        int
	TrustNative bool
	// Env: extra environment variables (KEY=VALUE) for the unit processes. PYTHONPATH entries go after the plugin directory.
	Env []string
	// Progress is called after each unit (from the collecting goroutine, serialized).
	Progress func(done, total int, o Outcome)
}

// Outcome is what one isolated run of a unit produced.
type Outcome struct {
	Unit       string
	Files      []string // repo-relative files the run opened, loaded or compiled (as reported, case as on disk)
	Dirs       []string // repo-relative directories listed by test code ("." is the root)
	Incomplete []string // why the collector could not see everything
	Exit       int      // process exit code (-1: did not exit normally)
	TimedOut   bool
	Err        string // the collector produced no usable output
	Elapsed    time.Duration
	Tail       string // last lines of the output, for failed runs
}

// Collect runs every unit in its own process with the collector plugin and returns the outcomes in unit order.
func Collect(ctx context.Context, opt RunOptions, units []Unit) ([]Outcome, error) {
	dir, err := os.MkdirTemp(opt.TmpDir, "bgraph-record-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	plug := filepath.Join(dir, "plugin")
	if err := os.MkdirAll(plug, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(plug, "bgraph_collect.py"), pluginSrc, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(plug, "bgraph_boot.py"), []byte(bootSrc), 0o644); err != nil {
		return nil, err
	}
	jobs := opt.Jobs
	if jobs <= 0 {
		jobs = 1
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 10 * time.Minute
	}
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
				o := runUnit(ctx, opt, plug, filepath.Join(dir, fmt.Sprintf("out-%d.json", i)), units[i])
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

// Argv returns the command (relative to the unit's project directory) that runs unit u with the collector: pytest with the
// plugin, or the Django driver through the bootstrap script. plug is the plugin directory.
func Argv(u Unit, plug string) []string {
	if u.Runner == "django" {
		base := u.Project
		args := []string{filepath.Join(plug, "bgraph_boot.py"), relFrom(u.Project, u.Config)}
		if path.Base(u.Config) == "manage.py" {
			args = append(args, "test")
		} else if path.Base(u.Config) == "runtests.py" {
			base = path.Dir(u.Config)
		}
		label := strings.ReplaceAll(strings.TrimSuffix(relFrom(base, u.ID), ".py"), "/", ".")
		return append(args, label)
	}
	args := []string{"-m", "pytest", "-p", "bgraph_collect", "-p", "no:cacheprovider", "-q"}
	if u.Config != "" {
		args = append(args, "--rootdir", relFrom(u.Project, path.Dir(u.Config)), "-c", relFrom(u.Project, u.Config))
	} else {
		args = append(args, "--rootdir", ".")
	}
	f := relFrom(u.Project, u.ID)
	if strings.HasPrefix(f, "-") || strings.HasPrefix(f, "@") {
		f = "./" + f
	}
	return append(args, f)
}

func runUnit(ctx context.Context, opt RunOptions, plug, outFile string, u Unit) (o Outcome) {
	o = Outcome{Unit: u.ID, Exit: -1}
	t0 := time.Now()
	defer func() { o.Elapsed = time.Since(t0) }()
	if u.Runner != "pytest" && u.Runner != "django" {
		o.Err = "runner " + u.Runner + " is not supported by the Python collector"
		return o
	}
	if u.Runner == "django" {
		rel := relFrom(u.Project, u.ID)
		if strings.HasPrefix(rel, "../") {
			o.Err = "test file outside the Django label root"
			return o
		}
	}
	rctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, opt.Python, Argv(u, plug)...)
	cmd.Dir = filepath.Join(opt.Root, filepath.FromSlash(u.Project))
	pp := plug
	var extra []string
	for _, kv := range opt.Env {
		if v, ok := strings.CutPrefix(kv, "PYTHONPATH="); ok {
			if v != "" {
				pp += string(os.PathListSeparator) + v
			}
			continue
		}
		extra = append(extra, kv)
	}
	if old := os.Getenv("PYTHONPATH"); old != "" {
		pp += string(os.PathListSeparator) + old
	}
	trust := "0"
	if opt.TrustNative {
		trust = "1"
	}
	cmd.Env = append(os.Environ(), "PYTHONPATH="+pp, "PYTHONDONTWRITEBYTECODE=1", "BGRAPH_COLLECT_ROOT="+opt.Root,
		"BGRAPH_COLLECT_OUT="+outFile, "BGRAPH_COLLECT_TRUST_NATIVE="+trust)
	cmd.Env = append(cmd.Env, extra...)
	var buf tailBuffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	o.Tail = buf.String()
	if rctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		o.TimedOut = true
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
	b, rerr := os.ReadFile(outFile)
	if rerr != nil {
		o.Err = "the collector wrote no output"
		return o
	}
	var data struct {
		Collector  string   `json:"collector"`
		Files      []string `json:"files"`
		Dirs       []string `json:"dirs"`
		Incomplete []string `json:"incomplete"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		o.Err = "unreadable collector output: " + err.Error()
		return o
	}
	if data.Collector != CollectorName {
		o.Err = "collector plugin version " + data.Collector + " differs from " + CollectorName
		return o
	}
	o.Files, o.Dirs, o.Incomplete = data.Files, data.Dirs, data.Incomplete
	return o
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

// relFrom returns target (repo-relative) relative to directory from (repo-relative).
func relFrom(from, to string) string {
	from, to = path.Clean(from), path.Clean(to)
	if from == "." || from == "" {
		return to
	}
	fp, tp := strings.Split(from, "/"), strings.Split(to, "/")
	if to == "." {
		tp = nil
	}
	i := 0
	for i < len(fp) && i < len(tp) && fp[i] == tp[i] {
		i++
	}
	var parts []string
	for j := i; j < len(fp); j++ {
		parts = append(parts, "..")
	}
	parts = append(parts, tp[i:]...)
	if len(parts) == 0 {
		return "."
	}
	return strings.Join(parts, "/")
}

// derived: directories whose files are caches or build metadata derived from tracked files (or from the environment), never
// inputs a change can reach except through those tracked files.
func derived(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "__pycache__", ".pytest_cache", ".hypothesis", ".mypy_cache", ".ruff_cache", ".tox", ".nox", ".eggs", ".git":
			return true
		}
		if strings.HasSuffix(seg, ".egg-info") || strings.HasSuffix(seg, ".dist-info") {
			return true
		}
	}
	return false
}

// pycSource maps a/__pycache__/m.cpython-313.pyc to a/m.py.
func pycSource(p string) (string, bool) {
	dir, file := path.Split(p)
	if path.Base(strings.TrimSuffix(dir, "/")) != "__pycache__" || !strings.HasSuffix(file, ".pyc") {
		return "", false
	}
	name, _, _ := strings.Cut(file, ".")
	return path.Join(path.Dir(strings.TrimSuffix(dir, "/")), name+".py"), true
}

// RecordOf turns an outcome into a record (docs/dynamic-layer.md §3). t is the working tree at the recorded commit (its keys are the
// blob OIDs of the content the test saw); root is the repository root on disk (to tell untracked files from absent ones).
func RecordOf(o Outcome, u Unit, t tree.Tree, root, env, collector, commit string, now time.Time) dynamic.Record {
	r := dynamic.Record{Eco: "python", Unit: u.ID, Collector: collector, Commit: commit, Deps: map[string]string{}, Env: env,
		Complete: true, Pass: o.Exit == 0 && !o.TimedOut && o.Err == "", Recorded: now.UTC()}
	var why []string
	switch {
	case o.TimedOut:
		why = append(why, "timeout")
	case o.Err != "":
		why = append(why, o.Err)
	}
	if o.Exit == 5 {
		why = append(why, "no tests ran (exit 5)")
	}
	why = append(why, o.Incomplete...)
	lookup := func(p string) (string, bool) {
		if t.Has(p) {
			return p, true
		}
		if m := t.FoldLookup(p); len(m) == 1 {
			return m[0], true
		}
		return "", false
	}
	add := func(p string) {
		if k := t.Key(p); k != "" {
			r.Deps[p] = k
		} else {
			why = append(why, "cannot hash "+p)
		}
	}
	for _, f := range o.Files {
		if p, ok := lookup(f); ok {
			add(p)
			continue
		}
		if src, ok := pycSource(f); ok {
			if p, ok := lookup(src); ok {
				add(p)
			}
			continue // a cached module: the source is the dependency (absent source: the cache is not importable)
		}
		if derived(f) {
			continue
		}
		st, err := os.Stat(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil || st.IsDir() {
			continue // absent (a negative dependency: additions are never narrowed) or a directory
		}
		why = append(why, "reads untracked file "+f)
	}
	if len(o.Dirs) > 0 {
		files := t.Files()
		for _, d := range o.Dirs {
			if d == "." {
				for _, f := range files {
					if !derived(f) {
						add(f)
					}
				}
				continue
			}
			pre := d + "/"
			if !t.IsDir(d) {
				// case-insensitive file system: find the tracked spelling of the directory
				for _, f := range files {
					if len(f) > len(pre) && strings.EqualFold(f[:len(pre)], pre) {
						pre = f[:len(pre)]
						break
					}
				}
			}
			i := sort.SearchStrings(files, pre)
			for ; i < len(files) && strings.HasPrefix(files[i], pre); i++ {
				add(files[i])
			}
		}
	}
	for _, f := range u.Files {
		if p, ok := lookup(f); ok {
			add(p)
		} else {
			why = append(why, "the unit's own file "+f+" is not in the tree")
		}
	}
	if len(why) > 0 {
		r.Complete = false
		r.Incomplete = strings.Join(dedupe(why), "; ")
	}
	return r
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// DefaultJobs is the default parallelism of `bgraph record`.
func DefaultJobs() int { return max(1, runtime.NumCPU()/2) }

package rbdyn

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
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed bgraph_rb_collect.rb
var collectorSrc []byte

// collectorFeature is the feature name RUBYOPT requires (the collector's file name without .rb).
const collectorFeature = "bgraph_rb_collect"

// MinitestLoader is the loader the runner adapter uses for Minitest / Test::Unit files (internal/runner: Rake::TestTask's).
const MinitestLoader = "ARGV.each { |f| require File.expand_path(f) }"

// Unit is one Ruby test unit (a test file) to record.
type Unit struct {
	ID      string // the test file, repo-relative
	Project string // the directory the runner runs from
	Runner  string // rspec, minitest or rails
	Config  string // "<Gemfile>" or "<Gemfile>|<lib,test>" (internal/eco/ruby)
}

// RunOptions configures Collect.
type RunOptions struct {
	Ruby    string // Ruby executable; its directory is put first on PATH for the unit processes (bundle, rspec, ruby)
	Root    string // repository root (absolute)
	TmpDir  string // where the collector and the per-unit outputs are written (a fresh subdirectory is used)
	Timeout time.Duration
	Jobs    int
	Env     []string // extra KEY=VALUE for the unit processes
	// Progress is called after each unit (serialized).
	Progress func(done, total int, o Outcome)
}

// Child is a process a recorded Ruby process started.
type Child struct {
	Pid  int    `json:"pid"`
	Ruby bool   `json:"ruby"`
	Cmd  string `json:"cmd"`
	Exec bool   `json:"exec"`
}

// procOut is the output of one Ruby process of a unit.
type procOut struct {
	Collector  string             `json:"collector"`
	Pid        int                `json:"pid"`
	Ppid       int                `json:"ppid"`
	Exec       bool               `json:"exec"`
	Methods    map[string][][]int `json:"methods"`
	Loaded     []string           `json:"loaded"`
	Reads      []string           `json:"reads"`
	Dirs       []string           `json:"dirs"`
	Children   []Child            `json:"children"`
	Incomplete []string           `json:"incomplete"`
}

// Method is one method of Coverage's table: the def line, the end line, how many times it ran.
type Method struct{ Start, End, Count int }

// Outcome is what one isolated run of a unit produced (absolute paths with forward slashes, as Ruby reports them).
type Outcome struct {
	Unit       string
	Methods    map[string][]Method // loaded files with a method table
	NoTable    map[string]bool     // loaded files without one in some process (loaded before the collector started)
	Loaded     []string            // every loaded file (coverage and $LOADED_FEATURES)
	Reads      []string            // files read as data
	Dirs       []string            // directories listed by test code
	Incomplete []string
	Tests      int  // tests run, from the runner's summary line (-1: no summary)
	Exit       int  // exit code (-1: did not exit normally)
	TimedOut   bool // the unit hit the timeout (its process tree was killed)
	Err        string
	Elapsed    time.Duration
	Tail       string
}

// Collect runs every unit in its own process tree with the collector and returns the outcomes in unit order.
func Collect(ctx context.Context, opt RunOptions, units []Unit) ([]Outcome, error) {
	dir, err := os.MkdirTemp(opt.TmpDir, "bgraph-rbrecord-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	lib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(lib, collectorFeature+".rb"), collectorSrc, 0o644); err != nil {
		return nil, err
	}
	jobs := max(1, opt.Jobs)
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
				od := filepath.Join(dir, fmt.Sprintf("out-%d", i))
				o := runUnit(ctx, opt, lib, od, units[i])
				os.RemoveAll(od)
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

// rubyConfig splits a unit's Config into the Gemfile and the -I directories (internal/runner).
func rubyConfig(c string) (gemfile string, libs []string) {
	g, l, _ := strings.Cut(c, "|")
	if l != "" {
		libs = strings.Split(l, ",")
	}
	return g, libs
}

// Argv returns the command that runs unit u as the runner adapter does (internal/runner, rubyCommands), from u's project
// directory, with the Ruby executable ruby: `bundle exec` goes through `ruby <bindir>/bundle` (or `ruby -S bundle`) so that the
// process is Ruby itself.
func Argv(u Unit, ruby string) ([]string, error) {
	gemfile, libs := rubyConfig(u.Config)
	f := relFrom(u.Project, u.ID)
	if strings.HasPrefix(f, "../") {
		return nil, errors.New("test file outside the project directory")
	}
	if strings.HasPrefix(f, "-") {
		f = "./" + f
	}
	script := func(name string) []string {
		p := filepath.Join(filepath.Dir(ruby), name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return []string{ruby, p}
		}
		return []string{ruby, "-S", name}
	}
	switch u.Runner {
	case "rspec":
		if gemfile != "" {
			return append(script("bundle"), "exec", "rspec", f), nil
		}
		return append(script("rspec"), f), nil
	case "minitest":
		var args []string
		if gemfile != "" {
			args = append(script("bundle"), "exec", "ruby")
		} else {
			args = []string{ruby}
		}
		for _, l := range libs {
			args = append(args, "-I"+l)
		}
		return append(args, "-e", MinitestLoader, f), nil
	case "rails":
		return []string{ruby, "bin/rails", "test", f}, nil
	}
	return nil, fmt.Errorf("runner %q is not supported by the Ruby collector", u.Runner)
}

// summary lines of RSpec, Minitest and Test::Unit.
var (
	reRSpecSummary    = regexp.MustCompile(`(?m)^(\d+) examples?, \d+ failures?`)
	reMinitestSummary = regexp.MustCompile(`(?m)^(\d+) (?:runs|tests), \d+ assertions`)
)

// testCount returns the number of tests the runner's last summary line reports (-1 when there is none).
func testCount(out string) int {
	n := -1
	best := -1
	for _, re := range []*regexp.Regexp{reRSpecSummary, reMinitestSummary} {
		for _, m := range re.FindAllStringSubmatchIndex(out, -1) {
			if m[0] > best {
				best = m[0]
				n, _ = strconv.Atoi(out[m[2]:m[3]])
			}
		}
	}
	return n
}

// unitEnv returns the environment of a unit's processes: the collector on RUBYLIB and first in RUBYOPT, the Ruby's directory
// first on PATH, the output directory and the repository root for the collector, then extra.
func unitEnv(lib, outDir, root, ruby string, extra []string) []string {
	env := os.Environ()
	get := func(k string) string {
		v := ""
		for _, kv := range env {
			if n, val, ok := strings.Cut(kv, "="); ok && (n == k || runtime.GOOS == "windows" && strings.EqualFold(n, k)) {
				v = val
			}
		}
		for _, kv := range extra {
			if n, val, ok := strings.Cut(kv, "="); ok && (n == k || runtime.GOOS == "windows" && strings.EqualFold(n, k)) {
				v = val
			}
		}
		return v
	}
	join := func(first, rest string) string {
		if rest == "" {
			return first
		}
		return first + string(os.PathListSeparator) + rest
	}
	rubyopt := "-r" + collectorFeature
	if old := get("RUBYOPT"); old != "" {
		rubyopt += " " + old
	}
	set := map[string]string{
		"RUBYOPT":        rubyopt,
		"RUBYLIB":        join(lib, get("RUBYLIB")),
		"PATH":           join(filepath.Dir(ruby), get("PATH")),
		"BGRAPH_RB_OUT":  outDir,
		"BGRAPH_RB_ROOT": filepath.ToSlash(root),
	}
	var out []string
	for _, kv := range append(env, extra...) {
		n, _, _ := strings.Cut(kv, "=")
		skip := false
		for k := range set {
			if n == k || runtime.GOOS == "windows" && strings.EqualFold(n, k) {
				skip = true
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}
	return out
}

func runUnit(ctx context.Context, opt RunOptions, lib, outDir string, u Unit) (o Outcome) {
	o = Outcome{Unit: u.ID, Exit: -1, Tests: -1}
	t0 := time.Now()
	defer func() { o.Elapsed = time.Since(t0) }()
	argv, err := Argv(u, opt.Ruby)
	if err != nil {
		o.Err = err.Error()
		return o
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		o.Err = err.Error()
		return o
	}
	rctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, argv[0], argv[1:]...)
	cmd.Dir = filepath.Join(opt.Root, filepath.FromSlash(u.Project))
	cmd.Env = unitEnv(lib, outDir, opt.Root, opt.Ruby, opt.Env)
	var buf tailBuffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	o.Tail = buf.String()
	o.Tests = testCount(o.Tail)
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
	mainPid := 0
	if cmd.Process != nil {
		mainPid = cmd.Process.Pid
	}
	merge(&o, outDir, mainPid)
	return o
}

// merge reads the per-process outputs of a unit and fills o (docs/dynamic-layer.md §10.4).
func merge(o *Outcome, outDir string, mainPid int) {
	ents, err := os.ReadDir(outDir)
	if err != nil {
		o.Err = "the collector wrote no output"
		return
	}
	var procs []procOut
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(outDir, e.Name()))
		if err != nil {
			o.Incomplete = append(o.Incomplete, "unreadable collector output "+e.Name())
			continue
		}
		var p procOut
		if err := json.Unmarshal(b, &p); err != nil {
			o.Incomplete = append(o.Incomplete, "unreadable collector output "+e.Name()+": "+err.Error())
			continue
		}
		if p.Collector != CollectorID {
			o.Incomplete = append(o.Incomplete, "collector version "+p.Collector+" differs from "+CollectorID)
			continue
		}
		procs = append(procs, p)
	}
	if len(procs) == 0 {
		o.Err = "the collector wrote no output"
		return
	}
	hasMain := false
	final := map[int]bool{}  // pids that wrote a final (at exit) output
	parent := map[int]bool{} // ppids of the processes that wrote an output
	for _, p := range procs {
		if p.Pid == mainPid {
			hasMain = true
		}
		if !p.Exec {
			final[p.Pid] = true
		}
		parent[p.Ppid] = true
	}
	if mainPid > 0 && !hasMain {
		o.Incomplete = append(o.Incomplete, "the test process wrote no coverage (it exited without running at_exit handlers, or crashed)")
	}
	counts := map[string]map[[2]int]int{}
	loaded := map[string]bool{}
	reads := map[string]bool{}
	dirs := map[string]bool{}
	o.NoTable = map[string]bool{}
	for _, p := range procs {
		o.Incomplete = append(o.Incomplete, p.Incomplete...)
		for f, ms := range p.Methods {
			loaded[f] = true
			if counts[f] == nil {
				counts[f] = map[[2]int]int{}
			}
			for _, m := range ms {
				if len(m) != 3 {
					o.Incomplete = append(o.Incomplete, "malformed method entry for "+f)
					continue
				}
				counts[f][[2]int{m[0], m[1]}] += m[2]
			}
		}
		for _, f := range p.Loaded {
			loaded[f] = true
			if _, ok := p.Methods[f]; !ok {
				o.NoTable[f] = true // loaded in this process before Coverage started, or not Ruby code
			}
		}
		for _, f := range p.Reads {
			reads[f] = true
		}
		for _, d := range p.Dirs {
			dirs[d] = true
		}
		for _, c := range p.Children {
			c.Cmd = strings.ReplaceAll(c.Cmd, ";", ",") // reasons are joined with "; "
			switch {
			case !c.Ruby:
				o.Incomplete = append(o.Incomplete, "starts a non-Ruby process: "+c.Cmd)
			case c.Pid <= 0:
				o.Incomplete = append(o.Incomplete, "starts a Ruby process whose pid is unknown: "+c.Cmd)
			case c.Exec && !final[c.Pid] && !parent[c.Pid]:
				o.Incomplete = append(o.Incomplete, "exec of a Ruby process that wrote no coverage: "+c.Cmd)
			case !c.Exec && !final[c.Pid] && !parent[c.Pid]:
				o.Incomplete = append(o.Incomplete, "starts a Ruby process that wrote no coverage: "+c.Cmd)
			}
		}
	}
	o.Methods = map[string][]Method{}
	for f, c := range counts {
		ms := make([]Method, 0, len(c))
		for k, n := range c {
			ms = append(ms, Method{k[0], k[1], n})
		}
		sort.Slice(ms, func(i, j int) bool {
			if ms[i].Start != ms[j].Start {
				return ms[i].Start < ms[j].Start
			}
			return ms[i].End < ms[j].End
		})
		o.Methods[f] = ms
	}
	o.Loaded, o.Reads, o.Dirs = keysOf(loaded), keysOf(reads), keysOf(dirs)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tailBuffer keeps the last 8 KiB written.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 16384 {
		t.b = append([]byte(nil), t.b[len(t.b)-8192:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.b
	if len(b) > 8192 {
		b = b[len(b)-8192:]
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

// DefaultJobs is the default parallelism of the Ruby recording.
func DefaultJobs() int { return max(1, runtime.NumCPU()/2) }

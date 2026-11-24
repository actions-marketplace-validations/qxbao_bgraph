package phpdyn

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

//go:embed bgraph_php_collect.php
var collectorSrc []byte

//go:embed bgraph_php_list.php
var listSrc []byte

// Runners of the PHP ecosystem (internal/eco/php).
const (
	RunnerPHPUnit = "phpunit"
	RunnerPest    = "pest"
)

// Unit is one PHP test unit (a Composer / PHPUnit project) whose test files are recorded.
type Unit struct {
	ID          string   // the project unit ID (<dir>::php)
	Project     string   // the directory the runner runs from (repo-relative)
	Runner      string   // phpunit or pest
	Config      string   // the PHPUnit XML configuration (repo-relative)
	BinDir      string   // the Composer bin directory of the runner (repo-relative)
	RunnerPaths []string // see model.TestUnit.RunnerPaths (Pest: --test-directory)
	Vendor      string   // the Composer vendor directory (repo-relative)
}

// FileUnit is one test file of a unit, recorded in a process tree of its own.
type FileUnit struct {
	Unit Unit
	File string // repo-relative test file
}

// ID returns the sub-unit ID of a test file: "<unit ID>#<file>".
func (f FileUnit) ID() string { return SubID(f.Unit.ID, f.File) }

// SubID returns the sub-unit ID of test file file of unit unit.
func SubID(unit, file string) string { return unit + "#" + file }

// RunOptions configures Collect and List.
type RunOptions struct {
	PHP     string // PHP executable
	Probe   Probe  // its probe (scan directory, version)
	Root    string // repository root (absolute)
	TmpDir  string // where the collector and the per-unit outputs are written (a fresh subdirectory is used)
	Timeout time.Duration
	Jobs    int
	Env     []string // extra KEY=VALUE for the unit processes
	Vendors []string // every Composer vendor directory of the repository (repo-relative): never dependencies
	// Progress is called after each test file (serialized).
	Progress func(done, total int, o Outcome)
}

// Check returns why the PHP executable of p cannot record (docs/dynamic-layer.md §11.4), "" when it can.
func Check(p Probe) string {
	major := 0
	if v, _, ok := strings.Cut(p.Version, "."); ok {
		major, _ = strconv.Atoi(v)
	}
	switch {
	case major < 8:
		return fmt.Sprintf("PHP %s: the collector needs PHP 8 or later", p.Version)
	case p.Xdebug == "":
		return "Xdebug is not loaded by this PHP executable (the collector needs Xdebug 3)"
	case !strings.HasPrefix(p.Xdebug, "3."):
		return "Xdebug " + p.Xdebug + ": the collector needs Xdebug 3"
	case p.ini("auto_prepend_file") != "":
		return "this PHP executable already has an auto_prepend_file (" + p.ini("auto_prepend_file") + "): the collector would replace it"
	}
	return ""
}

// Argv returns the command that records test file f of unit u, run from u's project directory: the runner as the runner
// adapter runs a narrowed unit with one operand, plus the flags that keep it from writing reports and from reading or writing the
// repository's result cache (cacheDir, outside the repository, replaces it).
func Argv(u Unit, file, php, root, cacheDir string) ([]string, error) {
	if u.Config == "" {
		return nil, errors.New("no PHPUnit XML configuration")
	}
	cfg := relFrom(u.Project, u.Config)
	f := relFrom(u.Project, file)
	if strings.HasPrefix(f, "../") {
		return nil, errors.New("test file outside the project directory")
	}
	if strings.HasPrefix(cfg, "-") {
		cfg = "./" + cfg
	}
	if strings.HasPrefix(f, "-") {
		f = "./" + f
	}
	if u.BinDir == "" || u.BinDir == "/" {
		return nil, errors.New("the runner's Composer bin directory is unknown")
	}
	argv := []string{php, filepath.Join(root, filepath.FromSlash(u.BinDir), u.Runner), "--configuration", cfg}
	if u.Runner == RunnerPest && len(u.RunnerPaths) == 1 {
		argv = append(argv, "--test-directory="+relFrom(u.Project, u.RunnerPaths[0]))
	}
	return append(argv, "--no-coverage", "--do-not-cache-result", "--cache-directory", cacheDir, "--no-logging", "--colors=never", f), nil
}

// Child is a process launch the runner's own package made (PHPUnit's process isolation).
type launch struct {
	Fn string `json:"fn"`
	At string `json:"at"`
}

// event is what the file:// proxy saw for one path in one process.
// outsideEvent is a file outside the repository (or in a vendor directory) and the process's own temporary directory that the
// process wrote (W: content written) or read or included (R).
type outsideEvent struct {
	P string `json:"p"`
	W bool   `json:"w"`
	R bool   `json:"r"`
}

type event struct {
	Kinds []string `json:"k"`
	Pre   bool     `json:"pre"` // the path existed before this process wrote it
	Write bool     `json:"w"`
}

// procOut is the output of one PHP process of a test file.
type procOut struct {
	Collector  string            `json:"collector"`
	Pid        int               `json:"pid"`
	Ppid       int               `json:"ppid"`
	Token      string            `json:"token"`
	Parent     string            `json:"parent"`
	Included   []string          `json:"included"`
	Coverage   map[string]string `json:"coverage"`
	Events     map[string]event  `json:"events"`
	Launches   []launch          `json:"launches"`
	Outside    []outsideEvent    `json:"outside"`
	Incomplete []string          `json:"incomplete"`
}

// UnmarshalJSON accepts PHP's encoding of empty maps as [].
func (p *procOut) UnmarshalJSON(b []byte) error {
	type plain procOut
	var raw struct {
		plain
		Coverage json.RawMessage `json:"coverage"`
		Events   json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*p = procOut(raw.plain)
	p.Coverage, p.Events = map[string]string{}, map[string]event{}
	if len(raw.Coverage) > 0 && string(raw.Coverage) != "[]" {
		if err := json.Unmarshal(raw.Coverage, &p.Coverage); err != nil {
			return err
		}
	}
	if len(raw.Events) > 0 && string(raw.Events) != "[]" {
		if err := json.Unmarshal(raw.Events, &p.Events); err != nil {
			return err
		}
	}
	return nil
}

// Path is what the processes of a test file did with one absolute path.
type Path struct {
	Kinds map[string]bool // include, read, write, stat, list
	Pre   bool            // existed before the run wrote it, in some process
}

// Outcome is what one isolated run of a test file produced (absolute paths with forward slashes).
type Outcome struct {
	Unit        string             // the sub-unit ID
	File        string             // the test file (repo-relative)
	Included    []string           // every included file
	Lines       map[string]LineSet // included files with a line table in every process that included them
	NoTable     map[string]bool    // included files without a line table in some process
	Written     []string           // files whose content the processes wrote outside the repository (or in a vendor directory) and their own temporary directory
	OutsideRead []string           // files read or included there
	Paths       map[string]Path    // file events of the proxy
	Incomplete  []string
	Tests       int  // tests run, from the runner's summary (-1: no summary)
	Exit        int  // exit code (-1: did not exit normally)
	TimedOut    bool // the test file hit the timeout (its process tree was killed)
	Err         string
	Elapsed     time.Duration
	Tail        string
}

// Collect runs every test file in its own process tree with the collector and returns the outcomes in order.
func Collect(ctx context.Context, opt RunOptions, files []FileUnit) ([]Outcome, error) {
	if why := Check(opt.Probe); why != "" {
		return nil, errors.New(why)
	}
	dir, err := os.MkdirTemp(opt.TmpDir, "bgraph-phprecord-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	collector := filepath.Join(dir, "bgraph_php_collect.php")
	if err := os.WriteFile(collector, collectorSrc, 0o644); err != nil {
		return nil, err
	}
	iniDir := filepath.Join(dir, "ini")
	if err := os.MkdirAll(iniDir, 0o755); err != nil {
		return nil, err
	}
	ini := fmt.Sprintf("auto_prepend_file=\"%s\"\nxdebug.mode=coverage,develop\n", collector)
	if err := os.WriteFile(filepath.Join(iniDir, "zz-bgraph-collect.ini"), []byte(ini), 0o644); err != nil {
		return nil, err
	}
	jobs := max(1, opt.Jobs)
	if opt.Timeout <= 0 {
		opt.Timeout = 10 * time.Minute
	}
	out := make([]Outcome, len(files))
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
				o := runFile(ctx, opt, iniDir, od, files[i])
				os.RemoveAll(od)
				mu.Lock()
				out[i] = o
				done++
				if opt.Progress != nil {
					opt.Progress(done, len(files), o)
				}
				mu.Unlock()
			}
		}()
	}
	for i := range files {
		if ctx.Err() != nil {
			break
		}
		idx <- i
	}
	close(idx)
	wg.Wait()
	return out, ctx.Err()
}

// lookup returns the value of k in env (last wins; case-insensitive on Windows).
func lookup(env []string, k string) (string, bool) {
	v, found := "", false
	for _, kv := range env {
		if n, val, ok := strings.Cut(kv, "="); ok && (n == k || runtime.GOOS == "windows" && strings.EqualFold(n, k)) {
			v, found = val, true
		}
	}
	return v, found
}

// unitEnv returns the environment of a test file's processes (docs/dynamic-layer.md §11.4).
func unitEnv(opt RunOptions, iniDir, outDir string, u Unit) []string {
	base := append(os.Environ(), opt.Env...)
	scan := iniDir
	if v, ok := lookup(base, "PHP_INI_SCAN_DIR"); ok && v != "" {
		scan += string(os.PathListSeparator) + v
	} else if opt.Probe.ScanDir != "" {
		scan += string(os.PathListSeparator) + opt.Probe.ScanDir
	}
	// The test file's own ini directory (sys_temp_dir) comes last, so that a configured sys_temp_dir does not win.
	scan += string(os.PathListSeparator) + filepath.Join(outDir, "ini")
	abs := func(rel string) string { return filepath.Join(opt.Root, filepath.FromSlash(rel)) }
	var vendors []string
	for _, v := range opt.Vendors {
		vendors = append(vendors, abs(v))
	}
	var runners []string
	if u.Vendor != "" {
		runners = []string{abs(path.Join(u.Vendor, "phpunit/phpunit")), abs(path.Join(u.Vendor, "pestphp/pest"))}
	}
	set := map[string]string{
		"PHP_INI_SCAN_DIR":  scan,
		"XDEBUG_MODE":       "coverage,develop",
		"COLUMNS":           "120",
		"LINES":             "50",
		"BGRAPH_PHP_OUT":    outDir,
		"BGRAPH_PHP_ROOT":   opt.Root,
		"BGRAPH_PHP_VENDOR": strings.Join(vendors, string(os.PathListSeparator)),
		"BGRAPH_PHP_RUNNER": strings.Join(runners, string(os.PathListSeparator)),
		// A fresh temporary and cache directory per test file: state one test file leaves there (generated caches,
		// compiled containers) must not be seen by another one while recording.
		"TMPDIR":         filepath.Join(outDir, "tmp"),
		"TEMP":           filepath.Join(outDir, "tmp"),
		"TMP":            filepath.Join(outDir, "tmp"),
		"XDG_CACHE_HOME": filepath.Join(outDir, "xdg-cache"),
	}
	var out []string
	for _, kv := range base {
		n, _, _ := strings.Cut(kv, "=")
		skip := n == "BGRAPH_PHP_PARENT" || n == "XDEBUG_CONFIG" || n == "XDEBUG_TRIGGER"
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

var (
	ansi = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")
	// PHPUnit 10+: "OK (3 tests, 5 assertions)", "Tests: 3, Assertions: 5, Failures: 1.", "No tests executed!".
	rePHPUnitOK    = regexp.MustCompile(`(?m)^OK \((\d+) tests?, \d+ assertions?\)`)
	rePHPUnitTests = regexp.MustCompile(`(?m)^Tests: (\d+), Assertions: \d+`)
	reNoTests      = regexp.MustCompile(`(?m)^No tests executed!|^\s*(?:INFO\s+)?No tests found\.`)
	// Pest: "Tests:    1 failed, 7 passed (13 assertions)".
	rePestTests = regexp.MustCompile(`(?m)^\s*Tests:\s+((?:\d+ [a-z]+(?:, )?)+)`)
	rePestPart  = regexp.MustCompile(`(\d+) [a-z]+`)
)

// testCount returns the number of tests the runner's last summary reports (-1 when there is none).
func testCount(out string) int {
	out = ansi.ReplaceAllString(out, "")
	n, best := -1, -1
	for _, re := range []*regexp.Regexp{rePHPUnitOK, rePHPUnitTests} {
		for _, m := range re.FindAllStringSubmatchIndex(out, -1) {
			if m[0] > best {
				best = m[0]
				n, _ = strconv.Atoi(out[m[2]:m[3]])
			}
		}
	}
	for _, m := range rePestTests.FindAllStringSubmatchIndex(out, -1) {
		if m[0] > best {
			best = m[0]
			n = 0
			for _, p := range rePestPart.FindAllStringSubmatch(out[m[2]:m[3]], -1) {
				x, _ := strconv.Atoi(p[1])
				n += x
			}
		}
	}
	for _, m := range reNoTests.FindAllStringIndex(out, -1) {
		if m[0] > best {
			best, n = m[0], 0
		}
	}
	return n
}

func runFile(ctx context.Context, opt RunOptions, iniDir, outDir string, f FileUnit) (o Outcome) {
	o = Outcome{Unit: f.ID(), File: f.File, Exit: -1, Tests: -1}
	t0 := time.Now()
	defer func() { o.Elapsed = time.Since(t0) }()
	argv, err := Argv(f.Unit, f.File, opt.PHP, opt.Root, filepath.Join(outDir, "cache"))
	if err != nil {
		o.Err = err.Error()
		return o
	}
	for _, d := range []string{"tmp", "xdg-cache", "ini"} {
		if err := os.MkdirAll(filepath.Join(outDir, d), 0o755); err != nil {
			o.Err = err.Error()
			return o
		}
	}
	ini := fmt.Sprintf("sys_temp_dir=\"%s\"\n", filepath.Join(outDir, "tmp"))
	if err := os.WriteFile(filepath.Join(outDir, "ini", "zz-bgraph-tmp.ini"), []byte(ini), 0o644); err != nil {
		o.Err = err.Error()
		return o
	}
	rctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, argv[0], argv[1:]...)
	cmd.Dir = filepath.Join(opt.Root, filepath.FromSlash(f.Unit.Project))
	cmd.Env = unitEnv(opt, iniDir, outDir, f.Unit)
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

// merge reads the per-process outputs of a test file and fills o (docs/dynamic-layer.md §11.4): every process must be the
// test process itself or a child the runner's own package launched directly, one output per launch.
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
	byToken := map[string]*procOut{}
	children := map[string][]*procOut{}
	roots := 0
	for i := range procs {
		p := &procs[i]
		byToken[p.Token] = p
		if p.Parent == "" {
			roots++
			if mainPid > 0 && p.Pid != mainPid {
				o.Incomplete = append(o.Incomplete, fmt.Sprintf("a process (pid %d) that is not the test process wrote output without a parent", p.Pid))
			}
			continue
		}
		children[p.Parent] = append(children[p.Parent], p)
	}
	if roots != 1 {
		o.Incomplete = append(o.Incomplete, fmt.Sprintf("%d processes wrote output as the test process (expected 1: the test process crashed, or a process lost the collector's environment)", roots))
	}
	for tok, cs := range children {
		par, ok := byToken[tok]
		if !ok {
			o.Incomplete = append(o.Incomplete, "a process whose parent wrote no output (it exited without running shutdown functions, or crashed)")
			continue
		}
		for _, c := range cs {
			if c.Ppid <= 0 || c.Ppid != par.Pid {
				o.Incomplete = append(o.Incomplete, "a PHP process was started through another program (a shell or wrapper)")
				break
			}
		}
	}
	for i := range procs {
		p := &procs[i]
		if n, c := len(p.Launches), len(children[p.Token]); n != c {
			at := ""
			if n > 0 {
				at = " (" + p.Launches[0].Fn + " at " + p.Launches[0].At + ")"
			}
			o.Incomplete = append(o.Incomplete, fmt.Sprintf("the runner started %d processes and %d wrote output%s", n, c, at))
		}
	}
	included := map[string]bool{}
	o.Lines = map[string]LineSet{}
	o.NoTable = map[string]bool{}
	o.Paths = map[string]Path{}
	for _, p := range procs {
		o.Incomplete = append(o.Incomplete, p.Incomplete...)
		for _, e := range p.Outside {
			if e.W {
				o.Written = append(o.Written, e.P)
			}
			if e.R {
				o.OutsideRead = append(o.OutsideRead, e.P)
			}
		}
		for _, f := range p.Included {
			included[f] = true
			tbl, ok := p.Coverage[f]
			if !ok {
				o.NoTable[f] = true
				continue
			}
			ls, ok := ParseLineTable(tbl)
			if !ok {
				o.Incomplete = append(o.Incomplete, "malformed line table for "+f)
				o.NoTable[f] = true
				continue
			}
			o.Lines[f] = o.Lines[f].Union(ls)
		}
		for f, e := range p.Events {
			cur := o.Paths[f]
			if cur.Kinds == nil {
				cur.Kinds = map[string]bool{}
			}
			for _, k := range e.Kinds {
				cur.Kinds[k] = true
			}
			cur.Pre = cur.Pre || e.Pre
			o.Paths[f] = cur
		}
	}
	for f := range o.NoTable {
		delete(o.Lines, f)
	}
	o.Included = keysOf(included)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ListOutcome is the runner's listing of a unit's test files.
type ListOutcome struct {
	Files   []string // absolute paths, forward slashes
	Runner  string   // "phpunit@<version>" or "pest@<version>" ("" unknown)
	Err     string
	Elapsed time.Duration
}

// List lists the test files of unit u with the embedded listing script (docs/dynamic-layer.md §11.3).
func List(ctx context.Context, opt RunOptions, u Unit) ListOutcome {
	t0 := time.Now()
	o := ListOutcome{}
	defer func() { o.Elapsed = time.Since(t0) }()
	if u.Config == "" || u.Vendor == "" {
		o.Err = "no PHPUnit XML configuration or Composer vendor directory"
		return o
	}
	dir, err := os.MkdirTemp(opt.TmpDir, "bgraph-phplist-")
	if err != nil {
		o.Err = err.Error()
		return o
	}
	defer os.RemoveAll(dir)
	script := filepath.Join(dir, "bgraph_php_list.php")
	if err := os.WriteFile(script, listSrc, 0o644); err != nil {
		o.Err = err.Error()
		return o
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	abs := func(rel string) string { return filepath.Join(opt.Root, filepath.FromSlash(rel)) }
	cmd := exec.CommandContext(ctx, opt.PHP, script, abs(path.Join(u.Vendor, "autoload.php")), abs(u.Config))
	cmd.Dir = abs(u.Project)
	cmd.Env = append(probeEnv(), opt.Env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		o.Err = fmt.Sprintf("listing the test files: %v %s", err, firstN(strings.TrimSpace(errb.String()+" "+out.String()), 300))
		return o
	}
	var res struct {
		Files   []string `json:"files"`
		PHPUnit string   `json:"phpunit"`
		Pest    string   `json:"pest"`
		Error   string   `json:"error"`
	}
	// The project's autoloaded files may print: the JSON object is the last line.
	s := strings.TrimSpace(out.String())
	if i := strings.LastIndex(s, "\n{"); i >= 0 {
		s = s[i+1:]
	}
	if err := json.Unmarshal([]byte(s), &res); err != nil {
		o.Err = "listing the test files: unexpected output " + firstN(s, 200)
		return o
	}
	if res.Error != "" {
		o.Err = "listing the test files: " + res.Error
		return o
	}
	switch u.Runner {
	case RunnerPest:
		if res.Pest != "" {
			o.Runner = "pest@" + res.Pest
		}
	default:
		if res.PHPUnit != "" {
			o.Runner = "phpunit@" + res.PHPUnit
		}
	}
	for _, f := range res.Files {
		o.Files = append(o.Files, filepath.ToSlash(f))
	}
	sort.Strings(o.Files)
	return o
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

// DefaultJobs is the default parallelism of the PHP recording.
func DefaultJobs() int { return max(1, runtime.NumCPU()/2) }

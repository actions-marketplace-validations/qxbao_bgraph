package godyn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Unit is one Go package unit whose test functions are recorded.
type Unit struct {
	ID     string // go:<dir>
	Dir    string // the package directory (repo-relative)
	Module string // the module directory the go command runs from (repo-relative)
}

// RunOptions configures Collect.
type RunOptions struct {
	Go        string // the go command
	Probe     Probe  // its probe (GOROOT, GOOS, GOMODCACHE)
	Root      string // repository root (absolute)
	TmpDir    string // where binaries and coverage directories are written (a fresh subdirectory is used)
	Timeout   time.Duration
	Jobs      int
	Env       []string // extra KEY=VALUE for the go command and the test processes
	BuildArgs []string // extra build arguments (-tags, -race...) of go list and go test -c
	// Keep reports whether a test function (sub-unit ID "go:<dir>#<Name>") is recorded (nil: every one).
	Keep func(unit, sub string) bool
	// Progress is called after each test function (serialized).
	Progress func(done, total int, o Outcome)
}

// Package is what the build of one package unit produced.
type Package struct {
	Unit  Unit
	Err   string   // go list, build or listing failure: no function of the package is recorded
	Names []string // top-level Test, Example and Fuzz functions (-test.list), in listed order
	// Files: every repository file compiled into the test binary or embedded in it, and the module files in effect (absolute
	// paths, forward slashes).
	Files []string
	// TestFiles: the package's own test files compiled into the binary (absolute; the canary).
	TestFiles []string
	// Outside: why the build depends on files that are neither in the repository nor in the module cache or GOROOT.
	Outside []string
	// Pkgs: import path → directory (absolute) of every package of the build closure inside the repository.
	Pkgs map[string]string
	// LineDirectives: a compiled repository Go file has a //line or /*line directive (generated parsers and scanners): coverage
	// then names other files and lines for its blocks. Those names are not the compiled files' own and are ignored; the body
	// guard refuses changes to any file with such a directive.
	LineDirectives bool
	bin            string
	Elapsed        time.Duration
}

// Outcome is what one isolated run of a test function produced.
type Outcome struct {
	Unit     string // the sub-unit ID
	Name     string
	Pkg      *Package
	Ran      bool // the output shows "=== RUN   <Name>"
	Skipped  bool // "--- SKIP: <Name>"
	NoTests  bool // "testing: warning: no tests to run"
	Exit     int  // exit code (-1: did not exit normally)
	TimedOut bool
	Err      string
	// Counters: counter files the run's processes wrote; MainWrote: one of them is the test process's own.
	Counters  int
	MainWrote bool
	// Started: processes started through syscall (os.StartProcess, os/exec), -1 when unknown.
	Started int
	// Lines: line tables of the covered files, by absolute path (forward slashes).
	Lines map[string]LineSet
	// Unmapped: covered file names of repository packages that name no file on disk.
	Unmapped   []string
	Incomplete []string
	Elapsed    time.Duration
	Tail       string
}

// DefaultJobs is the default parallelism of the Go recording.
func DefaultJobs() int { return max(1, runtime.NumCPU()/2) }

// abs returns the absolute path of a repo-relative path.
func (o RunOptions) abs(rel string) string { return filepath.Join(o.Root, filepath.FromSlash(rel)) }

// pkgArg returns the package argument of unit u for the go command run from its module directory.
func pkgArg(u Unit) string {
	if u.Dir == u.Module {
		return "."
	}
	rel := u.Dir
	if u.Module != "." {
		rel = strings.TrimPrefix(u.Dir, u.Module+"/")
	}
	return "./" + rel
}

// goEnv returns the environment of the go command and of the test processes, without GOCOVERDIR and GOFLAGS entries that a
// recording sets itself.
func goEnv(extra []string) []string {
	var out []string
	for _, kv := range append(os.Environ(), extra...) {
		k, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "GOCOVERDIR") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// listPkg is the part of `go list -json` the collector reads.
type listPkg struct {
	ImportPath      string
	Dir             string
	ForTest         string
	Standard        bool
	Goroot          bool
	GoFiles         []string
	CgoFiles        []string
	CompiledGoFiles []string
	CFiles          []string
	CXXFiles        []string
	MFiles          []string
	HFiles          []string
	FFiles          []string
	SFiles          []string
	SwigFiles       []string
	SwigCXXFiles    []string
	SysoFiles       []string
	EmbedFiles      []string
	TestGoFiles     []string
	XTestGoFiles    []string
	TestEmbedFiles  []string
	XTestEmbedFiles []string
	Module          *struct {
		Path  string
		Dir   string
		GoMod string
	}
	Error *struct {
		Err string
	}
	DepsErrors []struct {
		Err string
	}
}

// within reports whether absolute path p (forward slashes) lies in directory dir (forward slashes), ignoring case on Windows.
func within(p, dir string) bool {
	if dir == "" {
		return false
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		p, dir = strings.ToLower(p), strings.ToLower(dir)
	}
	dir = strings.TrimSuffix(dir, "/")
	return p == dir || strings.HasPrefix(p, dir+"/")
}

func slash(p string) string { return filepath.ToSlash(filepath.Clean(p)) }

// run runs the go command with args from dir and returns its standard output.
func runGo(ctx context.Context, opt RunOptions, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, opt.Go, args...)
	cmd.Dir = dir
	cmd.Env = goEnv(opt.Env)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%v: %s", err, firstN(strings.TrimSpace(errb.String()+" "+out.String()), 600))
	}
	return out.Bytes(), nil
}

// listDeps runs `go list -deps -test -json` for u and fills p's files, packages and outside-module reasons. It returns the
// -coverpkg patterns: every module of the closure whose directory is in the repository, outside vendor directories.
func listDeps(ctx context.Context, opt RunOptions, p *Package) ([]string, error) {
	u := p.Unit
	args := append([]string{"list", "-deps", "-test", "-json"}, opt.BuildArgs...)
	args = append(args, pkgArg(u))
	out, err := runGo(ctx, opt, opt.abs(u.Module), args...)
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	root := slash(opt.Root)
	if ev, err := filepath.EvalSymlinks(opt.Root); err == nil && slash(ev) != root {
		root = slash(ev)
	}
	inRepo := func(a string) bool { return within(slash(a), slash(opt.Root)) || within(slash(a), root) }
	unitDir := slash(opt.abs(u.Dir))
	modcache := slash(opt.Probe.Get("GOMODCACHE"))
	goroot := slash(opt.Probe.Get("GOROOT"))
	files := map[string]bool{}
	mods := map[string]bool{}
	modDirs := map[string]bool{}
	p.Pkgs = map[string]string{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var lp listPkg
		if err := dec.Decode(&lp); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("go list: unreadable output: %v", err)
		}
		if lp.Error != nil {
			return nil, fmt.Errorf("go list: %s: %s", lp.ImportPath, firstN(lp.Error.Err, 300))
		}
		for _, e := range lp.DepsErrors {
			return nil, fmt.Errorf("go list: %s: %s", lp.ImportPath, firstN(e.Err, 300))
		}
		if lp.Standard || lp.Goroot {
			continue
		}
		dir := slash(lp.Dir)
		if lp.Module != nil && lp.Module.Dir != "" {
			md := slash(lp.Module.Dir)
			switch {
			case inRepo(md):
				if !strings.Contains("/"+md+"/", "/vendor/") {
					mods[lp.Module.Path] = true
					modDirs[md] = true
				}
				if lp.Module.GoMod != "" {
					files[slash(lp.Module.GoMod)] = true
				}
			case modcache != "" && within(md, modcache), goroot != "" && within(md, goroot):
			default:
				p.Outside = append(p.Outside, "builds module "+lp.Module.Path+" from a directory outside the repository and the module cache: "+md)
			}
		}
		if !inRepo(dir) {
			continue
		}
		if ip, _, _ := strings.Cut(lp.ImportPath, " "); !strings.HasSuffix(ip, ".test") {
			p.Pkgs[ip] = dir
		}
		lists := [][]string{lp.GoFiles, lp.CgoFiles, lp.CompiledGoFiles, lp.CFiles, lp.CXXFiles, lp.MFiles, lp.HFiles, lp.FFiles,
			lp.SFiles, lp.SwigFiles, lp.SwigCXXFiles, lp.SysoFiles, lp.EmbedFiles}
		for _, l := range [][]string{lp.GoFiles, lp.CgoFiles} {
			for _, f := range l {
				if !p.LineDirectives && !filepath.IsAbs(f) {
					if b, err := os.ReadFile(filepath.Join(lp.Dir, f)); err == nil &&
						(bytes.Contains(b, []byte("//line ")) || bytes.Contains(b, []byte("/*line "))) {
						p.LineDirectives = true
					}
				}
			}
		}
		own := within(dir, unitDir) && within(unitDir, dir)
		if own {
			lists = append(lists, lp.TestGoFiles, lp.XTestGoFiles, lp.TestEmbedFiles, lp.XTestEmbedFiles)
		}
		for _, l := range lists {
			for _, f := range l {
				a := f
				if !filepath.IsAbs(a) {
					a = filepath.Join(lp.Dir, f)
				}
				a = slash(a)
				if !inRepo(a) {
					continue // the go command's own generated files (build cache)
				}
				files[a] = true
			}
		}
		if own {
			for _, l := range [][]string{lp.TestGoFiles, lp.XTestGoFiles} {
				for _, f := range l {
					p.TestFiles = append(p.TestFiles, slash(filepath.Join(lp.Dir, f)))
				}
			}
		}
	}
	// Module files in effect: go.sum beside every go.mod of the repository modules, the workspace file, vendor/modules.txt.
	for md := range modDirs {
		for _, n := range []string{"go.mod", "go.sum", "vendor/modules.txt"} {
			a := md + "/" + n
			if _, err := os.Stat(filepath.FromSlash(a)); err == nil {
				files[a] = true
			}
		}
	}
	if w, err := runGo(ctx, opt, opt.abs(u.Module), "env", "GOWORK"); err == nil {
		if gw := strings.TrimSpace(string(w)); gw != "" && gw != "off" {
			gw = slash(gw)
			if inRepo(gw) {
				files[gw] = true
				if _, err := os.Stat(filepath.FromSlash(gw + ".sum")); err == nil {
					files[gw+".sum"] = true
				}
				// A workspace's vendor directory applies to its modules.
				if _, err := os.Stat(filepath.FromSlash(path.Dir(gw) + "/vendor/modules.txt")); err == nil {
					files[path.Dir(gw)+"/vendor/modules.txt"] = true
				}
			} else {
				p.Outside = append(p.Outside, "builds with a go.work outside the repository: "+gw)
			}
		}
	} else {
		return nil, fmt.Errorf("go env GOWORK: %w", err)
	}
	for f := range files {
		p.Files = append(p.Files, f)
	}
	sort.Strings(p.Files)
	sort.Strings(p.TestFiles)
	p.TestFiles = slices.Compact(p.TestFiles)
	var pats []string
	for m := range mods {
		pats = append(pats, m+"/...")
	}
	sort.Strings(pats)
	return pats, nil
}

// build builds the test binary of p's unit with coverage of the repository's modules and of os and syscall (process counting).
func build(ctx context.Context, opt RunOptions, dir string, i int, p *Package) {
	t0 := time.Now()
	defer func() { p.Elapsed = time.Since(t0) }()
	pats, err := listDeps(ctx, opt, p)
	if err != nil {
		p.Err = err.Error()
		return
	}
	if len(pats) == 0 {
		p.Err = "no repository module in the build closure"
		return
	}
	bin := filepath.Join(dir, fmt.Sprintf("pkg-%d.test", i))
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	args := []string{"test", "-c", "-cover", "-covermode=atomic", "-coverpkg=" + strings.Join(append(pats, "os", "syscall"), ","), "-o", bin}
	args = append(args, opt.BuildArgs...)
	args = append(args, pkgArg(p.Unit))
	if _, err := runGo(ctx, opt, opt.abs(p.Unit.Module), args...); err != nil {
		p.Err = "go test -c: " + err.Error()
		return
	}
	if _, err := os.Stat(bin); err != nil {
		p.Err = "go test -c wrote no test binary (no test file for this build configuration)"
		return
	}
	p.bin = bin
	lctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(lctx, bin, "-test.list", ".*")
	cmd.Dir = opt.abs(p.Unit.Dir)
	cmd.Env = goEnv(opt.Env)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		p.Err = fmt.Sprintf("-test.list: %v %s", err, firstN(strings.TrimSpace(errb.String()+" "+out.String()), 300))
		return
	}
	for _, l := range strings.Split(out.String(), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.ContainsAny(l, " \t") {
			continue
		}
		if strings.HasPrefix(l, "Test") || strings.HasPrefix(l, "Example") || strings.HasPrefix(l, "Fuzz") {
			p.Names = append(p.Names, l)
		}
	}
	slices.Sort(p.Names)
	p.Names = slices.Compact(p.Names)
}

// Collect builds every unit's test binary, lists its test functions, then runs each kept function alone in a process tree of
// its own.
// It returns the packages (in the order of units) and the outcomes of the functions (by package, then name).
func Collect(ctx context.Context, opt RunOptions, units []Unit) ([]*Package, []Outcome, error) {
	if why := CheckFlags(opt.BuildArgs, GOFLAGS(opt.Env)); why != "" {
		return nil, nil, errors.New(why)
	}
	dir, err := os.MkdirTemp(opt.TmpDir, "bgraph-gorecord-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	jobs := max(1, opt.Jobs)
	if opt.Timeout <= 0 {
		opt.Timeout = 10 * time.Minute
	}
	pkgs := make([]*Package, len(units))
	for i, u := range units {
		pkgs[i] = &Package{Unit: u}
	}
	parallel(ctx, jobs, len(units), func(i int) { build(ctx, opt, dir, i, pkgs[i]) })
	if err := ctx.Err(); err != nil {
		return pkgs, nil, err
	}
	covdata := covdataTool(ctx, opt)
	spawn := &spawnLocator{goroot: opt.Probe.Get("GOROOT"), goos: opt.Probe.Get("GOOS")}
	type job struct {
		p    *Package
		name string
	}
	var jobsList []job
	left := map[*Package]int{}
	for _, p := range pkgs {
		if p.Err != "" {
			continue
		}
		for _, n := range p.Names {
			if opt.Keep != nil && !opt.Keep(p.Unit.ID, SubID(p.Unit.ID, n)) {
				continue
			}
			jobsList = append(jobsList, job{p, n})
			left[p]++
		}
		if left[p] == 0 {
			os.Remove(p.bin)
		}
	}
	outs := make([]Outcome, len(jobsList))
	var mu sync.Mutex
	done := 0
	parallel(ctx, jobs, len(jobsList), func(i int) {
		j := jobsList[i]
		cd := filepath.Join(dir, fmt.Sprintf("cov-%d", i))
		o := runFunc(ctx, opt, covdata, spawn, cd, j.p, j.name)
		os.RemoveAll(cd)
		mu.Lock()
		defer mu.Unlock()
		outs[i] = o
		done++
		if left[j.p]--; left[j.p] == 0 {
			os.Remove(j.p.bin)
		}
		if opt.Progress != nil {
			opt.Progress(done, len(jobsList), o)
		}
	})
	return pkgs, outs, ctx.Err()
}

// parallel runs f(0..n-1) on jobs goroutines, stopping to hand out work when ctx is done.
func parallel(ctx context.Context, jobs, n int, f func(int)) {
	idx := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				f(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		idx <- i
	}
	close(idx)
	wg.Wait()
}

// covdataTool returns the argv prefix that runs `go tool covdata`: the tool binary itself when `go tool -n` names it.
func covdataTool(ctx context.Context, opt RunOptions) []string {
	out, err := runGo(ctx, opt, opt.Root, "tool", "-n", "covdata")
	if p := strings.TrimSpace(string(out)); err == nil && p != "" && !strings.ContainsAny(p, "\n") {
		if _, err := os.Stat(p); err == nil {
			return []string{p}
		}
	}
	return []string{opt.Go, "tool", "covdata"}
}

// markWriter keeps the tail of a test process's output and notes the lines that say whether the test function ran.
type markWriter struct {
	tail    tailBuffer
	line    []byte
	name    string
	ran     bool
	skipped bool
	noTests bool
	timeout bool
}

func (m *markWriter) Write(p []byte) (int, error) {
	m.tail.Write(p)
	for _, c := range p {
		if c != '\n' {
			if len(m.line) < 4096 {
				m.line = append(m.line, c)
			}
			continue
		}
		m.check(strings.TrimRight(string(m.line), "\r"))
		m.line = m.line[:0]
	}
	return len(p), nil
}

func (m *markWriter) check(l string) {
	switch {
	case l == "=== RUN   "+m.name:
		m.ran = true
	case strings.HasPrefix(l, "--- SKIP: "+m.name+" "):
		m.skipped = true
	case strings.HasPrefix(l, "testing: warning: no tests to run"):
		m.noTests = true
	case strings.HasPrefix(l, "panic: test timed out after "):
		m.timeout = true
	}
}

func (m *markWriter) flush() {
	if len(m.line) > 0 {
		m.check(strings.TrimRight(string(m.line), "\r"))
		m.line = nil
	}
}

func runFunc(ctx context.Context, opt RunOptions, covdata []string, spawn *spawnLocator, covDir string, p *Package, name string) (o Outcome) {
	o = Outcome{Unit: SubID(p.Unit.ID, name), Name: name, Pkg: p, Exit: -1, Started: -1}
	t0 := time.Now()
	defer func() { o.Elapsed = time.Since(t0) }()
	if err := os.MkdirAll(covDir, 0o755); err != nil {
		o.Err = err.Error()
		return o
	}
	rctx, cancel := context.WithTimeout(ctx, opt.Timeout+time.Minute)
	defer cancel()
	cmd := exec.CommandContext(rctx, p.bin, "-test.run=^"+name+"$", "-test.count=1", "-test.v=true", "-test.paniconexit0",
		"-test.gocoverdir="+covDir, "-test.timeout="+opt.Timeout.String())
	cmd.Dir = opt.abs(p.Unit.Dir)
	cmd.Env = append(goEnv(opt.Env), "GOCOVERDIR="+covDir)
	mw := &markWriter{name: name}
	cmd.Stdout, cmd.Stderr = mw, mw
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	mw.flush()
	o.Tail = mw.tail.String()
	o.Ran, o.Skipped, o.NoTests = mw.ran, mw.skipped, mw.noTests
	if mw.timeout || rctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
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
	ents, err := os.ReadDir(covDir)
	if err != nil {
		o.Err = "coverage directory: " + err.Error()
		return o
	}
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "covcounters.") {
			continue
		}
		o.Counters++
		// covcounters.<meta hash>.<pid>.<time>
		if parts := strings.Split(n, "."); len(parts) >= 4 && parts[2] == strconv.Itoa(mainPid) {
			o.MainWrote = true
		}
	}
	if o.Counters == 0 {
		return o
	}
	prof := filepath.Join(covDir, "profile.txt")
	args := append(slices.Clone(covdata[1:]), "textfmt", "-i="+covDir, "-o="+prof)
	tctx, tcancel := context.WithTimeout(ctx, 5*time.Minute)
	defer tcancel()
	tc := exec.CommandContext(tctx, covdata[0], args...)
	tc.Env = goEnv(opt.Env)
	if out, err := tc.CombinedOutput(); err != nil {
		o.Incomplete = append(o.Incomplete, "go tool covdata textfmt: "+firstN(strings.TrimSpace(err.Error()+" "+string(out)), 300))
		return o
	}
	f, err := os.Open(prof)
	if err != nil {
		o.Incomplete = append(o.Incomplete, "no coverage profile: "+err.Error())
		return o
	}
	defer f.Close()
	readProfile(f, &o, spawn)
	return o
}

// block is one line of a text coverage profile.
type block struct {
	name                   string
	sl, sc, el, ec, n, cnt int
}

func parseBlock(l string) (block, bool) {
	i := strings.LastIndexByte(l, ':')
	if i <= 0 {
		return block{}, false
	}
	b := block{name: l[:i]}
	var err error
	rest := strings.Fields(l[i+1:])
	if len(rest) != 3 {
		return block{}, false
	}
	pos := strings.FieldsFunc(rest[0], func(r rune) bool { return r == '.' || r == ',' })
	if len(pos) != 4 {
		return block{}, false
	}
	nums := []*int{&b.sl, &b.sc, &b.el, &b.ec}
	for k, s := range pos {
		if *nums[k], err = strconv.Atoi(s); err != nil {
			return block{}, false
		}
	}
	if b.n, err = strconv.Atoi(rest[1]); err != nil {
		return block{}, false
	}
	if b.cnt, err = strconv.Atoi(rest[2]); err != nil {
		return block{}, false
	}
	return b, b.sl >= 0 && b.el >= 0
}

// readProfile reads a text profile into o: line tables of the repository files, the number of started processes.
func readProfile(r io.Reader, o *Outcome, spawn *spawnLocator) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	o.Lines = map[string]LineSet{}
	var sys []block
	unmapped := map[string]bool{}
	first := true
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if first {
			first = false
			if strings.HasPrefix(l, "mode:") {
				continue
			}
		}
		if l == "" {
			continue
		}
		b, ok := parseBlock(l)
		if !ok {
			o.Incomplete = append(o.Incomplete, "malformed coverage profile line "+firstN(l, 100))
			return
		}
		if strings.HasPrefix(b.name, "syscall/") {
			sys = append(sys, b)
			continue
		}
		abs, inRepo := o.Pkg.fileOf(b.name)
		if !inRepo {
			continue
		}
		if abs == "" || b.sl <= 0 || b.el < b.sl {
			// A name or position from a //line directive, or a file generated at build time.
			if !o.Pkg.LineDirectives {
				unmapped[b.name] = true
			}
			continue
		}
		ls := o.Lines[abs]
		if ls == nil {
			ls = LineSet{}
			o.Lines[abs] = ls
		}
		for k := b.sl; k <= b.el; k++ {
			ls[k] = ls[k] || b.cnt > 0
		}
	}
	if err := sc.Err(); err != nil {
		o.Incomplete = append(o.Incomplete, "unreadable coverage profile: "+err.Error())
		return
	}
	for n := range unmapped {
		o.Unmapped = append(o.Unmapped, n)
	}
	sort.Strings(o.Unmapped)
	o.Started = spawn.count(sys)
}

// fileOf maps a profile file name ("<import path>/<file>.go", or an absolute path from a //line directive) to an absolute path
// (forward slashes). inRepo is false for packages outside the repository; abs is "" for a repository package's name that names
// no file on disk.
func (p *Package) fileOf(name string) (abs string, inRepo bool) {
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		a := slash(name)
		for _, d := range p.Pkgs {
			if within(a, d) {
				if st, err := os.Stat(filepath.FromSlash(a)); err == nil && !st.IsDir() {
					return a, true
				}
				return "", true
			}
		}
		return "", false
	}
	i := strings.LastIndexByte(name, '/')
	if i < 0 {
		return "", false
	}
	d, ok := p.Pkgs[name[:i]]
	if !ok {
		return "", false
	}
	a := d + "/" + name[i+1:]
	if st, err := os.Stat(filepath.FromSlash(a)); err != nil || st.IsDir() {
		return "", true
	}
	return a, true
}

// spawnLocator finds the entry block of the syscall function every process start goes through (syscall.StartProcess on
// Windows, js and wasip1; syscall.forkExec, behind StartProcess and ForkExec, elsewhere) in the toolchain's sources.
type spawnLocator struct {
	goroot, goos string
	mu           sync.Mutex
	funcs        map[string][2]token.Position // syscall file → body span of the function ({} when absent)
}

func (s *spawnLocator) funcName() string {
	switch s.goos {
	case "windows", "js", "wasip1":
		return "StartProcess"
	}
	return "forkExec"
}

// span returns the body span of the spawn function in syscall file name ("syscall/x.go"), ok false when the file does not
// define it.
func (s *spawnLocator) span(name string) ([2]token.Position, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sp, ok := s.funcs[name]; ok {
		return sp, sp[0].Line > 0
	}
	if s.funcs == nil {
		s.funcs = map[string][2]token.Position{}
	}
	var sp [2]token.Position
	src := filepath.Join(s.goroot, "src", filepath.FromSlash(name))
	fset := token.NewFileSet()
	if f, err := parser.ParseFile(fset, src, nil, parser.SkipObjectResolution); err == nil && s.goroot != "" {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == s.funcName() && fd.Body != nil {
				sp = [2]token.Position{fset.Position(fd.Body.Lbrace), fset.Position(fd.Body.Rbrace)}
			}
		}
	}
	s.funcs[name] = sp
	return sp, sp[0].Line > 0
}

// count returns how many times the spawn function was entered, from the syscall blocks of a profile (-1: not found).
func (s *spawnLocator) count(sys []block) int {
	before := func(l1, c1, l2, c2 int) bool { return l1 < l2 || l1 == l2 && c1 < c2 }
	found := false
	var entry block
	for _, b := range sys {
		sp, ok := s.span(b.name)
		if !ok {
			continue
		}
		if before(b.sl, b.sc, sp[0].Line, sp[0].Column) || before(sp[1].Line, sp[1].Column, b.sl, b.sc) {
			continue
		}
		if !found || before(b.sl, b.sc, entry.sl, entry.sc) {
			entry, found = b, true
		}
	}
	if !found {
		return -1
	}
	return entry.cnt
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

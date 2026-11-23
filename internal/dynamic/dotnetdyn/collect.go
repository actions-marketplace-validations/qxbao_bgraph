package dotnetdyn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultJobs is the default number of test hosts run in parallel.
func DefaultJobs() int { return max(1, min(4, runtime.NumCPU()/2)) }

// Unit is one .NET test project to record.
type Unit struct {
	ID      string // unit ID ("dotnet:<project file>")
	Project string // repo-relative project file
	Dir     string // repo-relative project directory ("." for the root)
	MTP     bool   // global.json selects Microsoft.Testing.Platform (the project may also opt in itself; see evaluate)
}

// RunOptions configures Run.
type RunOptions struct {
	Root      string // repository root (absolute)
	TmpDir    string // where reports and run settings are written
	Jobs      int
	Timeout   time.Duration // per test host; <= 0: 10 minutes
	Env       []string      // extra environment variables for every dotnet process
	BuildArgs []string      // extra arguments of `dotnet build` and `dotnet test` (e.g. -c Release)
	// Progress is called after each class run (done counts class × framework runs).
	Progress func(done, total int, r ClassRun)
}

// ClassRun is one recording run: one test class on one target framework, in a test host of its own, with coverage.
type ClassRun struct {
	Class, Framework string
	Coverage         *Coverage // nil when no report was produced
	Outcome          TestOutcome
	Reported         bool   // the TRX report lists the class
	Why              string // why the run is unusable (no report, exit without TRX...)
	Elapsed          time.Duration
	Tail             string
}

// Outcome is everything a recording of one project produced.
type Outcome struct {
	Unit       Unit
	Frameworks []string
	// Why: the project could not be recorded at all (evaluation or build failed); no class record is made.
	Why string
	// Unrecorded: frameworks of the project that could not be run (the class list cannot be trusted: a class may exist only there).
	Unrecorded map[string]string
	// Classes: union over the frameworks of the classes the full runs reported, with their outcome in the full runs.
	Classes map[string]TestOutcome
	// On: class → the frameworks whose full run reported it (a class can exist on some frameworks only, under #if).
	On      map[string][]string
	Runs    []ClassRun
	Elapsed time.Duration
}

// runSettings makes Microsoft.CodeCoverage write Cobertura, instrument the test assembly too and keep auto-properties; the default
// exclusion lists are not merged in (newer versions honour it; older ones still drop [ExcludeFromCodeCoverage] code, which the
// guard handles by requiring the changed member to be instrumented). Instrumentation is dynamic only: static instrumentation
// rewrites the assemblies on disk, which parallel hosts share; where dynamic instrumentation is unavailable the reports come
// out empty and the canary makes the records incomplete. xUnit theories are not pre-enumerated: discovery would run
// the member data of every class of the assembly in each host (extra dependencies); a class's own data still runs when it
// executes.
const runSettings = `<?xml version="1.0" encoding="utf-8"?>
<RunSettings>
  <DataCollectionRunSettings>
    <DataCollectors>
      <DataCollector friendlyName="Code Coverage">
        <Configuration>
          <Format>cobertura</Format>
          <CodeCoverage>
            <Attributes><Exclude mergeDefaults="False"><Attribute>^Bgraph\.NeverMatches$</Attribute></Exclude></Attributes>
            <Sources><Exclude mergeDefaults="False"><Source>^$</Source></Exclude></Sources>
            <CompanyNames><Exclude mergeDefaults="False"><CompanyName>^$</CompanyName></Exclude></CompanyNames>
            <PublicKeyTokens><Exclude mergeDefaults="False"><PublicKeyToken>^$</PublicKeyToken></Exclude></PublicKeyTokens>
            <IncludeTestAssembly>True</IncludeTestAssembly>
            <SkipAutoProperties>False</SkipAutoProperties>
            <CollectFromChildProcesses>True</CollectFromChildProcesses>
            <EnableStaticManagedInstrumentation>False</EnableStaticManagedInstrumentation>
            <EnableDynamicManagedInstrumentation>True</EnableDynamicManagedInstrumentation>
          </CodeCoverage>
        </Configuration>
      </DataCollector>
    </DataCollectors>
  </DataCollectionRunSettings>
  <xUnit>
    <PreEnumerateTheories>false</PreEnumerateTheories>
  </xUnit>
</RunSettings>
`

// EscapeFilter escapes the characters that are operators in a VSTest --filter value.
func EscapeFilter(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\()|&=!~`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ClassFilter is the VSTest filter that runs exactly the tests of top-level class c and its nested classes.
func ClassFilter(c string) string {
	e := EscapeFilter(c)
	return "FullyQualifiedName~" + e + ".|FullyQualifiedName~" + e + "+"
}

func (o RunOptions) env() []string {
	return append(os.Environ(), append([]string{"DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1", "MSBUILDDISABLENODEREUSE=1",
		"DOTNET_CLI_UI_LANGUAGE=en", "VSTEST_TESTHOST_SHUTDOWN_TIMEOUT=60000"}, o.Env...)...)
}

// dotnet runs one dotnet command in the repository root and returns its combined output.
func (o RunOptions) dotnet(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(c, "dotnet", args...)
	cmd.Dir = o.Root
	cmd.Env = o.env()
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	// dotnet test starts vstest.console, the data collector and the test host, which inherit the output pipes: on a
	// timeout the whole tree is killed (Windows), and Wait gives up on the pipes after a delay.
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	if c.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out after %s", timeout)
	}
	return buf.String(), err
}

func (o RunOptions) timeout() time.Duration {
	if o.Timeout <= 0 {
		return 10 * time.Minute
	}
	return o.Timeout
}

// ErrNotTestProject is returned by Frameworks for a project that is not a test project (IsTestProject is not true).
var ErrNotTestProject = errors.New("not a test project")

// Frameworks evaluates the target frameworks of a test project (`dotnet msbuild -getProperty`).
func (o RunOptions) Frameworks(ctx context.Context, project string) ([]string, error) {
	ev, err := o.evaluate(ctx, project)
	return ev.fws, err
}

type evaluation struct {
	fws         []string
	runSettings string // RunSettingsFilePath
	mtp         bool   // the project opts in to Microsoft.Testing.Platform (UseMicrosoftTestingPlatformRunner, IsTestingPlatformApplication)
}

// evaluate restores a test project and evaluates its target frameworks, RunSettingsFilePath and test platform.
func (o RunOptions) evaluate(ctx context.Context, project string) (evaluation, error) {
	// IsTestProject comes from Microsoft.NET.Test.Sdk, whose props are only imported once the project is restored.
	if out, err := o.dotnet(ctx, 10*time.Minute, append([]string{"restore", project}, o.BuildArgs...)...); err != nil {
		return evaluation{}, fmt.Errorf("restore %s: %v: %s", project, err, tail(out, 6))
	}
	args := append([]string{"msbuild", project, "-getProperty:TargetFrameworks", "-getProperty:TargetFramework",
		"-getProperty:IsTestProject", "-getProperty:RunSettingsFilePath", "-getProperty:UseMicrosoftTestingPlatformRunner",
		"-getProperty:IsTestingPlatformApplication"}, o.BuildArgs...)
	out, err := o.dotnet(ctx, 5*time.Minute, args...)
	if err != nil {
		return evaluation{}, fmt.Errorf("evaluate %s: %v: %s", project, err, tail(out, 6))
	}
	var v struct{ Properties map[string]string }
	i := strings.IndexByte(out, '{')
	if i < 0 {
		return evaluation{}, fmt.Errorf("evaluate %s: no properties in the output: %s", project, tail(out, 4))
	}
	if err := json.Unmarshal([]byte(out[i:]), &v); err != nil {
		return evaluation{}, fmt.Errorf("evaluate %s: %v", project, err)
	}
	isTrue := func(k string) bool { return strings.EqualFold(strings.TrimSpace(v.Properties[k]), "true") }
	// Microsoft.Testing.Platform applications without Microsoft.NET.Test.Sdk may leave IsTestProject to the per-framework
	// evaluations (xUnit v3): IsTestingPlatformApplication says it too.
	if !isTrue("IsTestProject") && !isTrue("IsTestingPlatformApplication") {
		return evaluation{}, ErrNotTestProject
	}
	var fws []string
	for _, f := range strings.Split(v.Properties["TargetFrameworks"], ";") {
		if f = strings.TrimSpace(f); f != "" && !slices.Contains(fws, f) {
			fws = append(fws, f)
		}
	}
	if len(fws) == 0 {
		if f := strings.TrimSpace(v.Properties["TargetFramework"]); f != "" {
			fws = []string{f}
		}
	}
	if len(fws) == 0 {
		return evaluation{}, fmt.Errorf("evaluate %s: no target framework", project)
	}
	return evaluation{fws: fws, runSettings: strings.TrimSpace(v.Properties["RunSettingsFilePath"]),
		mtp: isTrue("UseMicrosoftTestingPlatformRunner") || isTrue("IsTestingPlatformApplication")}, nil
}

// Run records one test project: evaluate its frameworks, build it once, run the whole suite per framework (the class list), then
// every class per framework in a test host of its own with coverage, opt.Jobs at a time.
func Run(ctx context.Context, opt RunOptions, u Unit) (Outcome, error) {
	t0 := time.Now()
	out := Outcome{Unit: u, Unrecorded: map[string]string{}, Classes: map[string]TestOutcome{}, On: map[string][]string{}}
	defer func() { out.Elapsed = time.Since(t0) }()
	ev, err := opt.evaluate(ctx, u.Project)
	fws, settingsFile := ev.fws, ev.runSettings
	mtp := u.MTP || ev.mtp
	if errors.Is(err, ErrNotTestProject) {
		return out, err
	}
	if err != nil {
		out.Why = err.Error()
		return out, nil
	}
	out.Frameworks = fws
	if settingsFile != "" {
		// The recording's --settings replaces the project's run settings (environment variables, test run parameters): the
		// records would describe other runs than the narrowed ones.
		out.Why = "the project sets RunSettingsFilePath (" + settingsFile + "), which the recording would replace"
		return out, nil
	}
	work, err := os.MkdirTemp(opt.TmpDir, "dotnetdyn-")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(work)
	settings := filepath.Join(work, "bgraph.runsettings")
	content := runSettings
	if mtp {
		settings, content = filepath.Join(work, "bgraph.coverage.xml"), coverageSettings
	}
	if err := os.WriteFile(settings, []byte(content), 0o644); err != nil {
		return out, err
	}
	if o, err := opt.dotnet(ctx, opt.timeout()*3, append([]string{"build", u.Project, "-nologo"}, opt.BuildArgs...)...); err != nil {
		out.Why = fmt.Sprintf("build: %v: %s", err, tail(o, 8))
		return out, nil
	}
	var apps map[string]mtpApp
	var caps mtpCaps
	if mtp {
		if apps, err = opt.mtpApps(ctx, u.Project, fws); err == nil {
			caps, err = opt.probe(ctx, apps[fws[0]])
		}
		if err != nil {
			out.Why = "Microsoft.Testing.Platform: " + err.Error()
			return out, nil
		}
	}
	for _, fw := range fws {
		dir := filepath.Join(work, "list-"+safeName(fw))
		var res map[string]TestOutcome
		var o string
		if mtp {
			res, o, err = opt.mtpList(ctx, apps[fw], caps, dir)
		} else {
			args := append([]string{"test", u.Project, "--no-build", "-p:CollectCoverage=false", "-f", fw, "--logger", "trx;LogFileName=list.trx", "--results-directory", dir},
				opt.BuildArgs...)
			o, _ = opt.dotnet(ctx, opt.timeout()*3, args...)
			res, err = ParseTRXFile(filepath.Join(dir, "list.trx"))
		}
		if err != nil {
			out.Unrecorded[fw] = "the whole suite produced " + trxProblem(err) + ": " + tail(o, 6)
			continue
		}
		for c, r := range res {
			cur := out.Classes[c]
			cur.Tests += r.Tests
			cur.Failed += r.Failed
			out.Classes[c] = cur
			out.On[c] = append(out.On[c], fw)
		}
	}
	type job struct{ class, fw string }
	var jobs []job
	classes := make([]string, 0, len(out.Classes))
	for c := range out.Classes {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	for _, c := range classes {
		for _, fw := range out.On[c] {
			jobs = append(jobs, job{c, fw})
		}
	}
	n := opt.Jobs
	if n <= 0 {
		n = DefaultJobs()
	}
	runs := make([]ClassRun, len(jobs))
	var mu sync.Mutex
	done := 0
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				dir := filepath.Join(work, fmt.Sprintf("c%05d", i))
				if mtp {
					runs[i] = opt.mtpClass(ctx, apps[jobs[i].fw], caps, jobs[i].class, jobs[i].fw, settings, dir)
				} else {
					runs[i] = opt.runClass(ctx, u, jobs[i].class, jobs[i].fw, settings, dir)
				}
				if opt.Progress != nil {
					mu.Lock()
					done++
					opt.Progress(done, len(jobs), runs[i])
					mu.Unlock()
				}
			}
		}()
	}
	for i := range jobs {
		if ctx.Err() != nil {
			break
		}
		next <- i
	}
	close(next)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return out, err
	}
	out.Runs = runs
	return out, nil
}

// runClass runs one class on one framework with coverage. -p:CollectCoverage=false switches off coverlet.msbuild, which some
// repositories enable for every test run: it rewrites the assemblies on disk (racing between parallel hosts) and would stack a
// second instrumentation.
func (o RunOptions) runClass(ctx context.Context, u Unit, class, fw, settings, dir string) ClassRun {
	t0 := time.Now()
	r := ClassRun{Class: class, Framework: fw}
	args := append([]string{"test", u.Project, "--no-build", "-p:CollectCoverage=false", "-f", fw, "--filter", ClassFilter(class), "--collect", "Code Coverage",
		"--settings", settings, "--logger", "trx;LogFileName=run.trx", "--results-directory", dir}, o.BuildArgs...)
	out, err := o.dotnet(ctx, o.timeout(), args...)
	r.Elapsed = time.Since(t0)
	r.Tail = tail(out, 8)
	res, terr := ParseTRXFile(filepath.Join(dir, "run.trx"))
	if terr != nil {
		r.Why = trxProblem(terr)
		if err != nil {
			r.Why += " (" + err.Error() + ")"
		}
		return r
	}
	r.Outcome, r.Reported = res[class]
	covs, _ := filepath.Glob(filepath.Join(dir, "*", "*.cobertura.xml"))
	switch len(covs) {
	case 0:
		r.Why = "no coverage report (is Microsoft.NET.Test.Sdk referenced, and is the CLR profiler slot free?)"
	case 1:
		c, err := ParseCoberturaFile(covs[0])
		if err != nil {
			r.Why = err.Error()
		} else {
			r.Coverage = &c
		}
	default:
		r.Why = fmt.Sprintf("%d coverage reports for one run", len(covs))
	}
	if err != nil && r.Why == "" && !errors.As(err, new(*exec.ExitError)) {
		r.Why = err.Error() // a failing test exits non-zero: that is an outcome, not a broken run
	}
	return r
}

// trxProblem describes a TRX report that could not be used, without the temporary path.
func trxProblem(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "no TRX report"
	}
	return "no complete TRX report (" + err.Error() + ")"
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == ' ' {
			return '_'
		}
		return r
	}, s)
}

func tail(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, " | ")
}

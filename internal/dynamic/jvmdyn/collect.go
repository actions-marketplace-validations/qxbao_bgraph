package jvmdyn

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/eco/jvm"
)

// DefaultJobs is the default number of parallel test JVMs of a recording (forkCount / maxParallelForks).
func DefaultJobs() int { return max(1, min(4, runtime.NumCPU()/2)) }

// RunOptions configures one recording session (docs/dynamic-layer.md §9.4).
type RunOptions struct {
	Root      string // repository root (absolute)
	TmpDir    string // where the logs are written (a fresh subdirectory is used)
	JavaHome  string
	Agent     string // agent jar (BuildAgent)
	Jobs      int    // parallel test JVMs
	Timeout   time.Duration
	Env       []string // extra environment (KEY=VALUE); not fingerprinted
	BuildArgs []string // extra build-tool arguments (e.g. -Dmaven.repo.local=...)
	// Progress receives a line per build invocation.
	Progress func(msg string)
}

// Build is one Maven reactor or Gradle build to record.
type Build struct {
	Kind    string // jvm.RunnerMaven or jvm.RunnerGradle
	Dir     string // directory of the build (repo-relative, "." for the root)
	Wrapper string // repo-relative wrapper script ("" when none)
	Units   []Unit
	// All: every unit of the build is recorded (no -pl / task list needed).
	All bool
}

// Builds groups units by the build they belong to (the launch configuration of the unit).
func Builds(units []Unit, all map[string]int) []Build {
	idx := map[[3]string]int{}
	var out []Build
	for _, u := range units {
		bf, wr := jvm.ParseLaunchConfig(u.Config)
		dir := "."
		if bf != "" {
			dir = path.Dir(bf)
		}
		k := [3]string{u.Runner, dir, wr}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, Build{Kind: u.Runner, Dir: dir, Wrapper: wr})
		}
		out[i].Units = append(out[i].Units, u)
	}
	for i := range out {
		out[i].All = all != nil && all[out[i].Kind+"|"+out[i].Dir] == len(out[i].Units)
	}
	return out
}

// Outcome is the result of recording one build.
type Outcome struct {
	Build    Build
	LogDir   string
	Exit     int
	TimedOut bool
	Err      string
	Elapsed  time.Duration
	Tail     string
	Units    []UnitResult
}

const gradleInit = `// bgraph record (docs/dynamic-layer.md §9.4): one fresh test JVM per test class, with the collector agent.
allprojects {
  tasks.withType(Test).configureEach {
    forkEvery = 1
    maxParallelForks = %d
    jvmArgs(%s)
    ignoreFailures = true
    outputs.upToDateWhen { false }
    outputs.cacheIf { false }
  }
}
`

func launcherPath(root string, b Build, tool string) string {
	if b.Wrapper != "" {
		w := filepath.Join(root, filepath.FromSlash(b.Wrapper))
		if runtime.GOOS == "windows" {
			if tool == "mvn" {
				return w + ".cmd"
			}
			return w + ".bat"
		}
		return w
	}
	return tool
}

// Run records one build: every test class of its units in a JVM of its own with the agent.
func Run(ctx context.Context, opt RunOptions, b Build) (Outcome, error) {
	out := Outcome{Build: b}
	logDir, err := os.MkdirTemp(opt.TmpDir, "bgraph-record-jvm-")
	if err != nil {
		return out, err
	}
	if logDir, err = filepath.Abs(logDir); err != nil {
		return out, err
	}
	if strings.ContainsAny(logDir+opt.Agent, " \t") {
		return out, errors.New("the record directory or the agent path contains a space (JAVA_TOOL_OPTIONS / jvmArgs cannot carry it)")
	}
	out.LogDir = logDir
	agentArg := "-javaagent:" + opt.Agent + "=" + logDir
	jobs := max(1, opt.Jobs)
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Minute
	}
	env := append(os.Environ(), opt.Env...)
	env = append(env, "JAVA_HOME="+opt.JavaHome)
	var argv []string
	switch b.Kind {
	case jvm.RunnerMaven:
		argv = []string{launcherPath(opt.Root, b, "mvn"), "-B", "-ntp", fmt.Sprintf("-DforkCount=%d", jobs), "-DreuseForks=false",
			"-Dmaven.test.failure.ignore=true", "-DfailIfNoTests=false", "-Dsurefire.failIfNoSpecifiedTests=false"}
		argv = append(argv, opt.BuildArgs...)
		if !b.All {
			var pl []string
			for _, u := range b.Units {
				rel, err := filepath.Rel(filepath.FromSlash(b.Dir), filepath.FromSlash(u.Dir))
				if err != nil {
					return out, err
				}
				pl = append(pl, filepath.ToSlash(rel))
			}
			argv = append(argv, "-pl", strings.Join(pl, ","), "-am")
		}
		argv = append(argv, "test")
		tool := ""
		for _, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "JAVA_TOOL_OPTIONS") {
				tool = v
			}
		}
		env = append(env, "JAVA_TOOL_OPTIONS="+strings.TrimSpace(tool+" "+agentArg))
	case jvm.RunnerGradle:
		init := filepath.Join(logDir, "..", filepath.Base(logDir)+"-init.gradle")
		if err := os.WriteFile(init, []byte(fmt.Sprintf(gradleInit, jobs, groovyQuote(agentArg))), 0o644); err != nil {
			return out, err
		}
		defer os.Remove(init)
		argv = []string{launcherPath(opt.Root, b, "gradle"), "--console=plain", "--continue", "-I", init}
		argv = append(argv, opt.BuildArgs...)
		if b.All {
			argv = append(argv, "test") // every project that has a test task (an explicit :p:test fails where there is none)
		}
		for _, u := range b.Units {
			if b.All {
				break
			}
			_, _, pp, ok := jvm.ParseUnitID(u.ID)
			if !ok {
				pp = u.Package
			}
			if pp == "" || pp == ":" {
				argv = append(argv, ":test")
			} else {
				argv = append(argv, pp+":test")
			}
		}
	default:
		return out, fmt.Errorf("unknown JVM runner %q", b.Kind)
	}
	if opt.Progress != nil {
		opt.Progress(fmt.Sprintf("%s build in %s: %d units, %d forks", b.Kind, b.Dir, len(b.Units), jobs))
	}
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Dir = filepath.Join(opt.Root, filepath.FromSlash(b.Dir))
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	out.Elapsed = time.Since(start)
	out.Tail = tail(buf.String(), 2000)
	if cctx.Err() == context.DeadlineExceeded {
		out.TimedOut = true
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			out.Exit = ee.ExitCode()
		} else {
			out.Exit = -1
			out.Err = err.Error()
		}
	}
	buildWhy := ""
	switch {
	case out.TimedOut:
		buildWhy = "timed out after " + timeout.String()
	case out.Err != "":
		buildWhy = out.Err
	case out.Exit != 0:
		buildWhy = fmt.Sprintf("exit code %d", out.Exit)
	}
	for _, u := range b.Units {
		rep, err := ReadReports(opt.Root, u, start)
		ur := UnitResult{Unit: u, Reports: rep, BuildOK: buildWhy == "", BuildWhy: buildWhy}
		if err != nil {
			ur.BuildOK, ur.BuildWhy = false, "reports: "+err.Error()
		}
		out.Units = append(out.Units, ur)
	}
	return out, nil
}

type xmlSuite struct {
	Name     string `xml:"name,attr"`
	Failures int    `xml:"failures,attr"`
	Errors   int    `xml:"errors,attr"`
	Cases    []struct {
		Failure []struct{} `xml:"failure"`
		Error   []struct{} `xml:"error"`
	} `xml:"testcase"`
}

// ReadReports reads the JUnit XML reports the unit's test task wrote since start (Maven target/surefire-reports, Gradle
// build/test-results/**): top-level class → every test passed (failures/errors in any report of the class or its nested
// classes make it false).
func ReadReports(root string, u Unit, since time.Time) (Reports, error) {
	dir := filepath.Join(root, filepath.FromSlash(u.Dir))
	sub := filepath.Join(dir, "target", "surefire-reports")
	if u.Runner == jvm.RunnerGradle {
		sub = filepath.Join(dir, "build", "test-results")
	}
	rep := Reports{}
	err := filepath.WalkDir(sub, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() || !strings.HasPrefix(d.Name(), "TEST-") || !strings.HasSuffix(d.Name(), ".xml") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(since.Add(-2*time.Second)) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var s xmlSuite
		if err := xml.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("%s: %v", p, err)
		}
		// The suite is the class the runner ran (testcase class names may name other classes: suites built by test
		// libraries such as guava-testlib report their testers).
		if s.Name == "" {
			return fmt.Errorf("%s: test suite without a name", p)
		}
		red := s.Failures+s.Errors > 0
		for _, c := range s.Cases {
			red = red || len(c.Failure)+len(c.Error) > 0
		}
		n := TopLevel(s.Name)
		if ok, seen := rep[n]; !seen || ok {
			rep[n] = !red
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return rep, err
}

// groovyQuote quotes s as a single-quoted Groovy string (no interpolation).
func groovyQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(s) + "'"
}

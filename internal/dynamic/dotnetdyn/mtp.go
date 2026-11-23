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
	"strings"
	"time"
)

// Microsoft.Testing.Platform projects (docs/dynamic-layer.md §9.11): the test project builds an application that runs its own
// tests. The recording runs that application directly, with the VSTest-syntax --filter, a TRX report and code coverage from
// the platform's extension (Microsoft.Testing.Extensions.CodeCoverage) or, without it, from `dotnet-coverage collect`.

// coverageSettings is the Microsoft.CodeCoverage configuration of runSettings for the platform extension and dotnet-coverage.
const coverageSettings = `<?xml version="1.0" encoding="utf-8"?>
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
`

// mtpApp is how to start the test application of one target framework.
type mtpApp struct {
	argv []string // command and fixed arguments
	dir  string   // working directory
}

// mtpCaps are the options a test application offers (from its --help).
type mtpCaps struct {
	trx      []string // TRX report options before the file name option's value: e.g. --report-xunit-trx --report-xunit-trx-filename
	coverage bool     // the code coverage extension (--coverage)
	preEnum  bool     // xUnit v3 --pre-enumerate-theories
	tool     string   // dotnet-coverage executable when the extension is missing
}

// splitArgs splits a command line on blanks, honouring double quotes.
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	quoted, seen := false, false
	for _, r := range s {
		switch {
		case r == '"':
			quoted, seen = !quoted, true
		case (r == ' ' || r == '\t') && !quoted:
			if seen || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				seen = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if seen || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// mtpApps evaluates how to run the test application of every framework (RunCommand, RunArguments, RunWorkingDirectory).
func (o RunOptions) mtpApps(ctx context.Context, project string, fws []string) (map[string]mtpApp, error) {
	apps := map[string]mtpApp{}
	for _, fw := range fws {
		args := append([]string{"msbuild", project, "-getProperty:RunCommand", "-getProperty:RunArguments",
			"-getProperty:RunWorkingDirectory", "-getProperty:TargetPath", "-p:TargetFramework=" + fw}, o.BuildArgs...)
		out, err := o.dotnet(ctx, 5*time.Minute, args...)
		if err != nil {
			return nil, fmt.Errorf("evaluate the test application of %s (%s): %v: %s", project, fw, err, tail(out, 4))
		}
		var v struct{ Properties map[string]string }
		i := strings.IndexByte(out, '{')
		if i < 0 || json.Unmarshal([]byte(out[i:]), &v) != nil {
			return nil, fmt.Errorf("evaluate the test application of %s (%s): no properties", project, fw)
		}
		cmd := strings.TrimSpace(v.Properties["RunCommand"])
		if cmd == "" {
			return nil, fmt.Errorf("%s (%s) has no RunCommand: not a test application", project, fw)
		}
		dir := strings.TrimSpace(v.Properties["RunWorkingDirectory"])
		if dir == "" {
			dir = filepath.Dir(strings.TrimSpace(v.Properties["TargetPath"]))
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(o.Root, filepath.Dir(project), dir)
		}
		apps[fw] = mtpApp{argv: append([]string{cmd}, splitArgs(v.Properties["RunArguments"])...), dir: dir}
	}
	return apps, nil
}

// run starts a test application (or dotnet-coverage wrapping it) with extra arguments.
func (o RunOptions) run(ctx context.Context, timeout time.Duration, dir string, argv ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(c, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = o.env()
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	if c.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out after %s", timeout)
	}
	return buf.String(), err
}

// probe reads the options of a test application from its --help.
func (o RunOptions) probe(ctx context.Context, app mtpApp) (mtpCaps, error) {
	out, _ := o.run(ctx, 2*time.Minute, app.dir, append(app.argv, "--help")...)
	has := func(opt string) bool {
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) > 0 && f[0] == opt {
				return true
			}
		}
		return false
	}
	var caps mtpCaps
	if !has("--filter") {
		return caps, errors.New("the test application has no --filter option (VSTest filter syntax)")
	}
	switch {
	case has("--report-xunit-trx"):
		caps.trx = []string{"--report-xunit-trx", "--report-xunit-trx-filename"}
	case has("--report-trx"):
		caps.trx = []string{"--report-trx", "--report-trx-filename"}
	default:
		return caps, errors.New("the test application has no TRX report option (reference Microsoft.Testing.Extensions.TrxReport)")
	}
	caps.coverage = has("--coverage")
	caps.preEnum = has("--pre-enumerate-theories")
	if !caps.coverage {
		t, err := exec.LookPath("dotnet-coverage")
		if err != nil {
			if home, herr := os.UserHomeDir(); herr == nil {
				if p, perr := exec.LookPath(filepath.Join(home, ".dotnet", "tools", "dotnet-coverage")); perr == nil {
					t, err = p, nil
				}
			}
		}
		if err != nil {
			return caps, errors.New("no code coverage collector: reference Microsoft.Testing.Extensions.CodeCoverage or install the dotnet-coverage tool")
		}
		caps.tool = t
	}
	return caps, nil
}

// mtpList runs the whole suite of one framework and reads its TRX report.
func (o RunOptions) mtpList(ctx context.Context, app mtpApp, caps mtpCaps, dir string) (map[string]TestOutcome, string, error) {
	argv := append(append([]string{}, app.argv...), caps.trx[0], caps.trx[1], "list.trx", "--results-directory", dir)
	out, _ := o.run(ctx, o.timeout()*3, app.dir, argv...)
	res, err := ParseTRXFile(filepath.Join(dir, "list.trx"))
	return res, out, err
}

// mtpClass runs one class of one framework with coverage.
func (o RunOptions) mtpClass(ctx context.Context, app mtpApp, caps mtpCaps, class, fw, settings, dir string) ClassRun {
	t0 := time.Now()
	r := ClassRun{Class: class, Framework: fw}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.Why = err.Error()
		return r
	}
	cov := filepath.Join(dir, "coverage.cobertura.xml")
	test := append(append([]string{}, app.argv...), "--filter", ClassFilter(class), caps.trx[0], caps.trx[1], "run.trx",
		"--results-directory", dir)
	if caps.preEnum {
		test = append(test, "--pre-enumerate-theories", "off")
	}
	var argv []string
	if caps.coverage {
		argv = append(test, "--coverage", "--coverage-output-format", "cobertura", "--coverage-output", cov, "--coverage-settings", settings)
	} else {
		argv = append([]string{caps.tool, "collect", "-f", "cobertura", "-o", cov, "-s", settings, "--"}, test...)
	}
	out, err := o.run(ctx, o.timeout(), app.dir, argv...)
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
	if _, serr := os.Stat(cov); serr != nil {
		// The extension may name the file itself under the results directory.
		if found, _ := filepath.Glob(filepath.Join(dir, "*.cobertura.xml")); len(found) == 1 {
			cov = found[0]
		}
	}
	c, cerr := ParseCoberturaFile(cov)
	switch {
	case cerr != nil:
		r.Why = "no coverage report"
	default:
		r.Coverage = &c
	}
	if err != nil && r.Why == "" && !errors.As(err, new(*exec.ExitError)) {
		r.Why = err.Error() // a failing test exits non-zero: that is an outcome, not a broken run
	}
	return r
}

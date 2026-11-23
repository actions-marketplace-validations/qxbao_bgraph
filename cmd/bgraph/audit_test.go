package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qxbao/bgraph"
)

const junitBothFail = `<?xml version="1.0"?>
<testsuites>
  <testsuite name="src/a.test.ts">
    <testcase classname="src/a.test.ts" name="a works"><failure message="a broke"/></testcase>
  </testsuite>
  <testsuite name="src/b.test.ts">
    <testcase classname="src/b.test.ts" name="b works"><failure message="b flaked"/></testcase>
    <testcase classname="src/b.test.ts" name="b other"/>
  </testsuite>
</testsuites>
`

func auditRepo(t *testing.T, branch string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	gitEnvFor(t)
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", branch)
	git(t, root, "config", "core.autocrlf", "false")
	write(t, root, "package.json", `{"name":"p","devDependencies":{"vitest":"^1.0.0"}}`)
	write(t, root, "src/a.ts", "export const a = 1\n")
	write(t, root, "src/a.test.ts", "import { a } from './a'\nconsole.log(a)\n")
	write(t, root, "src/b.ts", "export const b = 1\n")
	write(t, root, "src/b.test.ts", "import { b } from './b'\nconsole.log(b)\n")
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "init")
	write(t, root, "src/a.ts", "export const a = 2\n")
	return root
}

func runCLI(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestAuditMissedFailure(t *testing.T) {
	root := auditRepo(t, "main")
	res := filepath.Join(t.TempDir(), "junit.xml")
	os.WriteFile(res, []byte(junitBothFail), 0o644)
	logf := filepath.Join(t.TempDir(), "sub", "audit.jsonl")

	code, out, errs := runCLI("audit", "--root", root, "--base", "HEAD", "--results", res, "--log", logf)
	if code != 3 {
		t.Fatalf("exit %d, want 3\nout:\n%s\nerr:\n%s", code, out, errs)
	}
	if !strings.Contains(out, "MISSED (1)") || !strings.Contains(out, "src/b.test.ts :: src/b.test.ts > b works") ||
		strings.Contains(out, "src/a.test.ts ::") || !strings.Contains(out, "clean shadow runs in a row: 0") {
		t.Errorf("output:\n%s", out)
	}

	code, out, _ = runCLI("audit", "--root", root, "--base", "HEAD", "--results", res, "--json")
	var rep bgraph.AuditReport
	if code != 3 || json.Unmarshal([]byte(out), &rep) != nil {
		t.Fatalf("json: exit %d\n%s", code, out)
	}
	if rep.Failed != 2 || rep.Selected != 1 || len(rep.Missed) != 1 || rep.Missed[0].File != "src/b.test.ts" || rep.Clean {
		t.Errorf("report: %+v", rep)
	}

	// Only the covered failure: clean, and the log streak builds on the earlier non-clean line.
	only := filepath.Join(t.TempDir(), "only-a.xml")
	os.WriteFile(only, []byte(`<testsuite name="src/a.test.ts"><testcase classname="src/a.test.ts" name="x"><failure/></testcase></testsuite>`), 0o644)
	for i, want := range []string{"1", "2"} {
		code, out, errs = runCLI("audit", "--root", root, "--base", "HEAD", "--results", only, "--log", logf)
		if code != 0 || !strings.Contains(out, "clean shadow runs in a row: "+want) {
			t.Fatalf("run %d: exit %d\n%s\n%s", i, code, out, errs)
		}
	}
	b, err := os.ReadFile(logf)
	if err != nil || len(strings.Split(strings.TrimSpace(string(b)), "\n")) != 3 {
		t.Fatalf("log: %v\n%s", err, b)
	}
	var line map[string]any
	first := strings.SplitN(string(b), "\n", 2)[0]
	if json.Unmarshal([]byte(first), &line) != nil || line["clean"] != false || line["scope"] != "graph" || line["failed"] != float64(2) {
		t.Errorf("log line: %s", first)
	}
}

func TestAuditFullScopeIsClean(t *testing.T) {
	root := auditRepo(t, "trunk") // no main/master/origin: default base fails -> no_base -> full
	res := filepath.Join(t.TempDir(), "junit.xml")
	os.WriteFile(res, []byte(junitBothFail), 0o644)
	code, out, errs := runCLI("audit", "--root", root, "--results", res)
	if code != 0 || !strings.Contains(out, "scope: full") || !strings.Contains(out, "no missed failure") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
}

func TestAuditUnmapped(t *testing.T) {
	root := auditRepo(t, "main")
	res := filepath.Join(t.TempDir(), "junit.xml")
	os.WriteFile(res, []byte(`<testsuites><testsuite name="math"><testcase classname="math adds" name="adds"><failure/></testcase></testsuite></testsuites>`), 0o644)
	code, out, _ := runCLI("audit", "--root", root, "--base", "HEAD", "--results", res)
	if code != 0 || !strings.Contains(out, "unmapped (1)") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if code, _, _ = runCLI("audit", "--root", root, "--base", "HEAD", "--results", res, "--strict"); code != 3 {
		t.Errorf("--strict: exit %d, want 3", code)
	}
}

// go test -json maps failures to Go units by import path: a failure in the selected package is covered, one in another
// package is missed, one in a package unknown to the repo is unmapped.
func TestAuditGoJSON(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	gitEnvFor(t)
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	git(t, root, "config", "core.autocrlf", "false")
	write(t, root, "go.mod", "module example.com/fx\n\ngo 1.22\n")
	for _, p := range []string{"a", "b"} {
		write(t, root, p+"/"+p+".go", "package "+p+"\n\nfunc F() int { return 1 }\n")
		write(t, root, p+"/"+p+"_test.go", "package "+p+"\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) { _ = F() }\n")
	}
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "init")
	write(t, root, "a/a.go", "package a\n\nfunc F() int { return 2 }\n")

	res := filepath.Join(t.TempDir(), "go-test.jsonl")
	os.WriteFile(res, []byte(strings.Join([]string{
		`{"Action":"run","Package":"example.com/fx/a","Test":"TestF"}`,
		`{"Action":"fail","Package":"example.com/fx/a","Test":"TestF"}`,
		`{"Action":"fail","Package":"example.com/fx/a"}`,
		`{"Action":"fail","Package":"example.com/fx/b","Test":"TestF/sub"}`,
		`{"Action":"fail","Package":"example.com/fx/b","Test":"TestF"}`,
		`{"Action":"fail","Package":"example.com/fx/b"}`,
		`{"Action":"fail","Package":"example.com/other"}`,
	}, "\n")+"\n"), 0o644)

	code, out, errs := runCLI("audit", "--root", root, "--base", "HEAD", "--results", res, "--json")
	var rep bgraph.AuditReport
	if code != 3 || json.Unmarshal([]byte(out), &rep) != nil {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if rep.Failed != 3 || rep.Selected != 1 || rep.Total != 2 || rep.Full || len(rep.Missed) != 1 || len(rep.Unmapped) != 1 {
		t.Fatalf("report: %+v", rep)
	}
	if m := rep.Missed[0]; m.Unit != "go:b" || m.File != "" || m.Test != "TestF/sub" {
		t.Errorf("missed: %+v", m)
	}
	if u := rep.Unmapped[0]; u.Test != "(package failed)" || !strings.Contains(u.Reason, "example.com/other") {
		t.Errorf("unmapped: %+v", u)
	}
	code, out, _ = runCLI("audit", "--root", root, "--base", "HEAD", "--results", res, "--format", "go-json")
	if code != 3 || !strings.Contains(out, "go:b :: TestF/sub") || !strings.Contains(out, "selected test units: 1 of 2") {
		t.Errorf("text: exit %d\n%s", code, out)
	}
}

func TestAuditSummary(t *testing.T) {
	logf := filepath.Join(t.TempDir(), "shadow.jsonl")
	os.WriteFile(logf, []byte(strings.Join([]string{
		// An old line without total/full, a full run, a run with a miss, two clean measured runs, garbage.
		`{"time":"t1","scope":"graph","selected":3,"failed":0,"missed":[],"unmapped":[],"clean":true}`,
		`{"time":"t2","scope":"full","selected":0,"total":10,"failed":1,"missed":[],"unmapped":[{"test":"x","reason":"r"}],"clean":true}`,
		`{"time":"t3","head":"abc","scope":"graph","selected":2,"total":10,"full":false,"failed":1,"missed":[{"unit":"go:b","test":"TestF","reason":"r"}],"unmapped":[],"clean":false}`,
		`not json`,
		`{"time":"t4","scope":"package","selected":4,"total":10,"failed":0,"missed":[],"unmapped":[],"clean":true}`,
		`{"time":"t5","scope":"none","selected":0,"total":10,"failed":0,"missed":[],"unmapped":[],"clean":true}`,
	}, "\n")+"\n"), 0o644)

	code, out, errs := runCLI("audit", "--summary", "--log", logf, "--json")
	var s auditSummary
	if code != 0 || json.Unmarshal([]byte(out), &s) != nil {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	// Shares: t2 1.0, t3 0.2, t4 0.4, t5 0 → mean 0.4, median 0.3.
	if s.Runs != 5 || s.Skipped != 1 || s.Clean != 4 || s.CleanStreak != 2 || len(s.MissedRuns) != 1 || s.MissedRuns[0].Tests[0] != "go:b :: TestF" ||
		s.Unmapped != 1 || s.UnmappedIn != 1 || s.FullRuns != 1 || s.Measured != 4 || s.UnmeasuredRuns != 1 ||
		!near(s.MeanShare, 0.4) || !near(s.MedianShare, 0.3) || !near(s.MeanSelected, 4) || !near(s.MeanTotal, 10) || !near(s.FullShare, 0.2) {
		t.Errorf("summary: %+v", s)
	}
	code, out, _ = runCLI("audit", "--summary", "--log", logf)
	for _, want := range []string{"runs: 5", "clean runs in a row: 2", "runs with missed failures: 1", "t3  head abc  1 missed",
		"go:b :: TestF", "full runs: 1 (20%)", "mean 40.0%, median 30.0% (4 runs)"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("text: exit %d, missing %q\n%s", code, want, out)
		}
	}
	if code, _, _ := runCLI("audit", "--summary"); code != 2 {
		t.Errorf("--summary without --log: exit %d, want 2", code)
	}
	if code, _, _ := runCLI("audit", "--summary", "--log", logf, "--results", "x"); code != 2 {
		t.Errorf("--summary with --results: exit %d, want 2", code)
	}
	if code, _, _ := runCLI("audit", "--summary", "--log", filepath.Join(t.TempDir(), "missing")); code != 1 {
		t.Errorf("missing log: exit %d, want 1", code)
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestAuditUsageAndErrors(t *testing.T) {
	if code, _, _ := runCLI("audit"); code != 2 {
		t.Errorf("no --results: exit %d, want 2", code)
	}
	if code, _, _ := runCLI("audit", "--results", "x", "--format", "tap"); code != 2 {
		t.Errorf("bad --format: exit %d, want 2", code)
	}
	if code, _, _ := runCLI("audit", "--results", filepath.Join(t.TempDir(), "missing.xml")); code != 1 {
		t.Errorf("missing results: exit %d, want 1", code)
	}
	bad := filepath.Join(t.TempDir(), "bad.xml")
	os.WriteFile(bad, []byte("not results"), 0o644)
	if code, _, _ := runCLI("audit", "--results", bad); code != 1 {
		t.Errorf("unparseable results: exit %d, want 1", code)
	}
}

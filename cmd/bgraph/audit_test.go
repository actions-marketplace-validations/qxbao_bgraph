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

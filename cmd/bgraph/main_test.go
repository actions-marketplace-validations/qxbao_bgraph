package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func gitEnvFor(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(empty, nil, 0o644)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	for _, k := range []string{"AUTHOR", "COMMITTER"} {
		t.Setenv("GIT_"+k+"_NAME", "t")
		t.Setenv("GIT_"+k+"_EMAIL", "t@example.com")
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.autocrlf=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCLIAffectedAndCommand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("không có git")
	}
	gitEnvFor(t)
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	git(t, root, "config", "core.autocrlf", "false")
	write(t, root, "package.json", `{"name":"p","devDependencies":{"vitest":"^1.0.0"}}`)
	write(t, root, "src/a.ts", "export const a = 1\n")
	write(t, root, "src/a.test.ts", "import { a } from './a'\nconsole.log(a)\n")
	write(t, root, "src/b.test.ts", "console.log('b')\n")
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "init")
	write(t, root, "src/a.ts", "export const a = 2\n")

	var out, errb bytes.Buffer
	if code := run([]string{"affected", "--root", root, "--base", "HEAD", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("affected: %d %s", code, errb.String())
	}
	var sel model.Selection
	if err := json.Unmarshal(out.Bytes(), &sel); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	var units []string
	for _, x := range sel.Tests {
		units = append(units, x.Unit)
	}
	has := func(u string) bool {
		for _, x := range units {
			if x == u {
				return true
			}
		}
		return false
	}
	if !has("src/a.test.ts") || has("src/b.test.ts") {
		t.Fatalf("selection sai: scope=%s units=%v widening=%v", sel.Scope, units, sel.Widening)
	}

	out.Reset()
	errb.Reset()
	if code := run([]string{"command", "--root", root, "--base", "HEAD"}, &out, &errb); code != 0 {
		t.Fatalf("command: %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "npx vitest run src/a.test.ts") || strings.Contains(out.String(), "b.test.ts") {
		t.Fatalf("command output: %s", out.String())
	}

	out.Reset()
	if code := run([]string{"explain", "--root", root, "--base", "HEAD", "src/a.test.ts"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "src/a.test.ts") {
		t.Fatalf("explain: %d %s", code, out.String())
	}
	out.Reset()
	if code := run([]string{"index", "--root", root}, &out, &errb); code != 0 {
		t.Fatalf("index: %d %s", code, errb.String())
	}
	if code := run([]string{"unresolved", "--root", root}, &out, &errb); code != 0 {
		t.Fatalf("unresolved: %d %s", code, errb.String())
	}
}

func TestCLIUsage(t *testing.T) {
	var o, e bytes.Buffer
	if run(nil, &o, &e) != 2 || run([]string{"bogus"}, &o, &e) != 2 || run([]string{"audit"}, &o, &e) != 2 ||
		run([]string{"unresolved", "--class", "x"}, &o, &e) != 2 || run([]string{"version"}, &o, &e) != 0 {
		t.Fatal("mã thoát sai")
	}
}

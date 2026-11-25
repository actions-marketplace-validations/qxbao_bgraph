package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIAgentInstallUninstall(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	gitEnvFor(t)
	root := t.TempDir()
	git(t, root, "init", "-q")
	write(t, root, "AGENTS.md", "# A\n")
	sub := filepath.Join(root, "sub")
	os.MkdirAll(sub, 0o755)

	var out, errb bytes.Buffer
	if code := run([]string{"agent", "install", "--root", sub, "--skill", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("install: %d %s", code, errb.String())
	}
	var cs []struct{ Path, Action string }
	if err := json.Unmarshal(out.Bytes(), &cs); err != nil || len(cs) != 2 || cs[0].Path != "AGENTS.md" || cs[0].Action != "updated" {
		t.Fatalf("install = %s (%v)", out.String(), err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents", "skills", "bgraph-test", "SKILL.md")); err != nil {
		t.Error(err)
	}
	out.Reset()
	if code := run([]string{"agent", "uninstall", "--root", sub}, &out, &errb); code != 0 || !strings.Contains(out.String(), "removed") {
		t.Fatalf("uninstall: %d %q %s", code, out.String(), errb.String())
	}
	if b, _ := os.ReadFile(filepath.Join(root, "AGENTS.md")); string(b) != "# A\n" {
		t.Errorf("AGENTS.md = %q", b)
	}
	if code := run([]string{"agent", "install", "--root", sub, "--target", "cursor"}, &out, &errb); code != 2 {
		t.Errorf("bad target: %d", code)
	}
	if code := run([]string{"agent", "frob"}, &out, &errb); code != 2 {
		t.Errorf("bad subcommand: %d", code)
	}
}

func TestCLIAgentHook(t *testing.T) {
	var out, errb bytes.Buffer
	in := `{"session_id":"cli-hook-test-` + t.Name() + `","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"go test ./pkg"}}`
	if code := runAgentHook(nil, strings.NewReader(in), &out, &errb); code != 0 || out.Len() != 0 {
		t.Errorf("partial run: %d %q %q", code, out.String(), errb.String())
	}
	if code := runAgentHook(nil, strings.NewReader("garbage"), &out, &errb); code != 0 || out.Len() != 0 {
		t.Errorf("garbage: %d %q", code, out.String())
	}
}

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpsertRemoveBlock(t *testing.T) {
	for name, orig := range map[string]string{
		"empty":     "",
		"lf":        "# Project\n\nSome rules.\n",
		"crlf":      "# Project\r\n\r\nSome rules.\r\n",
		"no-eol":    "# Project",
		"trailing":  "# Project\n\n\n",
		"only-text": "x\n",
	} {
		t.Run(name, func(t *testing.T) {
			once := UpsertBlock(orig, Block())
			if strings.Count(once, BlockStart) != 1 || strings.Count(once, BlockEnd) != 1 {
				t.Fatalf("markers: %q", once)
			}
			if twice := UpsertBlock(once, Block()); twice != once {
				t.Errorf("not idempotent:\n%q\n%q", once, twice)
			}
			if strings.Contains(orig, "\r\n") && strings.Count(once, "\n") != strings.Count(once, "\r\n") {
				t.Errorf("mixed line endings: %q", once)
			}
			updated := UpsertBlock(once, "new text\n")
			if !strings.Contains(updated, "new text") || strings.Contains(updated, "Running tests") {
				t.Errorf("replace: %q", updated)
			}
			back, ok := RemoveBlock(once)
			if !ok {
				t.Fatal("block not found")
			}
			if strings.TrimRight(back, "\r\n") != strings.TrimRight(orig, "\r\n") {
				t.Errorf("remove: got %q, want %q", back, orig)
			}
		})
	}
	// A block in the middle keeps the text around it.
	mid := "a\n\n" + BlockStart + "\nold\n" + BlockEnd + "\n\nb\n"
	if got, _ := RemoveBlock(mid); got != "a\n\nb\n" {
		t.Errorf("middle: %q", got)
	}
	// A start marker without an end marker is never treated as a block: nothing is lost.
	broken := "a\n" + BlockStart + "\nuser text\n"
	if got := UpsertBlock(broken, "x"); !strings.Contains(got, "user text") || !strings.HasPrefix(got, broken[:len(broken)-1]) {
		t.Errorf("broken: %q", got)
	}
	if _, ok := RemoveBlock(broken); ok {
		t.Error("broken block removed")
	}
}

func TestGuidanceText(t *testing.T) {
	if b := Block(); !strings.Contains(b, "bgraph command") || !strings.Contains(b, "scope: full") || strings.Contains(b, "MUST") {
		t.Errorf("block: %s", b)
	}
	s := Skill()
	if !strings.HasPrefix(s, "---\nname: "+SkillName+"\ndescription: ") || !strings.Contains(s, skillMarker) {
		t.Errorf("skill: %s", s)
	}
	if !strings.Contains(Instructions, ToolTestCommands) || !strings.Contains(Instructions, ToolDependents) {
		t.Errorf("instructions: %s", Instructions)
	}
}

func TestIsFullSuiteCommand(t *testing.T) {
	full := []string{
		"go test ./...",
		"go test -count=1 -race ./...",
		"cd sub && go test ./... 2>&1 | tail -20",
		"cd D:/Projects/bgraph && go test ./...",
		"npm test",
		"npm run test",
		"pnpm test -- --coverage",
		"yarn test",
		"bun test",
		"pytest",
		"pytest -q -x",
		"python -m pytest",
		"uv run pytest -q",
		"cargo test",
		"cargo test --workspace -- --nocapture",
		"cargo nextest run",
		"mvn -q test",
		"./mvnw verify",
		"./gradlew test",
		"gradlew.bat check",
		"dotnet test",
		"dotnet test App.sln",
		"npx vitest run",
		"npx jest --ci",
		"bundle exec rspec",
		"bin/rails test",
		"bundle exec rake test",
		"vendor/bin/phpunit",
		"make test",
		"CI=1 npm test",
		"tox",
	}
	partial := []string{
		"go test ./internal/foo",
		"go test -run TestX ./...",
		"go test",
		"go build ./...",
		"npm run build",
		"npm test -- src/a.test.ts",
		"pytest tests/test_a.py",
		"pytest -k slow",
		"cargo test parser",
		"cargo test -p core",
		"mvn -Dtest=FooTest test",
		"mvn -pl core test",
		"./gradlew test --tests com.x.FooTest",
		"dotnet test --filter Name~Foo",
		"dotnet test src/App.Tests/App.Tests.csproj",
		"npx vitest run src/a.test.ts",
		"npx jest -t parses",
		"bundle exec rspec spec/models/user_spec.rb",
		"bgraph command",
		"bgraph command | sh",
		"cd /d/Projects/bgraph && go test ./internal/foo",
		"git status",
		"make build",
		"",
	}
	for _, c := range full {
		if !IsFullSuiteCommand(c) {
			t.Errorf("IsFullSuiteCommand(%q) = false, want true", c)
		}
	}
	for _, c := range partial {
		if IsFullSuiteCommand(c) {
			t.Errorf("IsFullSuiteCommand(%q) = true, want false", c)
		}
	}
}

func TestHook(t *testing.T) {
	h := Hook{StateDir: t.TempDir()}
	input := func(session, tool, cmd string) []byte {
		b, _ := json.Marshal(map[string]any{"session_id": session, "hook_event_name": "PostToolUse", "tool_name": tool,
			"tool_input": map[string]any{"command": cmd}})
		return b
	}
	out, err := h.Handle(input("s/1", "Bash", "go test ./..."))
	if err != nil || out == nil {
		t.Fatalf("first: %s %v", out, err)
	}
	var obj struct {
		HSO struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &obj); err != nil || obj.HSO.Event != "PostToolUse" || obj.HSO.Context != Nudge {
		t.Errorf("output: %s %v", out, err)
	}
	if out, _ := h.Handle(input("s/1", "Bash", "npm test")); out != nil {
		t.Errorf("second nudge in the same session: %s", out)
	}
	if out, _ := h.Handle(input("s2", "Bash", "npm test")); out == nil {
		t.Error("another session is nudged")
	}
	if out, _ := h.Handle(input("ps", "PowerShell", "go test ./... 2>&1 | Select-Object -Last 20")); out == nil {
		t.Error("Claude Code's PowerShell tool is not nudged")
	}
	for _, in := range [][]byte{input("s3", "Bash", "go test ./pkg"), input("s3", "Edit", "go test ./..."), []byte("not json")} {
		if out, err := h.Handle(in); out != nil || err != nil {
			t.Errorf("Handle(%s) = %s, %v", in, out, err)
		}
	}
	// s3 was never nudged, so a real full run still is.
	if out, _ := h.Handle(input("s3", "Bash", "pytest")); out == nil {
		t.Error("s3 full run not nudged")
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

func read(t *testing.T, root, rel string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	return string(b), err == nil
}

func actions(cs []Change) map[string]string {
	m := map[string]string{}
	for _, c := range cs {
		m[c.Path] = c.Action
	}
	return m
}

func TestInstallUninstall(t *testing.T) {
	root := t.TempDir()
	claude := "# Rules\n\n@AGENTS.md\n"
	agentsMD := "# Agents\n\nBe careful.\n"
	settings := `{"permissions":{"allow":["Bash(go test:*)"]},"hooks":{"PostToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"fmt && lint"}]}]}}`
	write(t, root, "CLAUDE.md", claude)
	write(t, root, "AGENTS.md", agentsMD)
	write(t, root, ".claude/settings.local.json", settings)
	write(t, root, ".mcp.json", `{"mcpServers":{"other":{"command":"x"}}}`)

	opt := InstallOptions{Skill: true, Hook: true, MCP: true}
	cs, err := Install(root, opt)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"CLAUDE.md":                           "skipped", // imports AGENTS.md
		"AGENTS.md":                           "updated",
		".claude/skills/bgraph-test/SKILL.md": "created",
		".agents/skills/bgraph-test/SKILL.md": "created",
		".claude/settings.local.json":         "updated",
		".codex/hooks.json":                   "created",
		".mcp.json":                           "updated",
	}
	if got := actions(cs); len(got) != len(want) {
		t.Errorf("install = %v", got)
	} else {
		for k, v := range want {
			if got[k] != v {
				t.Errorf("install %s = %s, want %s", k, got[k], v)
			}
		}
	}
	if s, _ := read(t, root, "AGENTS.md"); !strings.HasPrefix(s, agentsMD) || !strings.Contains(s, BlockStart) {
		t.Errorf("AGENTS.md = %q", s)
	}
	s, _ := read(t, root, ".claude/settings.local.json")
	if !strings.Contains(s, `"fmt && lint"`) || !strings.Contains(s, HookCommand) || !strings.Contains(s, "Bash(go test:*)") ||
		!strings.Contains(s, `"Bash|PowerShell"`) {
		t.Errorf("settings = %s", s)
	}
	if s, _ := read(t, root, ".codex/hooks.json"); !strings.Contains(s, HookCommand) || !strings.Contains(s, `"PostToolUse"`) {
		t.Errorf(".codex/hooks.json = %s", s)
	}
	if s, _ := read(t, root, ".mcp.json"); !strings.Contains(s, `"other"`) || !strings.Contains(s, `"bgraph"`) {
		t.Errorf(".mcp.json = %s", s)
	}

	// Idempotent.
	cs, err = Install(root, opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Action != "unchanged" && c.Action != "skipped" {
			t.Errorf("second install changed %s: %s", c.Path, c.Action)
		}
	}

	// Uninstall restores the user's files and removes what bgraph created.
	if _, err := Uninstall(root); err != nil {
		t.Fatal(err)
	}
	if s, _ := read(t, root, "AGENTS.md"); s != agentsMD {
		t.Errorf("AGENTS.md after uninstall = %q", s)
	}
	if s, _ := read(t, root, "CLAUDE.md"); s != claude {
		t.Errorf("CLAUDE.md after uninstall = %q", s)
	}
	for _, rel := range []string{".claude/skills/bgraph-test/SKILL.md", ".agents/skills/bgraph-test"} {
		if _, ok := read(t, root, rel); ok {
			t.Errorf("%s still exists", rel)
		}
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			t.Errorf("%s still exists", rel)
		}
	}
	var got, orig map[string]any
	s, _ = read(t, root, ".claude/settings.local.json")
	json.Unmarshal([]byte(s), &got)
	json.Unmarshal([]byte(settings), &orig)
	gb, _ := json.Marshal(got)
	ob, _ := json.Marshal(orig)
	if string(gb) != string(ob) {
		t.Errorf("settings after uninstall = %s, want %s", gb, ob)
	}
	if s, _ := read(t, root, ".mcp.json"); strings.Contains(s, "bgraph") || !strings.Contains(s, "other") {
		t.Errorf(".mcp.json after uninstall = %s", s)
	}
	if cs, _ := Uninstall(root); len(cs) != 0 {
		t.Errorf("second uninstall = %v", cs)
	}
}

func TestInstallFreshRepo(t *testing.T) {
	root := t.TempDir()
	cs, err := Install(root, InstallOptions{Skill: true, Hook: true, MCP: true})
	if err != nil {
		t.Fatal(err)
	}
	a := actions(cs)
	if a["CLAUDE.md"] != "created" || a["AGENTS.md"] != "created" || a[".claude/settings.local.json"] != "created" {
		t.Errorf("install = %v", a)
	}
	if _, err := Uninstall(root); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		t.Errorf("left behind: %s", e.Name())
	}
	if _, ok := read(t, root, ".claude/settings.local.json"); ok {
		t.Error("empty settings file left behind")
	}
}

func TestInstallKeepsUserSkill(t *testing.T) {
	root := t.TempDir()
	write(t, root, ".claude/skills/bgraph-test/SKILL.md", "my own skill\n")
	cs, err := Install(root, InstallOptions{Targets: []Target{TargetClaude}, Skill: true})
	if err != nil {
		t.Fatal(err)
	}
	if a := actions(cs); a[".claude/skills/bgraph-test/SKILL.md"] != "skipped" || a["AGENTS.md"] != "" {
		t.Errorf("install = %v", a)
	}
	Uninstall(root)
	if s, _ := read(t, root, ".claude/skills/bgraph-test/SKILL.md"); s != "my own skill\n" {
		t.Errorf("user skill = %q", s)
	}
	// A customized MCP entry is the user's: neither replaced nor removed.
	write(t, root, ".mcp.json", `{"mcpServers":{"bgraph":{"command":"/opt/bgraph","args":["mcp","--root","."]}}}`)
	Install(root, InstallOptions{MCP: true})
	Uninstall(root)
	if s, _ := read(t, root, ".mcp.json"); !strings.Contains(s, "/opt/bgraph") {
		t.Errorf(".mcp.json = %s", s)
	}
}

func TestParseTargets(t *testing.T) {
	if ts, err := ParseTargets("auto"); err != nil || ts != nil {
		t.Errorf("auto = %v %v", ts, err)
	}
	if ts, err := ParseTargets("claude, agents"); err != nil || len(ts) != 2 {
		t.Errorf("both = %v %v", ts, err)
	}
	if _, err := ParseTargets("cursor"); err == nil {
		t.Error("unknown target accepted")
	}
}

func TestSpecs(t *testing.T) {
	specs, err := Specs()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range specs {
		names = append(names, s.Name)
		var in, out map[string]any
		if err := json.Unmarshal(s.InputSchema, &in); err != nil || in["type"] != "object" {
			t.Errorf("%s input schema: %s", s.Name, s.InputSchema)
		}
		if err := json.Unmarshal(s.OutputSchema, &out); err != nil || out["type"] != "object" {
			t.Errorf("%s output schema: %s", s.Name, s.OutputSchema)
		}
		if !s.ReadOnly || s.Description == "" {
			t.Errorf("%s: %+v", s.Name, s)
		}
	}
	if strings.Join(names, ",") != "affected_tests,test_commands,dependents_of,explain_selection" {
		t.Errorf("names = %v", names)
	}
	// The embedded Command is flattened, as encoding/json does.
	tc := string(specs[1].OutputSchema)
	for _, k := range []string{`"argv"`, `"shell"`, `"runner"`, `"project"`} {
		if !strings.Contains(tc, k) {
			t.Errorf("test_commands output schema lacks %s: %s", k, tc)
		}
	}
	if _, err := (&Tools{}).Call(context.Background(), "nope", nil); err == nil {
		t.Error("unknown tool accepted")
	}
}

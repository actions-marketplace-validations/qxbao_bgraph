package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Target is an instruction-file family.
type Target string

const (
	// TargetClaude is Claude Code: CLAUDE.md and .claude/skills.
	TargetClaude Target = "claude"
	// TargetAgents is the AGENTS.md family (Codex, Cursor, Antigravity, opencode...): AGENTS.md and .agents/skills.
	TargetAgents Target = "agents"
)

// HookCommand is the shell command the hook runs (Claude Code and Codex share the hook protocol).
const HookCommand = "bgraph agent hook"

// codexHooks is Codex's project hook file.
const codexHooks = ".codex/hooks.json"

// InstallOptions selects what Install writes. The instruction-file block is always written; the rest is opt-in.
type InstallOptions struct {
	// Targets: the instruction files to write. Empty: the existing ones among CLAUDE.md and AGENTS.md, or both when neither
	// exists. A CLAUDE.md that imports AGENTS.md (an `@AGENTS.md` line) is skipped when AGENTS.md is written.
	Targets []Target
	// Skill writes the bgraph-test skill (.claude/skills and/or .agents/skills, per target).
	Skill bool
	// Hook adds the PostToolUse hook, per target: Claude Code's to .claude/settings.local.json (personal, not committed: the
	// hook needs bgraph on PATH), Codex's to .codex/hooks.json (Codex reads it in a trusted project and runs it once the user approves it with /hooks).
	Hook bool
	// MCP adds the bgraph server to .mcp.json (Claude Code project servers).
	MCP bool
}

// Change is one file Install or Uninstall touched.
type Change struct {
	Path   string `json:"path"`   // repo-relative, slash-separated
	Action string `json:"action"` // created | updated | removed | unchanged | skipped
	Note   string `json:"note,omitempty"`
}

// Install writes bgraph's agent guidance into the repository at root. It is idempotent: a second run reports every file
// unchanged. Only bgraph's own block, skill, hook entry and server entry are written; nothing else in the files changes
// except that JSON files are re-indented.
func Install(root string, opt InstallOptions) ([]Change, error) {
	targets, err := resolveTargets(root, opt.Targets)
	if err != nil {
		return nil, err
	}
	var out []Change
	writeAgents := false
	for _, t := range targets {
		writeAgents = writeAgents || t == TargetAgents
	}
	for _, t := range targets {
		file := instructionFile(t)
		if t == TargetClaude && writeAgents {
			if b, err := os.ReadFile(filepath.Join(root, file)); err == nil && importsAgents(string(b)) {
				out = append(out, Change{Path: file, Action: "skipped", Note: "imports AGENTS.md"})
				continue
			}
		}
		c, err := editText(root, file, func(s string) string { return UpsertBlock(s, Block()) })
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
	if opt.Skill {
		for _, t := range targets {
			c, err := writeSkill(root, skillPath(t))
			if err != nil {
				return out, err
			}
			out = append(out, c)
		}
	}
	if opt.Hook {
		for _, t := range targets {
			// Claude Code on Windows runs shell commands through a PowerShell tool as well; Codex calls every shell command Bash.
			file, note, matcher := ".claude/settings.local.json", "", "Bash|PowerShell"
			if t == TargetAgents {
				file, note, matcher = codexHooks, "Codex runs it in a trusted project after approval: /hooks", "Bash"
			}
			c, err := editJSON(root, file, func(o map[string]any) bool { return addHook(o, matcher) })
			if err != nil {
				return out, err
			}
			if c.Action != "unchanged" {
				c.Note = note
			}
			out = append(out, c)
		}
	}
	if opt.MCP {
		c, err := editJSON(root, ".mcp.json", addMCP)
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Uninstall removes everything Install can write, whatever options it was given: the marked blocks, the generated skills,
// the hook entry and the server entry. A file left empty by the removal is deleted; anything else is kept.
func Uninstall(root string) ([]Change, error) {
	var out []Change
	for _, t := range []Target{TargetClaude, TargetAgents} {
		c, err := editText(root, instructionFile(t), func(s string) string { r, _ := RemoveBlock(s); return r })
		if err != nil {
			return out, err
		}
		if c.Action != "unchanged" {
			out = append(out, c)
		}
		c, err = removeSkill(root, skillPath(t))
		if err != nil {
			return out, err
		}
		if c.Action != "unchanged" {
			out = append(out, c)
		}
	}
	for _, e := range []struct {
		file string
		edit func(map[string]any) bool
	}{{".claude/settings.local.json", removeHook}, {".claude/settings.json", removeHook}, {codexHooks, removeHook}, {".mcp.json", removeMCP}} {
		c, err := editJSON(root, e.file, e.edit)
		if err != nil {
			return out, err
		}
		if c.Action != "unchanged" {
			out = append(out, c)
		}
	}
	return out, nil
}

func instructionFile(t Target) string {
	if t == TargetClaude {
		return "CLAUDE.md"
	}
	return "AGENTS.md"
}

func skillPath(t Target) string {
	if t == TargetClaude {
		return ".claude/skills/" + SkillName + "/SKILL.md"
	}
	return ".agents/skills/" + SkillName + "/SKILL.md"
}

// ParseTargets parses a comma-separated target list ("claude,agents"); "" and "auto" mean the default (nil).
func ParseTargets(s string) ([]Target, error) {
	if s == "" || s == "auto" {
		return nil, nil
	}
	var ts []Target
	for _, p := range strings.Split(s, ",") {
		switch t := Target(strings.TrimSpace(p)); t {
		case TargetClaude, TargetAgents:
			ts = append(ts, t)
		default:
			return nil, fmt.Errorf("unknown target %q (want claude, agents or auto)", p)
		}
	}
	return ts, nil
}

func resolveTargets(root string, ts []Target) ([]Target, error) {
	if len(ts) > 0 {
		return ts, nil
	}
	for _, t := range []Target{TargetClaude, TargetAgents} {
		if _, err := os.Stat(filepath.Join(root, instructionFile(t))); err == nil {
			ts = append(ts, t)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	if len(ts) == 0 {
		ts = []Target{TargetClaude, TargetAgents}
	}
	return ts, nil
}

var agentsImport = regexp.MustCompile(`(?m)^\s*@\.?/?AGENTS\.md\s*$`)

func importsAgents(s string) bool { return agentsImport.MatchString(s) }

// editText applies edit to a text file (missing = empty). An empty result deletes the file.
func editText(root, rel string, edit func(string) string) (Change, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	b, err := os.ReadFile(p)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Change{}, err
	}
	old := string(b)
	s := edit(old)
	switch {
	case s == old:
		return Change{Path: rel, Action: "unchanged"}, nil
	case strings.TrimSpace(s) == "":
		if !exists {
			return Change{Path: rel, Action: "unchanged"}, nil
		}
		return Change{Path: rel, Action: "removed"}, removeFile(root, p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return Change{}, err
	}
	action := "updated"
	if !exists {
		action = "created"
	}
	return Change{Path: rel, Action: action}, os.WriteFile(p, []byte(s), 0o644)
}

func writeSkill(root, rel string) (Change, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	if b, err := os.ReadFile(p); err == nil && !strings.Contains(string(b), skillMarker) {
		return Change{Path: rel, Action: "skipped", Note: "a skill not written by bgraph has this name"}, nil
	}
	return editText(root, rel, func(string) string { return Skill() })
}

func removeSkill(root, rel string) (Change, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) || err == nil && !strings.Contains(string(b), skillMarker) {
		return Change{Path: rel, Action: "unchanged"}, nil
	}
	if err != nil {
		return Change{}, err
	}
	if err := os.Remove(p); err != nil {
		return Change{}, err
	}
	removeEmptyParents(root, p)
	return Change{Path: rel, Action: "removed"}, nil
}

// editJSON applies edit to a JSON object file (missing = {}); edit reports whether it changed the object. An object left
// empty is deleted.
func editJSON(root, rel string, edit func(map[string]any) bool) (Change, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	b, err := os.ReadFile(p)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Change{}, err
	}
	obj := map[string]any{}
	if exists && len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &obj); err != nil {
			return Change{}, fmt.Errorf("%s: %w", rel, err)
		}
	}
	if !edit(obj) {
		return Change{Path: rel, Action: "unchanged"}, nil
	}
	if len(obj) == 0 {
		if !exists {
			return Change{Path: rel, Action: "unchanged"}, nil
		}
		return Change{Path: rel, Action: "removed"}, removeFile(root, p)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep `&&` and `<` readable in hook commands
	enc.SetIndent("", "  ")
	if err := enc.Encode(obj); err != nil {
		return Change{}, err
	}
	nb := bytes.TrimRight(buf.Bytes(), "\n")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return Change{}, err
	}
	action := "updated"
	if !exists {
		action = "created"
	}
	return Change{Path: rel, Action: action}, os.WriteFile(p, append(nb, '\n'), 0o644)
}

func removeFile(root, p string) error {
	if err := os.Remove(p); err != nil {
		return err
	}
	removeEmptyParents(root, p)
	return nil
}

// removeEmptyParents removes the now-empty directories above p, up to (not including) root.
func removeEmptyParents(root, p string) {
	root = filepath.Clean(root)
	for d := filepath.Dir(p); d != root && strings.HasPrefix(d, root+string(filepath.Separator)); d = filepath.Dir(d) {
		if os.Remove(d) != nil { // fails when not empty
			return
		}
	}
}

// addHook adds {"matcher": matcher, "hooks": [{"type": "command", "command": HookCommand}]} to hooks.PostToolUse.
func addHook(obj map[string]any, matcher string) bool {
	hooks, _ := obj["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	post, _ := hooks["PostToolUse"].([]any)
	for _, e := range post {
		if isOurHook(e) {
			return false
		}
	}
	hooks["PostToolUse"] = append(post, map[string]any{
		"matcher": matcher,
		"hooks":   []any{map[string]any{"type": "command", "command": HookCommand, "timeout": 10}},
	})
	obj["hooks"] = hooks
	return true
}

func removeHook(obj map[string]any) bool {
	hooks, _ := obj["hooks"].(map[string]any)
	post, _ := hooks["PostToolUse"].([]any)
	kept := post[:0:0]
	for _, e := range post {
		if !isOurHook(e) {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(post) {
		return false
	}
	if len(kept) == 0 {
		delete(hooks, "PostToolUse")
	} else {
		hooks["PostToolUse"] = kept
	}
	if len(hooks) == 0 {
		delete(obj, "hooks")
	}
	return true
}

func isOurHook(e any) bool {
	m, _ := e.(map[string]any)
	hs, _ := m["hooks"].([]any)
	if len(hs) != 1 {
		return false
	}
	h, _ := hs[0].(map[string]any)
	return h["command"] == HookCommand
}

// addMCP adds mcpServers.bgraph = {"command": "bgraph", "args": ["mcp"]}. An existing bgraph entry is kept as it is.
func addMCP(obj map[string]any) bool {
	servers, _ := obj["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if _, ok := servers["bgraph"]; ok {
		return false
	}
	servers["bgraph"] = map[string]any{"command": "bgraph", "args": []any{"mcp"}}
	obj["mcpServers"] = servers
	return true
}

// removeMCP removes mcpServers.bgraph only when it is the entry addMCP writes (a customized entry is the user's).
func removeMCP(obj map[string]any) bool {
	servers, _ := obj["mcpServers"].(map[string]any)
	e, _ := servers["bgraph"].(map[string]any)
	args, _ := e["args"].([]any)
	if e == nil || e["command"] != "bgraph" || len(args) != 1 || args[0] != "mcp" || len(e) != 2 {
		return false
	}
	delete(servers, "bgraph")
	if len(servers) == 0 {
		delete(obj, "mcpServers")
	}
	return true
}

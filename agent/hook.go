package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Nudge is the context the hook adds after an agent ran a whole test suite. It is a statement, not an order: the agent decides.
const Nudge = "That command ran the whole test suite. For the next check of this change, `bgraph command` prints the commands " +
	"that run only the tests the change can affect (run them from the repository root); `# scope: full` in its output means " +
	"the whole suite is needed after all."

// HookInput is the part of a PostToolUse hook input the hook reads. Claude Code and Codex send the same fields; both name
// their shell tool "Bash" (Claude Code on Windows also has "PowerShell") and pass the command as a string.
type HookInput struct {
	SessionID string `json:"session_id"`
	Event     string `json:"hook_event_name"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

// Hook is the PostToolUse hook of Claude Code and Codex (`bgraph agent hook`): after a Bash call that ran a whole test suite, it adds Nudge
// to the agent's context, at most once per session.
type Hook struct {
	// StateDir holds one empty file per session that was already nudged. Empty: <os.TempDir()>/bgraph-agent-hook.
	StateDir string
}

// Handle reads a hook input and returns the hook output, or nil when there is nothing to say. It never fails the agent's
// call: an input it does not understand yields nil.
func (h Hook) Handle(input []byte) ([]byte, error) {
	var in HookInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, nil
	}
	if in.Event != "" && in.Event != "PostToolUse" || in.ToolName != "Bash" && in.ToolName != "PowerShell" || !IsFullSuiteCommand(in.ToolInput.Command) {
		return nil, nil
	}
	if in.SessionID != "" {
		first, err := h.firstTime(in.SessionID)
		if err != nil || !first {
			return nil, err
		}
	}
	return json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "PostToolUse",
		"additionalContext": Nudge,
	}})
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// firstTime records the session and reports whether it was not recorded before.
func (h Hook) firstTime(session string) (bool, error) {
	dir := h.StateDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "bgraph-agent-hook")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(filepath.Join(dir, unsafeName.ReplaceAllString(session, "_")), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, f.Close()
}

// IsFullSuiteCommand reports whether a shell command runs a whole test suite: `go test ./...`, `npm test`, `pytest` or
// `cargo test` without a filter, `mvn test`, `gradle test`, `dotnet test`, `rspec`, `phpunit`, `make test` and similar.
// A command that names test files, packages or a filter is not a whole-suite run (nor is a `bgraph ...` segment).
// It errs on the side of false: a missed nudge costs nothing.
func IsFullSuiteCommand(cmd string) bool {
	for _, seg := range splitSegments(cmd) {
		if fullSuiteSegment(strings.Fields(seg)) {
			return true
		}
	}
	return false
}

func splitSegments(cmd string) []string {
	r := strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n", "&", "\n", "(", "\n", ")", "\n")
	return strings.Split(r.Replace(cmd), "\n")
}

var envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

func fullSuiteSegment(f []string) bool {
	// Drop environment assignments and command wrappers.
	for len(f) > 0 {
		switch {
		case envAssign.MatchString(f[0]):
			f = f[1:]
			continue
		case f[0] == "time" || f[0] == "env" || f[0] == "npx" || f[0] == "bunx" || f[0] == "command":
			f = f[1:]
			continue
		case len(f) > 1 && (f[0] == "uv" || f[0] == "poetry" || f[0] == "pdm" || f[0] == "hatch" || f[0] == "pipenv") && f[1] == "run",
			len(f) > 1 && (f[0] == "bundle" || f[0] == "pnpm") && f[1] == "exec",
			len(f) > 1 && f[0] == "yarn" && f[1] == "exec":
			f = f[2:]
			continue
		case len(f) > 2 && isPython(f[0]) && f[1] == "-m":
			f = f[2:]
			continue
		}
		break
	}
	if len(f) == 0 {
		return false
	}
	name := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(strings.ReplaceAll(f[0], `\`, "/")), ".bat"), ".cmd")
	args := f[1:]
	switch name {
	case "go":
		if len(args) == 0 || args[0] != "test" {
			return false
		}
		return has(args[1:], "./...") && !hasFlag(args[1:], "-run")
	case "npm", "pnpm", "yarn", "bun":
		if len(args) > 0 && args[0] == "run" {
			args = args[1:]
		}
		if len(args) == 0 || (args[0] != "test" && args[0] != "t") {
			return false
		}
		if name == "bun" && len(args) > 0 && args[0] == "test" {
			return noPositional(args[1:])
		}
		return noPositional(trimDashDash(args[1:]))
	case "pytest", "py.test":
		return noPositional(args) && !hasFlag(args, "-k")
	case "cargo":
		if len(args) > 0 && args[0] == "nextest" {
			args = args[1:]
			if len(args) == 0 || args[0] != "run" {
				return false
			}
		} else if len(args) == 0 || args[0] != "test" {
			return false
		}
		before, _ := cutDashDash(args[1:])
		return noPositional(before) && !hasFlag(before, "-p") && !hasFlag(before, "--package") && !hasFlag(before, "--test")
	case "mvn", "mvnw":
		return (has(args, "test") || has(args, "verify")) && !hasPrefix(args, "-Dtest=") && !hasFlag(args, "-pl") && !hasFlag(args, "--projects")
	case "gradle", "gradlew":
		return (has(args, "test") || has(args, "check")) && !hasFlag(args, "--tests")
	case "dotnet":
		if len(args) == 0 || args[0] != "test" {
			return false
		}
		for _, a := range args[1:] {
			if !strings.HasPrefix(a, "-") && !strings.HasSuffix(a, ".sln") && !strings.HasSuffix(a, ".slnx") {
				return false
			}
		}
		return !hasFlag(args[1:], "--filter")
	case "vitest":
		if len(args) > 0 && args[0] == "run" {
			args = args[1:]
		}
		return noPositional(args) && !hasFlag(args, "-t") && !hasFlag(args, "--testNamePattern")
	case "jest", "mocha", "rspec", "phpunit", "pest":
		return noPositional(args) && !hasFlag(args, "-t") && !hasFlag(args, "--testNamePattern") && !hasFlag(args, "--filter") &&
			!hasFlag(args, "--grep") && !hasFlag(args, "-e")
	case "rake", "rails":
		return len(args) == 1 && (args[0] == "test" || args[0] == "spec")
	case "make", "just":
		return len(args) == 1 && (args[0] == "test" || args[0] == "check")
	case "tox", "nox":
		return true
	}
	return false
}

func isPython(s string) bool {
	b := filepath.Base(strings.ReplaceAll(s, `\`, "/"))
	return b == "python" || b == "python3" || b == "py" || strings.HasPrefix(b, "python3.")
}

func has(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

func hasPrefix(args []string, p string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, p) {
			return true
		}
	}
	return false
}

// hasFlag matches `flag`, `flag=value` and `flagvalue` for a single-dash short flag.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") || (len(flag) == 2 && flag[0] == '-' && strings.HasPrefix(a, flag) && !strings.HasPrefix(a, "--")) {
			return true
		}
	}
	return false
}

// noPositional: no argument that is not a flag. A flag value that looks positional (`-n auto`) makes it false: no nudge.
func noPositional(args []string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return false
		}
	}
	return true
}

func cutDashDash(args []string) (before, after []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func trimDashDash(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}
	return args
}

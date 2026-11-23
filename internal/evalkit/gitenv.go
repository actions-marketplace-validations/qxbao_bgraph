// Package evalkit is bgraph's external evaluation kit (docs/evaluation.md): the hard T1 oracle (FAIL_TO_PASS) and commit-history replay
// with the RTSCheck invariants. The oracle does NOT depend on bgraph: it only uses git and file contents.
package evalkit

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SetupHermeticEnv sets up a hermetic git environment for this process and for every git that bgraph launches: no system/
// user config, fixed author, core.autocrlf=false, core.longpaths=true, core.protectNTFS=false, no password prompts. emptyCfgDir holds an empty config file.
func SetupHermeticEnv(emptyCfgDir string) error {
	if err := os.MkdirAll(emptyCfgDir, 0o755); err != nil {
		return err
	}
	cfg := filepath.Join(emptyCfgDir, "empty.gitconfig")
	if err := os.WriteFile(cfg, nil, 0o644); err != nil {
		return err
	}
	kv := map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   cfg,
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_AUTHOR_NAME":     "bgraph-eval",
		"GIT_AUTHOR_EMAIL":    "eval@example.invalid",
		"GIT_AUTHOR_DATE":     "2020-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME":  "bgraph-eval",
		"GIT_COMMITTER_EMAIL": "eval@example.invalid",
		"GIT_COMMITTER_DATE":  "2020-01-01T00:00:00Z",
		"GIT_CONFIG_COUNT":    "5",
		"GIT_CONFIG_KEY_0":    "core.autocrlf",
		"GIT_CONFIG_VALUE_0":  "false",
		"GIT_CONFIG_KEY_1":    "core.longpaths",
		"GIT_CONFIG_VALUE_1":  "true",
		"GIT_CONFIG_KEY_2":    "gc.auto",
		"GIT_CONFIG_VALUE_2":  "0",
		// Trees are built with plumbing only (temporary index, never checked out): Windows-invalid names such as "a:0" must not abort read-tree.
		"GIT_CONFIG_KEY_3":   "core.protectNTFS",
		"GIT_CONFIG_VALUE_3": "false",
		"GIT_CONFIG_KEY_4":   "maintenance.auto",
		"GIT_CONFIG_VALUE_4": "false",
	}
	for k, v := range kv {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

// git runs git in dir and returns stdout. The error contains stderr.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	return gitIn(ctx, dir, nil, "", args...)
}

// gitIn is like git but adds environment variables env (KEY=VALUE) and feeds stdin (if non-empty).
func gitIn(ctx context.Context, dir string, env []string, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 600 {
			msg = msg[:600]
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out.String(), nil
}

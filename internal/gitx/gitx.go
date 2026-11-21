// Package gitx is a thin, fast wrapper around the git CLI. Each git.exe spawn on Windows costs ~20–25 ms, so the hot path
// (Snapshot) uses only 3 parallel processes, plus one long-lived `git cat-file --batch` to read blobs of the base commit.
// No shell, no go-git, no cgo.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Repo is an open git repository. Safe for concurrent use from multiple goroutines.
type Repo struct {
	// Root is the absolute working directory root, in OS-native form.
	Root string

	spawns atomic.Int64
	cat    catFile
}

// Open finds the working root of the repo containing dir (git rev-parse --show-toplevel).
func Open(ctx context.Context, dir string) (*Repo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	r := &Repo{Root: abs}
	r.cat.r = r
	out, err := r.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	top := strings.TrimRight(string(out), "\r\n")
	if top == "" {
		return nil, fmt.Errorf("gitx: %s has no working directory (bare repo?)", dir)
	}
	r.Root = filepath.Clean(filepath.FromSlash(top))
	return r, nil
}

// Close stops the cat-file process if any. Safe to call multiple times.
func (r *Repo) Close() error { return r.cat.close() }

// Spawns returns the number of git processes launched through this Repo (used by performance tests).
func (r *Repo) Spawns() int64 { return r.spawns.Load() }

// gitEnv: inherits the environment, disables optional locks (no index rewrite on read-only use) and never prompts for passwords.
// Locale is not changed: parsing does not depend on messages.
func gitEnv() []string {
	return append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
}

func baseArgs(args []string) []string {
	return append([]string{"-c", "core.quotepath=off"}, args...)
}

func describe(args []string) string {
	s := "git " + strings.Join(args, " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// run executes a git command and returns stdout. Errors include the command and trimmed stderr.
func (r *Repo) run(ctx context.Context, args ...string) ([]byte, error) {
	out, _, err := r.runRaw(ctx, args...)
	return out, err
}

// runRaw is like run but also returns stderr (so callers can distinguish exit code 1 "no result").
func (r *Repo) runRaw(ctx context.Context, args ...string) (stdout, stderr []byte, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, "git", baseArgs(args)...)
	cmd.Dir = r.Root
	cmd.Env = gitEnv()
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	r.spawns.Add(1)
	if err := cmd.Run(); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			err = cerr
		}
		return nil, se.Bytes(), fmt.Errorf("gitx: %s: %w: %s", describe(args), err, strings.TrimSpace(se.String()))
	}
	return so.Bytes(), se.Bytes(), nil
}

// RevParse resolves rev to a full commit OID.
func (r *Repo) RevParse(ctx context.Context, rev string) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// MergeBase returns the merge-base of a and b; an error if there is none.
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	out, se, err := r.runRaw(ctx, "merge-base", "--end-of-options", a, b)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 && len(bytes.TrimSpace(se)) == 0 {
			return "", fmt.Errorf("gitx: no merge-base between %q and %q", a, b)
		}
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("gitx: no merge-base between %q and %q", a, b)
	}
	return s, nil
}

// IsShallow reports whether the repo is a shallow clone (history truncated).
func (r *Repo) IsShallow(ctx context.Context) (bool, error) {
	out, err := r.run(ctx, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(string(out)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("gitx: unparseable --is-shallow-repository output: %q", out)
}

// isHexOID: the string is a full OID (40 lowercase hex chars for sha1 or 64 for sha256).
func isHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

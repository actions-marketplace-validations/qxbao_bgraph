// Package pydyn is the Python part of the dynamic layer (docs/dynamic.md §4, §5): the environment fingerprint and the
// audit-hook collector behind `bgraph record`.
package pydyn

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/eco/python"
	"github.com/qxbao/bgraph/internal/tree"
)

// CollectorName is the collector's name and version: bump the number whenever what the collector observes changes
// (records of other versions are not used).
const CollectorName = "python-audit-1"

// Collector returns the collector ID of the current configuration: records made with trust_native_env carry a suffix so
// that they are not used under a stricter configuration (and the other way round).
func Collector(trustNativeEnv bool) string {
	if trustNativeEnv {
		return CollectorName + "+trust-native"
	}
	return CollectorName
}

//go:embed envinfo.py
var envScript []byte

// ResolvePython turns the configured interpreter into an executable path: a name without separator is looked up on PATH,
// anything else is relative to the repo root. Empty → "python".
func ResolvePython(root, configured string) (string, error) {
	if configured == "" {
		configured = "python"
	}
	if !strings.ContainsAny(configured, `/\`) {
		return exec.LookPath(configured)
	}
	p := configured
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, filepath.FromSlash(p))
	}
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

// pyEnv is the process environment for every Python run of the dynamic layer.
func pyEnv(extra ...string) []string {
	return append(append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONDONTWRITEBYTECODE=1"), extra...)
}

// Interp describes an interpreter: Lines enter the fingerprint; Sites are the sys.path directories (machine specific, not
// fingerprinted) whose modification times invalidate a cached probe.
type Interp struct {
	Lines []string
	Sites []string
}

// Probe runs the interpreter once and describes it (see envinfo.py). tmpDir is where the script is written (in a fresh
// subdirectory, removed afterwards). cwd is the directory it runs from (the repo root, like the tests).
func Probe(ctx context.Context, exe, cwd, tmpDir string) (Interp, error) {
	var in Interp
	dir, err := os.MkdirTemp(tmpDir, "bgraph-env-")
	if err != nil {
		return in, err
	}
	defer os.RemoveAll(dir)
	script := filepath.Join(dir, "bgraph_envinfo.py")
	if err := os.WriteFile(script, envScript, 0o644); err != nil {
		return in, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, script)
	cmd.Dir = cwd
	cmd.Env = pyEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		return in, fmt.Errorf("%s: %v %s", exe, err, msg)
	}
	for _, l := range strings.Split(strings.ReplaceAll(out.String(), "\r\n", "\n"), "\n") {
		switch {
		case l == "":
		case strings.HasPrefix(l, "site="):
			in.Sites = append(in.Sites, strings.TrimPrefix(l, "site="))
		default:
			in.Lines = append(in.Lines, l)
		}
	}
	if len(in.Lines) == 0 {
		return in, errors.New(exe + ": no environment description")
	}
	return in, nil
}

// EnvFiles returns, for every pytest configuration file of t (python.RunnerConfigFile), a digest of the part that configures
// pytest (python.RunnerConfigPart). Manifests and lockfiles are not included: the installed distributions are.
func EnvFiles(t tree.Tree) map[string]string {
	out := map[string]string{}
	for _, p := range t.Files() {
		if !python.RunnerConfigFile(p) {
			continue
		}
		b, err := t.Read(p)
		if err != nil {
			out[p] = "unreadable:" + t.Key(p)
			continue
		}
		h := sha256.Sum256([]byte(python.RunnerConfigPart(p, b)))
		out[p] = hex.EncodeToString(h[:])
	}
	return out
}

// Fingerprint is the environment fingerprint of docs/dynamic.md §5 for interpreter description interp and tree t (the base
// at selection time, the recorded commit at record time).
func Fingerprint(collector string, interp []string, t tree.Tree) string {
	return dynamic.Fingerprint(collector, interp, EnvFiles(t))
}

// ProbeCache caches Probe per executable path and modification time; an entry is also dropped when the modification time
// of one of the interpreter's sys.path directories changes (a package was installed or removed there).
type ProbeCache struct {
	mu sync.Mutex
	m  map[string]cached
}

type cached struct {
	in    Interp
	sites string
}

func mtimes(paths ...string) string {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString(p)
		if st, err := os.Stat(p); err == nil {
			b.WriteString("=" + st.ModTime().UTC().Format(time.RFC3339Nano))
		} else {
			b.WriteString("=?")
		}
		b.WriteByte(0)
	}
	return b.String()
}

// Get returns the description of exe, probing it when its path or mtime (or that of a sys.path directory) is new.
func (c *ProbeCache) Get(ctx context.Context, exe, cwd, tmpDir string) (Interp, error) {
	if _, err := os.Stat(exe); err != nil {
		return Interp{}, err
	}
	key := mtimes(exe)
	c.mu.Lock()
	if v, ok := c.m[key]; ok && mtimes(v.in.Sites...) == v.sites {
		c.mu.Unlock()
		return v.in, nil
	}
	c.mu.Unlock()
	v, err := Probe(ctx, exe, cwd, tmpDir)
	if err != nil {
		return Interp{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]cached{}
	}
	c.m[key] = cached{v, mtimes(v.Sites...)}
	return v, nil
}

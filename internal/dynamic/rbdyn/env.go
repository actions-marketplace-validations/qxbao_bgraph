// Package rbdyn is the Ruby part of the dynamic layer (docs/dynamic-layer.md §10): per-test-file records of the methods each
// test file executed (Ruby's Coverage in methods mode, one process tree per test file), the files it read, and the guards and
// narrowing that remove a selected Ruby test file when none of the changed methods ran in its recording.
package rbdyn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/tree"
)

// Eco is the ecosystem name of the records.
const Eco = "ruby"

// CollectorID identifies the collector and its record format (docs/dynamic-layer.md §10.3): bump it whenever what the collector
// observes changes (records of other versions are not used).
const CollectorID = "ruby-cov-1"

// ResolveRuby turns the configured Ruby into an executable path: a name without separator is looked up on PATH, anything else
// is relative to the repo root. Empty: "ruby".
func ResolveRuby(root, configured string) (string, error) {
	if configured == "" {
		configured = "ruby"
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

// withoutRubyEnv returns the process environment without the variables that make Ruby load code at startup.
func withoutRubyEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "RUBYOPT", "RUBYLIB", "BGRAPH_RB_OUT", "BGRAPH_RB_ROOT":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Probe returns RUBY_DESCRIPTION of the Ruby executable exe (run from an empty temporary directory, without RUBYOPT / RUBYLIB).
func Probe(ctx context.Context, exe string) (string, error) {
	dir, err := os.MkdirTemp("", "bgraph-rbenv-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-e", "print RUBY_DESCRIPTION")
	cmd.Dir = dir
	cmd.Env = withoutRubyEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %v %s", exe, err, strings.TrimSpace(errb.String()))
	}
	d := strings.TrimSpace(out.String())
	if d == "" {
		return "", errors.New(exe + ": no RUBY_DESCRIPTION")
	}
	return d, nil
}

// ProbeCache caches Probe per executable path and modification time.
type ProbeCache struct {
	mu sync.Mutex
	m  map[string]string
}

// Get returns RUBY_DESCRIPTION of exe, probing it when its path or modification time is new.
func (c *ProbeCache) Get(ctx context.Context, exe string) (string, error) {
	st, err := os.Stat(exe)
	if err != nil {
		return "", err
	}
	key := exe + "\x00" + st.ModTime().UTC().Format(time.RFC3339Nano)
	c.mu.Lock()
	if v, ok := c.m[key]; ok {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	v, err := Probe(ctx, exe)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]string{}
	}
	c.m[key] = v
	return v, nil
}

// dataDir: Gemfiles below these directories are test data, not projects (internal/eco/ruby).
func dataDir(p string) bool {
	for _, s := range strings.Split(path.Dir(p), "/") {
		switch s {
		case "spec", "test", "tests", "fixtures", "fixture", "vendor", "tmp", "node_modules", ".bundle":
			return true
		}
	}
	return false
}

// envVars are the environment variables that change how a Ruby test process loads code (besides the lockfiles).
var envVars = []string{"RUBYOPT", "RUBYLIB", "BUNDLE_GEMFILE", "BUNDLE_WITHOUT", "BUNDLE_WITH", "BUNDLE_ONLY", "BUNDLE_FROZEN",
	"BUNDLE_DEPLOYMENT"}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// EnvParts returns the parts of the environment fingerprint (docs/dynamic-layer.md §10.5) besides the Ruby description: the
// environment variables that change loading, and, per Ruby project of t (a directory with a Gemfile or gems.rb outside test
// data), the digest of its lockfile as it is on disk under root (Gemfile.lock / gems.locked: gems often do not commit it, the
// tests ran with the one on disk), plus .ruby-version and .bundle/config files on disk. A project without a lockfile on disk has
// no fingerprint (its gem set is not pinned).
func EnvParts(root string, t tree.Tree) (vars []string, files map[string]string, err error) {
	for _, k := range envVars {
		v, ok := os.LookupEnv(k)
		if !ok {
			continue
		}
		if k == "BUNDLE_GEMFILE" {
			if rel, rerr := filepath.Rel(root, v); rerr == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
				v = "<repo>/" + filepath.ToSlash(rel)
			}
		}
		vars = append(vars, k+"="+v)
	}
	files = map[string]string{}
	dirs := map[string]bool{}
	for _, p := range t.Files() {
		b := path.Base(p)
		if (b == "Gemfile" || b == "gems.rb") && !dataDir(p) {
			dirs[path.Dir(p)] = true
		}
	}
	if len(dirs) == 0 {
		return nil, nil, errors.New("no Gemfile: the gem set of the tests is not pinned by a lockfile")
	}
	read := func(rel string) ([]byte, bool) {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		return b, err == nil
	}
	for d := range dirs {
		found := false
		for _, n := range []string{"Gemfile.lock", "gems.locked"} {
			rel := path.Join(d, n)
			if b, ok := read(rel); ok {
				files[rel] = digest(b)
				found = true
			}
		}
		if !found {
			return nil, nil, fmt.Errorf("%s: no Gemfile.lock or gems.locked on disk (run bundle install): the gem set is not pinned", path.Join(d, "Gemfile"))
		}
		for _, n := range []string{".ruby-version", ".bundle/config"} {
			rel := path.Join(d, n)
			if b, ok := read(rel); ok {
				files[rel] = digest(b)
			}
		}
	}
	if g := os.Getenv("BUNDLE_GEMFILE"); g != "" {
		lock := g + ".lock"
		if filepath.Base(g) == "gems.rb" {
			lock = filepath.Join(filepath.Dir(g), "gems.locked")
		}
		b, err := os.ReadFile(lock)
		if err != nil {
			return nil, nil, fmt.Errorf("BUNDLE_GEMFILE=%s: no lockfile on disk: the gem set is not pinned", g)
		}
		files["<BUNDLE_GEMFILE lock>"] = digest(b)
	}
	return vars, files, nil
}

// Fingerprint is the environment fingerprint of docs/dynamic-layer.md §10.5: the collector, OS and architecture, the Ruby
// description, and EnvParts of tree t (the base at selection time, the recorded commit at record time).
func Fingerprint(desc, root string, t tree.Tree) (string, error) {
	vars, files, err := EnvParts(root, t)
	if err != nil {
		return "", err
	}
	interp := append([]string{"ruby " + desc, "os " + runtime.GOOS + "/" + runtime.GOARCH}, vars...)
	return dynamic.Fingerprint(CollectorID, interp, files), nil
}

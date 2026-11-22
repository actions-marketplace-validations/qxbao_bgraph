// Package nodedyn is the Node part of the dynamic layer (docs/dynamic.md §8): the --require collector behind `bgraph record`,
// the merging of its per-process logs, and the environment fingerprint.
package nodedyn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/eco/node"
	"github.com/qxbao/bgraph/internal/tree"
)

// CollectorName is the collector's name and version: bump the number whenever what the collector observes changes.
const CollectorName = "node-hook-1"

// Collector returns the collector ID of the current configuration (trust_native_env records carry a suffix).
func Collector(trustNativeEnv bool) string {
	if trustNativeEnv {
		return CollectorName + "+trust-native"
	}
	return CollectorName
}

// ResolveNode turns the configured executable into a path: a name without separator is looked up on PATH, anything else is
// relative to the repo root. Empty → "node".
func ResolveNode(root, configured string) (string, error) {
	if configured == "" {
		configured = "node"
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

const probeScript = `const v=process.versions;console.log(["node "+process.version,"modules "+v.modules,"napi "+(v.napi||""),"platform "+process.platform+" "+process.arch].join("\n"))`

// Probe runs node once and describes it (version, module ABI, N-API, platform).
func Probe(ctx context.Context, exe, cwd string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-e", probeScript)
	cmd.Dir = cwd
	var env []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, "NODE_OPTIONS") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %v %s", exe, err, strings.TrimSpace(errb.String()))
	}
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(out.String(), "\r\n", "\n"), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return nil, errors.New(exe + ": no environment description")
	}
	return lines, nil
}

// stateFiles are the package managers' records of the installed tree, in order of preference (docs/dynamic.md §8.4).
var stateFiles = []string{".package-lock.json", ".pnpm/lock.yaml", ".yarn-state.yml", ".yarn-integrity"}

// projectDirs: the repository root and every directory of t with a package.json outside node_modules.
func projectDirs(t tree.Tree) []string {
	dirs := []string{"."}
	for _, p := range t.Files() {
		if path.Base(p) == "package.json" && !node.InNodeModules(p) {
			dirs = append(dirs, path.Dir(p))
		}
	}
	slices.Sort(dirs)
	return slices.Compact(dirs)
}

// Installed describes the installed packages of every project directory of t that has a node_modules on disk (under root):
// the digest of the package manager's installed-state file, or the sorted name@version of the top-level packages.
// Files are the state files read (their modification times invalidate a cached fingerprint).
func Installed(root string, t tree.Tree) (lines, files []string, err error) {
	for _, d := range projectDirs(t) {
		nm := filepath.Join(root, filepath.FromSlash(d), "node_modules")
		st, err := os.Stat(nm)
		if err != nil || !st.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(d), ".pnp.cjs")); err == nil {
			return nil, nil, errors.New("Yarn Plug'n'Play is not supported by the Node collector")
		}
		found := false
		for _, sf := range stateFiles {
			p := filepath.Join(nm, filepath.FromSlash(sf))
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			h := sha256.Sum256(b)
			lines = append(lines, fmt.Sprintf("installed %s %s %s", d, sf, hex.EncodeToString(h[:])))
			files = append(files, p)
			found = true
			break
		}
		if !found {
			pkgs := topPackages(nm)
			h := sha256.Sum256([]byte(strings.Join(pkgs, "\n")))
			lines = append(lines, fmt.Sprintf("installed %s packages %s", d, hex.EncodeToString(h[:])))
			files = append(files, nm)
		}
	}
	return lines, files, nil
}

// topPackages lists name@version of node_modules/*/package.json and node_modules/@*/*/package.json.
func topPackages(nm string) []string {
	var out []string
	read := func(dir string) {
		b, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			return
		}
		var p struct{ Name, Version string }
		if json.Unmarshal(b, &p) == nil {
			out = append(out, p.Name+"@"+p.Version)
		}
	}
	ents, _ := os.ReadDir(nm)
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".") {
			continue
		}
		if strings.HasPrefix(n, "@") {
			sub, _ := os.ReadDir(filepath.Join(nm, n))
			for _, s := range sub {
				read(filepath.Join(nm, n, s.Name()))
			}
			continue
		}
		read(filepath.Join(nm, n))
	}
	slices.Sort(out)
	return out
}

// installKeys are package.json keys that only concern installation or publication (dropped from its fingerprint digest), with
// the harmless keys of the Node ClassifyChange.
var installKeys = map[string]bool{"dependencies": true, "devDependencies": true, "peerDependencies": true, "peerDependenciesMeta": true,
	"optionalDependencies": true, "bundleDependencies": true, "bundledDependencies": true, "overrides": true, "resolutions": true,
	"pnpm": true, "scripts": true, "engines": true, "packageManager": true, "files": true, "devEngines": true,
	"version": true, "description": true, "keywords": true, "author": true, "contributors": true, "homepage": true, "repository": true,
	"bugs": true, "license": true, "funding": true, "maintainers": true, "publishConfig": true}

// PackageJSONPart is the canonical JSON of a package.json without its installation/publication keys (the whole content when it
// does not parse).
func PackageJSONPart(b []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return string(b)
	}
	keep := map[string]any{}
	for k, v := range m {
		if installKeys[k] {
			continue
		}
		var x any
		if json.Unmarshal(v, &x) != nil {
			return string(b)
		}
		keep[k] = x
	}
	out, _ := json.Marshal(keep) // map keys are sorted
	return string(out)
}

// EnvFiles returns the configuration digests of tree t that enter the fingerprint (docs/dynamic.md §8.4).
func EnvFiles(t tree.Tree) map[string]string {
	out := map[string]string{}
	for _, p := range t.Files() {
		isPkg := path.Base(p) == "package.json" && !node.InNodeModules(p)
		if !isPkg && !node.EnvConfigFile(p) {
			continue
		}
		b, err := t.Read(p)
		if err != nil {
			out[p] = "unreadable:" + t.Key(p)
			continue
		}
		if isPkg {
			b = []byte(PackageJSONPart(b))
		}
		h := sha256.Sum256(b)
		out[p] = hex.EncodeToString(h[:])
	}
	return out
}

// Fingerprint is the environment fingerprint of docs/dynamic.md §8.4.
func Fingerprint(collector string, probe, installed []string, t tree.Tree) string {
	return dynamic.Fingerprint(collector, append(slices.Clone(probe), installed...), EnvFiles(t))
}

// ProbeCache caches Probe per executable path and modification time.
type ProbeCache struct {
	mu sync.Mutex
	m  map[string][]string
}

// Get returns the description of exe, probing it when its path or modification time is new.
func (c *ProbeCache) Get(ctx context.Context, exe, cwd string) ([]string, error) {
	st, err := os.Stat(exe)
	if err != nil {
		return nil, err
	}
	key := exe + "\x00" + st.ModTime().UTC().Format(time.RFC3339Nano)
	c.mu.Lock()
	if v, ok := c.m[key]; ok {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	v, err := Probe(ctx, exe, cwd)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string][]string{}
	}
	c.m[key] = v
	return v, nil
}

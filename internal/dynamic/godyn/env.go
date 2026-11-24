// Package godyn is the Go part of the dynamic layer: per-test-function records of what each top-level test function of a Go
// package executed (a test binary built once per package with `go test -c -cover -covermode=atomic`, every in-repository package
// plus os and syscall instrumented, each function run alone in a fresh process tree with GOCOVERDIR), a function-list record per
// package, and the guards and narrowing that run only the test functions of a selected Go package that executed a changed
// function. Compilation is never narrowed: a package with no kept function still runs `go test -run '^$'`.
package godyn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
)

// Eco is the ecosystem name of the records (internal/eco/golang.Name).
const Eco = "go"

// CollectorID identifies the collector and its record format: bump it whenever what the collector observes or writes changes
// (records of other versions are not used).
const CollectorID = "go-cover-1"

// ResolveGo turns the configured go command into an executable path: a name without separator is looked up on PATH, anything
// else is relative to the repo root. Empty: "go".
func ResolveGo(root, configured string) (string, error) {
	if configured == "" {
		configured = "go"
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

// probeVars are the `go env` values that change what a test binary is built from or how it runs. GOVERSION is the toolchain
// the go command selected (after GOTOOLCHAIN switching) for the repository root.
var probeVars = []string{"GOVERSION", "GOOS", "GOARCH", "GOAMD64", "GOARM", "GOARM64", "GO386", "GOMIPS", "GOMIPS64", "GOPPC64",
	"GORISCV64", "GOWASM", "CGO_ENABLED", "GOEXPERIMENT", "GOFLAGS", "GOFIPS140", "GOROOT"}

// cgoVars are added when cgo is enabled.
var cgoVars = []string{"CC", "CXX", "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS", "CGO_FFLAGS", "PKG_CONFIG"}

// Probe is what `go env -json` reports about the go command, run from the repository root.
type Probe struct {
	Vars map[string]string
}

// Get returns a variable of the probe ("" when unset).
func (p Probe) Get(k string) string { return p.Vars[k] }

// Lines returns the fingerprint lines of the probe. GOROOT is left out (the same toolchain may be installed elsewhere); the
// toolchain version and target are in.
func (p Probe) Lines() []string {
	keys := append([]string(nil), probeVars...)
	if p.Vars["CGO_ENABLED"] == "1" {
		keys = append(keys, cgoVars...)
	}
	var out []string
	for _, k := range keys {
		if k == "GOROOT" {
			continue
		}
		out = append(out, "go "+k+"="+p.Vars[k])
	}
	return out
}

// RunProbe runs `go env -json` with exe from dir.
func RunProbe(ctx context.Context, exe, dir string) (Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	args := append([]string{"env", "-json"}, probeVars...)
	args = append(args, cgoVars...)
	args = append(args, "GOMODCACHE") // not fingerprinted: where module sources live, not what they are
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return Probe{}, fmt.Errorf("%s env: %v %s", exe, err, strings.TrimSpace(errb.String()))
	}
	var vars map[string]string
	if err := json.Unmarshal(out.Bytes(), &vars); err != nil || vars["GOVERSION"] == "" {
		return Probe{}, fmt.Errorf("%s env: unexpected output %q", exe, firstN(out.String(), 200))
	}
	return Probe{Vars: vars}, nil
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ProbeCache caches RunProbe per executable path, modification time and directory.
type ProbeCache struct {
	mu sync.Mutex
	m  map[string]Probe
}

// Get returns the probe of exe run from dir, running it when its path or modification time is new.
func (c *ProbeCache) Get(ctx context.Context, exe, dir string) (Probe, error) {
	st, err := os.Stat(exe)
	if err != nil {
		return Probe{}, err
	}
	key := exe + "\x00" + st.ModTime().UTC().Format(time.RFC3339Nano) + "\x00" + dir
	c.mu.Lock()
	if v, ok := c.m[key]; ok {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	v, err := RunProbe(ctx, exe, dir)
	if err != nil {
		return Probe{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]Probe{}
	}
	c.m[key] = v
	return v, nil
}

// envVars are the environment variables that change how the go command builds or runs a test binary. Network settings
// (GOPROXY, GONOSUMDB...) are left out: they do not change a build whose go.sum is fixed.
var envVars = []string{"GOFLAGS", "GOEXPERIMENT", "GODEBUG", "CGO_ENABLED", "GOOS", "GOARCH", "GOWORK", "GO111MODULE",
	"GOTOOLCHAIN", "GOFIPS140"}

// envVarLines returns the environment variables of the fingerprint.
func envVarLines() []string {
	var out []string
	for _, k := range envVars {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, "env "+k+"="+v)
		}
	}
	sort.Strings(out)
	return out
}

// Fingerprint is the environment fingerprint of the Go layer: the collector, the go probe, the environment variables that change
// builds, and the recording's extra build arguments. Selection uses no build arguments (bgraph's `go test` commands carry none),
// so records made with --build-args are usable only when the same arguments reach every run through GOFLAGS instead. Module
// dependency state is in each record's dependencies (go.mod, go.sum), whose changes the guard refuses.
func Fingerprint(p Probe, buildArgs []string) string {
	lines := append(p.Lines(), envVarLines()...)
	if len(buildArgs) > 0 {
		lines = append(lines, "build-args "+strings.Join(buildArgs, "\x1f"))
	}
	return dynamic.Fingerprint(CollectorID, lines, nil)
}

// GOFLAGS returns the GOFLAGS the go command and the test processes of a recording see: the process environment's, then the
// recording's extra variables (the last setting wins, as in the child's environment).
func GOFLAGS(extra []string) string {
	v := os.Getenv("GOFLAGS")
	for _, kv := range extra {
		if k, val, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "GOFLAGS") {
			v = val
		}
	}
	return v
}

// coverFlag reports whether a build argument or GOFLAGS entry sets coverage itself.
func coverFlag(a string) bool {
	a = strings.TrimLeft(a, "-")
	name, _, _ := strings.Cut(a, "=")
	switch name {
	case "cover", "coverpkg", "covermode", "coverprofile", "test.coverprofile", "test.gocoverdir":
		return true
	}
	return false
}

// CheckFlags returns why the recording cannot run with these build arguments and GOFLAGS ("" when it can): the collector sets
// -cover, -covermode and -coverpkg itself.
func CheckFlags(buildArgs []string, goflags string) string {
	for _, a := range buildArgs {
		if coverFlag(a) {
			return "--build-args sets " + a + ": the collector sets -cover, -covermode and -coverpkg itself"
		}
	}
	for _, a := range strings.Fields(goflags) {
		if coverFlag(a) {
			return "GOFLAGS sets " + a + ": the collector sets -cover, -covermode and -coverpkg itself"
		}
	}
	return ""
}

package nodedyn

import (
	"bufio"
	"encoding/json"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// event is one line of a collector log (bgraph_collect.cjs, bgraph_hooks.mjs).
type event struct {
	Ev     string   `json:"ev"`
	Path   string   `json:"path"`
	URL    string   `json:"url"`
	Caller string   `json:"caller"`
	Haste  bool     `json:"haste"`
	Pid    int      `json:"pid"`
	Ppid   int      `json:"ppid"`
	Tid    int      `json:"tid"`
	Parent string   `json:"parent"`
	Token  string   `json:"token"`
	Sync   bool     `json:"sync"`
	File   string   `json:"file"`
	Args   []string `json:"args"`
	Dirs   []string `json:"dirs"`
	Cwd    string   `json:"cwd"`
	Msg    string   `json:"msg"`
}

// Analysis is what the logs of one unit's run say (docs/dynamic.md §8.2, §8.3).
type Analysis struct {
	Files      []string // repo-relative files loaded, read/opened (any mode) or stat'ed by repository code
	Dirs       []string // repo-relative directories listed by repository code ("." is the root)
	Toolchain  []string // allowlisted native addons loaded, "name@version" (sorted)
	Incomplete []string // why the collector could not see everything
}

// toolchainAddons are the packages whose native addons are accepted without trust_native_env (docs/dynamic.md §8.3): they parse
// or transform contents handed to them by JS, resolve paths, bundle the runner configuration (§8.3 rule 4), or watch files.
var toolchainAddons = []string{
	"@rollup/rollup-*", "@rolldown/binding-*", "rolldown",
	"@oxc-parser/binding-*", "@oxc-transform/binding-*", "@oxc-resolver/binding-*", "oxc-parser", "oxc-transform", "oxc-resolver",
	"@unrs/resolver-binding-*", "unrs-resolver",
	"@swc/core-*", "@swc/core",
	"lightningcss-*", "lightningcss",
	"fsevents", "@parcel/watcher-*", "@parcel/watcher",
}

func toolchainPackage(name string) bool {
	for _, p := range toolchainAddons {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// pkgOf returns the package a file belongs to by its last node_modules segment ("" when none) and the package directory.
func pkgOf(p string) (name, dir string) {
	s := filepath.ToSlash(p)
	i := strings.LastIndex(s, "/node_modules/")
	if i < 0 {
		return "", ""
	}
	rest := s[i+len("/node_modules/"):]
	parts := strings.SplitN(rest, "/", 3)
	switch {
	case len(parts) >= 2 && strings.HasPrefix(parts[0], "@"):
		name = parts[0] + "/" + parts[1]
	case len(parts) >= 1:
		name = parts[0]
	}
	if name == "" || name == "." || name == ".." {
		return "", ""
	}
	return name, filepath.FromSlash(s[:i+len("/node_modules/")] + name)
}

// fold: case-insensitive comparison on Windows.
func fold(s string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(s)
	}
	return s
}

// rel returns abs relative to root (slash form) when abs is inside root ("." for root itself).
func rel(root, abs string) (string, bool) {
	abs = filepath.Clean(abs)
	r := filepath.Clean(root)
	if fold(abs) == fold(r) {
		return ".", true
	}
	pre := r + string(filepath.Separator)
	if len(abs) > len(pre) && fold(abs[:len(pre)]) == fold(pre) {
		return filepath.ToSlash(abs[len(pre):]), true
	}
	return "", false
}

// repoCode reports whether a caller frame is repository code (inside the root, outside node_modules).
func repoCode(root, caller string) bool {
	if caller == "" {
		return false
	}
	r, ok := rel(root, caller)
	return ok && r != "." && !strings.Contains("/"+r+"/", "/node_modules/")
}

// runnerSpawn reports whether a spawn is one of the runner-internal ones of docs/dynamic.md §8.3 (they only receive contents
// or list files).
func runnerSpawn(e event) bool {
	pkg, _ := pkgOf(e.Caller)
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(e.File, "\\", "/")))
	base = strings.TrimSuffix(base, ".exe")
	args := strings.Join(e.Args, " ")
	switch pkg {
	case "esbuild":
		return strings.HasPrefix(base, "esbuild") && strings.Contains(args, "--service=")
	case "vite":
		return base == "cmd" && strings.Contains(args, "net use")
	case "jest-haste-map":
		return base == "watchman" || base == "find"
	}
	return false
}

type thread struct{ pid, tid int }

// Analyze reads every log of a unit's run in dir. root is the repository root (realpath); rootPID the pid of the runner process
// bgraph started; trust is trust_native_env.
func Analyze(dir, root string, rootPID int, trust bool) Analysis {
	var a Analysis
	why := map[string]bool{}
	bad := func(s string) { why[s] = true }
	ents, err := os.ReadDir(dir)
	if err != nil {
		bad("the collector wrote no output")
		return a.finish(why)
	}
	type spawnEv struct {
		event
		pid int
	}
	type workerEv struct {
		pid, tid int
		caller   string
	}
	started := map[thread]bool{}
	byToken := map[string][]event{}
	var spawns []spawnEv
	var workers []workerEv
	files := map[string]bool{}
	dirs := map[string]bool{}
	tool := map[string]bool{}
	addFile := func(abs string) {
		if r, ok := rel(root, abs); ok && r != "." {
			files[r] = true
		}
	}
	for _, de := range ents {
		name := de.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		stem := strings.TrimSuffix(name, ".jsonl")
		stem = strings.TrimSuffix(stem, "-hooks")
		pidS, _, _ := strings.Cut(stem, "-")
		pid, _ := strconv.Atoi(pidS)
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			bad("unreadable collector log " + name)
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<16), 1<<24)
		for sc.Scan() {
			var e event
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue // a line cut by a killed process
			}
			switch e.Ev {
			case "start":
				started[thread{e.Pid, e.Tid}] = true
				if e.Parent != "" {
					byToken[e.Parent] = append(byToken[e.Parent], e)
				}
			case "load":
				if e.URL != "" {
					if p, ok := fileURL(e.URL); ok {
						addFile(p)
					}
				} else {
					addFile(e.Path)
				}
			case "read":
				if !e.Haste {
					addFile(e.Path)
				}
			case "stat":
				if repoCode(root, e.Caller) {
					addFile(e.Path)
				}
			case "list":
				if repoCode(root, e.Caller) {
					if r, ok := rel(root, e.Path); ok {
						dirs[r] = true
					}
				}
			case "glob":
				if repoCode(root, e.Caller) {
					if r, ok := rel(root, e.Cwd); ok {
						dirs[r] = true
					} else {
						bad("fs.glob from repository code outside the repository root")
					}
				}
			case "cache":
				bad("reads vite optimized dependencies (bundled natively and cached)")
			case "native":
				if r, ok := rel(root, e.Path); ok && !strings.Contains("/"+r+"/", "/node_modules/") {
					bad("native addon in the repository: " + r)
					continue
				}
				name, pdir := pkgOf(e.Path)
				if name != "" && toolchainPackage(name) {
					if v, ok := pkgVersion(pdir, name); ok {
						tool[name+"@"+v] = true
						continue
					}
				}
				if !trust {
					what := name
					if what == "" {
						what = filepath.Base(e.Path)
					}
					bad("native addon from the environment: " + what)
				}
			case "wasi":
				for _, d := range e.Dirs {
					if _, ok := rel(root, d); ok {
						bad("WASI with a preopened repository directory")
					}
				}
			case "spawn":
				spawns = append(spawns, spawnEv{e, pid})
			case "worker":
				workers = append(workers, workerEv{pid, e.Tid, e.Caller})
			case "nohooks":
				bad("no ESM loader hooks: " + e.Msg)
			case "error":
				bad("collector error")
			}
		}
		f.Close()
	}
	if !started[thread{rootPID, 0}] {
		bad("the collector did not start in the runner process")
	}
	for _, s := range spawns {
		if !s.Sync && s.event.Pid == 0 {
			continue // the spawn failed: no process ran
		}
		observed := false
		for _, c := range byToken[s.Token] {
			if c.Ppid == s.pid && (s.Sync || c.Pid == s.event.Pid) {
				observed = true
			}
		}
		if observed || runnerSpawn(s.event) {
			continue
		}
		base := filepath.Base(strings.ReplaceAll(s.File, "\\", "/"))
		bad("spawns " + base + " (not observed)")
	}
	for _, w := range workers {
		if !started[thread{w.pid, w.tid}] {
			bad("worker thread without the collector")
		}
	}
	for f := range files {
		a.Files = append(a.Files, f)
	}
	for d := range dirs {
		a.Dirs = append(a.Dirs, d)
	}
	for t := range tool {
		a.Toolchain = append(a.Toolchain, t)
	}
	slices.Sort(a.Files)
	slices.Sort(a.Dirs)
	slices.Sort(a.Toolchain)
	return a.finish(why)
}

func (a Analysis) finish(why map[string]bool) Analysis {
	for w := range why {
		a.Incomplete = append(a.Incomplete, w)
	}
	slices.Sort(a.Incomplete)
	return a
}

// fileURL converts a file: URL to a local path.
func fileURL(u string) (string, bool) {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "file" {
		return "", false
	}
	s := p.Path
	if runtime.GOOS == "windows" {
		s = strings.TrimPrefix(s, "/")
	}
	return filepath.FromSlash(s), true
}

// pkgVersion reads the version of package name from dir/package.json, checking that the directory holds that package (a
// renamed copy of an allowlisted addon is not accepted).
func pkgVersion(dir, name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", false
	}
	var pj struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &pj) != nil || pj.Name != name || pj.Version == "" {
		return "", false
	}
	return pj.Version, true
}

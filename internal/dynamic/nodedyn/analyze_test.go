package nodedyn

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// logs writes synthetic collector logs: name → events.
func logs(t *testing.T, files map[string][]map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	for name, evs := range files {
		var b strings.Builder
		for _, e := range evs {
			j, _ := json.Marshal(e)
			b.Write(j)
			b.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func start(pid, ppid, tid int, parent string) map[string]any {
	return map[string]any{"ev": "start", "pid": pid, "ppid": ppid, "tid": tid, "parent": parent}
}

func writePkg(t *testing.T, dir, name, version string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"name": name, "version": version})
	if err := os.WriteFile(filepath.Join(dir, "package.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyzeClassification(t *testing.T) {
	root := t.TempDir()
	j := func(p ...string) string { return filepath.Join(append([]string{root}, p...)...) }
	nm := j("node_modules", "vitest", "dist", "index.js")
	dir := logs(t, map[string][]map[string]any{
		"100-0.jsonl": {
			start(100, 1, 0, ""),
			{"ev": "read", "path": j("src", "a.ts"), "caller": nm},                                                           // runner read: a dependency
			{"ev": "read", "path": j("src", "all.ts"), "caller": j("node_modules", "jest-haste-map", "w.js"), "haste": true}, // index read
			{"ev": "list", "path": j("test"), "caller": ""},                                                                  // helpers only (fdir)
			{"ev": "list", "path": j("src", "plugins"), "caller": nm},                                                        // runner listing
			{"ev": "stat", "path": j("package.json"), "caller": nm},                                                          // resolver stat
			{"ev": "load", "path": j("src", "legacy.cjs")},
		},
		"100-0-hooks.jsonl": {{"ev": "load", "url": "file:///" + filepath.ToSlash(j("src", "esm.mjs"))}},
		"200-0.jsonl": {
			start(200, 100, 0, "100-0-1"),
			{"ev": "list", "path": j("fixtures"), "caller": j("test", "a.test.ts")},      // repository listing
			{"ev": "stat", "path": j("data", "x.bin"), "caller": j("test", "a.test.ts")}, // repository stat
			{"ev": "read", "path": j("data", "y.json"), "caller": j("test", "a.test.ts")},
		},
	})

	// the spawn of 200 by 100
	f, _ := os.OpenFile(filepath.Join(dir, "100-0.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintf(f, `{"ev":"spawn","token":"100-0-1","pid":200,"file":"node","caller":%q}`+"\n", nm)
	f.Close()
	a := Analyze(dir, root, 100, false)
	if len(a.Incomplete) != 0 {
		t.Fatalf("incomplete: %v", a.Incomplete)
	}
	want := []string{"data/x.bin", "data/y.json", "src/a.ts", "src/esm.mjs", "src/legacy.cjs"}
	if !slices.Equal(a.Files, want) {
		t.Errorf("files %v, want %v", a.Files, want)
	}
	if !slices.Equal(a.Dirs, []string{"fixtures"}) {
		t.Errorf("dirs %v", a.Dirs)
	}
}

// TestAnalyzeIncomplete: every reason of docs/dynamic-layer.md §8.3 that the logs can show.
func TestAnalyzeIncomplete(t *testing.T) {
	root := t.TempDir()
	j := func(p ...string) string { return filepath.Join(append([]string{root}, p...)...) }
	rollup := j("node_modules", ".pnpm", "@rollup+rollup-x@4.1.0", "node_modules", "@rollup", "rollup-x")
	writePkg(t, rollup, "@rollup/rollup-x", "4.1.0")
	other := j("node_modules", "better-sqlite3")
	writePkg(t, other, "better-sqlite3", "11.0.0")
	fake := j("node_modules", "something")
	writePkg(t, fake, "something", "1.0.0")
	vite := j("node_modules", "vite", "dist", "node", "chunks", "config.js")
	esb := j("node_modules", "esbuild", "lib", "main.js")
	haste := j("node_modules", "jest-haste-map", "build", "index.js")
	test := j("test", "a.test.ts")
	cases := []struct {
		name  string
		evs   []map[string]any
		trust bool
		want  string // substring of a reason; "" = complete
	}{
		{"toolchain addon", []map[string]any{{"ev": "native", "path": filepath.Join(rollup, "rollup.x.node")}}, false, ""},
		{"non-allowlisted addon", []map[string]any{{"ev": "native", "path": filepath.Join(other, "build", "x.node")}}, false, "native addon from the environment: better-sqlite3"},
		{"non-allowlisted addon, trusted", []map[string]any{{"ev": "native", "path": filepath.Join(other, "build", "x.node")}}, true, ""},
		{"allowlisted file outside its package", []map[string]any{{"ev": "native", "path": filepath.Join(fake, "rollup.x.node")}}, false, "native addon from the environment: something"},
		{"allowlisted name, other package.json", []map[string]any{{"ev": "native", "path": j("node_modules", "@rollup", "rollup-y", "r.node")}}, false, "native addon from the environment: @rollup/rollup-y"},
		{"repository addon", []map[string]any{{"ev": "native", "path": j("build", "Release", "x.node")}}, true, "native addon in the repository: build/Release/x.node"},
		{"wasi", []map[string]any{{"ev": "wasi", "dirs": []string{j("data")}}}, false, "WASI"},
		{"vite deps", []map[string]any{{"ev": "cache", "path": j("node_modules", ".vite", "deps", "x.js")}}, false, "optimized dependencies"},
		{"unobserved spawn", []map[string]any{{"ev": "spawn", "token": "100-0-1", "pid": 300, "file": "git", "caller": test}}, false, "spawns git (not observed)"},
		{"shell spawn", []map[string]any{{"ev": "spawn", "token": "100-0-1", "pid": 300, "file": "/bin/sh", "args": []string{"-c", "node x"}, "caller": test}}, false, "spawns sh"},
		{"sync spawn unobserved", []map[string]any{{"ev": "spawn", "token": "100-0-2", "sync": true, "file": "node", "caller": test}}, false, "spawns node"},
		{"failed async spawn", []map[string]any{{"ev": "spawn", "token": "100-0-1", "pid": 0, "file": "nope", "caller": test}}, false, ""},
		{"esbuild service", []map[string]any{{"ev": "spawn", "token": "100-0-1", "pid": 300, "file": "/x/esbuild.exe", "args": []string{"--service=0.27.7", "--ping"}, "caller": esb}}, false, ""},
		{"esbuild build from test code", []map[string]any{{"ev": "spawn", "token": "100-0-1", "pid": 300, "file": "/x/esbuild.exe", "args": []string{"--service=0.27.7"}, "caller": test}}, false, "spawns esbuild.exe"},
		{"vite net use", []map[string]any{{"ev": "spawn", "token": "100-0-1", "pid": 300, "file": `C:\Windows\system32\cmd.exe`, "args": []string{"/d", "/s", "/c", `"net use"`}, "caller": vite}}, false, ""},
		{"other cmd from vite", []map[string]any{{"ev": "spawn", "token": "100-0-1", "pid": 300, "file": `C:\Windows\system32\cmd.exe`, "args": []string{"/c", "tsc"}, "caller": vite}}, false, "spawns"},
		{"jest watchman", []map[string]any{{"ev": "spawn", "token": "100-0-1", "sync": true, "file": "watchman", "args": []string{"--version"}, "caller": haste}}, false, ""},
		{"jest 22 execSync watchman", []map[string]any{{"ev": "spawn", "token": "100-0-1", "sync": true, "file": "watchman --version", "caller": haste}}, false, ""},
		{"shell command line with a pipe", []map[string]any{{"ev": "spawn", "token": "100-0-1", "sync": true, "file": "watchman --version | sh", "caller": haste}}, false, "spawns"},
		{"worker without collector", []map[string]any{{"ev": "worker", "tid": 3, "caller": test}}, false, "worker thread without the collector"},
		{"no hooks", []map[string]any{{"ev": "nohooks", "msg": "module.register is missing"}}, false, "no ESM loader hooks"},
	}
	for _, c := range cases {
		dir := logs(t, map[string][]map[string]any{"100-0.jsonl": append([]map[string]any{start(100, 1, 0, "")}, c.evs...)})
		a := Analyze(dir, root, 100, c.trust)
		got := strings.Join(a.Incomplete, "; ")
		switch {
		case c.want == "" && got != "":
			t.Errorf("%s: incomplete %q", c.name, got)
		case c.want != "" && !strings.Contains(got, c.want):
			t.Errorf("%s: incomplete %q, want %q", c.name, got, c.want)
		}
		if c.name == "toolchain addon" && !slices.Equal(a.Toolchain, []string{"@rollup/rollup-x@4.1.0"}) {
			t.Errorf("toolchain %v", a.Toolchain)
		}
	}
	// Observed children: an async spawn whose child (pid 300, ppid 100) reports the token; a worker with its own log.
	dir := logs(t, map[string][]map[string]any{
		"100-0.jsonl": {start(100, 1, 0, ""),
			{"ev": "spawn", "token": "100-0-1", "pid": 300, "file": "node", "caller": test},
			{"ev": "spawn", "token": "100-0-2", "sync": true, "file": "node", "caller": test},
			{"ev": "worker", "tid": 2, "caller": test}},
		"300-0.jsonl": {start(300, 100, 0, "100-0-1")},
		"301-0.jsonl": {start(301, 100, 0, "100-0-2")},
		"100-2.jsonl": {start(100, 1, 2, "")},
	})
	if a := Analyze(dir, root, 100, false); len(a.Incomplete) != 0 {
		t.Errorf("observed children: %v", a.Incomplete)
	}
	// A child reached through a shell reports the token but is not the spawned process.
	dir = logs(t, map[string][]map[string]any{
		"100-0.jsonl": {start(100, 1, 0, ""), {"ev": "spawn", "token": "100-0-1", "pid": 300, "file": "cmd.exe", "caller": test}},
		"400-0.jsonl": {start(400, 300, 0, "100-0-1")},
	})
	if a := Analyze(dir, root, 100, false); !strings.Contains(strings.Join(a.Incomplete, ";"), "spawns cmd.exe") {
		t.Errorf("shell child: %v", a.Incomplete)
	}
	// The runner process logged nothing: the collector did not load.
	dir = logs(t, map[string][]map[string]any{"555-0.jsonl": {start(555, 1, 0, "")}})
	if a := Analyze(dir, root, 100, false); !strings.Contains(strings.Join(a.Incomplete, ";"), "did not start in the runner") {
		t.Errorf("no root start: %v", a.Incomplete)
	}
	if a := Analyze(filepath.Join(root, "missing"), root, 100, false); !strings.Contains(strings.Join(a.Incomplete, ";"), "no output") {
		t.Errorf("no logs: %v", a.Incomplete)
	}
}

func TestToolchainPackage(t *testing.T) {
	for name, ok := range map[string]bool{
		"@rollup/rollup-win32-x64-msvc": true, "@rolldown/binding-linux-x64-gnu": true, "@unrs/resolver-binding-darwin-arm64": true,
		"@oxc-parser/binding-win32-x64-msvc": true, "@swc/core-linux-x64-gnu": true, "lightningcss-win32-x64-msvc": true,
		"fsevents": true, "@parcel/watcher-win32-x64": true,
		"rollup": false, "better-sqlite3": false, "@swc/coreX": false, "esbuild": false, "@rollup/plugin-node-resolve": false,
	} {
		if toolchainPackage(name) != ok {
			t.Errorf("%s: %v", name, !ok)
		}
	}
	if n, d := pkgOf(filepath.FromSlash("/r/node_modules/.pnpm/a@1/node_modules/@s/p/lib/x.node")); n != "@s/p" ||
		filepath.ToSlash(d) != "/r/node_modules/.pnpm/a@1/node_modules/@s/p" {
		t.Errorf("pkgOf: %s %s", n, d)
	}
}

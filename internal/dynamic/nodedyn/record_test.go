package nodedyn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestRecordOf(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "dist"), 0o755)
	os.WriteFile(filepath.Join(root, "dist", "index.js"), []byte("x"), 0o644)
	tr := tree.NewMapTree(map[string]string{
		"package.json": "{}", "src/a.ts": "a", "src/b.ts": "b", "fixtures/x/1.json": "1", "fixtures/x/2.json": "2", "fixtures/y.json": "y",
		"test/a.test.ts": "t",
	})
	u := Unit{ID: "test/a.test.ts", Project: ".", Runner: "vitest", Files: []string{"test/a.test.ts"}}
	o := Outcome{Unit: u.ID, Exit: 0, Analysis: Analysis{
		Files:     []string{"src/a.ts", "SRC/b.ts", "node_modules/.vite/x", "vitest.config.ts.timestamp-1-ab.mjs", "missing.ts", "src"},
		Dirs:      []string{"fixtures/x"},
		Toolchain: []string{"@rolldown/binding-x@1.0.0"},
	}}
	r := RecordOf(o, u, tr, root, "env", "c", "h", time.Unix(0, 0))
	var deps []string
	for d := range r.Deps {
		deps = append(deps, d)
	}
	slices.Sort(deps)
	want := []string{"fixtures/x/1.json", "fixtures/x/2.json", "src/a.ts", "src/b.ts", "test/a.test.ts"}
	if !slices.Equal(deps, want) {
		t.Fatalf("deps %v, want %v", deps, want)
	}
	if !r.Complete || !r.Pass || r.Eco != "node" || r.Deps["src/a.ts"] != tr.Key("src/a.ts") || !slices.Equal(r.Toolchain, o.Analysis.Toolchain) {
		t.Errorf("%+v", r)
	}
	o.Analysis.Files = append(o.Analysis.Files, "dist/index.js")
	if r := RecordOf(o, u, tr, root, "env", "c", "h", time.Now()); r.Complete || !strings.Contains(r.Incomplete, "untracked file dist/index.js") {
		t.Errorf("untracked: %+v", r)
	}
	for name, oc := range map[string]Outcome{
		"timeout": {Unit: u.ID, TimedOut: true, Exit: -1},
		"error":   {Unit: u.ID, Exit: -1, Err: "vitest is not installed"},
		"failed":  {Unit: u.ID, Exit: 1},
		"incomp":  {Unit: u.ID, Exit: 0, Analysis: Analysis{Incomplete: []string{"spawns git (not observed)"}}},
	} {
		r := RecordOf(oc, u, tr, root, "env", "c", "h", time.Now())
		switch name {
		case "failed":
			if r.Pass || !r.Complete {
				t.Errorf("%s: %+v", name, r)
			}
		case "incomp":
			if !r.Pass || r.Complete || r.Incomplete != "spawns git (not observed)" {
				t.Errorf("%s: %+v", name, r)
			}
		default:
			if r.Pass || r.Complete {
				t.Errorf("%s: %+v", name, r)
			}
		}
	}
}

func TestArgs(t *testing.T) {
	for _, c := range []struct {
		runner, version string
		want            string
	}{
		{"vitest", "5.0.1", "B run a.test.ts --coverage.enabled=false --reporter=dot --update=none --fsModuleCache=false"},
		{"vitest", "4.1.5", "B run a.test.ts --coverage.enabled=false --reporter=dot --update=none --experimental.fsModuleCache=false"},
		{"vitest", "3.2.4", "B run a.test.ts --coverage.enabled=false --reporter=dot"},
		{"jest", "22.4.4", "B --runTestsByPath a.test.ts -i --ci --no-cache --no-watchman --coverage=false"},
		{"mocha", "11.7.5", "B a.test.ts"},
		{"node-test", "", "--test a.test.ts"},
	} {
		a, err := Args(c.runner, "B", c.version, "a.test.ts")
		if err != nil || strings.Join(a, " ") != c.want {
			t.Errorf("%s %s: %v %v", c.runner, c.version, a, err)
		}
	}
	if a, _ := Args("mocha", "B", "1", "-x.js"); a[1] != "./-x.js" {
		t.Errorf("dash file: %v", a)
	}
	if _, err := Args("playwright", "B", "1", "a.spec.ts"); err == nil {
		t.Error("playwright accepted")
	}
}

func TestResolveBin(t *testing.T) {
	root := t.TempDir()
	pj := func(dir string, v any) {
		os.MkdirAll(dir, 0o755)
		b, _ := json.Marshal(v)
		os.WriteFile(filepath.Join(dir, "package.json"), b, 0o644)
	}
	pj(filepath.Join(root, "node_modules", "vitest"), map[string]any{"version": "5.0.1", "bin": map[string]string{"vitest": "vitest.mjs"}})
	pj(filepath.Join(root, "node_modules", "mocha"), map[string]any{"version": "11.0.0", "bin": map[string]string{"_mocha": "bin/_mocha", "mocha": "bin/mocha.js"}})
	os.MkdirAll(filepath.Join(root, "packages", "a"), 0o755)
	bin, v, err := ResolveBin(root, filepath.Join(root, "packages", "a"), "vitest")
	if err != nil || bin != filepath.Join(root, "node_modules", "vitest", "vitest.mjs") || v != "5.0.1" {
		t.Errorf("vitest: %s %s %v", bin, v, err)
	}
	if bin, _, _ := ResolveBin(root, root, "mocha"); bin != filepath.Join(root, "node_modules", "mocha", "bin", "mocha.js") {
		t.Errorf("mocha: %s", bin)
	}
	if _, _, err := ResolveBin(root, root, "jest"); err == nil {
		t.Error("jest found")
	}
}

func TestEnviron(t *testing.T) {
	t.Setenv("NODE_OPTIONS", "--max-old-space-size=4096")
	t.Setenv("BGRAPH_COLLECT_PARENT", "x")
	env := environ([]string{"FOO=1"}, "--require=/c/p.cjs")
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), "NODE_OPTIONS=") {
			n++
			if kv != "NODE_OPTIONS=--max-old-space-size=4096 --require=/c/p.cjs" {
				t.Errorf("%s", kv)
			}
		}
		if strings.HasPrefix(kv, "BGRAPH_COLLECT_PARENT=") {
			t.Errorf("inherited token: %s", kv)
		}
	}
	if n != 1 || !slices.Contains(env, "FOO=1") {
		t.Errorf("%v", env)
	}
	path, want := "/a b/x.cjs", `--require="/a b/x.cjs"`
	if runtime.GOOS == "windows" {
		path, want = `C:\a b\x.cjs`, `--require="C:/a b/x.cjs"`
	}
	if got := nodeOptionsToken(path); got != want {
		t.Errorf("quoted: %s", got)
	}
}

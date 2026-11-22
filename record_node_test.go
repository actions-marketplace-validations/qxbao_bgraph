package bgraph

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
)

// testNode returns a node executable with module.register (BGRAPH_TEST_NODE, else node on PATH) or skips.
func testNode(t *testing.T) string {
	t.Helper()
	n := os.Getenv("BGRAPH_TEST_NODE")
	if n == "" {
		p, err := exec.LookPath("node")
		if err != nil {
			t.Skip("no node (set BGRAPH_TEST_NODE)")
		}
		n = p
	}
	if out, err := exec.Command(n, "-e", "if (typeof require('node:module').register !== 'function') process.exit(3)").CombinedOutput(); err != nil {
		t.Skipf("%s has no module.register (Node >= 20.6 needed): %v %s", n, err, out)
	}
	return n
}

// TestRecordNodeIntegration runs the real Node collector with node:test on a tiny repository (docs/dynamic.md §8): a widening
// through a computed import() is narrowed for the tests that never load the changed module and kept for the one that does;
// a test that spawns a non-Node process is incomplete; a Node child process and a worker thread are observed.
func TestRecordNodeIntegration(t *testing.T) {
	nodeExe := testNode(t)
	load := "import { load } from '../src/loader.mjs';\n"
	root := testRepo(t, map[string]string{
		"package.json":    `{"name":"x","private":true,"type":"module"}` + "\n",
		"src/loader.mjs":  "export const load = (p) => import(new URL(p, import.meta.url).href);\n",
		"src/a.mjs":       "export const X = 1;\n",
		"src/b.mjs":       "export const Y = 1;\n",
		"src/data.txt":    "hello\n",
		"src/worker.mjs":  "import { parentPort } from 'node:worker_threads';\nimport fs from 'node:fs';\nparentPort.postMessage(fs.readFileSync(new URL('./data.txt', import.meta.url), 'utf8'));\n",
		"test/a.test.mjs": "import test from 'node:test';\nimport assert from 'node:assert';\n" + load + "test('a', async () => { assert.equal((await load('./a.mjs')).X, 1); });\n",
		"test/b.test.mjs": "import test from 'node:test';\nimport assert from 'node:assert';\n" + load + "test('b', async () => { assert.equal((await load('./b.mjs')).Y, 1); });\n",
		"test/reads.test.mjs": "import test from 'node:test';\nimport assert from 'node:assert';\nimport fs from 'node:fs';\n" + load +
			"test('r', async () => { await load('./b.mjs'); assert.equal(fs.readFileSync(new URL('../src/data.txt', import.meta.url), 'utf8'), 'hello\\n'); });\n",
		"test/spawn.test.mjs": "import test from 'node:test';\nimport { execFileSync } from 'node:child_process';\n" + load +
			"test('s', async () => { await load('./b.mjs'); execFileSync('git', ['--version']); });\n",
		"test/child.test.mjs": "import test from 'node:test';\nimport assert from 'node:assert';\nimport { execFileSync } from 'node:child_process';\nimport { Worker } from 'node:worker_threads';\n" + load +
			"test('c', async () => {\n  await load('./b.mjs');\n  assert.equal(execFileSync(process.execPath, ['-e', 'console.log(1)'], { encoding: 'utf8' }).trim(), '1');\n" +
			"  const w = new Worker(new URL('../src/worker.mjs', import.meta.url), { env: {} });\n  assert.equal(await new Promise((r) => w.once('message', r)), 'hello\\n');\n});\n",
		"test/fail.test.mjs": "import test from 'node:test';\nimport assert from 'node:assert';\n" + load + "test('f', async () => { await load('./b.mjs'); assert.fail('no'); });\n",
	})
	state := t.TempDir()
	ctx := context.Background()
	ix, err := Open(ctx, root, Options{MemoryOnly: true, StateDir: state, Node: nodeExe})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	sum, err := ix.Record(ctx, RecordOptions{Eco: "node", Jobs: 3, Timeout: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Units != 6 || sum.Recorded != 6 || sum.Failed != 1 || !slices.Equal(sum.Ecos, []string{"node"}) {
		t.Fatalf("summary %+v", sum)
	}
	recs, warn := dynamic.Load(dynamic.StorePath(state, "node"))
	if len(warn) > 0 || len(recs) != 6 {
		t.Fatalf("store: %d records, %v", len(recs), warn)
	}
	by := map[string]dynamic.Record{}
	for _, r := range recs {
		by[r.Unit] = r
	}
	for _, u := range []string{"test/a.test.mjs", "test/b.test.mjs", "test/reads.test.mjs", "test/child.test.mjs"} {
		if r := by[u]; !r.Complete || !r.Pass {
			t.Errorf("%s: complete=%v pass=%v (%s)", u, r.Complete, r.Pass, r.Incomplete)
		}
	}
	if r := by["test/a.test.mjs"]; r.Deps["src/a.mjs"] == "" || r.Deps["src/loader.mjs"] == "" || r.Deps["src/b.mjs"] != "" {
		t.Errorf("a.test deps %v", r.Deps)
	}
	if r := by["test/reads.test.mjs"]; r.Deps["src/data.txt"] == "" || r.Deps["src/b.mjs"] == "" {
		t.Errorf("reads.test deps %v", r.Deps)
	}
	if r := by["test/child.test.mjs"]; r.Deps["src/worker.mjs"] == "" || r.Deps["src/data.txt"] == "" {
		t.Errorf("child.test deps (worker thread with env {}) %v", r.Deps)
	}
	if r := by["test/spawn.test.mjs"]; r.Complete || !strings.Contains(r.Incomplete, "spawns git") || !r.Pass {
		t.Errorf("spawn.test: complete=%v pass=%v (%s)", r.Complete, r.Pass, r.Incomplete)
	}
	if r := by["test/fail.test.mjs"]; r.Pass {
		t.Error("failing test recorded as passing")
	}
	if out, err := exec.Command("git", "-C", root, "status", "--porcelain").CombinedOutput(); err != nil || len(out) > 0 {
		t.Errorf("recording dirtied the tree: %v %s", err, out)
	}

	write := func(rel, c string) {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	units := func() (kept, narrowed []string) {
		t.Helper()
		s, err := ix.Select(ctx, Change{Base: "HEAD", Worktree: true, Dynamic: DynamicOn})
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range s.Tests {
			kept = append(kept, x.Unit)
		}
		for _, x := range s.Narrowed {
			narrowed = append(narrowed, x.Unit)
		}
		return kept, narrowed
	}
	write("src/a.mjs", "export const X = 1; // edit\n")
	kept, narrowed := units()
	if !slices.Equal(kept, []string{"test/a.test.mjs", "test/fail.test.mjs", "test/spawn.test.mjs"}) ||
		!slices.Equal(narrowed, []string{"test/b.test.mjs", "test/child.test.mjs", "test/reads.test.mjs"}) {
		t.Fatalf("src/a.mjs: kept %v narrowed %v", kept, narrowed)
	}
	write("src/a.mjs", "export const X = 1;\n")
	write("src/b.mjs", "export const Y = 1; // edit\n")
	kept, narrowed = units()
	if !slices.Equal(narrowed, []string{"test/a.test.mjs"}) {
		t.Fatalf("src/b.mjs: kept %v narrowed %v", kept, narrowed)
	}
}

// TestRecordNodeVitest records a vitest project (docs/dynamic.md §8.1): sources are read by vite in the runner process, fixture
// reads happen in the pool workers. It needs a directory whose node_modules has vitest (BGRAPH_TEST_VITEST, linked into the
// test repository) and is skipped otherwise.
func TestRecordNodeVitest(t *testing.T) {
	nodeExe := testNode(t)
	dir := os.Getenv("BGRAPH_TEST_VITEST")
	if dir == "" {
		t.Skip("set BGRAPH_TEST_VITEST to a directory with node_modules/vitest")
	}
	nm := filepath.Join(dir, "node_modules")
	if _, err := os.Stat(filepath.Join(nm, "vitest", "package.json")); err != nil {
		t.Skipf("no vitest under %s", nm)
	}
	root := testRepo(t, map[string]string{
		".gitignore":        "node_modules\n",
		"package.json":      `{"name":"v","private":true,"type":"module","devDependencies":{"vitest":"*"}}` + "\n",
		"vitest.config.ts":  "import { defineConfig } from 'vitest/config'\nexport default defineConfig({ test: { include: ['test/**/*.test.ts'] } })\n",
		"src/loader.ts":     "export const load = (n: string) => import(`./plugins/${n}`)\n",
		"src/plugins/p1.ts": "export default 'p1'\n",
		"src/plugins/p2.ts": "export default 'p2'\n",
		"data/d.json":       "{\"k\":1}\n",
		"test/p1.test.ts": "import { expect, test } from 'vitest'\nimport { load } from '../src/loader'\nimport fs from 'node:fs'\n" +
			"test('p1', async () => {\n  expect((await load('p1.ts')).default).toBe('p1')\n  expect(JSON.parse(fs.readFileSync(new URL('../data/d.json', import.meta.url), 'utf8')).k).toBe(1)\n})\n",
		"test/p2.test.ts": "import { expect, test } from 'vitest'\nimport { load } from '../src/loader'\ntest('p2', async () => { expect((await load('p2.ts')).default).toBe('p2') })\n",
	})
	link := filepath.Join(root, "node_modules")
	if err := os.Symlink(nm, link); err != nil {
		if out, err2 := exec.Command("cmd", "/c", "mklink", "/J", link, nm).CombinedOutput(); err2 != nil {
			t.Skipf("cannot link node_modules: %v / %v %s", err, err2, out)
		}
	}
	t.Cleanup(func() { os.Remove(link) }) // unlink before the temporary directory is removed (never recurse into the target)
	state := t.TempDir()
	ctx := context.Background()
	ix, err := Open(ctx, root, Options{MemoryOnly: true, StateDir: state, Node: nodeExe})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	sum, err := ix.Record(ctx, RecordOptions{Eco: "node", Jobs: 2, Timeout: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Units != 2 || sum.Complete != 2 {
		t.Fatalf("summary %+v", sum)
	}
	recs, _ := dynamic.Load(dynamic.StorePath(state, "node"))
	for _, r := range recs {
		other := map[string]string{"test/p1.test.ts": "src/plugins/p2.ts", "test/p2.test.ts": "src/plugins/p1.ts"}[r.Unit]
		if r.Deps["src/loader.ts"] == "" || r.Deps[other] != "" {
			t.Errorf("%s deps %v", r.Unit, r.Deps)
		}
		if r.Unit == "test/p1.test.ts" && r.Deps["data/d.json"] == "" {
			t.Errorf("p1 fixture read not observed: %v", r.Deps)
		}
	}
}

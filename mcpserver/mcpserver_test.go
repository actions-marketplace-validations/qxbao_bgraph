package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/agent"
)

func testRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfg, nil, 0o644)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	for _, k := range []string{"AUTHOR", "COMMITTER"} {
		t.Setenv("GIT_"+k+"_NAME", "t")
		t.Setenv("GIT_"+k+"_EMAIL", "t@example.com")
	}
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "core.autocrlf=false"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for rel, c := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return root
}

// connect starts the server on an in-memory transport and returns a client session.
func connect(t *testing.T, root string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ix, err := bgraph.Open(ctx, root, bgraph.Options{MemoryOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	ct, st := mcp.NewInMemoryTransports()
	srv, err := New(ix, "test")
	if err != nil {
		t.Fatal(err)
	}
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// call invokes a tool and returns the structured result as a generic JSON object plus the text content.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (res *mcp.CallToolResult, obj map[string]any, text string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, &obj); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	return res, obj, text
}

func list(t *testing.T, obj map[string]any, key string) []any {
	t.Helper()
	v, ok := obj[key]
	if !ok {
		t.Fatalf("key %q missing from %v", key, obj)
	}
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("key %q is %T, want an array (never null)", key, v)
	}
	return l
}

func TestServer(t *testing.T) {
	files := map[string]string{
		"package.json":  `{"name":"p","devDependencies":{"vitest":"^1.0.0"}}`,
		"src/a.ts":      "export const a = 1\n",
		"src/a.test.ts": "import { a } from './a'\nconsole.log(a)\n",
	}
	for i := 0; i < 5; i++ {
		files[fmt.Sprintf("src/n%d.test.ts", i)] = "import { a } from './a'\nconsole.log(a)\n"
	}
	root := testRepo(t, files)
	if err := os.WriteFile(filepath.Join(root, "src", "a.ts"), []byte("export const a = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, root)
	ctx := context.Background()

	if got := cs.InitializeResult().Instructions; got != agent.Instructions {
		t.Errorf("instructions = %q", got)
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
		if tl.OutputSchema == nil {
			t.Errorf("%s has no output schema", tl.Name)
		}
		if a := tl.Annotations; a == nil || !a.ReadOnlyHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s annotations = %+v", tl.Name, a)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"affected_tests", "dependents_of", "explain_selection", "test_commands"}) {
		t.Fatalf("tools = %v", names)
	}

	// affected_tests
	res, obj, text := call(t, cs, "affected_tests", map[string]any{"base": "HEAD"})
	if res.IsError {
		t.Fatalf("affected_tests: %s", text)
	}
	if obj["scope"] != "graph" || obj["tests_total"] != float64(6) || obj["truncated"] != false {
		t.Errorf("affected_tests = %v", obj)
	}
	for _, k := range []string{"widening", "warnings", "changed", "tests", "always_run", "notes"} {
		list(t, obj, k)
	}
	if len(list(t, obj, "widening")) != 0 || !strings.Contains(text, "scope: graph") {
		t.Errorf("text = %q", text)
	}

	// Truncation: the long list is cut, the totals and the flag say so, the fail-closed keys stay present.
	_, obj, text = call(t, cs, "affected_tests", map[string]any{"base": "HEAD", "limit": 2})
	if len(list(t, obj, "tests")) != 2 || obj["tests_total"] != float64(6) || obj["truncated"] != true {
		t.Errorf("truncated affected_tests = %v", obj)
	}
	list(t, obj, "widening")
	list(t, obj, "warnings")
	if !strings.Contains(text, "TRUNCATED") {
		t.Errorf("text should announce truncation: %q", text)
	}

	// An unusable base is not an error: it widens to full, prominently.
	res, obj, text = call(t, cs, "affected_tests", map[string]any{"base": "no-such-ref"})
	if res.IsError || obj["scope"] != "full" || len(list(t, obj, "widening")) == 0 || !strings.HasPrefix(text, "SCOPE: FULL") {
		t.Errorf("full: err=%v obj=%v text=%q", res.IsError, obj, text)
	}

	// test_commands: one vitest command covering the six selected files, never truncated.
	res, obj, text = call(t, cs, "test_commands", map[string]any{"base": "HEAD", "limit": 1})
	if res.IsError {
		t.Fatalf("test_commands: %s", text)
	}
	cmds := list(t, obj, "commands")
	if obj["scope"] != "graph" || obj["tests_total"] != float64(6) || len(cmds) != 1 {
		t.Fatalf("test_commands = %v", obj)
	}
	c := cmds[0].(map[string]any)
	if c["runner"] != "vitest" || !strings.Contains(c["shell"].(string), "vitest run") || len(c["argv"].([]any)) < 6+2 {
		t.Errorf("command = %v", c)
	}
	if !strings.Contains(text, c["shell"].(string)) || !strings.HasPrefix(text, "scope: graph") {
		t.Errorf("text = %q", text)
	}
	list(t, obj, "widening")
	list(t, obj, "notes")
	res, obj, text = call(t, cs, "test_commands", map[string]any{"base": "no-such-ref"})
	if res.IsError || obj["scope"] != "full" || len(list(t, obj, "commands")) != 0 || !strings.HasPrefix(text, "SCOPE: FULL") {
		t.Errorf("full test_commands: obj=%v text=%q", obj, text)
	}
	if res, _, _ := call(t, cs, "test_commands", map[string]any{"bogus": 1}); !res.IsError {
		t.Error("an unknown argument should be a tool error")
	}

	// dependents_of
	res, obj, text = call(t, cs, "dependents_of", map[string]any{"files": []string{"src/a.ts", "src/missing.ts"}})
	if res.IsError {
		t.Fatalf("dependents_of: %s", text)
	}
	if len(list(t, obj, "dependents")) != 6 || obj["dependents_total"] != float64(6) || len(list(t, obj, "tests")) != 6 {
		t.Errorf("dependents_of = %v", obj)
	}
	if m := list(t, obj, "missing"); len(m) != 1 || m[0] != "src/missing.ts" {
		t.Errorf("missing = %v", m)
	}
	list(t, obj, "unresolved")
	first := list(t, obj, "dependents")[0].(map[string]any)
	if first["test"] != true || len(first["chain"].([]any)) != 2 {
		t.Errorf("first dependent = %v", first)
	}
	_, obj, _ = call(t, cs, "dependents_of", map[string]any{"files": []string{"src/a.ts"}, "limit": 1})
	if len(list(t, obj, "dependents")) != 1 || obj["truncated"] != true || obj["dependents_total"] != float64(6) {
		t.Errorf("truncated dependents_of = %v", obj)
	}
	list(t, obj, "unresolved")

	// explain_selection
	res, obj, text = call(t, cs, "explain_selection", map[string]any{"target": "src/a.test.ts", "base": "HEAD"})
	if res.IsError {
		t.Fatalf("explain_selection: %s", text)
	}
	if lines := list(t, obj, "lines"); len(lines) < 2 || !strings.Contains(lines[0].(string), "src/a.test.ts is selected because") {
		t.Errorf("explain lines = %v", lines)
	}
	list(t, obj, "widening")
	list(t, obj, "warnings")

	// Infrastructure / input errors are tool errors, not protocol failures.
	for name, args := range map[string]map[string]any{
		"dependents_of":     {"files": []string{"src/a.ts"}, "rev": "no-such-rev"},
		"explain_selection": {"target": " "},
	} {
		res, _, text := call(t, cs, name, args)
		if !res.IsError || text == "" {
			t.Errorf("%s: want a tool error, got %+v", name, res)
		}
	}
	if res, _, _ := call(t, cs, "dependents_of", map[string]any{"files": []string{}}); !res.IsError {
		t.Error("empty files should be a tool error")
	}
}

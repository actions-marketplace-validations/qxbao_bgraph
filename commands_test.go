package bgraph

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCommandsAndExplain(t *testing.T) {
	sel := Selection{Scope: TierGraph, Tests: []SelectedTest{{Unit: "src/a.test.ts", Project: ".", Runner: "vitest", Files: []string{"src/a.test.ts"}}}}
	plan, err := Commands(sel, CommandOptions{})
	if err != nil || plan.Scope != TierGraph || len(plan.Commands) != 1 || !slices.Equal(plan.Commands[0].Argv, []string{"npx", "vitest", "run", "src/a.test.ts"}) {
		t.Fatalf("plan %+v, err %v", plan, err)
	}
	if lines := Explain(sel, "src/a.test.ts"); len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "src/a.test.ts") {
		t.Errorf("explain: %q", lines)
	}

	full := Selection{Scope: TierFull, Widening: []Widening{{Tier: TierFull, Reason: ReasonNoBase}}}
	if plan, err := Commands(full, CommandOptions{}); err != nil || plan.Scope != TierFull || len(plan.Commands) != 0 {
		t.Errorf("full plan %+v, err %v", plan, err)
	}
}

func TestChangeExclude(t *testing.T) {
	root := testRepo(t, map[string]string{
		"package.json":  `{"name":"p","devDependencies":{"vitest":"^1.0.0"}}`,
		"src/a.ts":      "export const a = 1\n",
		"src/a.test.ts": "import { a } from './a'\nconsole.log(a)\n",
		"src/c.test.ts": "console.log('c')\n",
	})
	ctx := context.Background()
	ix, err := Open(ctx, root, Options{MemoryOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	write := func(rel, c string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sel := func(exclude ...string) Selection {
		t.Helper()
		s, err := ix.Select(ctx, Change{Base: "HEAD", Worktree: true, Exclude: exclude})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	units := func(s Selection) []string {
		var out []string
		for _, u := range s.Tests {
			out = append(out, u.Unit)
		}
		slices.Sort(out)
		return out
	}

	write(".tool/ledger/state.json", `{"n":1}`)
	if s := sel(); s.Scope == TierNone {
		t.Fatalf("an unknown changed file must widen without Exclude: %+v", s)
	}
	if s := sel(".tool/ledger/**"); s.Scope != TierNone || len(s.Changed) != 0 || len(s.Widening) != 0 {
		t.Fatalf("only excluded files changed: %+v", s)
	}

	write("src/a.ts", "export const a = 2\n")
	s := sel(".tool/ledger/**")
	if !slices.Equal(s.Changed, []string{"src/a.ts"}) || !slices.Equal(units(s), []string{"src/a.test.ts"}) || len(s.Widening) != 0 {
		t.Fatalf("excluded + source change: changed %v, units %v, widening %+v", s.Changed, units(s), s.Widening)
	}

	// A file the graph knows is never excluded.
	s = sel(".tool/ledger/**", "src/**")
	if !slices.Contains(s.Changed, "src/a.ts") || !slices.Equal(units(s), []string{"src/a.test.ts"}) {
		t.Fatalf("graph node excluded: changed %v, units %v", s.Changed, units(s))
	}
	if !slices.ContainsFunc(s.Warnings, func(w Widening) bool { return w.Reason == ReasonRuleRejected && w.Path == "src/a.ts" }) {
		t.Errorf("no rule_rejected warning: %+v", s.Warnings)
	}

	if _, err := ix.Select(ctx, Change{Base: "HEAD", Worktree: true, Exclude: []string{"a/{b"}}); err == nil {
		t.Error("invalid pattern accepted")
	}
}

// Data files inside a Go module are owned by the module (role data); Exclude still drops them when nothing references them.
func TestChangeExcludeGoModule(t *testing.T) {
	root := testRepo(t, map[string]string{
		"go.mod":         "module example.com/m\n\ngo 1.22\n",
		"a/a.go":         "package a\n\nconst A = 1\n",
		"a/a_test.go":    "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
		"b/b.go":         "package b\n\nimport \"example.com/m/a\"\n\nconst B = a.A\n",
		"b/b_test.go":    "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n",
		"c/c.go":         "package c\n",
		"c/goal_test.go": "package c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {}\n",
	})
	ctx := context.Background()
	ix, err := Open(ctx, root, Options{MemoryOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	p := filepath.Join(root, ".tool", "tasks", "01X", "state.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(`{"n":1}`), 0o644)
	if s, err := ix.Select(ctx, Change{Base: "HEAD", Worktree: true}); err != nil || s.Scope == TierNone {
		t.Fatalf("without Exclude a state file must widen: %+v %v", s, err)
	}
	s, err := ix.Select(ctx, Change{Base: "HEAD", Worktree: true, Exclude: []string{".tool/tasks/**"}})
	if err != nil || s.Scope != TierNone || len(s.Changed) != 0 || len(s.Tests) != 0 || len(s.Warnings) != 0 {
		t.Fatalf("excluded state file in a Go module: %+v %v", s, err)
	}

	// A test file and a Go source are never dropped; selected Go units carry their package directory.
	os.WriteFile(filepath.Join(root, "a", "a.go"), []byte("package a\n\nconst A = 2\n"), 0o644)
	s, err = ix.Select(ctx, Change{Base: "HEAD", Worktree: true, Exclude: []string{".tool/tasks/**", "a/**"}})
	if err != nil || !slices.Equal(s.Changed, []string{"a/a.go"}) {
		t.Fatalf("source excluded: %+v %v", s, err)
	}
	dirs := map[string]string{}
	for _, u := range s.Tests {
		dirs[u.Unit] = u.Dir
	}
	if dirs["go:a"] != "a" || dirs["go:b"] != "b" || len(dirs) != 2 {
		t.Errorf("units/dirs: %v", dirs)
	}
}

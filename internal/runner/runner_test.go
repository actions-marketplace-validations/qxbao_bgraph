package runner

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func sel(runner, project string, files ...string) model.Selection {
	s := model.Selection{Scope: model.TierGraph}
	for _, f := range files {
		s.Tests = append(s.Tests, model.SelectedTest{Unit: f, Project: project, Runner: runner, Files: []string{f}})
	}
	return s
}

func TestPerRunner(t *testing.T) {
	cases := []struct {
		name string
		sel  model.Selection
		want [][]string
	}{
		{"vitest", sel("vitest", ".", "src/a.test.ts"), [][]string{{"npx", "vitest", "run", "src/a.test.ts"}}},
		{"jest", sel("jest", ".", "src/b.test.ts", "src/a.test.ts"), [][]string{{"npx", "jest", "--runTestsByPath", "src/a.test.ts", "src/b.test.ts"}}},
		{"mocha", sel("mocha", ".", "t/a.js"), [][]string{{"npx", "mocha", "t/a.js"}}},
		{"ava", sel("ava", ".", "t/a.js"), [][]string{{"npx", "ava", "t/a.js"}}},
		{"node-test", sel("node-test", ".", "t/a.test.js"), [][]string{{"node", "--test", "t/a.test.js"}}},
		{"playwright regex", sel("playwright", ".", "e2e/a.spec.ts", "e2e/c++.spec.ts"), [][]string{{"npx", "playwright", "test", `e2e[\\/]a\.spec\.ts`, `e2e[\\/]c\+\+\.spec\.ts`}}},
		{"project-relative + windows slashes", sel("vitest", "packages/x", `packages\x\src\a.test.ts`), [][]string{{"npx", "vitest", "run", "src/a.test.ts"}}},
		{"special chars vitest", sel("vitest", ".", "src/a b (1).test.ts"), [][]string{{"npx", "vitest", "run", "src/a b (1).test.ts"}}},
		{"glob chars node-test falls back", sel("node-test", "p", "p/t/[id].test.js"), [][]string{{"node", "--test"}}},
		{"glob chars mocha falls back", sel("mocha", ".", "t/a*.js"), [][]string{{"npx", "mocha"}}},
		{"outside project falls back", sel("jest", "p", "q/a.test.ts"), [][]string{{"npx", "jest"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := Commands(c.sel, Options{})
			if err != nil {
				t.Fatal(err)
			}
			var got [][]string
			for _, cmd := range p.Commands {
				got = append(got, cmd.Argv)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestNoneFullEmpty(t *testing.T) {
	p, _ := Commands(model.Selection{Scope: model.TierNone}, Options{})
	if len(p.Commands) != 0 || p.Scope != model.TierNone {
		t.Fatalf("none: %+v", p)
	}
	p, _ = Commands(model.Selection{Scope: model.TierFull, Tests: sel("jest", ".", "a").Tests}, Options{})
	if len(p.Commands) != 0 || p.Scope != model.TierFull || !strings.Contains(strings.Join(p.Notes, ";"), "run the full suite") {
		t.Fatalf("full: %+v", p)
	}
	p, _ = Commands(model.Selection{Scope: model.TierGraph}, Options{}) // no test: fail-closed
	if len(p.Commands) != 0 || p.Scope != model.TierFull {
		t.Fatalf("empty graph: %+v", p)
	}
	p, _ = Commands(sel("gotest", ".", "a"), Options{}) // unknown runner (cargo is supported now)
	if len(p.Commands) != 0 || p.Scope != model.TierFull {
		t.Fatalf("unknown runner: %+v", p)
	}
}

func TestThreshold(t *testing.T) {
	s := sel("vitest", ".", "a", "b", "c")
	s.IndexStats = map[string]int{"units": 4}
	p, _ := Commands(s, Options{FullThreshold: 0.5})
	if p.Scope != model.TierFull || len(p.Commands) != 0 {
		t.Fatalf("%+v", p)
	}
	p, _ = Commands(s, Options{FullThreshold: 0.9})
	if p.Scope != model.TierGraph || len(p.Commands) != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestBatching(t *testing.T) {
	var files []string
	for i := 0; i < 50; i++ {
		files = append(files, fmt.Sprintf("src/some/long/dir/name/file%02d.test.ts", i))
	}
	p, err := Commands(sel("vitest", ".", files...), Options{MaxArgChars: 300})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Commands) < 2 {
		t.Fatalf("expected batches, got %d", len(p.Commands))
	}
	n := 0
	for i, c := range p.Commands {
		if c.Batch != i+1 || c.Batches != len(p.Commands) {
			t.Fatalf("batch numbering %+v", c)
		}
		if l := len(strings.Join(c.Argv, " ")); l > 300 {
			t.Fatalf("too long: %d", l)
		}
		n += len(c.Argv) - 3
	}
	if n != 50 {
		t.Fatalf("dropped paths: %d", n)
	}
	if len(p.Notes) == 0 {
		t.Fatal("no batching note")
	}
}

func TestPlaywrightListFile(t *testing.T) {
	dir := t.TempDir()
	// Short: still uses positional arguments despite ListDir (list files only when too long).
	if p, _ := Commands(sel("playwright", "web", "web/e2e/a.spec.ts"), Options{ListDir: dir}); p.Commands[0].ListFile != "" {
		t.Fatalf("short list but a list file was used: %+v", p.Commands[0])
	}
	p, err := Commands(sel("playwright", "web", "web/e2e/a.spec.ts", "web/e2e/b.spec.ts"), Options{ListDir: dir, MaxArgChars: 10})
	if err != nil {
		t.Fatal(err)
	}
	c := p.Commands[0]
	if c.ListFile == "" || c.Argv[3] != "--test-list" {
		t.Fatalf("%+v", c)
	}
	b, _ := os.ReadFile(c.ListFile)
	if string(b) != "e2e/a.spec.ts\ne2e/b.spec.ts\n" {
		t.Fatalf("list: %q", b)
	}
}

func TestGroupsByProjectRunner(t *testing.T) {
	s := model.Selection{Scope: model.TierPackage, Tests: []model.SelectedTest{
		{Unit: "b/x.test.ts", Project: "b", Runner: "jest", Files: []string{"b/x.test.ts"}},
		{Unit: "a/y.test.ts", Project: "a", Runner: "vitest", Files: []string{"a/y.test.ts"}},
	}}
	p, _ := Commands(s, Options{})
	if len(p.Commands) != 2 || p.Commands[0].Project != "a" || p.Commands[1].Project != "b" {
		t.Fatalf("%+v", p.Commands)
	}
}

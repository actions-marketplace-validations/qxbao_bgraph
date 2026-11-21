package runner

import (
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func pysel(runner, project, config string, files ...string) model.Selection {
	s := model.Selection{Scope: model.TierGraph}
	for _, f := range files {
		s.Tests = append(s.Tests, model.SelectedTest{Unit: f, Project: project, Runner: runner, Files: []string{f}, Config: config})
	}
	return s
}

func argvs(p Plan) [][]string {
	var out [][]string
	for _, c := range p.Commands {
		out = append(out, c.Argv)
	}
	return out
}

func TestPytestCommands(t *testing.T) {
	cases := []struct {
		name string
		sel  model.Selection
		want [][]string
	}{
		{"no config", pysel("pytest", "pkg", "", "pkg/tests/test_b.py", "pkg/tests/test_a.py"),
			[][]string{{"python", "-m", "pytest", "--rootdir", ".", "tests/test_a.py", "tests/test_b.py"}}},
		{"config in the project", pysel("pytest", ".", "pytest.ini", "tests/test_a.py"),
			[][]string{{"python", "-m", "pytest", "--rootdir", ".", "-c", "pytest.ini", "tests/test_a.py"}}},
		{"config above the project", pysel("pytest", "svc", "pyproject.toml", "svc/tests/test_a.py"),
			[][]string{{"python", "-m", "pytest", "--rootdir", "..", "-c", "../pyproject.toml", "tests/test_a.py"}}},
		{"option-like name", pysel("pytest", ".", "", "-x/test_a.py", "@f/test_b.py"),
			[][]string{{"python", "-m", "pytest", "--rootdir", ".", "./-x/test_a.py", "./@f/test_b.py"}}},
	}
	for _, c := range cases {
		p, err := Commands(c.sel, Options{})
		if err != nil || !reflect.DeepEqual(argvs(p), c.want) {
			t.Errorf("%s: got %v (%v), want %v", c.name, argvs(p), err, c.want)
		}
	}
	whole := model.Selection{Scope: model.TierModule, Tests: []model.SelectedTest{{Unit: "<py-all-tests:.>", Project: ".", Runner: "pytest", Whole: true}}}
	if p, _ := Commands(whole, Options{}); !reflect.DeepEqual(argvs(p), [][]string{{"python", "-m", "pytest", "--rootdir", "."}}) {
		t.Errorf("whole: %v", argvs(p))
	}
}

func TestPytestLongListUsesArgfile(t *testing.T) {
	var files []string
	for i := 0; i < 400; i++ {
		files = append(files, "tests/test_"+strings.Repeat("x", 20)+string(rune('a'+i%26))+strings.Repeat("y", i%7)+strconv.Itoa(i)+".py")
	}
	dir := t.TempDir()
	p, err := Commands(pysel("pytest", ".", "", files...), Options{ListDir: dir, MaxArgChars: 2000})
	if err != nil || len(p.Commands) != 1 || p.Commands[0].ListFile == "" {
		t.Fatalf("%v %+v", err, p)
	}
	last := p.Commands[0].Argv[len(p.Commands[0].Argv)-1]
	if !strings.HasPrefix(last, "@") {
		t.Fatalf("argv %v", p.Commands[0].Argv)
	}
	b, _ := os.ReadFile(p.Commands[0].ListFile)
	if strings.Count(string(b), "\n") != 400 || !strings.Contains(strings.Join(p.Notes, " "), "8.2") {
		t.Fatalf("list file or note wrong: %d lines, notes %v", strings.Count(string(b), "\n"), p.Notes)
	}
	// without a list dir the list is split into batches, never truncated
	p, _ = Commands(pysel("pytest", ".", "", files...), Options{MaxArgChars: 2000})
	n := 0
	for _, c := range p.Commands {
		n += len(c.Argv) - 5
	}
	if len(p.Commands) < 2 || n != 400 {
		t.Fatalf("batches %d, files %d", len(p.Commands), n)
	}
}

func TestDjangoCommands(t *testing.T) {
	p, _ := Commands(pysel("django", ".", "tests/runtests.py", "tests/queries/tests.py", "tests/http/test_x.py"), Options{})
	if want := [][]string{{"python", "tests/runtests.py", "http.test_x", "queries.tests"}}; !reflect.DeepEqual(argvs(p), want) {
		t.Errorf("runtests: %v", argvs(p))
	}
	p, _ = Commands(pysel("django", "site", "site/manage.py", "site/app/tests.py"), Options{})
	if want := [][]string{{"python", "manage.py", "test", "app.tests"}}; !reflect.DeepEqual(argvs(p), want) {
		t.Errorf("manage: %v", argvs(p))
	}
}

package godyn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qxbao/bgraph/internal/tree"
)

// fakeGoroot writes a GOROOT whose syscall package defines StartProcess (windows) and forkExec (unix) at known lines.
func fakeGoroot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "src", "syscall")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package syscall\n\nfunc StartProcess(argv0 string) (int, error) {\n\tif argv0 == \"\" {\n\t\treturn 0, nil\n\t}\n\treturn 1, nil\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "exec_windows.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	src = strings.Replace(src, "func StartProcess", "func forkExec", 1)
	if err := os.WriteFile(filepath.Join(dir, "exec_unix.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestReadProfile(t *testing.T) {
	repo := t.TempDir()
	pkgDir := filepath.ToSlash(filepath.Join(repo, "shop"))
	os.MkdirAll(filepath.Join(repo, "shop"), 0o755)
	os.WriteFile(filepath.Join(repo, "shop", "cart.go"), []byte(cartGo), 0o644)
	p := &Package{Pkgs: map[string]string{"example.com/shop": pkgDir}}
	goroot := fakeGoroot(t)
	for _, goos := range []string{"windows", "linux"} {
		file := "syscall/exec_windows.go"
		if goos == "linux" {
			file = "syscall/exec_unix.go"
		}
		prof := "mode: atomic\n" +
			"example.com/shop/cart.go:15.28,17.29 2 3\n" +
			"example.com/shop/cart.go:17.29,19.3 1 0\n" +
			"example.com/shop/cart.go:23.27,23.47 1 0\n" +
			"example.com/other/x.go:1.1,2.2 1 1\n" +
			"os/exec.go:10.1,11.2 1 4\n" +
			file + ":3.48,4.17 1 2\n" +
			file + ":4.17,6.3 1 1\n" +
			file + ":7.2,7.14 1 1\n"
		o := Outcome{Pkg: p}
		readProfile(strings.NewReader(prof), &o, &spawnLocator{goroot: goroot, goos: goos})
		if len(o.Incomplete) > 0 || len(o.Unmapped) > 0 {
			t.Fatalf("%s: %v %v", goos, o.Incomplete, o.Unmapped)
		}
		ls := o.Lines[pkgDir+"/cart.go"]
		if !ls[15] || !ls[17] || ls[18] || ls[19] || ls[23] || len(ls) != 6 {
			t.Errorf("%s: lines %v", goos, ls)
		}
		if o.Started != 2 {
			t.Errorf("%s: started %d", goos, o.Started)
		}
	}
	// No syscall coverage: unknown. A repository name that names no file: unmapped (incomplete), unless the package has //line
	// directives.
	o := Outcome{Pkg: p}
	readProfile(strings.NewReader("mode: atomic\nexample.com/shop/gen.go:1.1,2.2 1 1\nexample.com/shop/cart.go:233.0,45.0 1 0\n"), &o,
		&spawnLocator{goroot: goroot, goos: "linux"})
	if o.Started != -1 || len(o.Unmapped) != 2 {
		t.Errorf("started %d unmapped %v", o.Started, o.Unmapped)
	}
	p.LineDirectives = true
	o = Outcome{Pkg: p}
	readProfile(strings.NewReader("mode: atomic\nexample.com/shop/gen.rl:1.1,2.2 1 1\nexample.com/shop/cart.go:233.0,45.0 1 0\n"), &o,
		&spawnLocator{goroot: goroot, goos: "linux"})
	if len(o.Unmapped) != 0 || len(o.Incomplete) != 0 {
		t.Errorf("line directives: unmapped %v incomplete %v", o.Unmapped, o.Incomplete)
	}
	o = Outcome{Pkg: p}
	readProfile(strings.NewReader("mode: atomic\nexample.com/shop/cart.go:1.1 x\n"), &o, &spawnLocator{goroot: goroot, goos: "linux"})
	if len(o.Incomplete) == 0 {
		t.Error("malformed line accepted")
	}
}

// Fault injection: every incomplete reason of a function record.
func TestRecordOfIncomplete(t *testing.T) {
	repo := t.TempDir()
	files := map[string]string{"go.mod": "module example.com/shop\n", "shop/cart.go": cartGo, "shop/cart_test.go": "package shop\n"}
	for f, c := range files {
		p := filepath.Join(repo, filepath.FromSlash(f))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(c), 0o644)
	}
	os.WriteFile(filepath.Join(repo, "shop", "gen.go"), []byte("package shop\n"), 0o644) // untracked, generated at build time
	wt := tree.NewMapTree(files)
	abs := func(f string) string { return filepath.ToSlash(filepath.Join(repo, filepath.FromSlash(f))) }
	pkg := &Package{Unit: Unit{ID: unit, Dir: "shop", Module: "."}, Files: []string{abs("go.mod"), abs("shop/cart.go"), abs("shop/cart_test.go")},
		TestFiles: []string{abs("shop/cart_test.go")}, Pkgs: map[string]string{"example.com/shop": abs("shop")}}
	good := Outcome{Unit: SubID(unit, "TestTotal"), Name: "TestTotal", Pkg: pkg, Ran: true, Exit: 0, Counters: 1, MainWrote: true, Started: 0,
		Lines: map[string]LineSet{abs("shop/cart.go"): {15: true, 23: false}}}
	now := time.Now()
	r := RecordOf(good, wt, repo, env, "c1", now)
	if !r.Complete || !r.Pass || r.LineCov["shop/cart.go"] != "15|23" || len(r.Deps) != 3 {
		t.Fatalf("good: %+v", r)
	}
	cases := map[string]struct {
		f    func(o *Outcome)
		want string
		pass bool
	}{
		"unobserved process":  {func(o *Outcome) { o.Started = 1 }, "starts a process the collector does not observe", true},
		"observed helper":     {func(o *Outcome) { o.Started, o.Counters = 1, 2 }, "", true},
		"unknown starts":      {func(o *Outcome) { o.Started = -1 }, "cannot count the processes", true},
		"timeout":             {func(o *Outcome) { o.TimedOut = true }, "timeout", false},
		"no counter file":     {func(o *Outcome) { o.Counters, o.MainWrote = 0, false }, "no counter file", true},
		"main wrote nothing":  {func(o *Outcome) { o.Counters, o.MainWrote = 1, false }, "the test process wrote no counter file", true},
		"zero tests run":      {func(o *Outcome) { o.Ran, o.NoTests = false, true }, "did not run", false},
		"failed":              {func(o *Outcome) { o.Exit = 1 }, "", false},
		"start error":         {func(o *Outcome) { o.Err = "exec: not found" }, "exec: not found", false},
		"unmapped cover name": {func(o *Outcome) { o.Unmapped = []string{"example.com/shop/zz.go"} }, "maps to no tracked repository file", true},
		"untracked generated": {func(o *Outcome) {
			p := *pkg
			p.Files = append([]string{abs("shop/gen.go")}, p.Files...)
			o.Pkg = &p
		}, "compiles untracked repository file shop/gen.go", true},
		"missing canary": {func(o *Outcome) {
			p := *pkg
			p.TestFiles = nil
			o.Pkg = &p
		}, "canary", true},
		"outside module": {func(o *Outcome) {
			p := *pkg
			p.Outside = []string{"builds module example.com/x from a directory outside the repository and the module cache: /x"}
			o.Pkg = &p
		}, "outside the repository", true},
		"covered file not compiled": {func(o *Outcome) {
			o.Lines = map[string]LineSet{abs("go.mod"): {1: true}}
		}, "not among the compiled files", true},
	}
	for name, c := range cases {
		o := good
		c.f(&o)
		r := RecordOf(o, wt, repo, env, "c1", now)
		if c.want == "" {
			if !r.Complete {
				t.Errorf("%s: incomplete %q", name, r.Incomplete)
			}
		} else if r.Complete || !strings.Contains(r.Incomplete, c.want) {
			t.Errorf("%s: complete=%v %q, want %q", name, r.Complete, r.Incomplete, c.want)
		}
		if r.Pass != c.pass {
			t.Errorf("%s: pass=%v", name, r.Pass)
		}
	}
}

func TestListRecordOf(t *testing.T) {
	wt := tree.NewMapTree(map[string]string{"shop/a_test.go": "package shop\n", "shop/b_linux_test.go": "package shop\n",
		"shop/sub/c_test.go": "package sub\n", "shop/a.go": "package shop\n"})
	p := &Package{Unit: Unit{ID: unit, Dir: "shop", Module: "."}, Names: []string{"TestA", "TestB"}}
	r := ListRecordOf(p, wt, env, "c1", time.Now())
	if !r.Complete || !r.Pass || len(r.Deps) != 2 || r.Deps["shop/b_linux_test.go"] == "" || len(r.Classes) != 2 {
		t.Fatalf("%+v", r)
	}
	p.Names = nil
	if r := ListRecordOf(p, wt, env, "c1", time.Now()); r.Complete {
		t.Error("empty list complete")
	}
	p.Err = "go test -c: exit 1"
	if r := ListRecordOf(p, wt, env, "c1", time.Now()); r.Complete || r.Pass {
		t.Error("failed build usable")
	}
}

func TestCheckFlags(t *testing.T) {
	for _, c := range []struct {
		args    []string
		goflags string
		bad     bool
	}{
		{[]string{"-tags=x", "-race"}, "-mod=mod", false},
		{[]string{"-coverpkg=./..."}, "", true},
		{nil, "-cover", true},
		{[]string{"--covermode", "set"}, "", true},
	} {
		if got := CheckFlags(c.args, c.goflags) != ""; got != c.bad {
			t.Errorf("%v %q: %v", c.args, c.goflags, got)
		}
	}
}

func TestFingerprint(t *testing.T) {
	p := Probe{Vars: map[string]string{"GOVERSION": "go1.25.1", "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0", "GOROOT": "/a"}}
	q := Probe{Vars: map[string]string{"GOVERSION": "go1.25.1", "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0", "GOROOT": "/b"}}
	if Fingerprint(p, nil) != Fingerprint(q, nil) {
		t.Error("GOROOT changes the fingerprint")
	}
	if Fingerprint(p, nil) == Fingerprint(p, []string{"-tags=x"}) {
		t.Error("build arguments do not change the fingerprint")
	}
	q.Vars["GOVERSION"] = "go1.25.2"
	if Fingerprint(p, nil) == Fingerprint(q, nil) {
		t.Error("the toolchain version does not change the fingerprint")
	}
	t.Setenv("GOEXPERIMENT", "x")
	a := Fingerprint(p, nil)
	t.Setenv("GOEXPERIMENT", "y")
	if a == Fingerprint(p, nil) {
		t.Error("GOEXPERIMENT does not change the fingerprint")
	}
}

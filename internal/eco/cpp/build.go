package cpp

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	scan "github.com/qxbao/bgraph/internal/scan/cpp"
	"github.com/qxbao/bgraph/internal/tree"
)

type script struct {
	file  string
	calls []scan.Call
	err   error
}

type target struct {
	dir        string
	executable bool
	files      []string
	deps       []string
	// either holds target_sources entries named from another directory:
	// CMake resolves them from the caller's directory, or from the target's
	// declaring directory under older policies. At least one must exist.
	either [][2]string
}

type project struct {
	system, dir string
	scripts     map[string]script
	visited     map[string]bool
	targets     map[string]*target
	tests       map[string]string
	mesonValues map[string][]string
	opaque      string
}

func (p *project) fail(file string, line int, problem string) {
	if p.opaque == "" {
		p.opaque = fmt.Sprintf("%s:%d: %s", file, line, problem)
	}
}

func (p *project) read(dir string) {
	s, ok := p.scripts[dir]
	if !ok || p.visited[dir] {
		p.fail(path.Join(dir, manifestName(p.system)), 1, "missing or repeated build subdirectory")
		return
	}
	p.visited[dir] = true
	for _, c := range s.calls {
		valid := false
		switch p.system {
		case CMake:
			valid = p.cmake(dir, c)
		case Meson:
			valid = p.meson(dir, c)
		}
		if !valid {
			p.fail(s.file, c.Line, "cannot prove inputs of "+c.Name)
		}
	}
	if s.err != nil {
		p.fail(s.file, 1, s.err.Error())
	}
}

func (p *project) finish(t tree.Tree) {
	for _, name := range slices.Sorted(maps.Keys(p.tests)) {
		owner := p.tests[name]
		if tg := p.targets[owner]; tg == nil || !tg.executable {
			p.fail(path.Join(p.dir, manifestName(p.system)), 1, "test "+name+" has no literal executable target")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.targets)) {
		tg := p.targets[name]
		for _, pair := range tg.either {
			found := false
			for _, f := range pair {
				if t.Has(f) {
					tg.files = append(tg.files, f)
					found = true
				}
			}
			if !found {
				tg.files = append(tg.files, pair[0])
			}
		}
		tg.either = nil
		for _, f := range tg.files {
			if !t.Has(f) {
				p.fail(path.Join(p.dir, manifestName(p.system)), 1, "literal build input is absent: "+f)
			}
			if !scan.Native(f) {
				p.fail(path.Join(p.dir, manifestName(p.system)), 1, "unsupported native build input: "+f)
			}
		}
		for _, dep := range tg.deps {
			if p.targets[dep] == nil {
				p.fail(path.Join(p.dir, manifestName(p.system)), 1, "cannot resolve build target "+dep)
			}
		}
	}
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var targetName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]*$`)
var testName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]*$`)

func literal(s string) bool {
	return s != "" && !strings.ContainsAny(s, "$;\\\x00\r\n<>")
}

func local(dir, rel string) (string, bool) {
	if !literal(rel) || path.IsAbs(rel) || strings.ContainsAny(rel, ":*?[]~") {
		return "", false
	}
	f := path.Join(dir, rel)
	return f, f != ".." && !strings.HasPrefix(f, "../")
}

func files(dir string, names []string) ([]string, bool) {
	var paths []string
	for _, n := range names {
		f, ok := local(dir, n)
		if !ok {
			return nil, false
		}
		paths = append(paths, f)
	}
	return paths, true
}

func (p *project) subdir(dir, rel string) bool {
	d, ok := local(dir, rel)
	if !ok || d == dir || (dir != "." && !strings.HasPrefix(d, dir+"/")) {
		return false
	}
	p.read(d)
	return true
}

func (p *project) addTarget(dir, name string, executable bool, sources []string) bool {
	if !targetName.MatchString(name) || p.targets[name] != nil {
		return false
	}
	p.targets[name] = &target{dir: dir, executable: executable, files: sources}
	return true
}

func (p *project) addTest(name, owner string) bool {
	if !testName.MatchString(name) || !targetName.MatchString(owner) || p.tests[name] != "" {
		return false
	}
	p.tests[name] = owner
	return true
}

func (p *project) cmake(dir string, c scan.Call) bool {
	var args []string
	for _, a := range c.Args {
		if !literal(a.Text) {
			return false
		}
		args = append(args, a.Text)
	}
	switch c.Name {
	case "cmake_minimum_required", "project":
		return len(args) > 0
	case "enable_testing":
		return len(args) == 0
	case "add_subdirectory":
		return len(args) == 1 && p.subdir(dir, args[0])
	case "add_executable", "add_library":
		if len(args) < 2 {
			return false
		}
		src := args[1:]
		if slices.ContainsFunc(src, func(arg string) bool {
			return slices.Contains([]string{"ALIAS", "IMPORTED", "GLOBAL", "WIN32", "MACOSX_BUNDLE", "EXCLUDE_FROM_ALL"}, arg)
		}) {
			return false
		}
		if c.Name == "add_library" && slices.Contains([]string{"STATIC", "SHARED", "MODULE", "OBJECT", "INTERFACE"}, src[0]) {
			src = src[1:]
		}
		f, ok := files(dir, src)
		return ok && p.addTarget(dir, args[0], c.Name == "add_executable", f)
	case "target_link_libraries", "add_dependencies":
		if len(args) < 2 || p.targets[args[0]] == nil {
			return false
		}
		tg := p.targets[args[0]]
		for _, dep := range args[1:] {
			switch dep {
			case "PRIVATE", "PUBLIC", "INTERFACE", "debug", "optimized", "general":
				if c.Name == "target_link_libraries" {
					continue
				}
			}
			tg.deps = append(tg.deps, dep)
		}
		return true
	case "target_sources":
		if len(args) < 3 || p.targets[args[0]] == nil {
			return false
		}
		tg := p.targets[args[0]]
		for _, name := range args[1:] {
			switch name {
			case "PRIVATE", "PUBLIC", "INTERFACE":
				continue
			case "FILE_SET":
				return false
			}
			f, ok := local(dir, name)
			if !ok {
				return false
			}
			// Older CMake policies resolve relative entries from the target's
			// declaring directory instead of the caller's directory.
			if tg.dir != dir {
				alternate, ok := local(tg.dir, name)
				if !ok {
					return false
				}
				if alternate != f {
					tg.either = append(tg.either, [2]string{f, alternate})
					continue
				}
			}
			tg.files = append(tg.files, f)
			// Interface sources can be compiled by the consuming targets.
		}
		return true
	case "target_include_directories":
		if len(args) < 3 || p.targets[args[0]] == nil {
			return false
		}
		for _, name := range args[1:] {
			switch name {
			case "PRIVATE", "PUBLIC", "INTERFACE", "SYSTEM", "BEFORE", "AFTER":
				continue
			}
			if _, ok := local(dir, name); !ok {
				return false
			}
		}
		return true
	case "add_test":
		// CMake's legacy signature does not resolve executable target names.
		// Use the signature that explicitly carries target semantics.
		return len(args) == 4 && args[0] == "NAME" && args[2] == "COMMAND" && p.addTest(args[1], args[3])
	default:
		return false
	}
}

func (p *project) meson(dir string, c scan.Call) bool {
	if c.Assign != "" && (!identifier.MatchString(c.Assign) || p.targets[c.Assign] != nil || p.mesonValues[c.Assign] != nil) {
		return false
	}
	switch c.Name {
	case "project":
		// Default options can supply compiler and linker flags affecting
		// other paths and targets, so only accept positional literals.
		return c.Assign == "" && len(c.Args) >= 2 && allQuoted(c.Args)
	case "subdir":
		return c.Assign == "" && len(c.Args) == 1 && allQuoted(c.Args) && p.subdir(dir, c.Args[0].Text)
	case "files":
		if c.Assign == "" {
			return false
		}
		f, ok := p.mesonSources(dir, c.Args)
		if ok {
			p.mesonValues[c.Assign] = append([]string{}, f...)
		}
		return ok
	case "executable", "library", "static_library", "shared_library", "both_libraries":
		return p.mesonTarget(dir, c)
	case "test":
		if c.Assign != "" || len(c.Args) != 2 || !allQuoted(c.Args[:1]) {
			return false
		}
		a := c.Args[1]
		return a.Key == "" && !a.Quoted && a.List == nil && p.targets[a.Text] != nil && p.addTest(c.Args[0].Text, a.Text)
	default:
		return false
	}
}

func allQuoted(args []scan.Arg) bool {
	return len(args) != 0 && !slices.ContainsFunc(args, func(a scan.Arg) bool {
		return !a.Quoted || a.Key != "" || !literal(a.Text)
	})
}

func (p *project) mesonSources(dir string, args []scan.Arg) ([]string, bool) {
	var out []string
	for _, a := range args {
		if a.Key != "" {
			return nil, false
		}
		switch {
		case a.List != nil:
			f, ok := p.mesonSources(dir, a.List)
			if !ok {
				return nil, false
			}
			out = append(out, f...)
		case a.Quoted:
			f, ok := local(dir, a.Text)
			if !ok {
				return nil, false
			}
			out = append(out, f)
		default:
			f, ok := p.mesonValues[a.Text]
			if !ok {
				return nil, false
			}
			out = append(out, f...)
		}
	}
	return out, true
}

func (p *project) mesonTarget(dir string, c scan.Call) bool {
	if len(c.Args) < 2 || !allQuoted(c.Args[:1]) || !targetName.MatchString(c.Args[0].Text) || c.Assign == "" {
		return false
	}
	var src, args []scan.Arg
	var deps []string
	for _, a := range c.Args[1:] {
		switch a.Key {
		case "link_with", "link_whole":
			args = a.List
			if args == nil {
				a.Key = ""
				args = []scan.Arg{a}
			}
			for _, d := range args {
				if d.Quoted || d.Key != "" || d.List != nil || p.targets[d.Text] == nil {
					return false
				}
				deps = append(deps, d.Text)
			}
		case "", "sources":
			a.Key = ""
			src = append(src, a)
		default:
			return false
		}
	}
	f, ok := p.mesonSources(dir, src)
	if !ok || !p.addTarget(dir, c.Assign, c.Name == "executable", f) {
		return false
	}
	p.targets[c.Assign].deps = deps
	return true
}

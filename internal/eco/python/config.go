package python

import (
	"path"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/qxbao/bgraph/internal/tree"
)

// ---- INI ----

type ini struct {
	sections map[string]map[string]string
}

// parseINI reads the subset of INI that pytest.ini, tox.ini and setup.cfg use: [section], key = value / key: value, indented
// continuation lines (joined with "\n"), full-line # and ; comments.
func parseINI(src string) *ini {
	out := &ini{sections: map[string]map[string]string{}}
	var sec map[string]string
	key := ""
	for _, raw := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if trim[0] == '#' || trim[0] == ';' {
			continue
		}
		if raw[0] == ' ' || raw[0] == '\t' {
			if sec != nil && key != "" {
				sec[key] += "\n" + trim
			}
			continue
		}
		if trim[0] == '[' {
			if end := strings.IndexByte(trim, ']'); end > 0 {
				name := strings.TrimSpace(trim[1:end])
				sec = out.sections[name]
				if sec == nil {
					sec = map[string]string{}
					out.sections[name] = sec
				}
				key = ""
				continue
			}
		}
		if sec == nil {
			continue
		}
		i := strings.IndexAny(trim, "=:")
		if i < 0 {
			key = ""
			continue
		}
		key = strings.TrimSpace(trim[:i])
		sec[key] = strings.TrimSpace(trim[i+1:])
	}
	return out
}

// shlexSplit splits like shlex.split (POSIX): whitespace separates, quotes group, backslash escapes outside single quotes.
func shlexSplit(s string) []string {
	var out []string
	var cur strings.Builder
	in := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			in = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// ---- fnmatch ----

// fnmatch is Python's fnmatch.fnmatchcase: * matches any run of characters (including "/"), ? one character, [seq] and [!seq] a class.
func fnmatch(pat, name string) bool {
	return fnm([]rune(pat), []rune(name))
}

func fnm(p, s []rune) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		if pi < len(p) {
			switch p[pi] {
			case '*':
				star, mark = pi, si
				pi++
				continue
			case '?':
				pi++
				si++
				continue
			case '[':
				if ok, n := classMatch(p[pi:], s[si]); n > 0 {
					if ok {
						pi += n
						si++
						continue
					}
					goto fail
				}
				if s[si] == '[' {
					pi++
					si++
					continue
				}
			default:
				if p[pi] == s[si] {
					pi++
					si++
					continue
				}
			}
		}
	fail:
		if star >= 0 {
			mark++
			si, pi = mark, star+1
			continue
		}
		return false
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// classMatch matches r against the class at the start of p ("[abc]", "[!a-z]"); n is the class length, 0 when p does not start a class.
func classMatch(p []rune, r rune) (bool, int) {
	i := 1
	neg := false
	if i < len(p) && (p[i] == '!' || p[i] == '^') {
		neg = true
		i++
	}
	start := i
	if i < len(p) && p[i] == ']' {
		i++
	}
	for i < len(p) && p[i] != ']' {
		i++
	}
	if i >= len(p) {
		return false, 0
	}
	hit := false
	for j := start; j < i; j++ {
		if j+2 < i && p[j+1] == '-' {
			if p[j] <= r && r <= p[j+2] {
				hit = true
			}
			j += 2
		} else if p[j] == r {
			hit = true
		}
	}
	return hit != neg, i + 1
}

// fnmatchEx is pytest's fnmatch_ex: a pattern without "/" matches the base name, one with "/" matches the full path with an implicit "*/" prefix.
func fnmatchEx(pat, p string) bool {
	if !strings.Contains(pat, "/") {
		return fnmatch(pat, path.Base(p))
	}
	pat = strings.TrimPrefix(pat, "./")
	if !strings.HasPrefix(pat, "/") {
		pat = "*/" + pat
	}
	return fnmatch(pat, "/"+p)
}

// ---- pytest configuration ----

// pytestCfg is the pytest configuration that decides which files are tests.
type pytestCfg struct {
	file, dir string
	// pythonFiles, testpaths, norecurse: nil when the key is absent (pytest defaults apply).
	pythonFiles []string
	testpaths   []string
	norecurse   []string
	pythonpath  []string
	addopts     []string
	doctest     bool
	plugins     []string // -p name from addopts
	ignore      []string // --ignore paths, relative to dir
	ignoreGlob  []string
	// opaque lists keys whose value is computed (tox substitution, a non-string TOML value, an unreadable file): the set of test
	// files is then unknown.
	opaque []string
}

var defaultPythonFiles = []string{"test_*.py", "*_test.py"}

var defaultNorecurse = []string{"*.egg", ".*", "_darcs", "build", "CVS", "dist", "node_modules", "venv", "{arch}"}

// pytestConfigNames is pytest's search order inside one directory.
var pytestConfigNames = []string{"pytest.toml", ".pytest.toml", "pytest.ini", ".pytest.ini", "pyproject.toml", "tox.ini", "setup.cfg"}

// rawVal is a configuration value: a string (INI, or a TOML string) or a TOML list.
type rawVal struct {
	s    string
	list []any
	isL  bool
}

type cfgSource func(key string) (rawVal, bool)

// loadPytestAt returns the pytest configuration stored in directory dir, or nil when pytest would not take its configuration from there.
func loadPytestAt(t tree.Tree, dir string) *pytestCfg {
	for _, name := range pytestConfigNames {
		p := path.Join(dir, name)
		if !t.Has(p) {
			continue
		}
		b, err := t.Read(p)
		if err != nil {
			return &pytestCfg{file: p, dir: dir, opaque: []string{p + " unreadable"}}
		}
		var src cfgSource
		found := false
		isTox := false
		switch name {
		case "pytest.toml", ".pytest.toml":
			var doc map[string]any
			if err := toml.Unmarshal(b, &doc); err != nil {
				return &pytestCfg{file: p, dir: dir, opaque: []string{p + " unreadable"}}
			}
			tb, _ := doc["pytest"].(map[string]any)
			src, found = tomlSource(tb), true
		case "pytest.ini", ".pytest.ini":
			src, found = iniSource(parseINI(string(b)).sections["pytest"]), true
		case "pyproject.toml":
			var doc map[string]any
			if err := toml.Unmarshal(b, &doc); err != nil {
				return &pytestCfg{file: p, dir: dir, opaque: []string{p + " unreadable"}}
			}
			tool, _ := doc["tool"].(map[string]any)
			pt, _ := tool["pytest"].(map[string]any)
			if io, ok := pt["ini_options"].(map[string]any); ok {
				src, found = tomlSource(io), true
			} else if pt != nil {
				src, found = tomlSource(pt), true
			}
		case "tox.ini":
			if sec, ok := parseINI(string(b)).sections["pytest"]; ok {
				src, found, isTox = iniSource(sec), true, true
			}
		case "setup.cfg":
			if sec, ok := parseINI(string(b)).sections["tool:pytest"]; ok {
				src, found = iniSource(sec), true
			} else if sec, ok := parseINI(string(b)).sections["pytest"]; ok {
				src, found = iniSource(sec), true // deprecated spelling, still read by older pytest
			}
		}
		if !found {
			continue
		}
		return buildPytestCfg(p, dir, src, isTox)
	}
	return nil
}

func iniSource(sec map[string]string) cfgSource {
	return func(key string) (rawVal, bool) {
		v, ok := sec[key]
		return rawVal{s: v}, ok
	}
}

func tomlSource(tb map[string]any) cfgSource {
	return func(key string) (rawVal, bool) {
		v, ok := tb[key]
		if !ok {
			return rawVal{}, false
		}
		switch x := v.(type) {
		case string:
			return rawVal{s: x}, true
		case []any:
			return rawVal{list: x, isL: true}, true
		}
		return rawVal{s: "\x00"}, true // a type pytest would reject or a table: not literal
	}
}

func buildPytestCfg(file, dir string, src cfgSource, isTox bool) *pytestCfg {
	c := &pytestCfg{file: file, dir: dir}
	// list reads a list value. args: shlex-split (python_files, testpaths, norecursedirs); otherwise one entry per line.
	list := func(key string, args bool) ([]string, bool) {
		rv, ok := src(key)
		if !ok {
			return nil, false
		}
		if rv.isL {
			var out []string
			for _, x := range rv.list {
				s, ok := x.(string)
				if !ok {
					c.opaque = append(c.opaque, key+" has a non-string entry")
					return nil, false
				}
				out = append(out, s)
			}
			return out, true
		}
		if rv.s == "\x00" || isTox && strings.Contains(rv.s, "{") || strings.Contains(rv.s, "$") && strings.Contains(rv.s, "{") {
			c.opaque = append(c.opaque, key+" is computed")
			return nil, false
		}
		if args {
			return shlexSplit(rv.s), true
		}
		return splitLines(rv.s), true
	}
	if v, ok := list("python_files", true); ok {
		c.pythonFiles = v
		if c.pythonFiles == nil {
			c.pythonFiles = []string{}
		}
	}
	if v, ok := list("testpaths", true); ok {
		c.testpaths = v
	}
	if v, ok := list("norecursedirs", true); ok {
		c.norecurse = v
		if c.norecurse == nil {
			c.norecurse = []string{}
		}
	}
	if v, ok := list("pythonpath", false); ok {
		c.pythonpath = v
	}
	if rv, ok := src("addopts"); ok {
		switch {
		case rv.isL:
			for _, x := range rv.list {
				if s, ok := x.(string); ok {
					c.addopts = append(c.addopts, shlexSplit(s)...)
				}
			}
		case rv.s == "\x00" || isTox && strings.Contains(rv.s, "{"):
			c.opaque = append(c.opaque, "addopts is computed")
		default:
			c.addopts = shlexSplit(rv.s)
		}
	}
	c.applyAddopts()
	return c
}

// applyAddopts reads the options of addopts that change what is collected or loaded.
func (c *pytestCfg) applyAddopts() {
	o := c.addopts
	for i := 0; i < len(o); i++ {
		a := o[i]
		val := func(long string) (string, bool) {
			if a == long && i+1 < len(o) {
				i++
				return o[i], true
			}
			if strings.HasPrefix(a, long+"=") {
				return a[len(long)+1:], true
			}
			return "", false
		}
		switch {
		case a == "--doctest-modules":
			c.doctest = true
		case a == "-p" && i+1 < len(o):
			i++
			c.addPlugin(o[i])
		case strings.HasPrefix(a, "-p") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			c.addPlugin(strings.TrimPrefix(a[2:], "="))
		default:
			if v, ok := val("--ignore"); ok {
				c.ignore = append(c.ignore, path.Clean(strings.ReplaceAll(v, "\\", "/")))
			} else if v, ok := val("--ignore-glob"); ok {
				c.ignoreGlob = append(c.ignoreGlob, v)
			} else if v, ok := val("--override-ini"); ok {
				c.override(v)
			} else if a == "-o" && i+1 < len(o) {
				i++
				c.override(o[i])
			} else if strings.HasPrefix(a, "-o") && strings.Contains(a, "=") && !strings.HasPrefix(a, "--") {
				c.override(a[2:])
			}
		}
	}
}

func (c *pytestCfg) addPlugin(name string) {
	if name != "" && !strings.HasPrefix(name, "no:") {
		c.plugins = append(c.plugins, name)
	}
}

// override applies `-o key=value` for the keys that decide collection.
func (c *pytestCfg) override(kv string) {
	k, v, ok := strings.Cut(kv, "=")
	if !ok {
		return
	}
	switch strings.TrimSpace(k) {
	case "python_files":
		c.pythonFiles = shlexSplit(v)
	case "testpaths":
		c.testpaths = shlexSplit(v)
	case "norecursedirs":
		c.norecurse = shlexSplit(v)
	case "pythonpath":
		c.pythonpath = splitLines(v)
	}
}

func (c *pytestCfg) patterns() []string {
	if c != nil && c.pythonFiles != nil {
		return c.pythonFiles
	}
	return defaultPythonFiles
}

func (c *pytestCfg) norecursePatterns() []string {
	if c != nil && c.norecurse != nil {
		return c.norecurse
	}
	return defaultNorecurse
}

// ---- layout: where import roots live ----

// layout is what pyproject.toml, setup.cfg and setup.py say about where packages live.
type layout struct {
	roots []string // directories (repo-relative) that are import roots
	maps  []pkgMap // `package-dir` entries that map one package name to a directory
}

type pkgMap struct {
	name, dir string
}

var (
	reSetupPkgDir = regexp.MustCompile(`package_dir\s*=\s*\{\s*["']{2}\s*:\s*["']([^"']+)["']`)
	reSetupWhere  = regexp.MustCompile(`(?:where\s*=\s*|find(?:_namespace)?_packages\(\s*)["']([^"']+)["']`)
	reSetupMapped = regexp.MustCompile(`["']([A-Za-z_][\w.]*)["']\s*:\s*["']([^"']+)["']`)
)

func joinDir(dir, p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return dir
	}
	return path.Join(dir, p)
}

func strList(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func tbl(v any) map[string]any { m, _ := v.(map[string]any); return m }

func (l *layout) root(dir, p string) { l.roots = append(l.roots, joinDir(dir, p)) }

// readLayout collects the roots a project declares. Everything is a candidate (superset).
func readLayout(t tree.Tree, dir string) layout {
	var l layout
	if p := path.Join(dir, "pyproject.toml"); t.Has(p) {
		if b, err := t.Read(p); err == nil {
			var doc map[string]any
			if toml.Unmarshal(b, &doc) == nil {
				l.fromPyproject(dir, doc)
			}
		}
	}
	if p := path.Join(dir, "setup.cfg"); t.Has(p) {
		if b, err := t.Read(p); err == nil {
			ini := parseINI(string(b))
			for _, line := range splitLines(ini.sections["options"]["package_dir"]) {
				name, val, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				if name = strings.TrimSpace(name); name == "" {
					l.root(dir, strings.TrimSpace(val))
				} else {
					l.maps = append(l.maps, pkgMap{name, joinDir(dir, strings.TrimSpace(val))})
				}
			}
			for _, w := range splitLines(ini.sections["options.packages.find"]["where"]) {
				l.root(dir, w)
			}
		}
	}
	if p := path.Join(dir, "setup.py"); t.Has(p) {
		if b, err := t.Read(p); err == nil && len(b) < 1<<20 {
			for _, m := range reSetupPkgDir.FindAllSubmatch(b, -1) {
				l.root(dir, string(m[1]))
			}
			for _, m := range reSetupWhere.FindAllSubmatch(b, -1) {
				l.root(dir, string(m[1]))
			}
			if i := strings.Index(string(b), "package_dir"); i >= 0 {
				end := i + 400
				if end > len(b) {
					end = len(b)
				}
				for _, m := range reSetupMapped.FindAllSubmatch(b[i:end], -1) {
					if string(m[1]) != "" {
						l.maps = append(l.maps, pkgMap{string(m[1]), joinDir(dir, string(m[2]))})
					}
				}
			}
		}
	}
	return l
}

func (l *layout) fromPyproject(dir string, doc map[string]any) {
	tool := tbl(doc["tool"])
	if st := tbl(tool["setuptools"]); st != nil {
		switch pd := st["package-dir"].(type) {
		case map[string]any:
			for name, v := range pd {
				if s, ok := v.(string); ok {
					if name == "" {
						l.root(dir, s)
					} else {
						l.maps = append(l.maps, pkgMap{name, joinDir(dir, s)})
					}
				}
			}
		}
		for _, k := range []string{"packages"} {
			if f := tbl(tbl(st[k])["find"]); f != nil {
				for _, w := range strList(f["where"]) {
					l.root(dir, w)
				}
			}
		}
	}
	if h := tbl(tbl(tool["hatch"])["build"]); h != nil {
		for _, s := range strList(h["sources"]) {
			l.root(dir, s)
		}
		for s := range tbl(h["sources"]) {
			l.root(dir, s)
		}
		var scan func(m map[string]any)
		scan = func(m map[string]any) {
			for k, v := range m {
				switch k {
				case "packages", "include", "only-include":
					for _, p := range strList(v) {
						l.root(dir, path.Dir(strings.TrimSuffix(strings.ReplaceAll(p, "\\", "/"), "/")))
					}
				case "sources":
					for _, p := range strList(v) {
						l.root(dir, p)
					}
					for p := range tbl(v) {
						l.root(dir, p)
					}
				}
				if sub := tbl(v); sub != nil {
					scan(sub)
				}
			}
		}
		scan(h)
	}
	if po := tbl(tool["poetry"]); po != nil {
		if pk, ok := po["packages"].([]any); ok {
			for _, e := range pk {
				m := tbl(e)
				from, _ := m["from"].(string)
				inc, _ := m["include"].(string)
				l.root(dir, from)
				if inc != "" {
					l.root(dir, path.Join(from, path.Dir(inc)))
				}
			}
		}
	}
	if f := tbl(tbl(tool["flit"])["module"]); f != nil {
		l.root(dir, ".")
		l.root(dir, "src")
	}
	if pdm := tbl(tbl(tool["pdm"])["build"]); pdm != nil {
		for _, s := range strList(pdm["package-dir"]) {
			l.root(dir, s)
		}
		for _, p := range strList(pdm["includes"]) {
			l.root(dir, path.Dir(p))
		}
	}
	if mt := tbl(tool["maturin"]); mt != nil {
		for _, s := range strList(mt["python-source"]) {
			l.root(dir, s)
		}
		if _, ok := mt["python-source"]; !ok {
			l.root(dir, ".")
		}
	}
	if sk := tbl(tbl(tool["scikit-build"])["wheel"]); sk != nil {
		for _, p := range strList(sk["packages"]) {
			l.root(dir, path.Dir(p))
		}
	}
}

// pluginModules returns the module names of pytest11 entry points declared by the project files at dir.
func pluginModules(t tree.Tree, p string) []string {
	b, err := t.Read(p)
	if err != nil {
		return nil
	}
	var out []string
	add := func(v string) {
		mod, _, _ := strings.Cut(strings.TrimSpace(v), ":")
		mod = strings.TrimSpace(mod)
		if mod != "" {
			out = append(out, mod)
		}
	}
	switch path.Base(p) {
	case "pyproject.toml":
		var doc map[string]any
		if toml.Unmarshal(b, &doc) != nil {
			return nil
		}
		eps := tbl(tbl(doc["project"])["entry-points"])
		for k, v := range eps {
			if k == "pytest11" {
				for _, x := range tbl(v) {
					if s, ok := x.(string); ok {
						add(s)
					}
				}
			}
		}
		for _, x := range tbl(tbl(tbl(tbl(doc["tool"])["poetry"])["plugins"])["pytest11"]) {
			if s, ok := x.(string); ok {
				add(s)
			}
		}
	case "setup.cfg":
		for _, l := range splitLines(parseINI(string(b)).sections["options.entry_points"]["pytest11"]) {
			if _, v, ok := strings.Cut(l, "="); ok {
				add(v)
			}
		}
	case "setup.py":
		if i := strings.Index(string(b), "pytest11"); i >= 0 {
			rest := string(b[i:])
			if j := strings.IndexByte(rest, ']'); j > 0 {
				for _, m := range reStrLit.FindAllStringSubmatch(rest[:j], -1) {
					if _, v, ok := strings.Cut(m[1], "="); ok {
						add(v)
					}
				}
			}
		}
	}
	return out
}

var reStrLit = regexp.MustCompile(`["']([^"'\n]+)["']`)

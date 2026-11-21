package node

import (
	"path"
	"regexp"
	"strings"

	"github.com/qxbao/bgraph/internal/eco/node/resolve"
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/tree"
)

// testConfig is what can be read from a project's runner config (jest/vitest/vite/playwright...).
type testConfig struct {
	files   []string        // config files seen
	aliases []resolve.Alias // moduleNameMapper / resolve.alias that could be read
	// implicit: repo files the config points to (setupFiles, globalSetup, preset, transform...): every test in the project
	// depends on them.
	implicit []string
	// implicitGlobs: like implicit but globs (special characters in the literal path part escaped).
	implicitGlobs []string
	// opaque: the config has keys affecting resolution/test selection that cannot be read as literals → config_is_code.
	opaque []string
	// testGlobs/testRegex: test-file patterns declared by the config (Jest testMatch/testRegex, Vitest include). A file is a test if it matches
	// a config pattern OR the default pattern (union: never narrows the test set).
	testGlobs []*glob.Pattern
	testRegex []*regexp.Regexp
	// testsOpaque: test-file patterns cannot be read → the project's test set is unknown (Whole unit).
	testsOpaque bool
	// testIgnore: testPathIgnorePatterns that could be read (only constant string elements; spread/variable elements are dropped = excludes less
	// = still a superset). Applies only to matches by config pattern.
	testIgnore []*regexp.Regexp
	// roots: Jest roots directories (repo-root-relative) — where to look for tests and __mocks__ of external modules.
	roots []string
}

// isTest: p matches the config's test-file pattern.
func (tc *testConfig) isTest(p string) bool {
	for _, g := range tc.testGlobs {
		if g.Match(p) {
			return !tc.ignored(fakeAbs(p))
		}
	}
	abs := fakeAbs(p)
	for _, re := range tc.testRegex {
		// Jest matches testRegex against ABSOLUTE paths: fake prefix so patterns like "./packages/" have a character before them.
		if re.MatchString(abs) {
			return !tc.ignored(abs)
		}
	}
	return false
}

func (tc *testConfig) ignored(abs string) bool {
	for _, re := range tc.testIgnore {
		if re.MatchString(abs) {
			return true
		}
	}
	return false
}

// fakeAbs is the fake absolute path of repo file p (the repo root is /__repo__).
func fakeAbs(p string) string {
	if p == "." || p == "" {
		return "/__repo__"
	}
	return "/__repo__/" + p
}

// configBases are the names (without extension) of runner config files, searched in EVERY project directory (nested config such as
// tests/unit/jest.config.mjs run with --config).
var configBases = []string{"jest.config", "vitest.config", "vite.config", "playwright.config", "vitest.workspace", "jest.preset", "jest-preset"}

var runnerConfigName = regexp.MustCompile(`^(vitest|vite|jest|playwright)(\.[\w-]+)*\.(config|workspace)(\.[\w-]+)*\.(js|ts|mjs|cjs|mts|cts|json)$`)

// isRunnerConfig: p is a runner config file (jest.config.ts, vitest.config.e2e.mts, ...).
func isRunnerConfig(p string) bool {
	base := path.Base(p)
	// Names with a middle segment: vitest.compile.config.ts, vitest.unit.config.mts, jest.e2e.config.js (zod, many monorepos).
	if runnerConfigName.MatchString(base) {
		return true
	}
	for _, b := range configBases {
		if strings.HasPrefix(base, b+".") {
			switch path.Ext(base) {
			case ".js", ".ts", ".mjs", ".cjs", ".mts", ".cts", ".json":
				return true
			}
		}
	}
	return false
}

// resolutionKeys are config keys that change how imports resolve or the test set; when met and unreadable, widen.
// Not listed here (ignoring them is still a superset because the resolver takes the union of all candidates): modulePathIgnorePatterns (only hides some
// paths from the loader), dedupe (only merges copies of external packages).
var resolutionKeys = []string{"moduleNameMapper", "alias", "modulePaths", "moduleDirectories", "resolver", "roots", "rootDir",
	"haste", "projects", "workspace", "conditions", "mainFields", "extensions",
	"moduleFileExtensions", "customExportConditions", "testEnvironmentOptions"}

var strLit = regexp.MustCompile(`'((?:[^'\\\n]|\\.)*)'|"((?:[^"\\\n]|\\.)*)"|` + "`([^`$\\\\]*)`")

// readTestConfig reads the runner config files cfgFiles of project dir (and the "jest" key in package.json).
func readTestConfig(t tree.Tree, dir string, pkgJest []byte, cfgFiles []string) testConfig {
	var tc testConfig
	// Queue: config files that projects/workspace declare by path (arbitrary name) are also read as runner config.
	seen := map[string]bool{}
	queue := append([]string(nil), cfgFiles...)
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		b, err := t.Read(p)
		if err != nil {
			tc.opaque = append(tc.opaque, p+": "+err.Error())
			continue
		}
		tc.files = append(tc.files, p)
		src := string(b)
		tc.readOne(t, dir, p, src)
		queue = append(queue, projectConfigFiles(t, p, src)...)
	}
	if len(pkgJest) > 0 {
		tc.readOne(t, dir, path.Join(dir, "package.json"), string(pkgJest))
	}
	return tc
}

// readOne extracts from config text:
//   - every constant string pointing to a file/directory that really exists in the repo (relative to dir, or <rootDir>/) → implicit dependency of every test;
//   - moduleNameMapper / alias as an object literal with only constant strings → Alias;
//   - other unreadable resolution keys → opaque.
func (tc *testConfig) readOne(t tree.Tree, dir, file, src string) {
	src = stripJSComments(src)
	// <rootDir>: Jest's default is the directory of the config file (package.json → the project directory). A literal rootDir → follow it;
	// computed by code → unknown: test patterns use "**/" (superset), alias targets try both the config directory and the project directory.
	roots := []string{path.Dir(file)}
	if path.Dir(file) != dir {
		roots = append(roots, dir)
	}
	rootKnown := true
	for _, i := range keyIndexes(src, "rootDir") {
		if v, ok := literalValue(src[i:]); ok && quoteLen(src[i:]) != "" && !strings.Contains(v, "<rootDir>") {
			roots = []string{path.Clean(path.Join(path.Dir(file), v))}
		} else {
			rootKnown = false
		}
	}
	// include/testMatch inside these blocks do not define the test set (coverage.include is the files measured for coverage).
	tc.readTestPatterns(blankValues(src, "coverage", "optimizeDeps", "build", "server", "preview", "ssr", "deps", "css", "worker"), file, roots, rootKnown)
	// JSX runtime (oxc/esbuild/swc/tsconfig importSource): every test depends on <importSource>/jsx-runtime and
	// jsx-dev-runtime, not the whole directory.
	for _, key := range []string{"importSource", "jsxImportSource"} {
		for _, i := range keyIndexes(src, key) {
			if v, ok := literalValue(src[i:]); ok && v != "" {
				base := strings.TrimSuffix(v, "/")
				if strings.HasPrefix(base, "./") || strings.HasPrefix(base, "../") {
					base = escapeGlob(path.Join(path.Dir(file), base))
				} else {
					base = "**/" + escapeGlob(base)
				}
				for _, rt := range []string{"jsx-runtime", "jsx-dev-runtime"} {
					tc.implicitGlobs = append(tc.implicitGlobs, base+"/"+rt+"{,.js,.jsx,.ts,.tsx,.mjs,.cjs,.mts,.cts,.d.ts,/**}")
				}
			}
		}
	}
	// Values of keys that only EXCLUDE (exclude, coverage) or are handled separately (importSource) never create a
	// dependency: blank them out before gathering paths.
	scan := blankValues(src, "exclude", "coverage", "importSource", "jsxImportSource", "testPathIgnorePatterns", "include", "testMatch", "testRegex",
		"roots", "collectCoverageFrom", "coverageDirectory", "coverageReporters",
		"coveragePathIgnorePatterns", "watchPathIgnorePatterns")
	for _, m := range strLit.FindAllStringSubmatch(scan, -1) {
		s := m[1] + m[2] + m[3]
		if s == "" {
			continue
		}
		for _, cand := range configPathCandidates(dir, file, s) {
			if t.Has(cand) {
				tc.implicit = append(tc.implicit, cand)
			} else if t.IsDir(cand) && cand != "." && cand != dir {
				// directory (setupFiles dir, roots...): every file under it
				tc.implicitGlobs = append(tc.implicitGlobs, escapeGlob(cand)+"/**")
			}
		}
	}
	for _, key := range resolutionKeys {
		idxs := keyIndexes(src, key)
		for _, i := range idxs {
			// A literal null/undefined/false value means "not set" (babel: moduleNameMapper: null), not config-as-code.
			if v := strings.TrimLeft(src[i:], " \t\r\n"); strings.HasPrefix(v, "null") || strings.HasPrefix(v, "undefined") || strings.HasPrefix(v, "false") {
				continue
			}
			switch key {
			case "moduleNameMapper":
				if al, ok := parseMapper(src[i:], roots); ok {
					tc.aliases = append(tc.aliases, al...)
					continue
				}
			case "alias":
				if al, ok := parseViteAlias(src[i:], dir, file); ok {
					tc.aliases = append(tc.aliases, al...)
					continue
				}
			case "rootDir":
				// literal: already applied to roots; computed by code: test patterns use "**/", alias tries every root → still a superset.
				continue
			case "roots":
				if arr, ok := stringArray(src[i:]); ok {
					for _, r := range arr {
						for _, root := range roots {
							tc.roots = append(tc.roots, path.Clean(path.Join(root, strings.TrimPrefix(strings.ReplaceAll(r, "<rootDir>", ""), "/"))))
						}
					}
					continue // roots only limit where to look: no dependency created (already blanked out of path gathering)
				}
			case "moduleFileExtensions", "extensions", "mainFields", "conditions", "customExportConditions", "testEnvironmentOptions":
				// the resolver already tried every extension and took the union of every condition: safe to ignore.
				continue
			case "projects", "workspace":
				// An element is a string (path/glob to a sub-config, found because runner config is scanned in every
				// directory) or an object literal (inner keys were already scanned over the whole file text): readable. A variable/function
				// call → unknown which sub-project → opaque.
				if literalArrayOfStringsOrObjects(src[i:]) {
					continue
				}
			}
			tc.opaque = append(tc.opaque, file+": "+key)
		}
	}
}

// configPathCandidates: strings in config that may be a path to a repo file.
func configPathCandidates(dir, file, s string) []string {
	// "[id]", "{x}", "(group)" are valid file/directory names (Next.js...): only block real wildcards; a candidate is still accepted only
	// when it matches exactly a real file/directory.
	if strings.ContainsAny(s, "\n*?|^$") || strings.Contains(s, "://") || len(s) > 300 {
		return nil
	}
	s = strings.ReplaceAll(s, "<rootDir>", ".")
	if strings.HasPrefix(s, "/") || strings.Contains(s, ":") {
		return nil
	}
	var out []string
	if strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || strings.Contains(s, "/") || strings.Contains(s, ".") {
		for _, base := range []string{dir, path.Dir(file)} {
			c := path.Join(base, s)
			if c == ".." || strings.HasPrefix(c, "../") {
				continue
			}
			out = append(out, c)
		}
	}
	return out
}

// keyIndexes returns positions right after "key:" (or "\"key\":") in the config text.
func keyIndexes(src, key string) []int {
	var out []int
	for off := 0; ; {
		i := strings.Index(src[off:], key)
		if i < 0 {
			return out
		}
		i += off
		off = i + len(key)
		// Left boundary: an object key follows "{", ",", whitespace or the start of file; the quoted form has the opening quote immediately
		// before it and one of those characters before that ('typescript/no-this-alias' is not the key "alias").
		quoted := byte(0)
		k := i - 1
		if k >= 0 && (src[k] == '"' || src[k] == '\'') {
			quoted = src[k]
			k--
		}
		if k >= 0 {
			c := src[k]
			if !(c == '{' || c == ',' || c == ' ' || c == '\t' || c == '\n' || c == '\r') {
				continue
			}
		}
		j := off
		if quoted != 0 {
			if j >= len(src) || src[j] != quoted {
				continue
			}
			j++
		}
		for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
			j++
		}
		if j < len(src) && src[j] == ':' {
			out = append(out, j+1)
		} else if j < len(src) && (src[j] == ',' || src[j] == '}') {
			// shorthand { alias } → the value is a variable: unreadable
			out = append(out, j)
		}
	}
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// literalValue reads a constant string right after position s.
func literalValue(s string) (string, bool) {
	s = strings.TrimLeft(s, " \t\r\n")
	m := strLit.FindStringSubmatchIndex(s)
	if m == nil || m[0] != 0 {
		return "", false
	}
	sm := strLit.FindStringSubmatch(s)
	return unescapeJS(sm[1] + sm[2] + sm[3]), true
}

// unescapeJS removes one escape layer of a JS string: \n \t \r → control characters, any other \X → X.
func unescapeJS(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// balanced returns the content between the bracket pair open/close starting at s (leading whitespace skipped); brackets inside strings are ignored.
func balanced(s string, open, close byte) (string, bool) {
	s = strings.TrimLeft(s, " \t\r\n")
	if len(s) == 0 || s[0] != open {
		return "", false
	}
	depth := 0
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			if c == '\\' {
				i++
			} else if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			q = c
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[1:i], true
			}
		}
	}
	return "", false
}

// stringArray reads [ 'a', "b" ] consisting only of constant strings.
func stringArray(s string) ([]string, bool) {
	body, ok := balanced(s, '[', ']')
	if !ok {
		return nil, false
	}
	var out []string
	for _, part := range splitTop(body) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, ok := literalValue(part)
		if !ok || strings.TrimSpace(part) != quoteLen(part) {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// quoteLen returns the first constant-string part of part (to check that part is a single constant string).
func quoteLen(part string) string {
	part = strings.TrimSpace(part)
	m := strLit.FindStringIndex(part)
	if m == nil || m[0] != 0 {
		return ""
	}
	return part[:m[1]]
}

// splitTop splits on top-level commas.
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			if c == '\\' {
				i++
			} else if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			q = c
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// splitKV splits "key: value" at the top level.
func splitKV(part string) (string, string, bool) {
	part = strings.TrimSpace(part)
	var q byte
	depth := 0
	for i := 0; i < len(part); i++ {
		c := part[i]
		if q != 0 {
			if c == '\\' {
				i++
			} else if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			q = c
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
		case ':':
			if depth == 0 {
				return strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:]), true
			}
		}
	}
	return "", "", false
}

// parseMapper reads moduleNameMapper: { 'regex': 'target' | ['t1','t2'] }.
func parseMapper(s string, roots []string) ([]resolve.Alias, bool) {
	body, ok := balanced(s, '{', '}')
	if !ok {
		return nil, false
	}
	var out []resolve.Alias
	for _, part := range splitTop(body) {
		if strings.TrimSpace(part) == "" {
			continue
		}
		k, v, ok := splitKV(part)
		if !ok {
			return nil, false
		}
		key, ok := literalValue(k)
		if !ok || quoteLen(k) != k {
			return nil, false
		}
		var targets []string
		if strings.HasPrefix(v, "[") {
			targets, ok = stringArray(v)
		} else {
			var t string
			t, ok = literalValue(v)
			ok = ok && quoteLen(v) == v
			targets = []string{t}
		}
		if !ok {
			return nil, false
		}
		re, err := regexp.Compile(key)
		if err != nil {
			return nil, false
		}
		var ts []string
		for _, t := range targets {
			for _, root := range roots {
				u := t
				if strings.Contains(u, "<rootDir>") {
					u = strings.ReplaceAll(u, "<rootDir>", root)
				} else if strings.HasPrefix(u, "./") || strings.HasPrefix(u, "../") {
					u = path.Join(root, u)
				}
				ts = append(ts, path.Clean(u))
			}
		}
		out = append(out, resolve.Alias{Regex: re, Targets: ts})
	}
	return out, true
}

var pathResolveCall = regexp.MustCompile(`^(?:path\.)?(?:resolve|join)\(\s*(?:__dirname|process\.cwd\(\)|import\.meta\.dirname)\s*,\s*(['"])([^'"]*)['"]\s*\)$`)
var urlCall = regexp.MustCompile(`^fileURLToPath\(\s*new URL\(\s*(['"])([^'"]*)['"]\s*,\s*import\.meta\.url\s*\)\s*\)$`)

// parseViteAlias reads resolve.alias as { '@': path.resolve(__dirname, 'src') } or [{ find: '@', replacement: '...' }].
func parseViteAlias(s, dir, file string) ([]resolve.Alias, bool) {
	trim := strings.TrimLeft(s, " \t\r\n")
	target := func(v string) (string, bool) {
		v = strings.TrimSpace(v)
		if lit, ok := literalValue(v); ok && quoteLen(v) == v {
			if strings.HasPrefix(lit, "/") {
				return path.Clean(path.Join(dir, lit)), true // vite: absolute relative to the project root
			}
			return path.Clean(path.Join(dir, lit)), true
		}
		if m := pathResolveCall.FindStringSubmatch(v); m != nil {
			return path.Clean(path.Join(path.Dir(file), m[2])), true
		}
		if m := urlCall.FindStringSubmatch(v); m != nil {
			return path.Clean(path.Join(path.Dir(file), m[2])), true
		}
		return "", false
	}
	var out []resolve.Alias
	if strings.HasPrefix(trim, "[") {
		body, ok := balanced(trim, '[', ']')
		if !ok {
			return nil, false
		}
		for _, item := range splitTop(body) {
			if strings.TrimSpace(item) == "" {
				continue
			}
			obj, ok := balanced(item, '{', '}')
			if !ok {
				return nil, false
			}
			var find, repl string
			var okF, okR bool
			for _, kv := range splitTop(obj) {
				k, v, ok := splitKV(kv)
				if !ok {
					continue
				}
				switch strings.Trim(k, `'"`) {
				case "find":
					find, okF = literalValue(v)
					okF = okF && quoteLen(v) == v
				case "replacement":
					repl, okR = target(v)
				}
			}
			if !okF || !okR {
				return nil, false
			}
			out = append(out, resolve.Alias{Prefix: find, Targets: []string{repl}})
		}
		return out, true
	}
	body, ok := balanced(trim, '{', '}')
	if !ok {
		return nil, false
	}
	for _, part := range splitTop(body) {
		if strings.TrimSpace(part) == "" {
			continue
		}
		k, v, ok := splitKV(part)
		if !ok {
			return nil, false
		}
		key := k
		if lit, ok := literalValue(k); ok && quoteLen(k) == k {
			key = lit
		} else if !isIdent(k) {
			return nil, false
		}
		tg, ok := target(v)
		if !ok {
			return nil, false
		}
		out = append(out, resolve.Alias{Prefix: key, Targets: []string{tg}})
	}
	return out, true
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c == '$' || isAlnum(c)) {
			return false
		}
	}
	return true
}

// stripJSComments removes // and /* */ comments outside strings (enough for config files; regex literals are not handled).
func stripJSComments(s string) string {
	var b strings.Builder
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			b.WriteByte(c)
			if c == '\\' && i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			} else if c == q {
				q = 0
			}
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			q = c
			b.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '/' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return b.String()
			}
			i += j + 3
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// readTestPatterns reads Jest's testMatch (glob) and testRegex (regex) and Vitest's include. A pattern that cannot be converted →
// opaque (test set unknown → widen the project), because ignoring it would make real tests never get selected.
func (tc *testConfig) readTestPatterns(src, file string, roots []string, rootKnown bool) {
	addGlob := func(g string) bool {
		g = strings.TrimPrefix(g, "./")
		if strings.HasPrefix(g, "!") {
			return true // negation only removes tests: ignoring it is a superset
		}
		conv, ok := convertExtglob(g)
		if !ok {
			return false
		}
		var full []string
		switch {
		case strings.Contains(conv, "<rootDir>"):
			rest := strings.TrimLeft(strings.ReplaceAll(conv, "<rootDir>", ""), "/")
			if !rootKnown {
				full = append(full, "**/"+rest)
			} else {
				for _, r := range roots {
					full = append(full, path.Join(r, rest))
				}
			}
		case strings.HasPrefix(conv, "**/") || strings.HasPrefix(conv, "/"):
			full = append(full, "**/"+strings.TrimLeft(strings.TrimPrefix(conv, "**/"), "/"))
		default:
			for _, r := range roots {
				full = append(full, path.Join(r, conv))
			}
		}
		for _, f := range full {
			gp, err := glob.Compile(f)
			if err != nil {
				return false
			}
			tc.testGlobs = append(tc.testGlobs, gp)
		}
		return true
	}
	// testPathIgnorePatterns: only constant string elements; <rootDir> is replaced by the root when known, otherwise that pattern is dropped.
	for _, i := range keyIndexes(src, "testPathIgnorePatterns") {
		for _, pat := range literalElements(src[i:]) {
			if strings.Contains(pat, "<rootDir>") {
				if !rootKnown || len(roots) != 1 {
					continue
				}
				pat = strings.ReplaceAll(pat, "<rootDir>", regexp.QuoteMeta(fakeAbs(roots[0])))
			}
			if re, err := regexp.Compile(pat); err == nil {
				tc.testIgnore = append(tc.testIgnore, re)
			}
		}
	}
	opaqueTest := func(msg string) {
		tc.opaque = append(tc.opaque, msg)
		tc.testsOpaque = true
	}
	for _, key := range []string{"testMatch", "include"} {
		for _, i := range keyIndexes(src, key) {
			var vals []string
			if arr, ok := stringArray(src[i:]); ok {
				vals = arr
			} else if v, ok := literalValue(src[i:]); ok && quoteLen(src[i:]) != "" {
				vals = []string{v}
			} else if alts, ok := templateAlternatives(src[i:]); ok {
				vals = alts
			} else {
				opaqueTest(file + ": " + key)
				continue
			}
			for _, g := range vals {
				if !addGlob(g) {
					opaqueTest(file + ": " + key + " " + g)
				}
			}
		}
	}
	for _, i := range keyIndexes(src, "testRegex") {
		var pats []string
		if arr, ok := stringArray(src[i:]); ok {
			pats = arr
		} else if v, ok := literalValue(src[i:]); ok && quoteLen(src[i:]) != "" {
			pats = []string{v}
		} else if re, ok := regexLiteral(src[i:]); ok {
			pats = []string{re}
		} else if alts, ok := templateAlternatives(src[i:]); ok {
			pats = alts // union of every branch combination: a superset of the real regex
		} else {
			opaqueTest(file + ": testRegex")
			continue
		}
		for _, p := range pats {
			re, err := regexp.Compile(p)
			if err != nil {
				opaqueTest(file + ": testRegex " + p)
				continue
			}
			tc.testRegex = append(tc.testRegex, re)
		}
	}
}

// templateAlternatives expands a template literal `a${x}b` right after position s when each expression is a constant string or
// a ternary with two constant-string branches (`${cond ? "" : "|eslint"}`): returns every combination (at most 64). The real value is one
// of the combinations so their union is a superset. Any other expression → false.
func templateAlternatives(s string) ([]string, bool) {
	s = strings.TrimLeft(s, " \t\r\n")
	if !strings.HasPrefix(s, "`") {
		return nil, false
	}
	out := []string{""}
	var lit strings.Builder
	flush := func() {
		t := unescapeJS(lit.String())
		for i := range out {
			out[i] += t
		}
		lit.Reset()
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			lit.WriteByte(c)
			lit.WriteByte(s[i+1])
			i++
		case c == '`':
			flush()
			return out, true
		case c == '$' && i+1 < len(s) && s[i+1] == '{':
			body, ok := balanced(s[i+1:], '{', '}')
			if !ok {
				return nil, false
			}
			alts, ok := exprAlternatives(body)
			if !ok || len(out)*len(alts) > 64 {
				return nil, false
			}
			flush()
			var next []string
			for _, o := range out {
				for _, a := range alts {
					next = append(next, o+a)
				}
			}
			out = next
			i += 1 + len(body) + 1
		default:
			lit.WriteByte(c)
		}
	}
	return nil, false
}

// exprAlternatives: constant string → [v]; `cond ? A : B` with A, B constant strings (or nested ternaries like that) → the branches.
func exprAlternatives(e string) ([]string, bool) {
	e = strings.TrimSpace(e)
	if q := quoteLen(e); q != "" && q == e {
		v, _ := literalValue(e)
		return []string{v}, true
	}
	// split "?" and ":" at the top level (ignoring those in strings/brackets)
	q, colon := -1, -1
	depth := 0
	var quote byte
	for i := 0; i < len(e); i++ {
		c := e[i]
		if quote != 0 {
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '?':
			if depth == 0 && q < 0 && !(i+1 < len(e) && (e[i+1] == '?' || e[i+1] == '.')) {
				q = i
			}
		case ':':
			if depth == 0 && q >= 0 && colon < 0 {
				colon = i
			}
		}
	}
	if q < 0 || colon < 0 {
		return nil, false
	}
	a, ok1 := exprAlternatives(e[q+1 : colon])
	b, ok2 := exprAlternatives(e[colon+1:])
	if !ok1 || !ok2 {
		return nil, false
	}
	return append(a, b...), true
}

// regexLiteral reads /.../flags right after position s (content returned verbatim, already regex syntax).
func regexLiteral(s string) (string, bool) {
	s = strings.TrimLeft(s, " \t\r\n")
	if !strings.HasPrefix(s, "/") {
		return "", false
	}
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '\n':
			return "", false
		case '/':
			return s[1:i], true
		}
	}
	return "", false
}

// convertExtglob converts micromatch extglob to the glob package's syntax: ?(a|b) and *(a|b) → {,a,b}; @(a|b) and +(a|b) →
// {a,b}. "+(…)" only once is a narrower approximation of "one or more"; real test file names repeat at most once and still match the
// default pattern. !(…) is unsupported → false (the caller widens).
func convertExtglob(g string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		c := g[i]
		if (c == '?' || c == '@' || c == '+' || c == '*' || c == '!') && i+1 < len(g) && g[i+1] == '(' {
			j := strings.IndexByte(g[i+2:], ')')
			if j < 0 || c == '!' {
				return "", false
			}
			alts := strings.ReplaceAll(g[i+2:i+2+j], "|", ",")
			if c == '?' || c == '*' {
				b.WriteString("{," + alts + "}")
			} else {
				b.WriteString("{" + alts + "}")
			}
			i += 2 + j
			continue
		}
		b.WriteByte(c)
	}
	return b.String(), true
}

// literalArrayOfStringsOrObjects: [ 'a', "b", { ... }, ] consisting only of constant strings and object literals.
func literalArrayOfStringsOrObjects(s string) bool {
	body, ok := balanced(s, '[', ']')
	if !ok {
		return false
	}
	for _, part := range splitTop(body) {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
		case strings.HasPrefix(part, "{"):
			if obj, ok := balanced(part, '{', '}'); !ok || part != "{"+obj+"}" {
				return false
			}
		default:
			if quoteLen(part) != part {
				return false
			}
		}
	}
	return true
}

// blankValues returns a copy of src in which the values (string, array, object) of keys are replaced with whitespace.
func blankValues(src string, keys ...string) string {
	b := []byte(src)
	for _, key := range keys {
		for _, i := range keyIndexes(src, key) {
			if end := valueEnd(src, i); end > i {
				for k := i; k < end; k++ {
					if b[k] != '\n' {
						b[k] = ' '
					}
				}
			}
		}
	}
	return string(b)
}

// valueEnd returns the position right after the value starting at i (array/object with balanced brackets, constant string, or up to the comma/closing bracket
// at the top level).
func valueEnd(src string, i int) int {
	j := i
	for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
		j++
	}
	if j >= len(src) {
		return j
	}
	switch src[j] {
	case '[':
		if body, ok := balanced(src[j:], '[', ']'); ok {
			return j + len(body) + 2
		}
	case '{':
		if body, ok := balanced(src[j:], '{', '}'); ok {
			return j + len(body) + 2
		}
	}
	if q := quoteLen(src[j:]); q != "" {
		return j + len(q)
	}
	depth := 0
	for k := j; k < len(src); k++ {
		switch src[k] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				return k
			}
			depth--
		case ',':
			if depth == 0 {
				return k
			}
		}
	}
	return len(src)
}

// literalElements returns the constant-string elements of the array right after position s; other elements (spread, variable, call) are dropped.
func literalElements(s string) []string {
	body, ok := balanced(s, '[', ']')
	if !ok {
		return nil
	}
	var out []string
	for _, part := range splitTop(body) {
		part = strings.TrimSpace(part)
		if part != "" && quoteLen(part) == part {
			if v, ok := literalValue(part); ok {
				out = append(out, v)
			}
		}
	}
	return out
}

// projectConfigFiles: sub-config files that a file config's projects/workspace declare as strings (a path or glob to
// a JS/TS/JSON file). Directories ("packages/*") are not needed: runner config in them is found in every directory.
func projectConfigFiles(t tree.Tree, file, src string) []string {
	src = stripJSComments(src)
	var out []string
	for _, key := range []string{"projects", "workspace"} {
		for _, i := range keyIndexes(src, key) {
			for _, v := range literalElements(src[i:]) {
				if strings.HasPrefix(v, "!") {
					continue
				}
				rel := path.Join(path.Dir(file), v)
				if rel == ".." || strings.HasPrefix(rel, "../") {
					continue
				}
				if !strings.ContainsAny(v, "*?[{") {
					if t.Has(rel) && hasCfgExt(rel) {
						out = append(out, rel)
					}
					continue
				}
				g, err := glob.Compile(rel)
				if err != nil {
					continue
				}
				for _, f := range t.Files() {
					if hasCfgExt(f) && !inNodeModules(f) && g.Match(f) {
						out = append(out, f)
					}
				}
			}
		}
	}
	return out
}

func hasCfgExt(p string) bool {
	switch path.Ext(p) {
	case ".js", ".ts", ".mjs", ".cjs", ".mts", ".cts", ".json":
		return true
	}
	return false
}

package node

import (
	"path"
	"regexp"
	"strings"

	"bgraph/internal/eco/node/resolve"
	"bgraph/internal/glob"
	"bgraph/internal/tree"
)

// testConfig là phần đọc được từ cấu hình runner của một dự án (jest/vitest/vite/playwright...).
type testConfig struct {
	files   []string        // tệp cấu hình đã thấy
	aliases []resolve.Alias // moduleNameMapper / resolve.alias đọc được
	// implicit: tệp trong repo mà cấu hình trỏ tới (setupFiles, globalSetup, preset, transform...): mọi test của dự án
	// phụ thuộc vào chúng.
	implicit []string
	// implicitGlobs: như implicit nhưng là glob (đã thoát ký tự đặc biệt của phần đường dẫn chữ nghĩa).
	implicitGlobs []string
	// opaque: cấu hình có khoá ảnh hưởng phân giải/chọn test mà không đọc được dưới dạng literal → config_is_code.
	opaque []string
	// testGlobs/testRegex: mẫu tệp test do cấu hình khai (Jest testMatch/testRegex, Vitest include). Tệp là test nếu khớp
	// mẫu cấu hình HOẶC mẫu mặc định (hợp: không bao giờ thu hẹp tập test).
	testGlobs []*glob.Pattern
	testRegex []*regexp.Regexp
	// testsOpaque: mẫu tệp test không đọc được → tập test của dự án không biết (đơn vị Whole).
	testsOpaque bool
}

// isTest: p khớp mẫu tệp test của cấu hình.
func (tc *testConfig) isTest(p string) bool {
	for _, g := range tc.testGlobs {
		if g.Match(p) {
			return true
		}
	}
	for _, re := range tc.testRegex {
		// Jest so testRegex với đường dẫn TUYỆT ĐỐI: tiền tố giả để mẫu như "./packages/" có ký tự đứng trước.
		if re.MatchString("/__repo__/" + p) {
			return true
		}
	}
	return false
}

// configBases là tên (không đuôi) của tệp cấu hình runner, tìm ở MỌI thư mục của dự án (cấu hình lồng như
// tests/unit/jest.config.mjs chạy bằng --config).
var configBases = []string{"jest.config", "vitest.config", "vite.config", "playwright.config", "vitest.workspace", "jest.preset", "jest-preset"}

// isRunnerConfig: p là tệp cấu hình runner (jest.config.ts, vitest.config.e2e.mts, ...).
func isRunnerConfig(p string) bool {
	base := path.Base(p)
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

// resolutionKeys là khoá cấu hình làm đổi cách phân giải import hoặc tập test; gặp mà không đọc được thì nới.
// Không có ở đây (bỏ qua vẫn là tập cha vì resolver lấy hợp mọi ứng viên): modulePathIgnorePatterns (chỉ ẩn bớt đường
// dẫn khỏi bộ nạp), dedupe (chỉ gộp bản sao gói ngoài).
var resolutionKeys = []string{"moduleNameMapper", "alias", "modulePaths", "moduleDirectories", "resolver", "roots", "rootDir",
	"haste", "projects", "workspace", "conditions", "mainFields", "extensions",
	"moduleFileExtensions", "customExportConditions", "testEnvironmentOptions"}

var strLit = regexp.MustCompile(`'((?:[^'\\\n]|\\.)*)'|"((?:[^"\\\n]|\\.)*)"|` + "`([^`$\\\\]*)`")

// readTestConfig đọc các tệp cấu hình runner cfgFiles của dự án dir (và khoá "jest" trong package.json).
func readTestConfig(t tree.Tree, dir string, pkgJest []byte, cfgFiles []string) testConfig {
	var tc testConfig
	for _, p := range cfgFiles {
		b, err := t.Read(p)
		if err != nil {
			tc.opaque = append(tc.opaque, p+": "+err.Error())
			continue
		}
		tc.files = append(tc.files, p)
		tc.readOne(t, dir, p, string(b))
	}
	if len(pkgJest) > 0 {
		tc.readOne(t, dir, path.Join(dir, "package.json"), string(pkgJest))
	}
	return tc
}

// readOne trích từ văn bản cấu hình:
//   - mọi chuỗi hằng trỏ tới tệp/thư mục có thật trong repo (tương đối dir, hoặc <rootDir>/) → phụ thuộc ngầm của mọi test;
//   - moduleNameMapper / alias dạng object literal chỉ có chuỗi hằng → Alias;
//   - khoá phân giải khác không đọc được → opaque.
func (tc *testConfig) readOne(t tree.Tree, dir, file, src string) {
	src = stripJSComments(src)
	// <rootDir>: mặc định Jest là thư mục của tệp cấu hình (package.json → thư mục dự án). rootDir literal → theo đó;
	// tính bằng code → không biết: mẫu test dùng "**/" (tập cha), đích alias thử cả thư mục cấu hình lẫn thư mục dự án.
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
	tc.readTestPatterns(src, file, roots, rootKnown)
	// JSX runtime (oxc/esbuild/swc/tsconfig importSource): mọi test phụ thuộc <importSource>/jsx-runtime và
	// jsx-dev-runtime, không phải cả thư mục.
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
	// Giá trị của các khoá chỉ LOẠI TRỪ (exclude, coverage) hoặc đã xử lý riêng (importSource) không bao giờ tạo phụ
	// thuộc: xoá trắng trước khi gom đường dẫn.
	scan := blankValues(src, "exclude", "coverage", "importSource", "jsxImportSource", "testPathIgnorePatterns", "include", "testMatch", "testRegex",
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
				// thư mục (setupFiles dir, roots...): mọi tệp dưới đó
				tc.implicitGlobs = append(tc.implicitGlobs, escapeGlob(cand)+"/**")
			}
		}
	}
	for _, key := range resolutionKeys {
		idxs := keyIndexes(src, key)
		for _, i := range idxs {
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
				// literal: đã áp vào roots; tính bằng code: mẫu test dùng "**/", alias thử mọi gốc → vẫn là tập cha.
				continue
			case "roots":
				if _, ok := stringArray(src[i:]); ok {
					continue // thư mục đã thành implicit ở trên; roots chỉ thu hẹp tập test (chọn thừa không hại)
				}
			case "moduleFileExtensions", "extensions", "mainFields", "conditions", "customExportConditions", "testEnvironmentOptions":
				// resolver đã thử mọi đuôi và lấy hợp mọi điều kiện: an toàn khi bỏ qua.
				continue
			case "projects", "workspace":
				// Phần tử là chuỗi (đường dẫn/glob tới cấu hình con, được tìm thấy vì cấu hình runner được quét ở mọi
				// thư mục) hoặc object literal (khoá bên trong đã được quét trên toàn văn bản tệp): đọc được. Biến/lời gọi
				// hàm → không biết dự án con nào → mờ.
				if literalArrayOfStringsOrObjects(src[i:]) {
					continue
				}
			}
			tc.opaque = append(tc.opaque, file+": "+key)
		}
	}
}

// configPathCandidates: chuỗi trong cấu hình có thể là đường dẫn tới tệp của repo.
func configPathCandidates(dir, file, s string) []string {
	// "[id]", "{x}", "(group)" là tên tệp/thư mục hợp lệ (Next.js...): chỉ chặn wildcard thật; ứng viên vẫn chỉ được nhận
	// khi khớp chính xác một tệp/thư mục có thật.
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

// keyIndexes trả vị trí ngay sau "key:" (hoặc "\"key\":") trong văn bản cấu hình.
func keyIndexes(src, key string) []int {
	var out []int
	for off := 0; ; {
		i := strings.Index(src[off:], key)
		if i < 0 {
			return out
		}
		i += off
		off = i + len(key)
		// Biên trái: khoá của object đứng sau "{", ",", khoảng trắng hoặc đầu tệp; dạng có nháy thì dấu nháy mở đứng ngay
		// trước và trước nó là một trong các ký tự đó ('typescript/no-this-alias' không phải khoá "alias").
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
			// shorthand { alias } → giá trị là biến: không đọc được
			out = append(out, j)
		}
	}
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// literalValue đọc một chuỗi hằng ngay sau vị trí s.
func literalValue(s string) (string, bool) {
	s = strings.TrimLeft(s, " \t\r\n")
	m := strLit.FindStringSubmatchIndex(s)
	if m == nil || m[0] != 0 {
		return "", false
	}
	sm := strLit.FindStringSubmatch(s)
	return unescapeJS(sm[1] + sm[2] + sm[3]), true
}

// unescapeJS bỏ một lớp thoát của chuỗi JS: \n \t \r → ký tự điều khiển, \X khác → X.
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

// balanced trả nội dung giữa cặp ngoặc open/close bắt đầu ở s (bỏ khoảng trắng đầu); bỏ qua ngoặc trong chuỗi.
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

// stringArray đọc [ 'a', "b" ] chỉ gồm chuỗi hằng.
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

// quoteLen trả phần chuỗi hằng đầu tiên của part (để kiểm part chỉ là một chuỗi hằng).
func quoteLen(part string) string {
	part = strings.TrimSpace(part)
	m := strLit.FindStringIndex(part)
	if m == nil || m[0] != 0 {
		return ""
	}
	return part[:m[1]]
}

// splitTop tách theo dấu phẩy ở mức ngoài cùng.
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

// splitKV tách "key: value" ở mức ngoài cùng.
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

// parseMapper đọc moduleNameMapper: { 'regex': 'target' | ['t1','t2'] }.
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

// parseViteAlias đọc resolve.alias dạng { '@': path.resolve(__dirname, 'src') } hoặc [{ find: '@', replacement: '...' }].
func parseViteAlias(s, dir, file string) ([]resolve.Alias, bool) {
	trim := strings.TrimLeft(s, " \t\r\n")
	target := func(v string) (string, bool) {
		v = strings.TrimSpace(v)
		if lit, ok := literalValue(v); ok && quoteLen(v) == v {
			if strings.HasPrefix(lit, "/") {
				return path.Clean(path.Join(dir, lit)), true // vite: tuyệt đối theo root dự án
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

// stripJSComments bỏ comment // và /* */ ngoài chuỗi (đủ cho tệp cấu hình; không xử lý regex literal).
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

// readTestPatterns đọc testMatch (glob), testRegex (regex) của Jest và include của Vitest. Mẫu không chuyển được →
// opaque (không biết tập test → nới dự án), vì bỏ qua sẽ làm test thật không bao giờ được chọn.
func (tc *testConfig) readTestPatterns(src, file string, roots []string, rootKnown bool) {
	addGlob := func(g string) bool {
		g = strings.TrimPrefix(g, "./")
		if strings.HasPrefix(g, "!") {
			return true // phủ định chỉ bớt test: bỏ qua là tập cha
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
			pats = alts // hợp mọi tổ hợp nhánh: tập cha của regex thật
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

// templateAlternatives khai triển template literal `a${x}b` ngay sau vị trí s khi mỗi biểu thức là chuỗi hằng hoặc
// toán tử ba ngôi có hai nhánh chuỗi hằng (`${cond ? "" : "|eslint"}`): trả mọi tổ hợp (tối đa 64). Giá trị thật là một
// trong các tổ hợp nên hợp của chúng là tập cha. Biểu thức khác → false.
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

// exprAlternatives: chuỗi hằng → [v]; `cond ? A : B` với A, B là chuỗi hằng (hoặc ba ngôi lồng như vậy) → các nhánh.
func exprAlternatives(e string) ([]string, bool) {
	e = strings.TrimSpace(e)
	if q := quoteLen(e); q != "" && q == e {
		v, _ := literalValue(e)
		return []string{v}, true
	}
	// tách "?" và ":" ở mức ngoài cùng (bỏ qua trong chuỗi/ngoặc)
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

// regexLiteral đọc /.../flags ngay sau vị trí s (nội dung trả nguyên văn, đã là cú pháp regex).
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

// convertExtglob đổi extglob của micromatch sang cú pháp của gói glob: ?(a|b) và *(a|b) → {,a,b}; @(a|b) và +(a|b) →
// {a,b}. "+(…)" chỉ một lần là xấp xỉ hẹp hơn "một hoặc nhiều"; tên tệp test thực tế lặp không quá một lần và vẫn khớp
// mẫu mặc định. !(…) không hỗ trợ → false (người gọi nới).
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

// literalArrayOfStringsOrObjects: [ 'a', "b", { ... }, ] chỉ gồm chuỗi hằng và object literal.
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

// blankValues trả bản sao của src trong đó giá trị (chuỗi, mảng, object) của các khoá keys bị thay bằng khoảng trắng.
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

// valueEnd trả vị trí ngay sau giá trị bắt đầu ở i (mảng/object cân ngoặc, chuỗi hằng, hoặc tới dấu phẩy/ngoặc đóng
// ở mức ngoài cùng).
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

package node

import (
	"path"
	"regexp"
	"strings"

	"bgraph/internal/eco/node/resolve"
	"bgraph/internal/tree"
)

// testConfig là phần đọc được từ cấu hình runner của một dự án (jest/vitest/vite/playwright...).
type testConfig struct {
	files   []string        // tệp cấu hình đã thấy
	aliases []resolve.Alias // moduleNameMapper / resolve.alias đọc được
	// implicit: tệp trong repo mà cấu hình trỏ tới (setupFiles, globalSetup, preset, transform...): mọi test của dự án
	// phụ thuộc vào chúng.
	implicit []string
	// opaque: cấu hình có khoá ảnh hưởng phân giải/chọn test mà không đọc được dưới dạng literal → config_is_code.
	opaque []string
}

// resolutionKeys là khoá cấu hình làm đổi cách phân giải import hoặc tập test; gặp mà không đọc được thì nới.
// Không có ở đây (bỏ qua vẫn là tập cha vì resolver lấy hợp mọi ứng viên): modulePathIgnorePatterns (chỉ ẩn bớt đường
// dẫn khỏi bộ nạp), dedupe (chỉ gộp bản sao gói ngoài).
var resolutionKeys = []string{"moduleNameMapper", "alias", "modulePaths", "moduleDirectories", "resolver", "roots", "rootDir",
	"haste", "projects", "workspace", "conditions", "mainFields", "extensions",
	"moduleFileExtensions", "customExportConditions", "testEnvironmentOptions"}

var strLit = regexp.MustCompile(`'((?:[^'\\\n]|\\.)*)'|"((?:[^"\\\n]|\\.)*)"|` + "`([^`$\\\\]*)`")

// readTestConfig đọc cấu hình runner trong thư mục dự án dir.
func readTestConfig(t tree.Tree, dir string, pkgJest []byte) testConfig {
	var tc testConfig
	var cands []string
	for _, base := range []string{"jest.config", "vitest.config", "vite.config", "playwright.config", "vitest.workspace", "jest.preset", "jest-preset"} {
		for _, ext := range []string{".js", ".ts", ".mjs", ".cjs", ".mts", ".cts", ".json"} {
			p := path.Join(dir, base+ext)
			if t.Has(p) {
				cands = append(cands, p)
			}
		}
	}
	for _, p := range cands {
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
	for _, m := range strLit.FindAllStringSubmatch(src, -1) {
		s := m[1] + m[2] + m[3]
		if s == "" {
			continue
		}
		for _, cand := range configPathCandidates(dir, file, s) {
			if t.Has(cand) {
				tc.implicit = append(tc.implicit, cand)
			} else if t.IsDir(cand) && cand != "." && cand != dir {
				// thư mục (setupFiles dir, roots...): mọi tệp dưới đó
				tc.implicit = append(tc.implicit, cand+"/**")
			}
		}
	}
	for _, key := range resolutionKeys {
		idxs := keyIndexes(src, key)
		for _, i := range idxs {
			switch key {
			case "moduleNameMapper":
				if al, ok := parseMapper(src[i:], dir); ok {
					tc.aliases = append(tc.aliases, al...)
					continue
				}
			case "alias":
				if al, ok := parseViteAlias(src[i:], dir, file); ok {
					tc.aliases = append(tc.aliases, al...)
					continue
				}
			case "rootDir":
				if v, ok := literalValue(src[i:]); ok && (v == "." || v == "./" || v == "<rootDir>") {
					continue
				}
			case "roots":
				if _, ok := stringArray(src[i:]); ok {
					continue // thư mục đã thành implicit ở trên; roots chỉ thu hẹp tập test (chọn thừa không hại)
				}
			case "moduleFileExtensions", "extensions", "mainFields", "conditions", "customExportConditions", "testEnvironmentOptions":
				// resolver đã thử mọi đuôi và lấy hợp mọi điều kiện: an toàn khi bỏ qua.
				continue
			case "projects", "workspace":
				if arr, ok := stringArray(src[i:]); ok {
					_ = arr // dự án con có cấu hình riêng, được đọc khi phân tích thư mục của chúng
					continue
				}
			}
			tc.opaque = append(tc.opaque, file+": "+key)
		}
	}
}

// configPathCandidates: chuỗi trong cấu hình có thể là đường dẫn tới tệp của repo.
func configPathCandidates(dir, file, s string) []string {
	if strings.ContainsAny(s, "\n*?{}()[]|^$") || strings.Contains(s, "://") || len(s) > 300 {
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
		// biên trái: không phải một phần của định danh dài hơn
		if i > 0 {
			c := src[i-1]
			if c == '_' || c == '$' || c == '.' || isAlnum(c) {
				continue
			}
		}
		j := off
		if j < len(src) && (src[j] == '"' || src[j] == '\'') {
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
	return sm[1] + sm[2] + sm[3], true
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
func parseMapper(s, dir string) ([]resolve.Alias, bool) {
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
			t = strings.ReplaceAll(t, "<rootDir>", dir)
			if !strings.HasPrefix(t, dir) && (strings.HasPrefix(t, "./") || strings.HasPrefix(t, "../")) {
				t = path.Join(dir, t)
			}
			ts = append(ts, path.Clean(t))
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

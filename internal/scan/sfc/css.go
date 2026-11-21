package sfc

import (
	"strconv"
	"strings"
)

type cssImp struct {
	spec string
	line int // dòng 1-based trong nội dung khối
}

// scanCSS tìm @import (mọi dạng), @use/@forward (scss/sass), @plugin (less). ext: "css", "scss", "sass", "less".
// Trả lý do uncertain (rỗng nếu chắc chắn). Bỏ qua comment, chuỗi, và URL ngoài.
func scanCSS(src []byte, ext string) ([]cssImp, string) {
	var out []cssImp
	n := len(src)
	line := 1
	lineComments := ext == "scss" || ext == "sass" || ext == "less"
	isWS := func(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' }
	// skipWS bỏ khoảng trắng và comment /* */ (và // nếu hỗ trợ), cập nhật line.
	skipWS := func(j int) int {
		for j < n {
			c := src[j]
			if c == '\n' {
				line++
				j++
			} else if isWS(c) {
				j++
			} else if c == '/' && j+1 < n && src[j+1] == '*' {
				j += 2
				for j < n && !(src[j] == '*' && j+1 < n && src[j+1] == '/') {
					if src[j] == '\n' {
						line++
					}
					j++
				}
				j += 2
			} else if lineComments && c == '/' && j+1 < n && src[j+1] == '/' {
				for j < n && src[j] != '\n' {
					j++
				}
			} else {
				break
			}
		}
		if j > n {
			j = n
		}
		return j
	}
	readStr := func(j int) (string, int, bool) { // src[j] là dấu nháy
		q := src[j]
		k := j + 1
		var sb strings.Builder
		for k < n {
			c := src[k]
			if c == '\\' && k+1 < n {
				sb.WriteByte(src[k+1])
				k += 2
				continue
			}
			if c == q {
				return sb.String(), k + 1, true
			}
			if c == '\n' {
				return "", k, false
			}
			sb.WriteByte(c)
			k++
		}
		return "", k, false
	}
	dyn := func(s string) bool {
		return strings.Contains(s, "#{") || strings.Contains(s, "@{") || strings.Contains(s, "$")
	}
	emit := func(s string, ln int) string {
		if dyn(s) {
			return "css_dynamic_import: " + s
		}
		if s == "" {
			return ""
		}
		if remote(s) || strings.HasPrefix(s, "sass:") {
			return ""
		}
		out = append(out, cssImp{s, ln})
		return ""
	}

	i := 0
	for i < n {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == '/' && i+1 < n && src[i+1] == '*':
			i = skipWS(i)
		case lineComments && c == '/' && i+1 < n && src[i+1] == '/':
			i = skipWS(i)
		case c == '"' || c == '\'':
			_, k, _ := readStr(i)
			if k <= i {
				k = i + 1
			}
			i = k
		case (c == 'u' || c == 'U') && i+3 < n && strings.EqualFold(string(src[i:i+4]), "url(") && (i == 0 || !isIdent(src[i-1])):
			// url( ... ) ngoài @import: bỏ qua cả nội dung (có thể chứa // hoặc /*)
			j := skipWS(i + 4)
			if j < n && (src[j] == '"' || src[j] == '\'') {
				_, k, _ := readStr(j)
				i = k
			} else {
				for j < n && src[j] != ')' && src[j] != '\n' {
					j++
				}
				i = j
			}
		case c == '@':
			j := i + 1
			for j < n && isIdent(src[j]) {
				j++
			}
			kw := strings.ToLower(string(src[i+1 : j]))
			ok := kw == "import" || ((ext == "scss" || ext == "sass") && (kw == "use" || kw == "forward")) || (ext == "less" && kw == "plugin")
			if !ok {
				i = j
				break
			}
			atLine := line
			for {
				j = skipWS(j)
				if j < n && src[j] == '(' && kw != "use" && kw != "forward" { // tuỳ chọn less: (reference, css)
					for j < n && src[j] != ')' {
						if src[j] == '\n' {
							line++
						}
						j++
					}
					j = skipWS(j + 1)
				}
				if j >= n {
					return out, "css_import_eof: @" + kw + " at end of block (line " + itoa(atLine) + ")"
				}
				if src[j] == '"' || src[j] == '\'' {
					s, k, ok := readStr(j)
					if !ok {
						return out, "css_unterminated_string: after @" + kw + " (line " + itoa(atLine) + ")"
					}
					if r := emit(s, atLine); r != "" {
						return out, r
					}
					j = k
				} else if j+3 < n && strings.EqualFold(string(src[j:j+4]), "url(") {
					j = skipWS(j + 4)
					var s string
					if j < n && (src[j] == '"' || src[j] == '\'') {
						var k int
						var ok bool
						s, k, ok = readStr(j)
						if !ok {
							return out, "css_unterminated_string: in url() (line " + itoa(atLine) + ")"
						}
						j = skipWS(k)
					} else {
						k := j
						for k < n && src[k] != ')' && src[k] != '\n' {
							k++
						}
						s = strings.TrimSpace(string(src[j:k]))
						j = k
					}
					if j >= n || src[j] != ')' {
						return out, "css_import_form: unterminated url() (line " + itoa(atLine) + ")"
					}
					j++
					if r := emit(s, atLine); r != "" {
						return out, r
					}
				} else {
					return out, "css_import_form: unsupported @" + kw + " argument (line " + itoa(atLine) + ")"
				}
				if kw == "import" || kw == "plugin" {
					k := skipWS(j)
					if k < n && src[k] == ',' {
						j = k + 1
						continue
					}
				}
				break
			}
			i = j
		default:
			i++
		}
	}
	return out, ""
}

func itoa(n int) string { return strconv.Itoa(n) }

func isIdent(c byte) bool {
	return c == '-' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c >= 0x80
}

package rust

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// tokKind is the class of a token. Whitespace and comments produce no token.
type tokKind uint8

const (
	tIdent tokKind = iota + 1 // identifier or keyword; a raw identifier (r#x) keeps its "r#" prefix so it never equals a keyword
	tPunct                    // one punctuation byte
	tStr                      // any string literal: "..", r#".."#, b"..", br"..", c".."
	tChar                     // character or byte literal
	tNum
	tLife // lifetime or label
)

type token struct {
	kind       tokKind
	line       int32
	start, end int32 // byte range in the source
}

// lexer output limits.
const maxTokens = 4 << 20

// lex splits Rust source into tokens. reason != "" means the source is malformed enough that the token list cannot be trusted
// (unterminated comment, string or character literal).
func lex(src []byte) (toks []token, reason string) {
	n := len(src)
	i := 0
	line := int32(1)
	if n >= 3 && src[0] == 0xEF && src[1] == 0xBB && src[2] == 0xBF {
		i = 3
	}
	// Shebang (cargo scripts): `#!` that does not start an inner attribute.
	if i+1 < n && src[i] == '#' && src[i+1] == '!' {
		j := i + 2
		for j < n && (src[j] == ' ' || src[j] == '\t') {
			j++
		}
		if j >= n || src[j] != '[' {
			for i < n && src[i] != '\n' {
				i++
			}
		}
	}
	toks = make([]token, 0, n/6+16)
	add := func(k tokKind, start, end int, ln int32) {
		toks = append(toks, token{k, ln, int32(start), int32(end)})
	}
	for i < n {
		if len(toks) > maxTokens {
			return nil, "too many tokens"
		}
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r' || c == 0x0b || c == 0x0c:
			i++
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			depth := 1
			i += 2
			for i < n && depth > 0 {
				switch {
				case src[i] == '/' && i+1 < n && src[i+1] == '*':
					depth++
					i += 2
				case src[i] == '*' && i+1 < n && src[i+1] == '/':
					depth--
					i += 2
				default:
					if src[i] == '\n' {
						line++
					}
					i++
				}
			}
			if depth > 0 {
				return nil, "unterminated block comment"
			}
		case c == '"':
			ln := line
			end, nl, ok := scanCooked(src, i)
			if !ok {
				return nil, "unterminated string literal"
			}
			line += nl
			add(tStr, i, end, ln)
			i = end
		case c == '\'':
			end, kind, ok := scanQuote(src, i)
			if !ok {
				return nil, "malformed character literal"
			}
			add(kind, i, end, line)
			i = end
		case isIdentStart(c):
			// String-literal prefixes: r"..", r#".."#, b"..", br"..", b'x', c"..", cr"..".
			if c == 'r' || c == 'b' || c == 'c' {
				k := i
				if c != 'r' {
					k++
				}
				if k < n && src[k] == 'r' && k+1 < n && (src[k+1] == '"' || src[k+1] == '#') {
					h := 0
					m := k + 1
					for m < n && src[m] == '#' {
						h++
						m++
					}
					if m < n && src[m] == '"' {
						ln := line
						end, nl, ok := scanRaw(src, m, h)
						if !ok {
							return nil, "unterminated raw string literal"
						}
						line += nl
						add(tStr, i, end, ln)
						i = end
						continue
					}
				}
				if k > i && k < n && src[k] == '"' {
					ln := line
					end, nl, ok := scanCooked(src, k)
					if !ok {
						return nil, "unterminated string literal"
					}
					line += nl
					add(tStr, i, end, ln)
					i = end
					continue
				}
				if c == 'b' && k < n && src[k] == '\'' {
					end, kind, ok := scanQuote(src, k)
					if !ok || kind != tChar {
						return nil, "malformed byte literal"
					}
					add(tChar, i, end, line)
					i = end
					continue
				}
				if c == 'r' && i+2 < n && src[i+1] == '#' && isIdentStart(src[i+2]) {
					j := i + 2
					for j < n && isIdentCont(src[j]) {
						j++
					}
					add(tIdent, i, j, line)
					i = j
					continue
				}
			}
			j := i + 1
			for j < n && isIdentCont(src[j]) {
				j++
			}
			add(tIdent, i, j, line)
			i = j
		case c >= '0' && c <= '9':
			j := i + 1
			for j < n {
				switch {
				case isIdentCont(src[j]) && src[j] < 0x80:
					j++
					continue
				case src[j] == '.' && j+1 < n && src[j+1] >= '0' && src[j+1] <= '9':
					j++
					continue
				}
				break
			}
			add(tNum, i, j, line)
			i = j
		case c >= 0x80:
			// Non-ASCII outside identifiers (a stray symbol): keep the whole rune as one punct token.
			_, sz := utf8.DecodeRune(src[i:])
			if sz < 1 {
				sz = 1
			}
			add(tPunct, i, i+sz, line)
			i += sz
		default:
			add(tPunct, i, i+1, line)
			i++
		}
	}
	return toks, ""
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// scanCooked scans a string whose opening quote is at i. Returns the end offset (after the closing quote) and the newlines inside.
func scanCooked(src []byte, i int) (end int, newlines int32, ok bool) {
	n := len(src)
	j := i + 1
	for j < n {
		switch src[j] {
		case '\\':
			if j+1 < n && src[j+1] == '\n' {
				newlines++
			}
			j += 2
			continue
		case '"':
			return j + 1, newlines, true
		case '\n':
			newlines++
		}
		j++
	}
	return 0, 0, false
}

// scanRaw scans a raw string whose opening quote is at q, with h hashes.
func scanRaw(src []byte, q, h int) (end int, newlines int32, ok bool) {
	n := len(src)
	j := q + 1
	for j < n {
		if src[j] == '\n' {
			newlines++
		} else if src[j] == '"' {
			k := 0
			for k < h && j+1+k < n && src[j+1+k] == '#' {
				k++
			}
			if k == h {
				return j + 1 + h, newlines, true
			}
		}
		j++
	}
	return 0, 0, false
}

// scanQuote disambiguates a character literal from a lifetime at the quote at i.
func scanQuote(src []byte, i int) (end int, kind tokKind, ok bool) {
	n := len(src)
	if i+1 >= n {
		return 0, 0, false
	}
	if src[i+1] == '\\' {
		// Escaped character literal: scan to the closing quote (a short run; '\u{10FFFF}' is the longest).
		j := i + 3
		for j < n && j < i+16 && src[j] != '\'' && src[j] != '\n' {
			j++
		}
		if j < n && src[j] == '\'' {
			return j + 1, tChar, true
		}
		return 0, 0, false
	}
	_, sz := utf8.DecodeRune(src[i+1:])
	if sz < 1 {
		sz = 1
	}
	if i+1+sz < n && src[i+1+sz] == '\'' {
		return i + 2 + sz, tChar, true
	}
	if isIdentStart(src[i+1]) {
		j := i + 1
		for j < n && isIdentCont(src[j]) {
			j++
		}
		return j, tLife, true
	}
	// A lone quote followed by something else (e.g. inside weird macro input): treat as punctuation-like lifetime of length 1.
	return i + 1, tLife, true
}

// strValue returns the value of a string-literal token (escapes decoded for cooked strings).
func strValue(src []byte, t token) string {
	s := src[t.start:t.end]
	k := 0
	for k < len(s) && (s[k] == 'b' || s[k] == 'c') {
		k++
	}
	raw := false
	if k < len(s) && s[k] == 'r' {
		raw = true
		k++
	}
	h := 0
	for k < len(s) && s[k] == '#' {
		h++
		k++
	}
	if k >= len(s) || s[k] != '"' {
		return ""
	}
	body := s[k+1:]
	if len(body) < 1+h {
		return ""
	}
	body = body[:len(body)-1-h]
	if raw {
		return string(body)
	}
	if !containsByte(body, '\\') {
		return string(body)
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' || i+1 >= len(body) {
			b.WriteByte(c)
			continue
		}
		i++
		switch body[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '0':
			b.WriteByte(0)
		case '\\', '"', '\'':
			b.WriteByte(body[i])
		case 'x':
			if i+2 < len(body) {
				if v, err := strconv.ParseUint(string(body[i+1:i+3]), 16, 8); err == nil {
					b.WriteByte(byte(v))
					i += 2
				}
			}
		case 'u':
			if i+1 < len(body) && body[i+1] == '{' {
				if e := indexByte(body[i:], '}'); e > 0 {
					if v, err := strconv.ParseUint(strings.ReplaceAll(string(body[i+2:i+e]), "_", ""), 16, 32); err == nil {
						b.WriteRune(rune(v))
					}
					i += e
				}
			}
		case '\n', '\r':
			// Line continuation: the backslash, the newline and the following whitespace are dropped.
			for i+1 < len(body) && (body[i+1] == ' ' || body[i+1] == '\t' || body[i+1] == '\n' || body[i+1] == '\r') {
				i++
			}
		default:
			b.WriteByte('\\')
			b.WriteByte(body[i])
		}
	}
	return b.String()
}

func containsByte(b []byte, c byte) bool { return indexByte(b, c) >= 0 }

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

package python

import "strings"

// tokKind is the class of a token. Whitespace and comments produce no token.
type tokKind uint8

const (
	tName    tokKind = iota + 1 // identifier or keyword
	tPunct                      // one punctuation byte, or a two-byte operator (==, +=, :=, ->, **, //, <<, >>, ...)
	tStr                        // any string literal; aux indexes lexed.strs
	tNum                        // number
	tNewline                    // end of a logical line (only emitted outside brackets)
)

type token struct {
	kind       tokKind
	sstart     bool  // first token of a statement (start of file, after a logical newline, `;`, or a `:` outside brackets)
	line       int32 // 1-based
	start, end int32 // byte range in the source
	aux        int32 // tStr: index into lexed.strs
}

// strInfo describes a string literal token.
type strInfo struct {
	val   string // decoded value; for an f-string, the static head up to the first replacement field
	field bool   // f-string with at least one replacement field
	bytes bool   // b"..." literal
	cut   bool   // val is not the full value (too long)
}

type lexed struct {
	src  []byte
	toks []token
	strs []strInfo
}

const (
	maxTokens   = 4 << 20
	maxStrKeep  = 4096 // longer literals keep no value (an import name or path is never this long)
	maxFDepth   = 64   // f-string nesting limit
	maxBrackets = 1 << 20
)

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentCont(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

// stringPrefix reports whether name is a string-literal prefix, and its flags.
func stringPrefix(name []byte) (ok, raw, f, b bool) {
	if len(name) == 0 || len(name) > 2 {
		return
	}
	for _, c := range name {
		switch c | 0x20 {
		case 'r':
			raw = true
		case 'f':
			f = true
		case 'b':
			b = true
		case 'u':
			if len(name) != 1 {
				return false, false, false, false
			}
		default:
			return false, false, false, false
		}
	}
	if len(name) == 2 && !(raw && (f || b)) {
		return false, false, false, false
	}
	return true, raw, f, b
}

// lex splits Python source into tokens. reason != "" means the source is malformed enough that the token list cannot be trusted
// (unterminated string, unbalanced brackets).
func lex(src []byte) (lx *lexed, reason string) {
	n := len(src)
	lx = &lexed{src: src, toks: make([]token, 0, n/5+16)}
	i := 0
	line := int32(1)
	if n >= 3 && src[0] == 0xEF && src[1] == 0xBB && src[2] == 0xBF {
		i = 3
	}
	depth := 0
	atStart := true // next token starts a statement
	add := func(k tokKind, start, end int, ln int32, aux int32) {
		lx.toks = append(lx.toks, token{kind: k, sstart: atStart, line: ln, start: int32(start), end: int32(end), aux: aux})
		atStart = false
	}
	for i < n {
		if len(lx.toks) > maxTokens {
			return nil, "too many tokens"
		}
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
			if depth == 0 && len(lx.toks) > 0 && lx.toks[len(lx.toks)-1].kind != tNewline {
				add(tNewline, i-1, i, line-1, 0)
			}
			if depth == 0 {
				atStart = true
			}
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == 0x0b:
			i++
		case c == '\\':
			j := i + 1
			if j < n && src[j] == '\r' {
				j++
			}
			if j < n && src[j] == '\n' {
				line++
				i = j + 1
			} else {
				i++
			}
		case c == '#':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '"' || c == '\'':
			ln := line
			end, si, ok, why := lx.lexString(i, false, false, false, &line)
			if !ok {
				return nil, why
			}
			add(tStr, i, end, ln, int32(si))
			i = end
		case isIdentStart(c):
			j := i + 1
			for j < n && isIdentCont(src[j]) {
				j++
			}
			if j < n && (src[j] == '"' || src[j] == '\'') {
				if ok, raw, f, b := stringPrefix(src[i:j]); ok {
					ln := line
					end, si, ok2, why := lx.lexString(j, raw, f, b, &line)
					if !ok2 {
						return nil, why
					}
					add(tStr, i, end, ln, int32(si))
					i = end
					continue
				}
			}
			add(tName, i, j, line, 0)
			i = j
		case c >= '0' && c <= '9' || c == '.' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9':
			j := i + 1
			for j < n {
				d := src[j]
				switch {
				case isIdentCont(d) || d == '.':
					j++
				case (d == '+' || d == '-') && (src[j-1]|0x20) == 'e' && !isHex(src[i:j]):
					j++
				default:
					goto numEnd
				}
			}
		numEnd:
			add(tNum, i, j, line, 0)
			i = j
		default:
			j := i + 1
			if j < n {
				switch d := src[j]; {
				case d == '=' && strings.IndexByte("=!<>+-*/%&|^@:", c) >= 0,
					c == '-' && d == '>', c == '*' && d == '*', c == '/' && d == '/', c == '<' && d == '<', c == '>' && d == '>':
					j++
				}
			}
			switch c {
			case '(', '[', '{':
				depth++
				if depth > maxBrackets {
					return nil, "brackets nested too deep"
				}
			case ')', ']', '}':
				if depth > 0 {
					depth--
				}
			}
			add(tPunct, i, j, line, 0)
			if c == ';' || c == ':' && depth == 0 && j == i+1 {
				atStart = true
			}
			i = j
		}
	}
	if depth != 0 {
		return nil, "unbalanced brackets at end of file"
	}
	return lx, ""
}

func isHex(num []byte) bool {
	return len(num) > 1 && num[0] == '0' && (num[1]|0x20) == 'x'
}

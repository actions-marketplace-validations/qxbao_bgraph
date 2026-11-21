package jvm

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// A tiny lexer for Gradle build scripts (Groovy and Kotlin DSL): comments and every string form are recognized so that
// identifiers inside them are never mistaken for code; everything else is split into identifiers and single-character punctuation.

type tokKind byte

const (
	tIdent tokKind = 'i'
	tStr   tokKind = 's'
	tPunct tokKind = 'p'
)

type token struct {
	kind tokKind
	text string // identifier, string contents (raw, escapes not decoded) or the punctuation character
	dyn  bool   // string with an interpolation ($x or ${...})
	line int
}

func isIdentStart(r rune) bool { return r == '_' || r == '$' || unicode.IsLetter(r) }
func isIdentPart(r rune) bool  { return isIdentStart(r) || unicode.IsDigit(r) }

// lexGradle tokenizes src. kotlin enables nested block comments. ok=false when a string or comment is unterminated.
func lexGradle(src string, kotlin bool) (toks []token, ok bool) {
	line := 1
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r':
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
				case kotlin && src[i] == '/' && i+1 < n && src[i+1] == '*':
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
				return toks, false
			}
		case c == '"' || c == '\'':
			quote := c
			start := line
			if i+2 < n && src[i+1] == quote && src[i+2] == quote {
				// triple-quoted
				i += 3
				j := i
				for {
					if j+2 >= n {
						return toks, false
					}
					if src[j] == quote && src[j+1] == quote && src[j+2] == quote {
						break
					}
					if src[j] == '\\' {
						j++
					}
					if j < n && src[j] == '\n' {
						line++
					}
					j++
				}
				s := src[i:j]
				toks = append(toks, token{kind: tStr, text: s, dyn: quote == '"' && strings.Contains(s, "$"), line: start})
				i = j + 3
				continue
			}
			i++
			j := i
			dyn := false
			depth := 0
			for j < n {
				ch := src[j]
				if ch == '\\' {
					j += 2
					continue
				}
				if quote == '"' && ch == '$' && j+1 < n && (src[j+1] == '{' || isIdentStart(rune(src[j+1]))) {
					dyn = true
					if src[j+1] == '{' {
						// skip the interpolated expression, which may contain quotes
						depth = 1
						j += 2
						for j < n && depth > 0 {
							switch src[j] {
							case '{':
								depth++
							case '}':
								depth--
							case '\n':
								line++
							case '"', '\'':
								// a string inside the interpolated expression
								q := src[j]
								j++
								for j < n && src[j] != q && src[j] != '\n' {
									if src[j] == '\\' {
										j++
									}
									j++
								}
							}
							j++
						}
						continue
					}
				}
				if ch == quote {
					break
				}
				if ch == '\n' {
					// An unterminated single-line string (or a Kotlin char literal): stop at the line end.
					break
				}
				j++
			}
			if j >= n || src[j] != quote {
				return toks, false
			}
			toks = append(toks, token{kind: tStr, text: src[i:j], dyn: dyn, line: start})
			i = j + 1
		case c == '`':
			j := strings.IndexByte(src[i+1:], '`')
			if j < 0 {
				return toks, false
			}
			toks = append(toks, token{kind: tIdent, text: src[i+1 : i+1+j], line: line})
			i += j + 2
		default:
			r, sz := utf8.DecodeRuneInString(src[i:])
			if isIdentStart(r) {
				j := i + sz
				for j < n {
					r2, sz2 := utf8.DecodeRuneInString(src[j:])
					if !isIdentPart(r2) {
						break
					}
					j += sz2
				}
				toks = append(toks, token{kind: tIdent, text: src[i:j], line: line})
				i = j
				continue
			}
			if unicode.IsDigit(r) {
				j := i + sz
				for j < n && (src[j] == '.' && j+1 < n && unicode.IsDigit(rune(src[j+1])) || unicode.IsDigit(rune(src[j])) || unicode.IsLetter(rune(src[j])) || src[j] == '_') {
					j++
				}
				i = j
				continue
			}
			toks = append(toks, token{kind: tPunct, text: string(r), line: line})
			i += sz
		}
	}
	return toks, true
}

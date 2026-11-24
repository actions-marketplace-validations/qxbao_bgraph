// Package swift lexes Swift manifests and extracts literal paths from Swift source.
package swift

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Kind uint8

const (
	Word Kind = iota
	Punctuation
	String
	ComputedString
	Regex
)

type Token struct {
	Kind      Kind
	Text      string
	LineBreak bool
}

type lexer struct {
	src   string
	pos   int
	depth int
}

func Lex(src []byte) ([]Token, error) {
	if !utf8.Valid(src) {
		return nil, fmt.Errorf("invalid UTF-8")
	}
	l := lexer{src: string(src)}
	var ts []Token
	for {
		previous := l.pos
		if err := l.skipTrivia(); err != nil {
			return ts, err
		}
		if l.pos == len(l.src) {
			return ts, nil
		}
		lineBreak := strings.ContainsAny(l.src[previous:l.pos], "\r\n")
		t, err := l.token()
		if err != nil {
			return ts, err
		}
		t.LineBreak = lineBreak
		ts = append(ts, t)
	}
}

func (l *lexer) skipTrivia() error {
	for l.pos < len(l.src) {
		switch {
		case strings.ContainsRune(" \t\r\n", rune(l.src[l.pos])):
			l.pos++
		case l.take("//"):
			n := strings.IndexAny(l.src[l.pos:], "\r\n")
			if n < 0 {
				l.pos = len(l.src)
			} else {
				l.pos += n
			}
		case l.take("/*"):
			for depth := 1; depth > 0; {
				if l.pos == len(l.src) {
					return fmt.Errorf("unterminated block comment")
				}
				switch {
				case l.take("/*"):
					depth++
				case l.take("*/"):
					depth--
				default:
					l.pos++
				}
			}
		default:
			return nil
		}
	}
	return nil
}

func (l *lexer) take(text string) bool {
	if !strings.HasPrefix(l.src[l.pos:], text) {
		return false
	}
	l.pos += len(text)
	return true
}

func wordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c >= utf8.RuneSelf
}

func (l *lexer) token() (Token, error) {
	start := l.pos
	if l.src[l.pos] == '#' || l.src[l.pos] == '"' {
		for l.pos < len(l.src) && l.src[l.pos] == '#' {
			l.pos++
		}
		hashes := l.src[start:l.pos]
		if l.take("\"") {
			return l.stringToken(hashes)
		}
		if hashes != "" && l.take("/") {
			return l.regex(hashes)
		}
		l.pos = start
	}
	if wordByte(l.src[l.pos]) {
		for l.pos < len(l.src) && wordByte(l.src[l.pos]) {
			l.pos++
		}
		return Token{Kind: Word, Text: l.src[start:l.pos]}, nil
	}
	l.pos++
	// Join adjacent Swift operator characters to keep e.g. ".." distinct from "." ".".
	if strings.ContainsRune(".=+-*/%<>!&|^?~", rune(l.src[start])) {
		for l.pos < len(l.src) && strings.ContainsRune(".=+-*/%<>!&|^?~", rune(l.src[l.pos])) &&
			!strings.HasPrefix(l.src[l.pos:], "/*") && !strings.HasPrefix(l.src[l.pos:], "//") {
			l.pos++
		}
	}
	return Token{Kind: Punctuation, Text: l.src[start:l.pos]}, nil
}

func (l *lexer) stringToken(hashes string) (Token, error) {
	l.depth++
	defer func() { l.depth-- }()
	if l.depth > 100 {
		return Token{}, fmt.Errorf("nesting limit exceeded in Swift string")
	}
	multiline := l.take("\"\"")
	close := "\"" + hashes
	if multiline {
		close = "\"\"" + close
	}
	escape := "\\" + hashes
	computed := multiline // Swift multiline indentation stripping is outside the manifest subset.
	var value strings.Builder
	for l.pos < len(l.src) {
		switch {
		case l.take(close):
			if computed {
				return Token{Kind: ComputedString, Text: value.String()}, nil
			}
			return Token{Kind: String, Text: value.String()}, nil
		case l.take(escape):
			if l.pos == len(l.src) {
				return Token{}, fmt.Errorf("unterminated Swift string escape")
			}
			c := l.src[l.pos]
			l.pos++
			switch c {
			case '(':
				computed = true
				if err := l.interpolation(); err != nil {
					return Token{}, err
				}
			case '0':
				value.WriteByte(0)
			case 'n':
				value.WriteByte('\n')
			case 'r':
				value.WriteByte('\r')
			case 't':
				value.WriteByte('\t')
			case '\\', '"', '\'':
				value.WriteByte(c)
			case 'u':
				r, err := l.scalar()
				if err != nil {
					return Token{}, err
				}
				value.WriteRune(r)
			case '\n', '\r', ' ', '\t':
				if !multiline {
					return Token{}, fmt.Errorf("line continuation outside multiline Swift string")
				}
				// The value of a multiline token is never accepted as a manifest literal.
				for l.pos < len(l.src) && strings.ContainsRune(" \t", rune(l.src[l.pos])) {
					l.pos++
				}
			default:
				return Token{}, fmt.Errorf("unsupported Swift string escape")
			}
		default:
			c := l.src[l.pos]
			if !multiline && (c == '\n' || c == '\r') {
				return Token{}, fmt.Errorf("newline in single-line Swift string")
			}
			l.pos++
			value.WriteByte(c)
		}
	}
	return Token{}, fmt.Errorf("unterminated Swift string")
}

func (l *lexer) scalar() (rune, error) {
	if !l.take("{") {
		return 0, fmt.Errorf("invalid Swift Unicode escape")
	}
	n := strings.IndexByte(l.src[l.pos:], '}')
	if n < 1 || n > 8 {
		return 0, fmt.Errorf("invalid Swift Unicode escape")
	}
	v, err := strconv.ParseUint(l.src[l.pos:l.pos+n], 16, 32)
	l.pos += n + 1
	if err != nil || v > utf8.MaxRune || !utf8.ValidRune(rune(v)) {
		return 0, fmt.Errorf("invalid Swift Unicode scalar")
	}
	return rune(v), nil
}

func (l *lexer) interpolation() error {
	for depth := 1; depth > 0; {
		if err := l.skipTrivia(); err != nil {
			return err
		}
		if l.pos == len(l.src) {
			return fmt.Errorf("unterminated Swift string interpolation")
		}
		t, err := l.token()
		if err != nil {
			return err
		}
		if t.Kind != Punctuation {
			continue
		}
		switch t.Text {
		case "(":
			depth++
		case ")":
			depth--
		}
	}
	return nil
}

// Extended regexes have an unambiguous hash-delimited opening. Their
// contents are never parsed as manifest expressions or as runtime paths.
func (l *lexer) regex(hashes string) (Token, error) {
	close := "/" + hashes
	for l.pos < len(l.src) {
		if l.take(close) {
			return Token{Kind: Regex}, nil
		}
		if l.src[l.pos] == '\\' && l.pos+1 < len(l.src) {
			l.pos++
		}
		l.pos++
	}
	return Token{}, fmt.Errorf("unterminated Swift regex literal")
}

package php

import "strings"

// Token kinds of Tokens.
const (
	TokHTML       byte = 'h' // inline HTML outside <?php ... ?>
	TokOpenTag    byte = 'o' // <?php or <?=
	TokCloseTag   byte = 'x' // ?>
	TokComment    byte = 'c' // //, # or /* */ comment
	TokDocComment byte = 'D' // /** */ doc comment (read by reflection)
	TokIdent      byte = 'i' // identifier or keyword
	TokNumber     byte = 'n'
	TokString     byte = 's' // quoted string, heredoc or nowdoc (Text is the raw source, interpolations included)
	TokPunct      byte = 'p' // one punctuation character (Text)
	TokHalt       byte = 'd' // everything after __halt_compiler(); (data)
)

// Token is one token of Tokens. Line and EndLine are the 1-based lines of its first and last character.
type Token struct {
	Kind          byte
	Text          string
	Line, EndLine int
}

// Tokens lexes PHP source completely (comments, inline HTML, string interpolation, heredoc and nowdoc, attributes as
// punctuation, __halt_compiler data) for the dynamic layer's body guard (docs/dynamic-layer.md §11.2). It is fail-closed: the
// second result is a description of the first construct it cannot read with certainty ("" when the whole file was read), and
// callers must not trust the tokens then. Short open tags (<? without php) are such a construct: their meaning depends on
// short_open_tag.
func Tokens(src []byte) ([]Token, string) {
	l := fullLexer{s: string(src), line: 1}
	var out []Token
	html := true
	for l.pos < len(l.s) && l.problem == "" {
		if html {
			j := strings.Index(l.s[l.pos:], "<?")
			if j < 0 {
				out = append(out, l.take(TokHTML, len(l.s)-l.pos))
				break
			}
			if j > 0 {
				out = append(out, l.take(TokHTML, j))
			}
			rest := l.s[l.pos+2:]
			switch {
			case len(rest) >= 3 && strings.EqualFold(rest[:3], "php") && (len(rest) == 3 || space(rest[3])):
				out = append(out, l.take(TokOpenTag, 5))
			case strings.HasPrefix(rest, "="):
				out = append(out, l.take(TokOpenTag, 3))
			default:
				l.problem = "short open tag (its meaning depends on short_open_tag)"
				continue
			}
			html = false
			continue
		}
		t, ok := l.code()
		if !ok {
			continue
		}
		out = append(out, t)
		switch {
		case t.Kind == TokCloseTag:
			html = true
		case t.Kind == TokIdent && strings.EqualFold(t.Text, "__halt_compiler") && !memberAccess(out[:len(out)-1]):
			// __halt_compiler ( ) ; or ?> : everything after is data (__COMPILER_HALT_OFFSET__).
			for _, want := range []string{"(", ")"} {
				n, ok := l.code()
				for ok && (n.Kind == TokComment || n.Kind == TokDocComment) {
					out = append(out, n)
					n, ok = l.code()
				}
				if !ok || n.Kind != TokPunct || n.Text != want {
					if l.problem == "" {
						l.problem = "malformed __halt_compiler"
					}
					return out, l.problem
				}
				out = append(out, n)
			}
			n, ok := l.code()
			for ok && (n.Kind == TokComment || n.Kind == TokDocComment) {
				out = append(out, n)
				n, ok = l.code()
			}
			if !ok || !(n.Kind == TokPunct && n.Text == ";" || n.Kind == TokCloseTag) {
				if l.problem == "" {
					l.problem = "malformed __halt_compiler"
				}
				return out, l.problem
			}
			out = append(out, n)
			if l.pos < len(l.s) {
				out = append(out, l.take(TokHalt, len(l.s)-l.pos))
			}
			return out, l.problem
		}
	}
	return out, l.problem
}

// memberAccess reports whether the last non-comment token of prev makes the next identifier a member name (->, ?->, ::).
func memberAccess(prev []Token) bool {
	i := len(prev) - 1
	for i >= 0 && (prev[i].Kind == TokComment || prev[i].Kind == TokDocComment) {
		i--
	}
	if i < 1 || prev[i].Kind != TokPunct {
		return false
	}
	switch prev[i].Text {
	case ">":
		return prev[i-1].Kind == TokPunct && prev[i-1].Text == "-"
	case ":":
		return prev[i-1].Kind == TokPunct && prev[i-1].Text == ":"
	}
	return false
}

type fullLexer struct {
	s       string
	pos     int
	line    int
	problem string
}

// take returns the next n bytes as a token of kind k and advances.
func (l *fullLexer) take(k byte, n int) Token {
	text := l.s[l.pos : l.pos+n]
	t := Token{Kind: k, Text: text, Line: l.line}
	l.line += strings.Count(text, "\n")
	t.EndLine = l.line
	if strings.HasSuffix(text, "\n") && n > 0 {
		t.EndLine = l.line - 1
		if t.EndLine < t.Line {
			t.EndLine = t.Line
		}
	}
	l.pos += n
	return t
}

// code returns the next token in PHP mode (ok false at the end or on a problem).
func (l *fullLexer) code() (Token, bool) {
	for l.pos < len(l.s) {
		rest := l.s[l.pos:]
		c := rest[0]
		switch {
		case space(c):
			n := 1
			for n < len(rest) && space(rest[n]) {
				n++
			}
			l.take(0, n)
		case strings.HasPrefix(rest, "?>"):
			n := 2
			// The close tag eats one directly following newline.
			if strings.HasPrefix(rest[2:], "\r\n") {
				n += 2
			} else if strings.HasPrefix(rest[2:], "\n") {
				n++
			}
			return l.take(TokCloseTag, n), true
		case strings.HasPrefix(rest, "/*"):
			j := strings.Index(rest[2:], "*/")
			if j < 0 {
				l.problem = "unterminated block comment"
				return Token{}, false
			}
			k := TokComment
			if strings.HasPrefix(rest, "/**") && len(rest) > 3 && space(rest[3]) {
				k = TokDocComment
			}
			return l.take(k, j+4), true
		case strings.HasPrefix(rest, "//") || c == '#' && !strings.HasPrefix(rest, "#["):
			n := strings.IndexAny(rest, "\r\n")
			if n < 0 {
				n = len(rest)
			}
			if j := strings.Index(rest[:n], "?>"); j >= 0 {
				n = j
			}
			return l.take(TokComment, n), true
		case c == '\'':
			n, ok := singleQuoted(rest)
			if !ok {
				l.problem = "unterminated string"
				return Token{}, false
			}
			return l.take(TokString, n), true
		case c == '"' || c == '`':
			n, why := interpolated(rest, c)
			if why != "" {
				l.problem = why
				return Token{}, false
			}
			return l.take(TokString, n), true
		case strings.HasPrefix(rest, "<<<"):
			n, why := heredocLen(rest)
			if why != "" {
				l.problem = why
				return Token{}, false
			}
			return l.take(TokString, n), true
		case ident(c):
			n := 1
			for n < len(rest) && (ident(rest[n]) || digit(rest[n])) {
				n++
			}
			return l.take(TokIdent, n), true
		case digit(c):
			n := 1
			for n < len(rest) && (ident(rest[n]) || digit(rest[n]) || rest[n] == '.' && n+1 < len(rest) && digit(rest[n+1])) {
				n++
			}
			return l.take(TokNumber, n), true
		default:
			return l.take(TokPunct, 1), true
		}
	}
	return Token{}, false
}

func digit(c byte) bool { return c >= '0' && c <= '9' }

// singleQuoted returns the length of the single-quoted string at the start of s.
func singleQuoted(s string) (int, bool) {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '\'':
			return i + 1, true
		}
	}
	return 0, false
}

// interpolated returns the length of the double-quoted or backtick string at the start of s, following {$ ... } and ${ ... }
// interpolations (code that may itself contain strings and braces).
func interpolated(s string, quote byte) (int, string) {
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\':
			i++
		case c == quote:
			return i + 1, ""
		case c == '{' && i+1 < len(s) && s[i+1] == '$', c == '$' && i+1 < len(s) && s[i+1] == '{':
			n, why := interpolation(s[i:])
			if why != "" {
				return 0, why
			}
			i += n - 1
		}
	}
	return 0, "unterminated string"
}

// interpolation returns the length of the {$ ... } or ${ ... } interpolation at the start of s.
func interpolation(s string) (int, string) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1, ""
			}
		case '\'':
			n, ok := singleQuoted(s[i:])
			if !ok {
				return 0, "unterminated string in an interpolation"
			}
			i += n - 1
		case '"', '`':
			n, why := interpolated(s[i:], c)
			if why != "" {
				return 0, why
			}
			i += n - 1
		case '\n':
			// Allowed by PHP, but rare; follow it.
		}
	}
	return 0, "unterminated string interpolation"
}

// heredocLen returns the length of the heredoc or nowdoc at the start of s (through its closing label).
func heredocLen(s string) (int, string) {
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return 0, "unterminated or invalid heredoc"
	}
	head := strings.TrimRight(s[3:nl], "\r")
	head = strings.TrimLeft(head, " \t")
	switch {
	case len(head) >= 2 && head[0] == '\'' && head[len(head)-1] == '\'':
		head = head[1 : len(head)-1]
	case len(head) >= 2 && head[0] == '"' && head[len(head)-1] == '"':
		head = head[1 : len(head)-1]
	}
	label := head
	valid := label != "" && ident(label[0])
	for i := 1; valid && i < len(label); i++ {
		valid = ident(label[i]) || digit(label[i])
	}
	if !valid {
		return 0, "unterminated or invalid heredoc"
	}
	pos := nl + 1
	for pos <= len(s) {
		line := s[pos:]
		if j := strings.IndexByte(line, '\n'); j >= 0 {
			line = line[:j]
		}
		trimmed := strings.TrimLeft(line, " \t")
		if rest, ok := strings.CutPrefix(trimmed, label); ok && (rest == "" || !ident(rest[0]) && !digit(rest[0])) {
			return pos + (len(line) - len(rest)), ""
		}
		if pos+len(line) >= len(s) {
			break
		}
		pos += len(line) + 1
	}
	return 0, "unterminated or invalid heredoc"
}

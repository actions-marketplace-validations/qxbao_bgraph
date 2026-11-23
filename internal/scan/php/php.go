// Package php scans PHP include and require expressions. Symbol and namespace
// resolution belongs to the Composer project, not this scanner.
package php

import (
	"strconv"
	"strings"

	"github.com/qxbao/bgraph/internal/model"
)

// Source reports the PHP source and template extensions recognized by the scanner.
func Source(p string) bool {
	p = strings.ToLower(p)
	for _, ext := range []string{".php", ".phtml", ".inc", ".php3", ".php4", ".php5", ".php7", ".phpt", ".module", ".install", ".theme"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}

type token struct {
	kind byte
	text string
	line int
}

// Scan encodes a source-directory-relative include with Suffix="__DIR__".
// A bare string has both runtime-working-directory and include_path semantics.
func Scan(src []byte) model.ScanResult {
	l := lexer{s: string(src), line: 1, template: true}
	var tokens []token
	for l.s != "" {
		tokens = append(tokens, l.next())
	}
	var out model.ScanResult
	out.Uncertain = l.problem != ""
	out.UncertainReason = l.problem
	for i, t := range tokens {
		if t.kind != 'i' || member(tokens[:i]) {
			continue
		}
		switch strings.ToLower(t.text) {
		case "include", "include_once", "require", "require_once":
			out.Imports = append(out.Imports, operand(tokens[i+1:], t.line))
		case "file_get_contents", "readfile", "file", "fopen", "parse_ini_file", "simplexml_load_file",
			"file_exists", "is_file", "is_dir", "scandir", "opendir", "realpath":
			if i+1 < len(tokens) && tokens[i+1].kind == '(' {
				ref := operand(tokens[i+2:], t.line)
				ref.Kind = model.ImportRead
				out.Imports = append(out.Imports, ref)
			}
		case "chdir":
			if i+1 < len(tokens) && tokens[i+1].kind == '(' {
				out.Uncertain = true
				out.UncertainReason = "PHP code changes the runtime working directory"
			}
		}
	}
	return out
}

// member reports whether the identifier after prev is not a global language
// construct or function call: a variable ($include), a property or method
// ($request->file, $x?->file, self::INCLUDE), or a name being declared or
// instantiated (new File, function file, const INCLUDE).
func member(prev []token) bool {
	n := len(prev)
	if n == 0 {
		return false
	}
	last := prev[n-1]
	switch last.kind {
	case '$':
		return true
	case '>':
		return n >= 2 && prev[n-2].kind == '-'
	case ':':
		return n >= 2 && prev[n-2].kind == ':'
	case 'i':
		switch strings.ToLower(last.text) {
		case "new", "function", "fn", "const", "class", "interface", "trait", "enum", "instanceof":
			return true
		}
	}
	return false
}

func operand(ts []token, line int) model.ImportRef {
	r := model.ImportRef{Kind: model.ImportRequire, Line: line, Dynamic: true}
	parens := 0
	for len(ts) > 0 && ts[0].kind == '(' {
		parens++
		ts = ts[1:]
	}
	var value strings.Builder
	if n, parents := magicDir(ts, 0); n != 0 {
		r.Suffix = "__DIR__"
		value.WriteByte('/')
		value.WriteString(strings.Repeat("../", parents))
		ts = ts[n:]
	} else {
		if len(ts) == 0 || ts[0].kind != 's' {
			return r
		}
		value.WriteString(ts[0].text)
		ts = ts[1:]
	}
	needSeparator := r.Suffix == "__DIR__"
	for len(ts) >= 2 && ts[0].kind == '.' {
		switch {
		case ts[1].kind == 's':
			if needSeparator && !strings.HasPrefix(ts[1].text, "/") {
				// __DIR__ concatenated with a bare name does not insert a separator.
				return r
			}
			value.WriteString(ts[1].text)
		case ts[1].kind == 'i' && ts[1].text == "DIRECTORY_SEPARATOR":
			value.WriteByte('/')
		default:
			r.Spec = value.String()
			return r
		}
		needSeparator = false
		ts = ts[2:]
	}
	r.Spec = value.String()
	if r.Suffix == "__DIR__" {
		r.Spec = "/" + strings.TrimLeft(r.Spec, "/")
	}
	for ; parens > 0 && len(ts) > 0 && ts[0].kind == ')'; parens-- {
		ts = ts[1:]
	}
	if parens == 0 && (len(ts) == 0 || strings.ContainsRune(";),}]", rune(ts[0].kind)) || ts[0].kind == 'e') {
		r.Dynamic = false
	}
	return r
}

// magicDir returns a source-relative directory and the number of parent
// steps in a literal __DIR__ or dirname(__DIR__/__FILE__) expression.
func magicDir(ts []token, depth int) (consumed, parents int) {
	if len(ts) == 0 || ts[0].kind != 'i' || depth == 8 {
		return 0, 0
	}
	if strings.EqualFold(ts[0].text, "__DIR__") {
		return 1, 0
	}
	if !strings.EqualFold(ts[0].text, "dirname") || len(ts) < 4 || ts[1].kind != '(' {
		return 0, 0
	}
	n := 0
	if ts[2].kind == 'i' && strings.EqualFold(ts[2].text, "__FILE__") {
		n, parents = 1, -1
	} else {
		n, parents = magicDir(ts[2:], depth+1)
	}
	end := n + 2
	if n == 0 || end >= len(ts) {
		return 0, 0
	}
	steps := 1
	if ts[end].kind == ',' && end+2 < len(ts) && ts[end+1].kind == 'n' {
		var err error
		count := ts[end+1].text
		steps, err = strconv.Atoi(count)
		if err != nil || strings.HasPrefix(count, "0") || steps > 64 {
			return 0, 0
		}
		end += 2
	}
	if ts[end].kind != ')' {
		return 0, 0
	}
	return end + 1, parents + steps
}

type lexer struct {
	s        string
	line     int
	template bool
	problem  string
}

func (l *lexer) advance(n int) {
	l.line += strings.Count(l.s[:n], "\n")
	l.s = l.s[n:]
}

func (l *lexer) next() token {
	for l.s != "" {
		c := l.s[0]
		switch {
		case l.template:
			j := strings.Index(l.s, "<?")
			if j < 0 {
				l.advance(len(l.s))
				continue
			}
			l.advance(j + 2)
			if len(l.s) > 3 && strings.EqualFold(l.s[:3], "php") && space(l.s[3]) {
				l.advance(3)
			} else if strings.HasPrefix(l.s, "=") {
				l.advance(1)
			}
			l.template = false
			return token{kind: 'e', line: l.line}
		case strings.HasPrefix(l.s, "?>"):
			l.advance(2)
			l.template = true
			return token{kind: 'e', line: l.line}
		case strings.HasPrefix(l.s, "/*"):
			j := strings.Index(l.s[2:], "*/")
			if j < 0 {
				l.problem = "unterminated block comment"
				l.advance(len(l.s))
				continue
			}
			l.advance(j + 4)
		case strings.HasPrefix(l.s, "//") || (c == '#' && !strings.HasPrefix(l.s, "#[")):
			n := strings.IndexAny(l.s, "\r\n")
			if n < 0 {
				n = len(l.s)
			}
			if j := strings.Index(l.s[:n], "?>"); j >= 0 {
				n = j
			}
			l.advance(n)
		case space(c):
			l.advance(1)
		case c == '\'' || c == '"' || c == '`':
			return l.quoted()
		case strings.HasPrefix(l.s, "<<<"):
			return l.heredoc()
		case ident(c):
			n := 1
			for n < len(l.s) && (ident(l.s[n]) || l.s[n] >= '0' && l.s[n] <= '9') {
				n++
			}
			t := token{kind: 'i', text: l.s[:n], line: l.line}
			l.advance(n)
			return t
		case c >= '0' && c <= '9':
			n := 1
			for n < len(l.s) && l.s[n] >= '0' && l.s[n] <= '9' {
				n++
			}
			t := token{kind: 'n', text: l.s[:n], line: l.line}
			l.advance(n)
			return t
		default:
			t := token{kind: c, line: l.line}
			l.advance(1)
			return t
		}
	}
	return token{kind: 'e', line: l.line}
}

func (l *lexer) quoted() token {
	quote := l.s[0]
	t := token{kind: 's', line: l.line}
	if quote == '`' {
		t.kind = 'd'
	}
	var value strings.Builder
	for i := 1; i < len(l.s); i++ {
		c := l.s[i]
		switch {
		case c == quote:
			t.text = value.String()
			l.advance(i + 1)
			return t
		case c == '\\' && i+1 < len(l.s):
			next := l.s[i+1]
			if next == quote || next == '\\' || (quote == '"' && next == '$') {
				i++
				value.WriteByte(next)
				continue
			}
			if quote != '\'' {
				t.kind = 'd' // numeric and other escape sequences may alter the path
			}
		case c == '$' && quote != '\'':
			t.kind = 'd'
			if i+1 < len(l.s) && l.s[i+1] == '{' || i > 1 && l.s[i-1] == '{' {
				l.problem = "complex PHP string interpolation"
			}
		}
		value.WriteByte(c)
	}
	l.problem = "unterminated string"
	l.advance(len(l.s))
	t.kind = 'd'
	return t
}

func (l *lexer) heredoc() token {
	t := token{kind: 'd', line: l.line}
	if nl := strings.IndexByte(l.s, '\n'); nl >= 0 {
		label := strings.Trim(strings.TrimSpace(l.s[3:nl]), "\"'")
		valid := len(label) != 0 && ident(label[0])
		for i := 1; valid && i < len(label); i++ {
			valid = ident(label[i]) || label[i] >= '0' && label[i] <= '9'
		}
		if valid {
			l.advance(nl + 1)
			for l.s != "" {
				trimmed := strings.TrimLeft(l.s, " \t")
				if rest, ok := strings.CutPrefix(trimmed, label); ok && (rest == "" || !ident(rest[0]) && (rest[0] < '0' || rest[0] > '9')) {
					l.advance(len(l.s) - len(rest))
					return t
				}
				j := strings.IndexByte(l.s, '\n')
				if j < 0 {
					break
				}
				l.advance(j + 1)
			}
		}
	}
	l.problem = "unterminated or invalid heredoc"
	l.advance(len(l.s))
	return t
}

func ident(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= 0x80 }
func space(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v'
}

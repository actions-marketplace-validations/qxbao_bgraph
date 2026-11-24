// Package cpp recognizes literal CMake and Meson build declarations.
package cpp

import (
	"fmt"
	"strings"
)

type Arg struct {
	Text   string
	Quoted bool
	Key    string
	List   []Arg
}

type Call struct {
	Name   string
	Assign string
	Args   []Arg
	Line   int
}

type token struct {
	text string
	kind byte
	line int
}

type lexer struct {
	src   string
	line  int
	meson bool
}

func CMake(src []byte) ([]Call, error) { return parse(string(src), false) }
func Meson(src []byte) ([]Call, error) { return parse(string(src), true) }

func (l *lexer) take(n int) string {
	s := l.src[:n]
	l.line += strings.Count(s, "\n")
	l.src = l.src[n:]
	return s
}

func bracket(s string) (int, string) {
	if !strings.HasPrefix(s, "[") {
		return 0, ""
	}
	i := 1
	for i < len(s) && s[i] == '=' {
		i++
	}
	if i < len(s) && s[i] == '[' {
		return i + 1, "]" + s[1:i] + "]"
	}
	return 0, ""
}

func (l *lexer) next() (token, error) {
	for len(l.src) != 0 {
		switch l.src[0] {
		case ' ', '\t', '\r', '\n':
			l.take(1)
		case '#':
			if !l.meson {
				if n, end := bracket(l.src[1:]); n != 0 {
					i := strings.Index(l.src[n+1:], end)
					if i < 0 {
						return token{}, fmt.Errorf("line %d: unterminated CMake bracket comment", l.line)
					}
					l.take(n + 1 + i + len(end))
					continue
				}
			}
			i := strings.IndexByte(l.src, '\n')
			if i < 0 {
				i = len(l.src)
			}
			l.take(i)
		default:
			return l.value()
		}
	}
	return token{line: l.line}, nil
}

func (l *lexer) value() (token, error) {
	line := l.line
	c := l.src[0]
	if !l.meson && c == '[' {
		if n, end := bracket(l.src); n != 0 {
			i := strings.Index(l.src[n:], end)
			if i >= 0 {
				s := l.take(n + i + len(end))[n : n+i]
				return l.cmakeQuoted(s, line)
			}
			return token{}, fmt.Errorf("line %d: unterminated CMake bracket argument", line)
		}
	}
	if !l.meson && c == '"' || l.meson && c == '\'' {
		quote := l.take(1)
		if l.meson && strings.HasPrefix(l.src, quote+quote) {
			quote += l.take(2)
		}
		var b strings.Builder
		for len(l.src) > 0 && !strings.HasPrefix(l.src, quote) {
			if l.src[0] == '\\' {
				// Preserve the escape so the consumer can reject any value
				// whose interpretation may differ from the literal spelling.
				if len(l.src) == 1 {
					return token{}, fmt.Errorf("line %d: incomplete escape", l.line)
				}
				b.WriteString(l.take(2))
			} else {
				b.WriteString(l.take(1))
			}
		}
		if !strings.HasPrefix(l.src, quote) {
			return token{}, fmt.Errorf("line %d: unterminated quoted string", line)
		}
		l.take(len(quote))
		if l.meson {
			return token{b.String(), 'q', line}, nil
		}
		return l.cmakeQuoted(b.String(), line)
	}
	delims := "()"
	if l.meson {
		delims += "[]=:,{}.+*/!?<>-%"
	}
	if strings.IndexByte(delims, c) >= 0 {
		return token{l.take(1), c, line}, nil
	}
	separators := " \t\r\n#\"" + delims
	if l.meson {
		separators += "'"
	}
	n := strings.IndexAny(l.src, separators)
	if n == 0 {
		return token{}, fmt.Errorf("line %d: unexpected byte %q", line, c)
	}
	if n < 0 {
		n = len(l.src)
	}
	if !l.meson && n < len(l.src) && l.src[n] == '"' {
		return token{}, fmt.Errorf("line %d: adjacent CMake arguments", line)
	}
	return token{l.take(n), 'w', line}, nil
}

func (l *lexer) cmakeQuoted(s string, line int) (token, error) {
	if len(l.src) > 0 && !strings.ContainsRune(" \t\r\n)#", rune(l.src[0])) {
		return token{}, fmt.Errorf("line %d: adjacent CMake arguments", line)
	}
	return token{s, 'q', line}, nil
}

type parser struct {
	tokens []token
	meson  bool
}

func parse(src string, meson bool) ([]Call, error) {
	l := lexer{src: strings.TrimPrefix(src, "\ufeff"), line: 1, meson: meson}
	p := parser{meson: meson}
	var scanErr error
	for {
		t, err := l.next()
		scanErr = err
		if err != nil || t.kind == 0 {
			p.tokens = append(p.tokens, token{line: l.line})
			break
		}
		p.tokens = append(p.tokens, t)
	}
	var calls []Call
	for p.peek().kind != 0 {
		call, err := p.call()
		if err != nil {
			if call.Name == "project" {
				// Preserve the root declaration even when its arguments use
				// expressions outside the supported subset.
				calls = append(calls, call)
			}
			return calls, err
		}
		calls = append(calls, call)
	}
	return calls, scanErr
}

func (p *parser) peek() token { return p.tokens[0] }
func (p *parser) pop() token {
	t := p.peek()
	if t.kind != 0 {
		p.tokens = p.tokens[1:]
	}
	return t
}

func (p *parser) call() (Call, error) {
	t := p.pop()
	call := Call{Name: t.text, Line: t.line}
	if t.kind != 'w' {
		return call, fmt.Errorf("line %d: expected build command", t.line)
	}
	if p.meson && p.peek().kind == '=' {
		p.pop()
		call.Assign = call.Name
		name := p.pop()
		if name.kind != 'w' {
			return call, fmt.Errorf("line %d: expected Meson function", call.Line)
		}
		call.Name = name.text
	}
	if p.pop().kind != '(' {
		return call, fmt.Errorf("line %d: unsupported build statement %s", call.Line, call.Name)
	}
	if !p.meson {
		call.Name = strings.ToLower(call.Name)
	}
	var err error
	call.Args, err = p.args(')', 0)
	return call, err
}

func (p *parser) args(end byte, depth int) ([]Arg, error) {
	if depth > 64 {
		return nil, fmt.Errorf("line %d: too many nested lists", p.peek().line)
	}
	var args []Arg
	for p.peek().kind != end {
		t := p.pop()
		a := Arg{}
		if p.meson && t.kind == 'w' && p.peek().kind == ':' {
			a.Key = t.text
			p.pop()
			t = p.pop()
		}
		switch {
		case t.kind == 'w' || t.kind == 'q':
			a.Text, a.Quoted = t.text, t.kind == 'q'
		case !p.meson && t.kind == '(':
			group, err := p.args(')', depth+1)
			if err != nil {
				return nil, err
			}
			a.List = append([]Arg{}, group...)
		case p.meson && t.kind == '[':
			list, err := p.args(']', depth+1)
			if err != nil {
				return nil, err
			}
			a.List = append([]Arg{}, list...)
		default:
			return nil, fmt.Errorf("line %d: unsupported argument", t.line)
		}
		args = append(args, a)
		if p.meson && p.peek().kind != end && p.pop().kind != ',' {
			return nil, fmt.Errorf("line %d: expected comma", t.line)
		}
	}
	p.pop()
	return args, nil
}

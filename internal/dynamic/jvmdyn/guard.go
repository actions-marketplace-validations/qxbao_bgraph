package jvmdyn

// Constant guard of the JVM class layer (docs/dynamic-layer.md §9.2). Compilers copy the value of a constant variable (Java: a final
// field of primitive or String type with a constant initializer, any interface field; Kotlin: const val) and annotation element
// defaults into the class files that use them, so a test using A.CONST never loads A and its record does not list A's file.
// The guard is lexical and fail-closed: a changed hunk that touches a declaration that may be such a constant, or a file the
// lexer cannot place, refuses class narrowing.

import (
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// tok is one lexical token. k: 'i' identifier or keyword, 's' string/char/text-block literal, 'n' number, 'p' punctuation.
type tok struct {
	k             byte
	s             string
	line, endLine int // 1-based lines of the first and last character
}

// span is a guarded declaration: lines from..to (1-based, inclusive).
type span struct {
	from, to int
	what     string
}

// ConstantGuard returns "" when changing Java/Kotlin source file p from base to head cannot alter a value the compiler copies
// into other class files; otherwise the reason (one line, names the declaration and line).
func ConstantGuard(p string, base, head []byte) string {
	var parse func([]byte) ([]span, bool)
	switch strings.ToLower(path.Ext(p)) {
	case ".java":
		parse = javaSpans
	case ".kt":
		parse = kotlinSpans
	default:
		return p + ": not a Java or Kotlin source"
	}
	if string(base) == string(head) {
		return ""
	}
	bs, ok := parse(base)
	if !ok {
		return p + ": cannot be lexed reliably at the base"
	}
	hs, ok := parse(head)
	if !ok {
		return p + ": cannot be lexed reliably at the head"
	}
	for _, h := range diffHunks(splitLines(base), splitLines(head)) {
		if s, ok := touched(bs, h.b0, h.b1); ok {
			return fmt.Sprintf("%s:%d (base): change touches %s", p, s.from, s.what)
		}
		if s, ok := touched(hs, h.h0, h.h1); ok {
			return fmt.Sprintf("%s:%d: change touches %s", p, s.from, s.what)
		}
	}
	return ""
}

// touched reports the first span that intersects lines [a, b) (0-based, half-open).
func touched(spans []span, a, b int) (span, bool) {
	if b <= a {
		return span{}, false
	}
	for _, s := range spans {
		if a+1 <= s.to && b >= s.from {
			return s, true
		}
	}
	return span{}, false
}

// TopLevelTypes returns the binary names ("pkg.Name") of the top-level types declared in src; ok=false when the file cannot be
// lexed or placed reliably.
func TopLevelTypes(p string, src []byte) ([]string, bool) {
	switch strings.ToLower(path.Ext(p)) {
	case ".java":
		t, ok := lexJava(src)
		if !ok {
			return nil, false
		}
		jp := &jparser{t: t}
		jp.file()
		if jp.bad {
			return nil, false
		}
		return qualify(jp.pkg, jp.types), true
	case ".kt":
		t, ok := lexKotlin(src)
		if !ok {
			return nil, false
		}
		kp, ok := newKParser(t)
		if !ok {
			return nil, false
		}
		pkg, ok := kotlinPackage(t)
		if !ok {
			return nil, false
		}
		var names []string
		for i, x := range t {
			if kp.depth[i] != 0 || x.k != 'i' || (x.s != "class" && x.s != "interface" && x.s != "object") {
				continue
			}
			if i > 0 && t[i-1].s == ":" && i > 1 && t[i-2].s == ":" {
				continue // Foo::class
			}
			if i+1 >= len(t) || t[i+1].k != 'i' {
				continue // object expression or object : Type
			}
			names = append(names, strings.Trim(t[i+1].s, "`"))
		}
		return qualify(pkg, names), true
	}
	return nil, false
}

func qualify(pkg string, names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if pkg != "" {
			n = pkg + "." + n
		}
		out = append(out, n)
	}
	return out
}

// PackageName returns the package declaration of a Java/Kotlin file ("" for the default package); ok=false when unreadable.
func PackageName(p string, src []byte) (string, bool) {
	switch strings.ToLower(path.Ext(p)) {
	case ".java":
		t, ok := lexJava(src)
		if !ok {
			return "", false
		}
		jp := &jparser{t: t}
		jp.skipAnnotations()
		if jp.is("package") {
			jp.i++
			name, ok := jp.dotted()
			if !ok || !jp.is(";") {
				return "", false
			}
			return name, true
		}
		return "", true
	case ".kt":
		t, ok := lexKotlin(src)
		if !ok {
			return "", false
		}
		return kotlinPackage(t)
	}
	return "", false
}

// ---------------------------------------------------------------------------------------------------------------------------
// Lexing

// unicodeEscapes translates Java unicode escapes (JLS §3.3). ok=false when an escape yields a line terminator (line numbers
// would shift) or is malformed.
func unicodeEscapes(b []byte) ([]byte, bool) {
	if !strings.Contains(string(b), `\u`) {
		return b, true
	}
	out := make([]byte, 0, len(b))
	run := 0 // contiguous raw backslashes before i
	for i := 0; i < len(b); {
		c := b[i]
		if c == '\\' && run%2 == 0 && i+1 < len(b) && b[i+1] == 'u' {
			j := i + 1
			for j < len(b) && b[j] == 'u' {
				j++
			}
			if j+4 > len(b) {
				return nil, false
			}
			v := 0
			for _, h := range b[j : j+4] {
				d := hexVal(h)
				if d < 0 {
					return nil, false
				}
				v = v*16 + d
			}
			if v == '\n' || v == '\r' {
				return nil, false
			}
			out = utf8.AppendRune(out, rune(v))
			i = j + 4
			run = 0
			continue
		}
		if c == '\\' {
			run++
		} else {
			run = 0
		}
		out = append(out, c)
		i++
	}
	return out, true
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

func identStart(r rune) bool { return r == '_' || r == '$' || unicode.IsLetter(r) }
func identPart(r rune) bool  { return identStart(r) || unicode.IsDigit(r) }

// lexer is shared by the Java and Kotlin tokenizers.
type lexer struct {
	b    []byte
	i    int
	line int
	out  []tok
}

func (l *lexer) emit(k byte, s string, line int) {
	l.out = append(l.out, tok{k: k, s: s, line: line, endLine: l.line})
}

// common lexes identifiers, numbers and punctuation at l.i; it returns false for any other byte.
func (l *lexer) common() bool {
	c := l.b[l.i]
	r, n := utf8.DecodeRune(l.b[l.i:])
	switch {
	case identStart(r):
		s := l.i
		for l.i < len(l.b) {
			r, n := utf8.DecodeRune(l.b[l.i:])
			if !identPart(r) {
				break
			}
			l.i += n
		}
		l.emit('i', string(l.b[s:l.i]), l.line)
	case c >= '0' && c <= '9' || c == '.' && l.i+1 < len(l.b) && l.b[l.i+1] >= '0' && l.b[l.i+1] <= '9':
		s := l.i
		l.i++
		for l.i < len(l.b) {
			d := l.b[l.i]
			if d >= '0' && d <= '9' || d >= 'a' && d <= 'z' || d >= 'A' && d <= 'Z' || d == '_' || d == '.' {
				l.i++
				continue
			}
			if (d == '+' || d == '-') && strings.ContainsRune("eEpP", rune(l.b[l.i-1])) {
				l.i++
				continue
			}
			break
		}
		l.emit('n', string(l.b[s:l.i]), l.line)
	case r == utf8.RuneError && n <= 1 && c >= 0x80:
		return false
	case c < 0x80:
		l.emit('p', string(c), l.line)
		l.i++
	default:
		return false // non-ASCII non-letter outside literals
	}
	return true
}

// quoted consumes a '...' or "..." literal starting at l.i (the quote); escapes skip one character; a line break is an error.
func (l *lexer) quoted(q byte) bool {
	line := l.line
	j := l.i + 1
	for j < len(l.b) {
		switch l.b[j] {
		case '\\':
			j += 2
			continue
		case '\n':
			return false
		case q:
			l.i = j + 1
			l.emit('s', string(q), line)
			return true
		}
		j++
	}
	return false
}

// lexJava tokenizes Java source (after unicode-escape translation); ok=false on unterminated comments or literals.
func lexJava(src []byte) ([]tok, bool) {
	b, ok := unicodeEscapes(src)
	if !ok {
		return nil, false
	}
	l := &lexer{b: b, line: 1}
	for l.i < len(b) {
		c := b[l.i]
		switch {
		case c == '\n':
			l.line++
			l.i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\f':
			l.i++
		case c == '/' && l.i+1 < len(b) && b[l.i+1] == '/':
			for l.i < len(b) && b[l.i] != '\n' {
				l.i++
			}
		case c == '/' && l.i+1 < len(b) && b[l.i+1] == '*':
			j := strings.Index(string(b[l.i+2:]), "*/")
			if j < 0 {
				return nil, false
			}
			l.line += strings.Count(string(b[l.i:l.i+2+j]), "\n")
			l.i += j + 4
		case c == '"' && strings.HasPrefix(string(b[l.i:]), `"""`):
			line := l.line
			j := l.i + 3
			for ; j < len(b); j++ {
				if b[j] == '\\' {
					j++
					continue
				}
				if strings.HasPrefix(string(b[j:]), `"""`) {
					break
				}
			}
			if j >= len(b) {
				return nil, false
			}
			l.line += strings.Count(string(b[l.i:j]), "\n")
			l.i = j + 3
			l.emit('s', `"""`, line)
		case c == '"' || c == '\'':
			if !l.quoted(c) {
				return nil, false
			}
		default:
			if !l.common() {
				return nil, false
			}
		}
	}
	return l.out, true
}

// lexKotlin tokenizes Kotlin source; string templates are part of their string token.
func lexKotlin(src []byte) ([]tok, bool) {
	l := &lexer{b: src, line: 1}
	if strings.HasPrefix(string(src), "#!") {
		for l.i < len(src) && src[l.i] != '\n' {
			l.i++
		}
	}
	if !l.kotlinTokens(false) {
		return nil, false
	}
	return l.out, true
}

// kotlinTokens lexes until the end of input, or (inTemplate) until the '}' closing a ${...} template, which it consumes.
func (l *lexer) kotlinTokens(inTemplate bool) bool {
	b := l.b
	depth := 0
	for l.i < len(b) {
		c := b[l.i]
		switch {
		case c == '\n':
			l.line++
			l.i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\f':
			l.i++
		case c == '/' && l.i+1 < len(b) && b[l.i+1] == '/':
			for l.i < len(b) && b[l.i] != '\n' {
				l.i++
			}
		case c == '/' && l.i+1 < len(b) && b[l.i+1] == '*':
			nest := 0
			for {
				if l.i+1 >= len(b) {
					return false
				}
				switch {
				case b[l.i] == '/' && b[l.i+1] == '*':
					nest++
					l.i += 2
				case b[l.i] == '*' && b[l.i+1] == '/':
					nest--
					l.i += 2
				default:
					if b[l.i] == '\n' {
						l.line++
					}
					l.i++
				}
				if nest == 0 {
					break
				}
			}
		case c == '`':
			j := l.i + 1
			for j < len(b) && b[j] != '`' && b[j] != '\n' {
				j++
			}
			if j >= len(b) || b[j] != '`' {
				return false
			}
			l.emit('i', string(b[l.i:j+1]), l.line)
			l.i = j + 1
		case c == '\'':
			if !l.quoted('\'') {
				return false
			}
		case c == '"':
			if !l.kotlinString() {
				return false
			}
		case inTemplate && c == '{':
			depth++
			l.emit('p', "{", l.line)
			l.i++
		case inTemplate && c == '}':
			if depth == 0 {
				l.i++
				return true
			}
			depth--
			l.emit('p', "}", l.line)
			l.i++
		default:
			if !l.common() {
				return false
			}
		}
	}
	return !inTemplate
}

// kotlinString consumes a "..." or """...""" string with $name and ${...} templates; the template tokens are discarded.
func (l *lexer) kotlinString() bool {
	b := l.b
	line := l.line
	raw := strings.HasPrefix(string(b[l.i:]), `"""`)
	if raw {
		l.i += 3
	} else {
		l.i++
	}
	for l.i < len(b) {
		c := b[l.i]
		switch {
		case raw && c == '"' && strings.HasPrefix(string(b[l.i:]), `"""`):
			j := l.i
			for j < len(b) && b[j] == '"' {
				j++
			}
			l.i = j // the closing quotes are the last three of the run
			l.emit('s', `"""`, line)
			return true
		case !raw && c == '"':
			l.i++
			l.emit('s', `"`, line)
			return true
		case !raw && c == '\\':
			l.i += 2
		case !raw && c == '\n':
			return false
		case c == '$' && l.i+1 < len(b) && b[l.i+1] == '{':
			l.i += 2
			saved := l.out
			l.out = nil
			if !l.kotlinTokens(true) {
				return false
			}
			l.out = saved
		case c == '\n':
			l.line++
			l.i++
		default:
			l.i++
		}
	}
	return false
}

// ---------------------------------------------------------------------------------------------------------------------------
// Java structure

type jparser struct {
	t     []tok
	i     int
	bad   bool
	pkg   string
	types []string
	spans []span
}

func (p *jparser) at(k int) tok {
	if p.i+k < len(p.t) {
		return p.t[p.i+k]
	}
	return tok{}
}

func (p *jparser) is(s string) bool { return p.i < len(p.t) && p.t[p.i].s == s && p.t[p.i].k != 's' }

func (p *jparser) dotted() (string, bool) {
	if p.at(0).k != 'i' {
		return "", false
	}
	var parts []string
	for {
		if p.at(0).k != 'i' {
			return "", false
		}
		parts = append(parts, p.t[p.i].s)
		p.i++
		if !p.is(".") {
			break
		}
		p.i++
		if p.is("*") {
			parts = append(parts, "*")
			p.i++
			break
		}
	}
	return strings.Join(parts, "."), true
}

// skipBalanced skips the bracketed group starting at p.i ('(', '[' or '{').
func (p *jparser) skipBalanced() {
	var stack []string
	for p.i < len(p.t) {
		x := p.t[p.i]
		p.i++
		if x.k != 'p' {
			continue
		}
		switch x.s {
		case "(":
			stack = append(stack, ")")
		case "[":
			stack = append(stack, "]")
		case "{":
			stack = append(stack, "}")
		case ")", "]", "}":
			if len(stack) == 0 || stack[len(stack)-1] != x.s {
				p.bad = true
				return
			}
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			return
		}
	}
	p.bad = true
}

// skipAnnotations skips annotations (@Name, @a.b.Name(args)), not @interface.
func (p *jparser) skipAnnotations() {
	for !p.bad && p.is("@") && !(p.at(1).k == 'i' && p.at(1).s == "interface") {
		p.i++
		if _, ok := p.dotted(); !ok {
			p.bad = true
			return
		}
		if p.is("(") {
			p.skipBalanced()
		}
	}
}

var javaModifiers = map[string]bool{"public": true, "protected": true, "private": true, "static": true, "final": true,
	"abstract": true, "native": true, "synchronized": true, "transient": true, "volatile": true, "strictfp": true,
	"default": true, "sealed": true}

// prefix skips annotations and modifiers; it returns the modifiers seen.
func (p *jparser) prefix() map[string]bool {
	mods := map[string]bool{}
	for !p.bad && p.i < len(p.t) {
		switch {
		case p.is("@") && !(p.at(1).k == 'i' && p.at(1).s == "interface"):
			p.skipAnnotations()
		case p.at(0).k == 'i' && javaModifiers[p.at(0).s]:
			mods[p.at(0).s] = true
			p.i++
		case p.is("non") && p.at(1).s == "-" && p.at(2).s == "sealed":
			p.i += 3
		default:
			return mods
		}
	}
	return mods
}

// typeKind reports the kind of type declaration at p.i ("" if none) and how many tokens the keyword takes.
func (p *jparser) typeKind() (string, int) {
	x := p.at(0)
	if x.k == 'p' && x.s == "@" && p.at(1).s == "interface" {
		return "annotation", 2
	}
	if x.k != 'i' {
		return "", 0
	}
	switch x.s {
	case "class", "interface", "enum":
		return x.s, 1
	case "record":
		if p.at(1).k == 'i' && (p.at(2).s == "(" || p.at(2).s == "<") {
			return "record", 1
		}
	}
	return "", 0
}

func (p *jparser) file() {
	for !p.bad && p.i < len(p.t) {
		switch {
		case p.is(";"):
			p.i++
		case p.is("package"):
			p.i++
			name, ok := p.dotted()
			if !ok || !p.is(";") {
				p.bad = true
				return
			}
			p.pkg = name
			p.i++
		case p.is("import"):
			for p.i < len(p.t) && !p.is(";") {
				p.i++
			}
			if !p.is(";") {
				p.bad = true
				return
			}
			p.i++
		default:
			p.prefix()
			if p.is("package") {
				continue // annotated package declaration (package-info.java)
			}
			kind, n := p.typeKind()
			if kind == "" || p.bad {
				p.bad = true
				return
			}
			p.typeDecl(kind, n, true)
		}
	}
}

// typeDecl parses a type declaration whose keyword is at p.i (n tokens).
func (p *jparser) typeDecl(kind string, n int, top bool) {
	p.i += n
	if p.at(0).k != 'i' {
		p.bad = true
		return
	}
	if top {
		p.types = append(p.types, p.t[p.i].s)
	}
	p.i++
	for !p.bad && p.i < len(p.t) {
		switch {
		case p.is("{"):
			p.body(kind)
			return
		case p.is("(") || p.is("["):
			p.skipBalanced()
		case p.is("@"):
			p.skipAnnotations()
		case p.is(";") || p.is("}") || p.is(")") || p.is("]"):
			p.bad = true
			return
		default:
			p.i++
		}
	}
	p.bad = true
}

// body parses a type body starting at '{'.
func (p *jparser) body(kind string) {
	p.i++
	if kind == "enum" {
		for !p.bad {
			if p.i >= len(p.t) {
				p.bad = true
				return
			}
			switch {
			case p.is("}"):
				p.i++
				return
			case p.is(";"):
				p.i++
			case p.is("(") || p.is("{") || p.is("["):
				p.skipBalanced()
				continue
			case p.is(")") || p.is("]"):
				p.bad = true
				return
			default:
				p.i++
				continue
			}
			break
		}
	}
	for !p.bad {
		if p.i >= len(p.t) {
			p.bad = true
			return
		}
		switch {
		case p.is(";"):
			p.i++
			continue
		case p.is("}"):
			p.i++
			return
		case p.is("{"):
			p.skipBalanced()
			continue
		}
		start := p.i
		mods := p.prefix()
		if p.bad {
			return
		}
		if p.is("{") { // static initializer
			p.skipBalanced()
			continue
		}
		if k, n := p.typeKind(); k != "" {
			if kind == "annotation" {
				p.guard(start, -1, "a nested type of an annotation type")
			}
			p.typeDecl(k, n, false)
			continue
		}
		p.member(kind, start, mods)
	}
}

// member parses a field or method declaration after its prefix.
func (p *jparser) member(kind string, start int, mods map[string]bool) {
	typeStart := p.i
	sawEq, sawParen := false, false
	eq := -1
	for {
		if p.bad {
			return
		}
		if p.i >= len(p.t) {
			p.bad = true
			return
		}
		x := p.t[p.i]
		if x.k != 'p' {
			p.i++
			continue
		}
		switch x.s {
		case "@":
			if sawEq {
				p.i++
			} else {
				p.skipAnnotations()
			}
			continue
		case "(":
			if !sawEq {
				sawParen = true
			}
			p.skipBalanced()
			continue
		case "[":
			p.skipBalanced()
			continue
		case "{":
			if sawEq {
				p.skipBalanced()
				continue
			}
			if !sawParen && !(kind == "record" && p.i == typeStart+1 && p.t[typeStart].k == 'i') {
				p.bad = true // not a method body nor a compact canonical record constructor
				return
			}
			p.skipBalanced() // method body
			if kind == "annotation" {
				p.guard(start, p.i-1, "a member of an annotation type")
			}
			return
		case "=":
			if !sawEq {
				sawEq, eq = true, p.i
			}
		case ";":
			end := p.i
			p.i++
			field := !sawParen
			switch {
			case kind == "annotation":
				p.guard(start, end, "a member of an annotation type (element default or constant)")
			case field && kind == "interface":
				p.guard(start, end, "an interface field (implicitly static final)")
			case field && mods["final"] && sawEq && (eq-1 <= typeStart || constCapable(p.t[typeStart:eq-1])):
				p.guard(start, end, "a final field that may be a compile-time constant")
			}
			return
		case "}", ")", "]":
			p.bad = true
			return
		}
		p.i++
	}
}

func (p *jparser) guard(from, to int, why string) {
	if to < 0 {
		to = from
	}
	var sb strings.Builder
	for k := from; k <= to && k < len(p.t) && sb.Len() < 60; k++ {
		if p.t[k].s == "=" || p.t[k].s == "{" {
			break
		}
		if sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(p.t[k].s)
	}
	p.spans = append(p.spans, span{from: p.t[from].line, to: p.t[to].endLine, what: why + " `" + sb.String() + "`"})
}

var constTypes = map[string]bool{"boolean": true, "byte": true, "short": true, "char": true, "int": true, "long": true,
	"float": true, "double": true, "Boolean": true, "Byte": true, "Short": true, "Character": true, "Integer": true, "Long": true,
	"Float": true, "Double": true, "String": true, "var": true}

// constCapable: the field type tokens (annotations included) may denote a type of a constant variable. Unknown → true.
func constCapable(ts []tok) bool {
	var parts []string
	for k := 0; k < len(ts); k++ {
		if ts[k].s == "@" {
			k++ // annotation name; arguments were skipped by member() only before '=', so be conservative
			for k+2 < len(ts) && ts[k+1].s == "." {
				k += 2
			}
			continue
		}
		parts = append(parts, ts[k].s)
	}
	if len(parts) == 0 {
		return true
	}
	s := strings.Join(parts, "")
	if strings.ContainsAny(s, "[<") {
		return false
	}
	if strings.ContainsAny(s, "()") {
		return true
	}
	s = strings.TrimPrefix(s, "java.lang.")
	if constTypes[s] {
		return true
	}
	for _, x := range parts {
		if x != "." && !(len(x) > 0 && identStart([]rune(x)[0])) {
			return true // not a plain (qualified) name: unknown
		}
	}
	return false
}

func javaSpans(src []byte) ([]span, bool) {
	t, ok := lexJava(src)
	if !ok {
		return nil, false
	}
	p := &jparser{t: t}
	p.file()
	if p.bad {
		return nil, false
	}
	return p.spans, true
}

// ---------------------------------------------------------------------------------------------------------------------------
// Kotlin structure

type kparser struct {
	t     []tok
	depth []int // nesting depth of each token (a closer has the depth outside its group)
	match []int // index of the matching closer for openers, -1 otherwise
}

func newKParser(t []tok) (*kparser, bool) {
	kp := &kparser{t: t, depth: make([]int, len(t)), match: make([]int, len(t))}
	var stack []int
	closer := map[string]string{")": "(", "]": "[", "}": "{"}
	for i, x := range t {
		kp.match[i] = -1
		kp.depth[i] = len(stack)
		if x.k != 'p' {
			continue
		}
		switch x.s {
		case "(", "[", "{":
			stack = append(stack, i)
		case ")", "]", "}":
			if len(stack) == 0 || t[stack[len(stack)-1]].s != closer[x.s] {
				return nil, false
			}
			kp.match[stack[len(stack)-1]] = i
			stack = stack[:len(stack)-1]
			kp.depth[i] = len(stack)
		}
	}
	if len(stack) != 0 {
		return nil, false
	}
	return kp, true
}

var kotlinModifiers = map[string]bool{"public": true, "private": true, "internal": true, "protected": true, "override": true,
	"actual": true, "expect": true, "open": true, "final": true, "abstract": true, "external": true, "inline": true,
	"lateinit": true, "data": true, "value": true, "sealed": true, "inner": true, "enum": true, "annotation": true, "const": true,
	"operator": true, "infix": true, "suspend": true, "tailrec": true, "companion": true, "vararg": true, "noinline": true,
	"crossinline": true, "reified": true}

var kotlinStarters = map[string]bool{"val": true, "var": true, "fun": true, "class": true, "interface": true, "object": true,
	"typealias": true, "init": true, "constructor": true, "import": true, "package": true, "get": false}

func (kp *kparser) lineFirst(i int) bool { return i == 0 || kp.t[i-1].endLine < kp.t[i].line }

// declStart walks back from i over modifier keywords at the same depth.
func (kp *kparser) declStart(i int) int {
	for i > 0 && kp.t[i-1].k == 'i' && kotlinModifiers[kp.t[i-1].s] && kp.depth[i-1] == kp.depth[i] {
		i--
	}
	return i
}

func kotlinSpans(src []byte) ([]span, bool) {
	t, ok := lexKotlin(src)
	if !ok {
		return nil, false
	}
	kp, ok := newKParser(t)
	if !ok {
		return nil, false
	}
	var spans []span
	for i, x := range t {
		if x.k != 'i' {
			continue
		}
		switch x.s {
		case "const":
			j := i + 1
			for j < len(t) && t[j].k == 'i' && kotlinModifiers[t[j].s] {
				j++
			}
			if j >= len(t) || t[j].s != "val" {
				continue // an identifier named const
			}
			d := kp.depth[i]
			k := j + 1
			for ; k < len(t); k++ {
				if kp.depth[k] < d {
					break
				}
				if t[k].s == ";" && t[k].k == 'p' && kp.depth[k] == d {
					k++
					break
				}
				if k > j+1 && kp.depth[k] == d && kp.lineFirst(k) &&
					(t[k].s == "@" || t[k].k == 'i' && (kotlinModifiers[t[k].s] || kotlinStarters[t[k].s])) {
					break
				}
			}
			s := kp.declStart(i)
			name := ""
			if j+1 < len(t) {
				name = t[j+1].s
			}
			spans = append(spans, span{from: t[s].line, to: t[k-1].endLine, what: "Kotlin const val " + name})
		case "annotation":
			j := i + 1
			for j < len(t) && t[j].k == 'i' && kotlinModifiers[t[j].s] && t[j].s != "class" {
				j++
			}
			if j+1 >= len(t) || t[j].s != "class" || t[j+1].k != 'i' {
				continue
			}
			end := j + 1
			k := j + 2
			for k < len(t) && t[k].k == 'i' && (kotlinModifiers[t[k].s] || t[k].s == "constructor") {
				k++
			}
			if k < len(t) && t[k].s == "(" && kp.match[k] > 0 {
				end = kp.match[k]
				k = end + 1
			}
			if k < len(t) && t[k].s == "{" && kp.match[k] > 0 {
				end = kp.match[k]
			}
			s := kp.declStart(i)
			spans = append(spans, span{from: t[s].line, to: t[end].endLine, what: "Kotlin annotation class " + t[j+1].s + " (element defaults)"})
		}
	}
	return spans, true
}

// kotlinPackage reads the package header after file annotations.
func kotlinPackage(t []tok) (string, bool) {
	i := 0
	for i < len(t) && t[i].s == "@" {
		if i+2 < len(t) && t[i+1].s == "file" && t[i+2].s == ":" {
			i += 3
		} else {
			i++
		}
		if i < len(t) && t[i].s == "[" {
			if !skipGroup(t, &i) {
				return "", false
			}
			continue
		}
		for i < len(t) && t[i].k == 'i' {
			i++
			if i < len(t) && t[i].s == "." {
				i++
				continue
			}
			break
		}
		if i < len(t) && t[i].s == "(" {
			if !skipGroup(t, &i) {
				return "", false
			}
		}
	}
	if i >= len(t) || t[i].s != "package" || t[i].k != 'i' {
		return "", true
	}
	i++
	var parts []string
	for i < len(t) && t[i].k == 'i' {
		parts = append(parts, strings.Trim(t[i].s, "`"))
		i++
		if i < len(t) && t[i].s == "." && t[i].line == t[i-1].line {
			i++
			continue
		}
		break
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "."), true
}

func skipGroup(t []tok, i *int) bool {
	n := 0
	for ; *i < len(t); *i++ {
		switch t[*i].s {
		case "(", "[", "{":
			if t[*i].k == 'p' {
				n++
			}
		case ")", "]", "}":
			if t[*i].k == 'p' {
				n--
			}
		}
		if n == 0 {
			*i++
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------------------------------------------------------
// Line diff

type hunk struct{ b0, b1, h0, h1 int } // base lines [b0,b1), head lines [h0,h1), 0-based

func splitLines(b []byte) []string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return strings.Split(s, "\n")
}

// diffHunks computes the changed regions between a and b (Myers' O(ND) algorithm after trimming the common prefix and suffix).
// When the edit distance is very large the middle is one hunk (wider, never narrower).
func diffHunks(a, b []string) []hunk {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	a2, b2 := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if len(a2) == 0 && len(b2) == 0 {
		return nil
	}
	whole := []hunk{{pre, pre + len(a2), pre, pre + len(b2)}}
	if len(a2) == 0 || len(b2) == 0 {
		return whole
	}
	ids := map[string]int{}
	id := func(s string) int {
		if v, ok := ids[s]; ok {
			return v
		}
		ids[s] = len(ids)
		return len(ids) - 1
	}
	x := make([]int, len(a2))
	y := make([]int, len(b2))
	for i, s := range a2 {
		x[i] = id(s)
	}
	for i, s := range b2 {
		y[i] = id(s)
	}
	n, m := len(x), len(y)
	maxD := n + m
	if maxD > 1000 {
		maxD = 1000
	}
	off := maxD + 1
	v := make([]int, 2*maxD+3)
	var trace [][]int
	found := -1
	for d := 0; d <= maxD && found < 0; d++ {
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			var i int
			if k == -d || k != d && v[off+k-1] < v[off+k+1] {
				i = v[off+k+1]
			} else {
				i = v[off+k-1] + 1
			}
			j := i - k
			for i < n && j < m && x[i] == y[j] {
				i++
				j++
			}
			v[off+k] = i
			if i >= n && j >= m {
				found = d
				break
			}
		}
	}
	if found < 0 {
		return whole
	}
	// Backtrack: mark matched (diagonal) pairs.
	type pair struct{ i, j int }
	var diag []pair
	i, j := n, m
	for d := found; d > 0; d-- {
		vv := trace[d]
		k := i - j
		var pk int
		if k == -d || k != d && vv[off+k-1] < vv[off+k+1] {
			pk = k + 1
		} else {
			pk = k - 1
		}
		pi := vv[off+pk]
		pj := pi - pk
		mi := pi + 1 // a deletion from a (move right)
		if pk == k+1 {
			mi = pi // an insertion from b (move down)
		}
		for i > mi {
			diag = append(diag, pair{i - 1, j - 1})
			i--
			j--
		}
		i, j = pi, pj
	}
	for i > 0 && j > 0 {
		diag = append(diag, pair{i - 1, j - 1})
		i--
		j--
	}
	// diag is in reverse order; walk forward collecting gaps.
	var out []hunk
	ci, cj := 0, 0
	for k := len(diag) - 1; k >= -1; k-- {
		ni, nj := n, m
		if k >= 0 {
			ni, nj = diag[k].i, diag[k].j
		}
		if ni > ci || nj > cj {
			out = append(out, hunk{pre + ci, pre + ni, pre + cj, pre + nj})
		}
		ci, cj = ni+1, nj+1
	}
	return out
}

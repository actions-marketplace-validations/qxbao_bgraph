package evalkit

import (
	"bytes"
	"path"
	"strconv"
	"strings"
)

// Ruby mutation generator for T2: a small lexer (comments, =begin/=end blocks, heredoc bodies, strings with interpolation, percent
// literals, regexps and symbols are skipped as units) finds one-token flips in non-test Ruby code: comparison, boolean and +/- operator
// swaps, true/false, `and`/`or`, `if`/`unless`, integer literals +-1, simple string literal changes and `return x` -> `return nil`. The
// lexer is conservative, not exact: a mutant it gets wrong is at worst a different one-line change of the same file (still a valid T2
// change); a mutant that breaks the syntax is checked with `ruby -c` by the oracle.

type rbTokKind int

const (
	rbIdent rbTokKind = iota // identifiers, keywords, constants, @ivars, $globals
	rbNum
	rbStr // string, symbol, regexp, percent literal, heredoc start, char literal
	rbOp
)

type rbTok struct {
	kind       rbTokKind
	start, end int
	line       int
	sp         bool // whitespace before the token
	simpleStr  bool // a one-line '...' or "..." literal without interpolation or escapes
}

func (t rbTok) text(src []byte) string { return string(src[t.start:t.end]) }

var rbOps3 = []string{"**=", "<=>", "===", "...", "<<=", ">>=", "&&=", "||="}
var rbOps2 = []string{"**", "==", "!=", ">=", "<=", "&&", "||", "<<", ">>", "+=", "-=", "*=", "/=", "%=", "|=", "&=", "^=", "=~", "!~",
	"..", "::", "->", "=>", "&."}

// rbKeywords are the reserved words that do not end an operand.
var rbKeywords = map[string]bool{"alias": true, "and": true, "begin": true, "break": true, "case": true, "class": true, "def": true,
	"defined?": true, "do": true, "else": true, "elsif": true, "ensure": true, "for": true, "if": true, "in": true, "module": true,
	"next": true, "not": true, "or": true, "redo": true, "rescue": true, "retry": true, "return": true, "then": true, "undef": true,
	"unless": true, "until": true, "when": true, "while": true, "yield": true, "puts": true, "p": true, "raise": true}

// rbOperandEnd reports whether a token ends an operand (so that a following / is division, % is modulo, << is a shift).
func rbOperandEnd(src []byte, t *rbTok) bool {
	if t == nil {
		return false
	}
	switch t.kind {
	case rbNum, rbStr:
		return true
	case rbIdent:
		return !rbKeywords[t.text(src)]
	}
	s := t.text(src)
	return s == ")" || s == "]" || s == "}"
}

func rbCloser(c byte) byte {
	switch c {
	case '(':
		return ')'
	case '[':
		return ']'
	case '{':
		return '}'
	case '<':
		return '>'
	}
	return c
}

type rbLexer struct {
	src      []byte
	pos      int
	line     int
	toks     []rbTok
	heredocs []rbHeredoc // pending heredoc bodies (start after the current line)
}

type rbHeredoc struct {
	id     string
	indent bool // <<- or <<~: the terminator may be indented
}

// lexRuby returns the significant tokens of src.
func lexRuby(src []byte) []rbTok {
	lx := &rbLexer{src: src, line: 1}
	lx.run()
	return lx.toks
}

func (lx *rbLexer) prev() *rbTok {
	if len(lx.toks) == 0 {
		return nil
	}
	return &lx.toks[len(lx.toks)-1]
}

func (lx *rbLexer) emit(kind rbTokKind, start int, sp bool) {
	lx.toks = append(lx.toks, rbTok{kind: kind, start: start, end: lx.pos, line: lx.line, sp: sp})
}

// lineStart reports whether pos is at the start of a line.
func (lx *rbLexer) lineStart() bool { return lx.pos == 0 || lx.src[lx.pos-1] == '\n' }

func (lx *rbLexer) restOfLineIs(word string) bool {
	return bytes.HasPrefix(lx.src[lx.pos:], []byte(word)) &&
		(lx.pos+len(word) == len(lx.src) || !isIdentChar(lx.src[lx.pos+len(word)]))
}

func (lx *rbLexer) skipToEOL() {
	for lx.pos < len(lx.src) && lx.src[lx.pos] != '\n' {
		lx.pos++
	}
}

// newline consumes a '\n' and any heredoc bodies that start after it.
func (lx *rbLexer) newline() {
	lx.pos++
	lx.line++
	for _, h := range lx.heredocs {
		for lx.pos < len(lx.src) {
			s := lx.pos
			lx.skipToEOL()
			l := strings.TrimRight(string(lx.src[s:lx.pos]), "\r")
			if lx.pos < len(lx.src) {
				lx.pos++
				lx.line++
			}
			if h.indent {
				l = strings.TrimLeft(l, " \t")
			}
			if l == h.id {
				break
			}
		}
	}
	lx.heredocs = nil
}

func (lx *rbLexer) run() {
	src := lx.src
	n := len(src)
	for lx.pos < n {
		if lx.lineStart() {
			if lx.restOfLineIs("=begin") {
				for lx.pos < n {
					lx.skipToEOL()
					if lx.pos < n {
						lx.newline()
					}
					if lx.restOfLineIs("=end") {
						lx.skipToEOL()
						break
					}
				}
				continue
			}
			if lx.restOfLineIs("__END__") {
				return
			}
		}
		c := src[lx.pos]
		sp := lx.pos > 0 && (src[lx.pos-1] == ' ' || src[lx.pos-1] == '\t')
		switch {
		case c == '\n':
			lx.newline()
		case c == ' ' || c == '\t' || c == '\r' || c == '\f':
			lx.pos++
		case c == '\\' && lx.pos+1 < n && (src[lx.pos+1] == '\n' || src[lx.pos+1] == '\r'):
			lx.pos++
			for lx.pos < n && src[lx.pos] == '\r' {
				lx.pos++
			}
			if lx.pos < n && src[lx.pos] == '\n' {
				lx.newline()
			}
		case c == '#':
			lx.skipToEOL()
		case c == '"' || c == '`':
			s := lx.pos
			line := lx.line
			simple := lx.quoted(c, c, true)
			lx.toks = append(lx.toks, rbTok{kind: rbStr, start: s, end: lx.pos, line: line, sp: sp, simpleStr: simple})
		case c == '\'':
			s := lx.pos
			line := lx.line
			simple := lx.quoted('\'', '\'', false)
			lx.toks = append(lx.toks, rbTok{kind: rbStr, start: s, end: lx.pos, line: line, sp: sp, simpleStr: simple})
		case c == '@' || c == '$':
			s := lx.pos
			lx.pos++
			if c == '@' && lx.pos < n && src[lx.pos] == '@' {
				lx.pos++
			}
			if c == '$' && lx.pos < n && !isIdentStart(src[lx.pos]) {
				lx.pos++ // $~, $1, $!, ...
			}
			for lx.pos < n && isIdentChar(src[lx.pos]) {
				lx.pos++
			}
			lx.emit(rbIdent, s, sp)
		case isIdentStart(c) || c >= 0x80:
			s := lx.pos
			for lx.pos < n && (isIdentChar(src[lx.pos]) || src[lx.pos] >= 0x80) {
				lx.pos++
			}
			if lx.pos < n && (src[lx.pos] == '?' || src[lx.pos] == '!') && (lx.pos+1 >= n || src[lx.pos+1] != '=' || lx.pos+2 < n && src[lx.pos+2] == '=') {
				lx.pos++
			}
			lx.emit(rbIdent, s, sp)
		case c >= '0' && c <= '9':
			s := lx.pos
			lx.number()
			lx.emit(rbNum, s, sp)
		case c == ':' && lx.pos+1 < n && src[lx.pos+1] != ':' && (lx.prev() == nil || !lx.colonEndsKey()):
			s := lx.pos
			lx.pos++
			d := src[lx.pos]
			switch {
			case d == '"' || d == '\'':
				lx.quoted(d, d, d == '"')
			case isIdentStart(d) || d == '@' || d == '$' || d >= 0x80:
				lx.pos++
				for lx.pos < n && (isIdentChar(src[lx.pos]) || src[lx.pos] >= 0x80) {
					lx.pos++
				}
				if lx.pos < n && (src[lx.pos] == '?' || src[lx.pos] == '!' || src[lx.pos] == '=') && (lx.pos+1 >= n || src[lx.pos+1] != '=' && src[lx.pos+1] != '>') {
					lx.pos++
				}
			case strings.IndexByte("+-*/<=>!~%&|^[", d) >= 0 && !(d == '=' && lx.pos+1 < n && src[lx.pos+1] == '>'):
				for lx.pos < n && strings.IndexByte("+-*/<=>!~%&|^[]@", src[lx.pos]) >= 0 {
					lx.pos++
				}
			default:
				lx.emit(rbOp, s, sp) // a lone ':' (ternary, hash key separator)
				continue
			}
			lx.emit(rbStr, s, sp)
		case c == '%' && lx.percentStart(sp):
			s := lx.pos
			line := lx.line
			lx.pos++
			if strings.IndexByte("qQwWiIrsx", src[lx.pos]) >= 0 && lx.pos+1 < n && !isIdentChar(src[lx.pos+1]) && src[lx.pos+1] != ' ' {
				lx.pos++
			}
			open := src[lx.pos]
			lx.quoted(open, rbCloser(open), true)
			if open == '/' || src[s+1] == 'r' {
				for lx.pos < n && isIdentChar(src[lx.pos]) {
					lx.pos++
				}
			}
			lx.toks = append(lx.toks, rbTok{kind: rbStr, start: s, end: lx.pos, line: line, sp: sp})
		case c == '/' && lx.regexStart(sp):
			s := lx.pos
			line := lx.line
			lx.quoted('/', '/', true)
			for lx.pos < n && isIdentChar(src[lx.pos]) {
				lx.pos++
			}
			lx.toks = append(lx.toks, rbTok{kind: rbStr, start: s, end: lx.pos, line: line, sp: sp})
		case c == '<' && lx.heredocStart(sp):
			// handled (token emitted, body queued)
		case c == '?' && lx.charLiteral():
			lx.toks = append(lx.toks, rbTok{kind: rbStr, start: lx.pos - 2, end: lx.pos, line: lx.line, sp: sp})
		default:
			s := lx.pos
			l := 1
			for _, o := range rbOps3 {
				if bytes.HasPrefix(src[lx.pos:], []byte(o)) {
					l = 3
					break
				}
			}
			if l == 1 {
				for _, o := range rbOps2 {
					if bytes.HasPrefix(src[lx.pos:], []byte(o)) {
						l = 2
						break
					}
				}
			}
			lx.pos += l
			lx.emit(rbOp, s, sp)
		}
	}
}

// colonEndsKey reports whether the ':' at pos is the end of a `key:` label or a ternary separator rather than a symbol start.
func (lx *rbLexer) colonEndsKey() bool {
	p := lx.prev()
	if p.end != lx.pos {
		return false // `a ? b :c` is rare; a spaced ':' followed by a name is a symbol
	}
	return p.kind == rbIdent || p.kind == rbStr
}

func (lx *rbLexer) number() {
	src, n := lx.src, len(lx.src)
	lx.pos++
	for lx.pos < n {
		c := src[lx.pos]
		switch {
		case isIdentChar(c):
			lx.pos++
		case c == '.' && lx.pos+1 < n && src[lx.pos+1] >= '0' && src[lx.pos+1] <= '9':
			lx.pos++
		case (c == '+' || c == '-') && (src[lx.pos-1] == 'e' || src[lx.pos-1] == 'E') && !bytes.ContainsAny(src[lx.pos-2:lx.pos], "xX"):
			lx.pos++
		default:
			return
		}
	}
}

// quoted consumes a delimited literal whose opening delimiter is at pos; interp: #{...} is code (skipped with nesting). Returns whether the
// literal is simple: on one line, no escape, no interpolation.
func (lx *rbLexer) quoted(open, close byte, interp bool) bool {
	src, n := lx.src, len(lx.src)
	lx.pos++
	depth := 1
	simple := open == '"' || open == '\''
	for lx.pos < n {
		c := src[lx.pos]
		switch {
		case c == '\\':
			simple = false
			lx.pos += 2
			continue
		case c == '\n':
			simple = false
			lx.line++
		case interp && c == '#' && lx.pos+1 < n && src[lx.pos+1] == '{':
			simple = false
			lx.pos += 2
			lx.interpolation()
			continue
		case c == close && open != close:
			depth--
			if depth == 0 {
				lx.pos++
				return simple
			}
		case c == open && open != close:
			depth++
		case c == close:
			lx.pos++
			return simple
		}
		lx.pos++
	}
	return false
}

// interpolation skips the code of #{...} (pos just after '{') up to the matching '}', honouring nested strings.
func (lx *rbLexer) interpolation() {
	src, n := lx.src, len(lx.src)
	depth := 1
	for lx.pos < n && depth > 0 {
		switch c := src[lx.pos]; c {
		case '{':
			depth++
		case '}':
			depth--
		case '"', '\'', '`':
			lx.quoted(c, c, c != '\'')
			continue
		case '\n':
			lx.line++
		}
		lx.pos++
	}
}

func (lx *rbLexer) percentStart(sp bool) bool {
	src, n := lx.src, len(lx.src)
	if lx.pos+1 >= n {
		return false
	}
	d := src[lx.pos+1]
	p := lx.prev()
	if rbOperandEnd(src, p) && !(p.kind == rbIdent && sp && d != ' ' && d != '=') {
		return false
	}
	if strings.IndexByte("qQwWiIrsx", d) >= 0 {
		return lx.pos+2 < n && !isIdentChar(src[lx.pos+2]) && src[lx.pos+2] != ' '
	}
	return strings.IndexByte("([{<|!/^", d) >= 0
}

func (lx *rbLexer) regexStart(sp bool) bool {
	p := lx.prev()
	if p == nil {
		return true
	}
	if !rbOperandEnd(lx.src, p) {
		return true
	}
	// `split /,/`: an identifier, a space, then no space
	return p.kind == rbIdent && sp && lx.pos+1 < len(lx.src) && lx.src[lx.pos+1] != ' ' && lx.src[lx.pos+1] != '='
}

// heredocStart recognises <<ID, <<-ID, <<~ID (ID possibly quoted) and queues its body.
func (lx *rbLexer) heredocStart(sp bool) bool {
	src, n := lx.src, len(lx.src)
	i := lx.pos + 2
	if i >= n || src[lx.pos+1] != '<' {
		return false
	}
	indent := false
	if src[i] == '-' || src[i] == '~' {
		indent = true
		i++
	} else if p := lx.prev(); rbOperandEnd(src, p) && !(p.kind == rbIdent && sp) {
		return false // a << b
	}
	if i >= n {
		return false
	}
	var id string
	switch q := src[i]; {
	case q == '"' || q == '\'' || q == '`':
		j := bytes.IndexByte(src[i+1:], q)
		if j < 0 {
			return false
		}
		id = string(src[i+1 : i+1+j])
		i += j + 2
	case q >= 'A' && q <= 'Z' || q == '_' || indent && isIdentStart(q):
		j := i
		for j < n && isIdentChar(src[j]) {
			j++
		}
		id = string(src[i:j])
		i = j
	default:
		return false
	}
	s := lx.pos
	lx.pos = i
	lx.emit(rbStr, s, sp)
	lx.heredocs = append(lx.heredocs, rbHeredoc{id: id, indent: indent})
	return true
}

// charLiteral recognises ?a (pos at '?') when no operand precedes; on success pos is after the literal.
func (lx *rbLexer) charLiteral() bool {
	src, n := lx.src, len(lx.src)
	if rbOperandEnd(src, lx.prev()) || lx.pos+1 >= n {
		return false
	}
	c := src[lx.pos+1]
	if c == ' ' || c == '\n' || c == '\t' || c == '\\' {
		return false
	}
	if lx.pos+2 < n && isIdentChar(src[lx.pos+2]) && isIdentChar(c) {
		return false
	}
	lx.pos += 2
	return true
}

// rbSkipFirst are the line heads whose operators are not mutated (loading, definitions, declarations).
var rbSkipFirst = map[string]bool{"require": true, "require_relative": true, "autoload": true, "load": true, "class": true, "module": true,
	"def": true, "include": true, "extend": true, "prepend": true, "attr_reader": true, "attr_writer": true, "attr_accessor": true,
	"alias": true, "alias_method": true, "private": true, "protected": true, "public": true, "private_constant": true,
	"module_function": true, "using": true, "gem": true}

// rbScan returns the mutation candidates of one Ruby source file.
func rbScan(src []byte) []rustCand {
	toks := lexRuby(src)
	var cands []rustCand
	add := func(s, e int, repl, op string) { cands = append(cands, rustCand{Start: s, End: e, Repl: repl, Op: op}) }
	txt := func(k int) string {
		if k < 0 || k >= len(toks) {
			return ""
		}
		return toks[k].text(src)
	}
	for ls := 0; ls < len(toks); {
		le := ls
		for le < len(toks) && toks[le].line == toks[ls].line {
			le++
		}
		if !rbSkipFirst[txt(ls)] {
			rbScanLine(src, toks, ls, le, txt, add)
		}
		ls = le
	}
	return cands
}

func rbScanLine(src []byte, toks []rbTok, ls, le int, txt func(int) string, add func(s, e int, repl, op string)) {
	afterDot := func(k int) bool {
		p := txt(k - 1)
		return k > ls && (p == "." || p == "&." || p == "::" || p == "def")
	}
	isKey := func(k int) bool { // `if:` label
		return k+1 < le && toks[k+1].start == toks[k].end && txt(k+1) == ":"
	}
	for k := ls; k < le; k++ {
		t := toks[k]
		s := t.text(src)
		switch t.kind {
		case rbIdent:
			if afterDot(k) || isKey(k) {
				continue
			}
			switch s {
			case "true":
				add(t.start, t.end, "false", "true-false")
			case "false":
				add(t.start, t.end, "true", "false-true")
			case "and":
				add(t.start, t.end, "or", "and-or")
			case "or":
				add(t.start, t.end, "and", "or-and")
			case "if":
				add(t.start, t.end, "unless", "if-unless")
			case "unless":
				add(t.start, t.end, "if", "unless-if")
			case "return":
				rbReturnNil(src, toks, k, le, add)
			}
		case rbNum:
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil || txt(k-1) == "." {
				continue
			}
			switch v {
			case 0:
				add(t.start, t.end, "1", "int-inc")
			case 1:
				add(t.start, t.end, "0", "int-dec")
			default:
				add(t.start, t.end, strconv.FormatInt(v+1, 10), "int-inc")
			}
		case rbStr:
			if !t.simpleStr || isKey(k) || t.end-t.start > 42 {
				continue
			}
			body := s[1 : len(s)-1]
			add(t.start, t.end, s[:1]+body+"_mut"+s[len(s)-1:], "str-change")
		case rbOp:
			operand := k > ls && rbOperandEnd(src, &toks[k-1])
			switch s {
			case "==":
				add(t.start, t.end, "!=", "eq-ne")
			case "!=":
				add(t.start, t.end, "==", "ne-eq")
			case "<":
				if operand {
					add(t.start, t.end, ">=", "lt-ge")
				}
			case ">":
				if operand {
					add(t.start, t.end, "<=", "gt-le")
				}
			case "<=":
				add(t.start, t.end, ">", "le-gt")
			case ">=":
				add(t.start, t.end, "<", "ge-lt")
			case "&&":
				add(t.start, t.end, "||", "and-or")
			case "||":
				add(t.start, t.end, "&&", "or-and")
			case "+", "-":
				if operand && t.sp && t.end < len(src) && (src[t.end] == ' ') && k+1 < le {
					if s == "+" {
						add(t.start, t.end, "-", "add-sub")
					} else {
						add(t.start, t.end, "+", "sub-add")
					}
				}
			}
		}
	}
}

// rbReturnNil adds `return x` -> `return nil` when x is a simple expression (names, literals, dots) up to the end of the line or a modifier.
func rbReturnNil(src []byte, toks []rbTok, k, le int, add func(s, e int, repl, op string)) {
	end := k + 1
	for end < le {
		w := toks[end].text(src)
		if w == "if" || w == "unless" {
			break
		}
		switch toks[end].kind {
		case rbNum, rbStr:
		case rbIdent:
			if rbKeywords[w] {
				return
			}
		default:
			if w != "." && w != "::" {
				return
			}
		}
		end++
	}
	if end == k+1 || (end == k+2 && toks[k+1].text(src) == "nil") {
		return
	}
	add(toks[k+1].start, toks[end-1].end, "nil", "return-nil")
}

// rbCandidateFile reports whether a repo path is non-test Ruby source that T2 may mutate: a .rb file below one of the prefixes (default
// lib/), outside test directories.
func rbCandidateFile(f string, prefixes []string) bool {
	if !strings.HasSuffix(f, ".rb") {
		return false
	}
	if len(prefixes) == 0 {
		prefixes = []string{"lib/"}
	}
	ok := false
	for _, p := range prefixes {
		ok = ok || strings.HasPrefix(f, p)
	}
	if !ok {
		return false
	}
	base := path.Base(f)
	if strings.HasSuffix(base, "_spec.rb") || strings.HasSuffix(base, "_test.rb") {
		return false
	}
	for _, seg := range strings.Split(path.Dir(f), "/") {
		switch seg {
		case "test", "tests", "spec", "fixtures", "vendor", "examples", "benchmarks":
			return false
		}
	}
	return true
}

// GenRubyMutants deterministically picks up to n Ruby mutants spread over the files (see spreadMutants).
func GenRubyMutants(files []RustMutantSource, seed, idPrefix string, n int) []T2Mutant {
	cands := map[string][]rustCand{}
	for _, f := range files {
		cands[f.Path] = rbScan(f.Data)
	}
	return spreadMutants(files, cands, nil, seed, idPrefix, n)
}

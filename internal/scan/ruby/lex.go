package ruby

import (
	"strings"
)

// tokKind is the kind of a lexical token.
type tokKind uint8

const (
	tNL    tokKind = iota + 1 // a newline in code (a possible statement end)
	tIdent                    // identifier, keyword, constant or method name (with a trailing ? or !)
	tIVar                     // @x, @@x
	tGVar                     // $x, $:, $LOAD_PATH...
	tSym                      // :name, :+ (aux = -1) or :"..." / :'...' (aux = the string)
	tStr                      // a string-like literal: aux indexes lexed.strs
	tNum                      // a number
	tOp                       // punctuation or operator
)

// token is one lexical token. start/end are byte offsets into the source.
type token struct {
	kind       tokKind
	sp         bool // white space (or a line continuation) right before the token
	start, end int32
	line       int32
	aux        int32 // tStr / quoted tSym: index into lexed.strs
}

// strKind is the syntactic form of a string-like literal.
type strKind uint8

const (
	sSingle  strKind = iota + 1 // '...', %q
	sDouble                     // "...", %Q, %()
	sXStr                       // `...`, %x
	sWords                      // %w, %i (no interpolation)
	sIWords                     // %W, %I
	sRegex                      // /.../, %r
	sHeredoc                    // <<ID, <<-ID, <<~ID
	sChar                       // ?a
	sSymStr                     // %s, :"...", :'...'
)

// strPart is one piece of a string literal: static text, or the tokens of an interpolation #{...}.
type strPart struct {
	lit    string
	code   []token
	isCode bool
}

type strInfo struct {
	kind  strKind
	parts []strPart
}

// static returns the literal's text when it has no interpolation.
func (s *strInfo) static() (string, bool) {
	var b strings.Builder
	for _, p := range s.parts {
		if p.isCode {
			return "", false
		}
		b.WriteString(p.lit)
	}
	return b.String(), true
}

// lexed is the result of lexing one file.
type lexed struct {
	toks []token
	strs []strInfo
}

type heredoc struct {
	id      string
	squiggy bool // <<- or <<~: the terminator may be indented
	interp  bool
	str     int32 // index into strs
}

const maxDepth = 64

type lexer struct {
	src     []byte
	pos     int
	end     int // lexing stops here (the end of a heredoc body while its interpolations are lexed)
	line    int32
	strs    []strInfo
	pending []heredoc
	locals  map[string]bool
	err     string
	depth   int
	// ambiguous is set when the last regex / percent literal / heredoc decision was a heuristic (an identifier followed by a space).
	ambiguous bool
}

// lex lexes a Ruby source. A non-empty reason means the token stream cannot be trusted.
func lex(src []byte) (*lexed, string) {
	lx := &lexer{src: src, end: len(src), line: 1, locals: map[string]bool{}}
	if len(src) >= 3 && src[0] == 0xEF && src[1] == 0xBB && src[2] == 0xBF {
		lx.pos = 3
	}
	toks := lx.code(0)
	if lx.err == "" && len(lx.pending) > 0 {
		lx.fail("unterminated heredoc <<" + lx.pending[0].id)
	}
	if lx.err != "" {
		return nil, lx.err
	}
	return &lexed{toks: toks, strs: lx.strs}, ""
}

func (lx *lexer) fail(why string) {
	if lx.err == "" {
		lx.err = why
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentChar(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v' }

func (lx *lexer) at(i int) byte {
	if i >= 0 && i < lx.end {
		return lx.src[i]
	}
	return 0
}

// valueKeywords: after these keywords an expression starts (a / is a regex, << a heredoc, % a literal).
var valueKeywords = map[string]bool{
	"and": true, "begin": true, "case": true, "do": true, "else": true, "elsif": true, "ensure": true, "for": true, "if": true,
	"in": true, "not": true, "or": true, "rescue": true, "return": true, "then": true, "unless": true, "until": true, "when": true,
	"while": true, "yield": true, "break": true, "next": true, "defined?": true,
}

// valueEnd: after these keywords the expression is complete (a / is a division).
var valueEnd = map[string]bool{"end": true, "self": true, "true": true, "false": true, "nil": true, "__FILE__": true,
	"__LINE__": true, "__dir__": true, "__method__": true, "__ENCODING__": true, "redo": true, "retry": true}

// state of the token before the current position.
type prevState uint8

const (
	pValue   prevState = iota + 1 // an operand ended: binary operator expected
	pBeg                          // an expression may start
	pIdentSp                      // a (non-local) identifier followed by white space: a command argument may start
	pDef                          // right after `def`, `.` or `::`: a method name (possibly an operator) follows
)

func (lx *lexer) state(toks []token, sp bool) prevState {
	if len(toks) == 0 {
		return pBeg
	}
	t := toks[len(toks)-1]
	switch t.kind {
	case tNL:
		return pBeg
	case tNum, tStr, tSym, tIVar, tGVar:
		return pValue
	case tOp:
		txt := string(lx.src[t.start:t.end])
		switch txt {
		case ")", "]", "}":
			return pValue
		case ".", "&.":
			return pDef
		case "::":
			return pDef
		}
		return pBeg
	case tIdent:
		txt := string(lx.src[t.start:t.end])
		if txt == "def" {
			return pDef
		}
		if valueKeywords[txt] {
			return pBeg
		}
		if valueEnd[txt] {
			return pValue
		}
		// After `.meth` the identifier is a method name (never a local variable).
		afterDot := len(toks) >= 2 && toks[len(toks)-2].kind == tOp && (lx.opIs(toks[len(toks)-2], ".") || lx.opIs(toks[len(toks)-2], "&."))
		if lower := txt[0] >= 'a' && txt[0] <= 'z' || txt[0] == '_'; sp && lower && (afterDot || !lx.locals[txt]) {
			return pIdentSp
		}
		return pValue
	}
	return pBeg
}

func (lx *lexer) opIs(t token, s string) bool {
	return t.kind == tOp && string(lx.src[t.start:t.end]) == s
}

// threeOps / twoOps: multi-character operators, longest first.
var threeOps = []string{"**=", "<=>", "===", "...", "<<=", ">>=", "&&=", "||=", "&.:"}
var twoOps = []string{"**", "==", "!=", ">=", "<=", "&&", "||", "<<", ">>", "+=", "-=", "*=", "/=", "%=", "|=", "&=", "^=", "=~",
	"!~", "..", "::", "->", "=>", "&."}

func (lx *lexer) emit(toks []token, kind tokKind, start int, sp bool, line int32, aux int32) []token {
	return append(toks, token{kind: kind, sp: sp, start: int32(start), end: int32(lx.pos), line: line, aux: aux})
}

func (lx *lexer) newStr(kind strKind) int32 {
	lx.strs = append(lx.strs, strInfo{kind: kind})
	return int32(len(lx.strs) - 1)
}

// atLineStart: pos is at column 0.
func (lx *lexer) atLineStart() bool { return lx.pos == 0 || lx.src[lx.pos-1] == '\n' }

// lineIs reports whether the line starting at pos begins with word followed by white space or the line end.
func (lx *lexer) lineIs(word string) bool {
	if lx.end-lx.pos < len(word) || string(lx.src[lx.pos:lx.pos+len(word)]) != word {
		return false
	}
	c := lx.at(lx.pos + len(word))
	return c == 0 || c == '\n' || isSpace(c)
}

func (lx *lexer) skipLine() {
	for lx.pos < lx.end && lx.src[lx.pos] != '\n' {
		lx.pos++
	}
}

// code lexes code until the end, or (stop == '}') until the unmatched closing brace of an interpolation (which is consumed).
func (lx *lexer) code(stop byte) []token {
	lx.depth++
	defer func() { lx.depth-- }()
	if lx.depth > maxDepth {
		lx.fail("nesting too deep")
		return nil
	}
	var toks []token
	if lx.depth == 1 {
		toks = make([]token, 0, (lx.end-lx.pos)/5+8)
	}
	braces := 0
	sp := false
	for lx.pos < lx.end && lx.err == "" {
		c := lx.src[lx.pos]
		if lx.atLineStart() {
			if c == '=' && lx.lineIs("=begin") {
				lx.blockComment()
				continue
			}
			if c == '_' && lx.lineIs("__END__") && stop == 0 && lx.depth == 1 {
				lx.pos = lx.end
				break
			}
		}
		start, line := lx.pos, lx.line
		switch {
		case isSpace(c):
			lx.pos++
			sp = true
			continue
		case c == '\\' && lx.at(lx.pos+1) == '\n':
			lx.pos += 2
			lx.line++
			sp = true
			continue
		case c == '\\' && lx.at(lx.pos+1) == '\r' && lx.at(lx.pos+2) == '\n':
			lx.pos += 3
			lx.line++
			sp = true
			continue
		case c == '#':
			lx.skipLine()
			continue
		case c == '\n':
			lx.pos++
			lx.line++
			if len(toks) > 0 && toks[len(toks)-1].kind != tNL {
				toks = append(toks, token{kind: tNL, start: int32(start), end: int32(lx.pos), line: line})
			}
			sp = false
			if len(lx.pending) > 0 {
				lx.heredocBodies()
			}
			continue
		}
		st := lx.state(toks, sp)
		lx.ambiguous = false
		switch {
		case isIdentStart(c):
			lx.ident()
			toks = lx.emit(toks, tIdent, start, sp, line, -1)
			lx.noteLocal(toks, lx.src[start:lx.pos])
		case isDigit(c):
			lx.number()
			toks = lx.emit(toks, tNum, start, sp, line, -1)
		case c == '@':
			lx.pos++
			if lx.at(lx.pos) == '@' {
				lx.pos++
			}
			for lx.pos < lx.end && isIdentChar(lx.src[lx.pos]) {
				lx.pos++
			}
			toks = lx.emit(toks, tIVar, start, sp, line, -1)
		case c == '$':
			lx.gvar()
			toks = lx.emit(toks, tGVar, start, sp, line, -1)
		case c == '"' || c == '\'' || c == '`':
			if c == '`' && st == pDef {
				lx.pos++
				toks = lx.emit(toks, tIdent, start, sp, line, -1) // def `(cmd) / obj.`
				break
			}
			lx.pos++
			kind := sDouble
			if c == '\'' {
				kind = sSingle
			} else if c == '`' {
				kind = sXStr
			}
			idx := lx.newStr(kind)
			lx.strs[idx].parts = lx.strBody(c, 0, kind != sSingle)
			toks = lx.emit(toks, tStr, start, sp, line, idx)
		case c == ':':
			toks = lx.colon(toks, start, sp, line, st)
		case c == '?':
			if (st == pBeg || st == pIdentSp && !isSpace(lx.at(lx.pos+1))) && lx.charLiteral() {
				idx := lx.newStr(sChar)
				lx.strs[idx].parts = []strPart{{lit: string(lx.src[start+1 : lx.pos])}}
				toks = lx.emit(toks, tStr, start, sp, line, idx)
				break
			}
			lx.pos++
			toks = lx.emit(toks, tOp, start, sp, line, -1)
		case c == '/':
			if st == pDef {
				lx.pos++
				toks = lx.emit(toks, tIdent, start, sp, line, -1)
				break
			}
			next := lx.at(lx.pos + 1)
			if st == pBeg || st == pIdentSp && !isSpace(next) && next != '=' {
				amb := st == pIdentSp
				lx.pos++
				idx := lx.newStr(sRegex)
				l0 := lx.line
				lx.strs[idx].parts = lx.strBody('/', 0, true)
				lx.regexFlags()
				if amb && lx.line != l0 {
					lx.fail("ambiguous / spans lines")
				}
				toks = lx.emit(toks, tStr, start, sp, line, idx)
				break
			}
			toks = lx.op(toks, start, sp, line)
		case c == '%':
			if st == pDef {
				lx.pos++
				toks = lx.emit(toks, tIdent, start, sp, line, -1)
				break
			}
			if (st == pBeg || st == pIdentSp && !isSpace(lx.at(lx.pos+1)) && lx.at(lx.pos+1) != '=') && lx.percentLiteral(st == pIdentSp) {
				toks = lx.emit(toks, tStr, start, sp, line, int32(len(lx.strs)-1))
				break
			}
			toks = lx.op(toks, start, sp, line)
		case c == '<' && lx.at(lx.pos+1) == '<' && st != pDef && st != pValue && lx.heredocStart(toks, st):
			toks = lx.emit(toks, tStr, start, sp, line, int32(len(lx.strs)-1))
		case c == '{':
			braces++
			lx.pos++
			toks = lx.emit(toks, tOp, start, sp, line, -1)
		case c == '}':
			if stop == '}' && braces == 0 {
				lx.pos++
				return toks
			}
			braces--
			lx.pos++
			toks = lx.emit(toks, tOp, start, sp, line, -1)
		default:
			if st == pDef && (c == '+' || c == '-' || c == '*' || c == '<' || c == '>' || c == '=' || c == '!' || c == '~' || c == '[' || c == '&' || c == '|' || c == '^') {
				// An operator method name (def <=>, obj.+): one identifier-like token.
				lx.opName()
				toks = lx.emit(toks, tIdent, start, sp, line, -1)
				break
			}
			toks = lx.op(toks, start, sp, line)
			lx.blockParams(toks)
		}
		sp = false
	}
	if stop == '}' && lx.err == "" {
		lx.fail("unterminated interpolation")
	}
	return toks
}

// opName consumes an operator used as a method name after def / . / ::.
func (lx *lexer) opName() {
	for _, o := range []string{"[]=", "[]", "<=>", "===", "==", "=~", "!~", "!=", "**", "+@", "-@", "<<", ">>", "<=", ">=", "!"} {
		if strings.HasPrefix(string(lx.src[lx.pos:min(lx.end, lx.pos+3)]), o) {
			lx.pos += len(o)
			return
		}
	}
	lx.pos++
}

func (lx *lexer) op(toks []token, start int, sp bool, line int32) []token {
	rest := lx.src[lx.pos:min(lx.end, lx.pos+3)]
	n := 1
	for _, o := range threeOps {
		if len(rest) >= 3 && string(rest[:3]) == o {
			n = 3
			break
		}
	}
	if n == 1 {
		for _, o := range twoOps {
			if len(rest) >= 2 && string(rest[:2]) == o {
				n = 2
				break
			}
		}
	}
	lx.pos += n
	return lx.emit(toks, tOp, start, sp, line, -1)
}

// blockParams records the names between the pipes of `{ |a, b|` / `do |a, (b, c)|` as local variables.
func (lx *lexer) blockParams(toks []token) {
	n := len(toks)
	if n < 2 || !lx.opIs(toks[n-1], "|") {
		return
	}
	prev := toks[n-2]
	if !(prev.kind == tOp && lx.opIs(prev, "{")) && !(prev.kind == tIdent && string(lx.src[prev.start:prev.end]) == "do") {
		return
	}
	for i := lx.pos; i < lx.end; i++ {
		c := lx.src[i]
		if c == '|' || c == '\n' {
			return
		}
		if isIdentStart(c) && (i == 0 || !isIdentChar(lx.src[i-1])) {
			j := i
			for j < lx.end && isIdentChar(lx.src[j]) {
				j++
			}
			lx.locals[string(lx.src[i:j])] = true
			i = j - 1
		}
	}
}

// noteLocal records `name =` (not ==, =~, =>) and method parameters `def m(a, b = 1)` as local variables.
func (lx *lexer) noteLocal(toks []token, name []byte) {
	i := lx.pos
	for i < lx.end && isSpace(lx.src[i]) {
		i++
	}
	c, d := lx.at(i), lx.at(i+1)
	if c == '=' && d != '=' && d != '~' && d != '>' || (c == '+' || c == '-' || c == '*' || c == '/' || c == '|' || c == '&') && d == '=' ||
		(c == '|' || c == '&') && d == c && lx.at(i+2) == '=' {
		lx.locals[string(name)] = true
		return
	}
	// Parameters: an identifier right after `(` or `,` of a def's parameter list.
	if n := len(toks); n >= 2 && toks[n-2].kind == tOp && (lx.opIs(toks[n-2], "(") || lx.opIs(toks[n-2], ",") || lx.opIs(toks[n-2], "*") ||
		lx.opIs(toks[n-2], "&") || lx.opIs(toks[n-2], "**")) {
		for j := n - 2; j >= 0 && j >= n-40; j-- {
			if toks[j].kind == tNL {
				return
			}
			if toks[j].kind == tIdent && string(lx.src[toks[j].start:toks[j].end]) == "def" {
				lx.locals[string(name)] = true
				return
			}
		}
	}
	// Parameters without parentheses: `def m a, b`.
	if n := len(toks); n >= 3 && toks[n-3].kind == tIdent && string(lx.src[toks[n-3].start:toks[n-3].end]) == "def" && toks[n-1].sp {
		lx.locals[string(name)] = true
	}
}

func (lx *lexer) ident() {
	for lx.pos < lx.end && isIdentChar(lx.src[lx.pos]) {
		lx.pos++
	}
	if c := lx.at(lx.pos); (c == '?' || c == '!') && lx.at(lx.pos+1) != '=' {
		// A method name ending in ? or ! (empty?, save!) — but not `a ?b : c` style ternaries after a local.
		if !(c == '?' && lx.at(lx.pos+1) == ':') {
			lx.pos++
		}
	} else if (c == '?' || c == '!') && lx.at(lx.pos+1) == '=' && lx.at(lx.pos+2) == '=' {
		lx.pos++ // empty?== x
	}
}

func (lx *lexer) number() {
	for lx.pos < lx.end {
		c := lx.src[lx.pos]
		switch {
		case isIdentChar(c):
			if (c == 'e' || c == 'E') && (lx.at(lx.pos+1) == '+' || lx.at(lx.pos+1) == '-') && isDigit(lx.at(lx.pos+2)) &&
				!(lx.pos+1 < lx.end && lx.src[lx.pos-1] == 'x') {
				lx.pos += 2
				continue
			}
			lx.pos++
		case c == '.' && isDigit(lx.at(lx.pos+1)):
			lx.pos++
		default:
			return
		}
	}
}

func (lx *lexer) gvar() {
	lx.pos++ // $
	c := lx.at(lx.pos)
	switch {
	case isIdentStart(c) || isDigit(c):
		for lx.pos < lx.end && isIdentChar(lx.src[lx.pos]) {
			lx.pos++
		}
	case c == '-':
		lx.pos++
		if isIdentChar(lx.at(lx.pos)) {
			lx.pos++
		}
	case c != 0 && strings.IndexByte("~*$?!@/\\;,.=:<>\"&`'+0", c) >= 0:
		lx.pos++
	}
}

// colon lexes ::, a symbol (:name, :+, :"str") or a plain colon.
func (lx *lexer) colon(toks []token, start int, sp bool, line int32, st prevState) []token {
	n := lx.at(lx.pos + 1)
	switch {
	case n == ':':
		lx.pos += 2
		return lx.emit(toks, tOp, start, sp, line, -1)
	case n == '"' || n == '\'':
		// :"sym" — but in `cond ? a :"b"` it is the same thing lexically.
		lx.pos += 2
		idx := lx.newStr(sSymStr)
		lx.strs[idx].parts = lx.strBody(n, 0, n == '"')
		return lx.emit(toks, tSym, start, sp, line, idx)
	case isIdentStart(n) && (st != pValue || sp && !isSpace(lx.at(lx.pos+1))):
		lx.pos++
		lx.ident()
		if c := lx.at(lx.pos); c == '=' && lx.at(lx.pos+1) != '=' && lx.at(lx.pos+1) != '>' && lx.at(lx.pos+1) != '~' {
			lx.pos++ // :name=
		}
		return lx.emit(toks, tSym, start, sp, line, -1)
	case st != pValue && n != 0 && !isSpace(n) && n != '\n' && strings.IndexByte("+-*/%<>=!~[&|^`@$", n) >= 0:
		lx.pos++
		switch {
		case n == '$':
			lx.gvar() // :$&, :$`, :$', :$LOAD_PATH
		case n == '@':
			lx.pos++
			for lx.pos < lx.end && (isIdentChar(lx.src[lx.pos]) || lx.src[lx.pos] == '@') {
				lx.pos++
			}
		default:
			lx.opName()
		}
		return lx.emit(toks, tSym, start, sp, line, -1)
	}
	lx.pos++
	return lx.emit(toks, tOp, start, sp, line, -1)
}

// charLiteral consumes ?a / ?\n / ?\C-a when it is one; returns false (consuming nothing) otherwise.
func (lx *lexer) charLiteral() bool {
	c := lx.at(lx.pos + 1)
	if c == 0 || isSpace(c) || c == '\n' {
		return false
	}
	if c == '\\' {
		i := lx.pos + 2
		if lx.at(i) == 0 {
			return false
		}
		// \C-x, \M-x, \M-\C-x, \u{...}, \uXXXX, \x41, octal: consume the escape generously.
		for k := 0; k < 3 && (lx.at(i) == 'C' || lx.at(i) == 'M') && lx.at(i+1) == '-'; k++ {
			i += 2
			if lx.at(i) == '\\' {
				i++
			}
		}
		if lx.at(i) == 'u' && lx.at(i+1) == '{' {
			for i < lx.end && lx.src[i] != '}' && lx.src[i] != '\n' {
				i++
			}
		} else if lx.at(i) == 'u' || lx.at(i) == 'x' {
			i++
			for k := 0; k < 4 && isHex(lx.at(i)); k++ {
				i++
			}
			i--
		}
		lx.pos = min(i+1, lx.end)
		if lx.at(lx.pos) == '\n' {
			lx.line++
		}
		return true
	}
	if isIdentChar(c) && isIdentChar(lx.at(lx.pos+2)) {
		return false // ?ab is not a character literal
	}
	// A multi-byte UTF-8 character.
	i := lx.pos + 2
	if c >= 0xC0 {
		for i < lx.end && lx.src[i]&0xC0 == 0x80 {
			i++
		}
	}
	lx.pos = i
	return true
}

func isHex(c byte) bool { return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

func closer(c byte) byte {
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
	return 0
}

// percentLiteral consumes %q(..) %w[..] %(..) ... when the text at pos is one.
func (lx *lexer) percentLiteral(heuristic bool) bool {
	t := lx.at(lx.pos + 1)
	d := t
	kind := sDouble
	i := lx.pos + 2
	switch t {
	case 'q':
		kind = sSingle
	case 'Q':
		kind = sDouble
	case 'w', 'i':
		kind = sWords
	case 'W', 'I':
		kind = sIWords
	case 'r':
		kind = sRegex
	case 's':
		kind = sSymStr
	case 'x':
		kind = sXStr
	default:
		i = lx.pos + 1
	}
	if i == lx.pos+2 {
		d = lx.at(i)
	}
	if d == 0 || isIdentChar(d) || isSpace(d) || d == '\n' {
		return false
	}
	if heuristic && i == lx.pos+1 && closer(d) == 0 && d != '|' && d != '!' {
		return false // `x %foo` after a method name is too ambiguous to be a literal with an exotic delimiter
	}
	typeless := i == lx.pos+1 // %(...) rather than %w(...): more ambiguous after a method name
	lx.pos = i + 1
	idx := lx.newStr(kind)
	interp := kind == sDouble || kind == sIWords || kind == sRegex || kind == sXStr
	l0 := lx.line
	lx.strs[idx].parts = lx.strBody(orClose(d), opener(d), interp)
	if kind == sRegex {
		lx.regexFlags()
	}
	if heuristic && typeless && lx.line != l0 {
		lx.fail("ambiguous % literal spans lines")
	}
	return true
}

func orClose(d byte) byte {
	if c := closer(d); c != 0 {
		return c
	}
	return d
}

func opener(d byte) byte {
	if closer(d) != 0 {
		return d
	}
	return 0
}

func (lx *lexer) regexFlags() {
	for lx.pos < lx.end && (lx.src[lx.pos] >= 'a' && lx.src[lx.pos] <= 'z') {
		lx.pos++
	}
}

// strBody lexes a literal body after its opening delimiter, up to and including term (nesting open/term pairs when open != 0).
func (lx *lexer) strBody(term, open byte, interp bool) []strPart {
	var parts []strPart
	var b []byte
	nest := 0
	for lx.pos < lx.end {
		c := lx.src[lx.pos]
		switch {
		case c == '\\':
			n := lx.at(lx.pos + 1)
			if n == '\n' {
				lx.line++
			}
			if n == 0 {
				lx.pos++
				continue
			}
			b = appendEscape(b, n, interp, term)
			lx.pos += 2
			continue
		case c == '\n':
			lx.line++
		case open != 0 && c == open:
			nest++
		case c == term:
			if nest == 0 {
				lx.pos++
				if len(b) > 0 || len(parts) == 0 {
					parts = append(parts, strPart{lit: string(b)})
				}
				return parts
			}
			nest--
		case interp && c == '#' && lx.at(lx.pos+1) == '{':
			if len(b) > 0 {
				parts = append(parts, strPart{lit: string(b)})
				b = nil
			}
			lx.pos += 2
			code := lx.code('}')
			parts = append(parts, strPart{code: code, isCode: true})
			if lx.err != "" {
				return parts
			}
			continue
		}
		b = append(b, c)
		lx.pos++
	}
	lx.fail("unterminated literal")
	return parts
}

// appendEscape appends the value of escape \n (approximate: enough for path literals).
func appendEscape(b []byte, n byte, interp bool, term byte) []byte {
	if !interp {
		if n == '\\' || n == term || n == '\'' {
			return append(b, n)
		}
		return append(b, '\\', n)
	}
	switch n {
	case 'n':
		return append(b, '\n')
	case 't':
		return append(b, '\t')
	case 's':
		return append(b, ' ')
	case '\n':
		return b
	case '0':
		return append(b, 0)
	}
	if isIdentChar(n) {
		return append(b, '\\', n) // \u, \x, \e...: not a path character anyway
	}
	return append(b, n)
}

func (lx *lexer) blockComment() {
	for lx.pos < lx.end {
		lx.skipLine()
		if lx.pos < lx.end {
			lx.pos++
			lx.line++
		}
		if lx.lineIs("=end") {
			lx.skipLine()
			return
		}
	}
	lx.fail("unterminated =begin")
}

// heredocStart consumes <<ID / <<-ID / <<~ID / <<'ID' / <<"ID" when it is a heredoc (the body is read at the end of the line).
func (lx *lexer) heredocStart(toks []token, st prevState) bool {
	i := lx.pos + 2
	squiggy := false
	if c := lx.at(i); c == '-' || c == '~' {
		squiggy = true
		i++
	}
	if st == pIdentSp && lx.at(lx.pos+2) == ' ' {
		return false
	}
	// `class << self` is a singleton class, never a heredoc.
	if n := len(toks); n > 0 && toks[n-1].kind == tIdent && string(lx.src[toks[n-1].start:toks[n-1].end]) == "class" {
		return false
	}
	q := lx.at(i)
	var id string
	interp := true
	switch {
	case q == '\'' || q == '"' || q == '`':
		j := i + 1
		for j < lx.end && lx.src[j] != q && lx.src[j] != '\n' {
			j++
		}
		if lx.at(j) != q {
			return false
		}
		id = string(lx.src[i+1 : j])
		interp = q != '\''
		i = j + 1
	case isIdentStart(q):
		j := i
		for j < lx.end && isIdentChar(lx.src[j]) {
			j++
		}
		id = string(lx.src[i:j])
		i = j
	default:
		return false
	}
	if id == "" {
		return false
	}
	lx.pos = i
	idx := lx.newStr(sHeredoc)
	lx.pending = append(lx.pending, heredoc{id: id, squiggy: squiggy, interp: interp, str: idx})
	return true
}

// heredocBodies reads the bodies of the pending heredocs, in order, starting at pos (the line after their start).
func (lx *lexer) heredocBodies() {
	hs := lx.pending
	lx.pending = nil
	for _, h := range hs {
		bodyStart, bodyLine := lx.pos, lx.line
		bodyEnd := -1
		for lx.pos < lx.end {
			ls := lx.pos
			lx.skipLine()
			l := string(lx.src[ls:lx.pos])
			l = strings.TrimSuffix(l, "\r")
			if h.squiggy {
				l = strings.TrimLeft(l, " \t")
			}
			if lx.pos < lx.end {
				lx.pos++ // \n
			}
			lx.line++
			if l == h.id {
				bodyEnd = ls
				break
			}
		}
		if bodyEnd < 0 {
			lx.fail("unterminated heredoc <<" + h.id)
			return
		}
		after, afterLine := lx.pos, lx.line
		if !h.interp {
			lx.strs[h.str].parts = []strPart{{lit: string(lx.src[bodyStart:bodyEnd])}}
			continue
		}
		// Lex the interpolations inside the body (limited to the body).
		saveEnd := lx.end
		lx.pos, lx.line, lx.end = bodyStart, bodyLine, bodyEnd
		parts := lx.heredocParts()
		lx.end = saveEnd
		lx.pos, lx.line = after, afterLine
		lx.strs[h.str].parts = parts
		if lx.err != "" {
			return
		}
	}
}

func (lx *lexer) heredocParts() []strPart {
	var parts []strPart
	var b []byte
	for lx.pos < lx.end {
		c := lx.src[lx.pos]
		switch {
		case c == '\\' && lx.pos+1 < lx.end:
			b = appendEscape(b, lx.src[lx.pos+1], true, 0)
			if lx.src[lx.pos+1] == '\n' {
				lx.line++
			}
			lx.pos += 2
			continue
		case c == '\n':
			lx.line++
		case c == '#' && lx.at(lx.pos+1) == '{':
			if len(b) > 0 {
				parts = append(parts, strPart{lit: string(b)})
				b = nil
			}
			lx.pos += 2
			code := lx.code('}')
			parts = append(parts, strPart{code: code, isCode: true})
			if lx.err != "" {
				return parts
			}
			continue
		}
		b = append(b, c)
		lx.pos++
	}
	if len(b) > 0 || len(parts) == 0 {
		parts = append(parts, strPart{lit: string(b)})
	}
	return parts
}

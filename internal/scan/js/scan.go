// Package js scans import/require/mock... references from JS/TS source with a hand-written single-pass lexer.
//
// FAIL-CLOSED principle: when the lexer is not sure it understood the file, it returns Uncertain=true with an UncertainReason ("code: detail"),
// and never silently returns a partial list. An extra import is acceptable; a missed import is a bug.
package js

import (
	"bytes"
	"strconv"
	"unicode/utf8"

	"github.com/qxbao/bgraph/internal/model"
)

// MaxFileSize: files larger than this threshold (bytes) are treated as uncertain (not scanned).
const MaxFileSize = 4 << 20

const maxDepth = 256

// Token classes.
const (
	tNone uint8 = iota
	tWord
	tStr
	tNum
	tTmpl
	tRegex
	tJSX
	tPunct
)

// Special punctuation codes (ch of tPunct); single-character punctuation uses the character itself.
const (
	pArrow  uint8 = 1 // =>
	pQDot   uint8 = 2 // ?.
	pSpread uint8 = 3 // ...
	pIncDec uint8 = 4 // ++ --
)

// Kinds of elements on the bracket stack.
const (
	kParen uint8 = iota + 1
	kParenCond
	kBracket
	kBlock
	kObj
	kClass // body of class/interface/enum ("member" context)
	kSub   // ${ } of a template or {expr} of JSX: sentinel for the nested lex
)

// State "after a closing bracket".
const (
	pcBare uint8 = iota + 1
	pcMethod
)

type rec struct {
	c, ch, w uint8
	dot      bool // word following '.' or '?.' (or #private): is a property name
}

type entry struct {
	kind uint8
	bare bool  // a bare `require` inside this bracket (may be a function parameter)
	cap  int32 // 1 + capture index (0 = none)
}

type pcState struct {
	kind uint8
	slot int
}

type scanner struct {
	src  []byte
	i    int
	line int
	jsx  bool

	imports []model.ImportRef
	stack   []entry
	caps    []capture

	ring [8]rec
	rn   int
	rx   bool // does the next `/` open a regex?

	watch  bool // there is state to check at the next token (slow path)
	stop   bool
	reason string
	depth  int

	// import/export statement being read
	stmt   uint8 // 0 none, 1 import, 2 export
	stN    int
	stType bool

	// `require` waiting for the next token to tell whether it is a bare value
	reqPend bool
	reqPrev rec
	reqLine int
	reqDot  bool

	pc pcState

	clsPending int // 1 + stack depth at `class`/`interface`/`enum`

	// callee candidate computed at a word (mock/glob/resolve/context/URL)
	cand    candidate
	candIdx int
	candArm bool
	gen     int // depth of <...> after the candidate (generic call)
	genEnd  int

	arrCap int32
}

func uncertain(code, detail string) model.ScanResult {
	return model.ScanResult{Imports: []model.ImportRef{}, Uncertain: true, UncertainReason: code + ": " + detail}
}

func jsxExt(path string) bool {
	k := len(path)
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			k = i
			break
		}
		if path[i] == '/' || path[i] == '\\' {
			break
		}
	}
	switch lower(path[k:]) {
	case ".ts", ".mts", ".cts":
		return false
	}
	return true
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// Scan scans one JS/TS file. path is only used to pick the JSX mode (by file extension).
func Scan(path string, src []byte) model.ScanResult {
	if len(src) > MaxFileSize {
		return uncertain("file_too_large", strconv.Itoa(len(src))+" bytes exceeds "+strconv.Itoa(MaxFileSize))
	}
	if bytes.IndexByte(src, 0) >= 0 {
		return uncertain("binary_content", "NUL byte in source")
	}
	if !utf8.Valid(src) {
		return uncertain("invalid_utf8", "source is not valid UTF-8")
	}
	s := &scanner{src: src, line: 1, rx: true, genEnd: -1}
	s.jsx = jsxExt(path)
	i := 0
	if bytes.HasPrefix(src, []byte{0xEF, 0xBB, 0xBF}) {
		i = 3
	}
	if bytes.HasPrefix(src[i:], []byte("#!")) {
		if k := bytes.IndexByte(src[i:], '\n'); k >= 0 {
			i += k
		} else {
			i = len(src)
		}
	}
	s.i = i
	s.lex(0, false)
	if !s.stop {
		if len(s.stack) > 0 {
			s.fail("unbalanced_brackets", "unclosed bracket at end of file (line "+strconv.Itoa(s.line)+")")
		} else if s.reqPend || s.pc.kind == pcBare {
			s.flag("require_value", "require used as a value at end of file")
		}
	}
	out := make([]model.ImportRef, 0, len(s.imports))
	for _, r := range s.imports {
		if r.Line >= 0 {
			out = append(out, r)
		}
	}
	return model.ScanResult{Imports: out, Uncertain: s.reason != "", UncertainReason: s.reason}
}

// fail: fatal error, stop scanning.
func (s *scanner) fail(code, detail string) {
	if s.reason == "" {
		s.reason = code + ": " + detail
	}
	s.stop = true
	s.i = len(s.src)
}

// flag: uncertain but keep scanning to collect more imports.
func (s *scanner) flag(code, detail string) {
	if s.reason == "" {
		s.reason = code + ": " + detail
	}
}

func (s *scanner) r(k int) rec { return s.ring[(s.rn-k)&7] }

func (s *scanner) ringPush(c, ch, w uint8, dot bool) {
	s.rn++
	s.ring[s.rn&7] = rec{c, ch, w, dot}
}

func (s *scanner) push(e entry) {
	if len(s.stack) >= 100000 {
		s.fail("too_deep", "bracket nesting too deep")
		return
	}
	s.stack = append(s.stack, e)
}

// tok records a token (not a string/template literal).
func (s *scanner) tok(c, ch, w uint8, dot bool) {
	if s.watch {
		s.slow(c, ch, w)
	}
	if n := len(s.stack); n > 0 {
		if cp := s.stack[n-1].cap; cp != 0 {
			s.feed(cp, c, ch, w)
		}
	}
	s.ringPush(c, ch, w, dot)
	switch c {
	case tWord:
		s.rx = !dot && w != 0 && w <= wLastRx
	case tPunct:
		if ch != pIncDec {
			s.rx = true
		}
	default:
		s.rx = false
	}
}

// tokLit records a string/template literal and feeds its value to the capture (if any).
func (s *scanner) tokLit(c uint8, val, tail string, hasSub bool, ln int, ok bool) {
	if s.watch {
		s.slow(c, 0, 0)
	}
	if cp := s.topCap(); cp != 0 {
		s.feedLit(cp, val, tail, hasSub, ln, ok)
	}
	s.ringPush(c, 0, 0, false)
	s.rx = false
}

// slow: the slow path, run before handling a token when there is pending state.
func (s *scanner) slow(c, ch, w uint8) {
	if s.pc.kind != 0 {
		pk, slot := s.pc.kind, s.pc.slot
		s.pc.kind = 0
		switch pk {
		case pcBare:
			if !(c == tPunct && (ch == pArrow || ch == '{' || ch == ':')) {
				s.flag("require_value", "require passed as a value near line "+strconv.Itoa(s.line))
			}
		case pcMethod:
			if c == tPunct && (ch == '{' || ch == ':' || ch == '?') {
				s.imports[slot].Line = -1 // method declaration named import/require
			}
		}
	}
	if s.candArm {
		s.candArm = false
		if c == tPunct && ch == '<' {
			s.gen = 1
		}
	} else if s.gen > 0 && c == tPunct {
		switch ch {
		case '<':
			s.gen++
		case '>':
			s.gen--
			if s.gen == 0 {
				s.genEnd = s.rn + 1
			}
		case ';':
			s.gen = 0
		}
	}
	if s.reqDot {
		s.reqDot = false
		if !(c == tWord && reqProp(w)) {
			s.flag("require_value", "unsupported use of require.<prop> near line "+strconv.Itoa(s.line))
		}
	}
	if s.reqPend {
		s.reqPend = false
		s.resolveReq(c, ch)
	}
	if s.stmt != 0 {
		allowed := false
		switch c {
		case tWord, tStr:
			allowed = true
		case tPunct:
			allowed = ch == '{' || ch == '}' || ch == ',' || ch == '*'
		}
		if !allowed {
			s.stmt = 0
		} else {
			if c == tWord && w == wType && s.stN == 0 {
				s.stType = true
			}
			if c == tPunct && ch == ',' && s.stType && s.stN == 1 {
				s.stType = false
			}
			s.stN++
		}
	}
	s.watch = s.stmt != 0 || s.reqPend || s.reqDot || s.pc.kind != 0 || s.gen > 0 || s.candArm
}

func (s *scanner) resolveReq(c, ch uint8) {
	p := s.reqPrev
	if c == tPunct {
		switch ch {
		case '(', pArrow:
			return
		case '.', pQDot:
			s.reqDot = true
			return
		case ':', '?':
			if p.c == tPunct && (p.ch == '{' || p.ch == ',' || p.ch == '(' || p.ch == ';') {
				return // object key / interface property / typed parameter
			}
		case ',', ')':
			if p.c == tPunct && (p.ch == '(' || p.ch == ',') {
				if n := len(s.stack); n > 0 && s.stack[n-1].kind == kParen {
					s.stack[n-1].bare = true // may be a function parameter: decided when the bracket closes
					return
				}
			}
		}
	}
	s.flag("require_value", "require used as a value at line "+strconv.Itoa(s.reqLine))
}

func (s *scanner) checkRequire() {
	r1 := s.r(1)
	if r1.c == tWord && !r1.dot {
		switch r1.w {
		case wTypeof, wConst, wLet, wVar, wFunction, wClass:
			return
		}
	}
	if s.stmt != 0 {
		return
	}
	s.reqPend = true
	s.reqPrev = r1
	s.reqLine = s.line
	s.watch = true
}

func isDot(r rec) bool { return r.c == tPunct && (r.ch == '.' || r.ch == pQDot) }

// candAt computes the callee candidate at word w just pushed into the ring (r(0) is the word).
func (s *scanner) candAt(w uint8) candidate {
	r0 := s.r(0)
	switch w {
	case wResolve, wContext:
		if r0.dot && isDot(s.r(1)) {
			if r2 := s.r(2); r2.c == tWord && r2.w == wRequire && !r2.dot {
				if w == wResolve {
					return candidate{model.ImportResolve, fNormal, true}
				}
				return candidate{model.ImportGlob, fCtx, true}
			}
		}
	case wGlob, wGlobEager:
		if r0.dot && isDot(s.r(1)) {
			if r2 := s.r(2); r2.c == tWord && r2.w == wMeta && isDot(s.r(3)) {
				if r4 := s.r(4); r4.c == tWord && r4.w == wImport && !r4.dot {
					return candidate{model.ImportGlob, fGlob, true}
				}
			}
		}
	case wURL:
		if r1 := s.r(1); !r0.dot && r1.c == tWord && r1.w == wNew && !r1.dot {
			return candidate{model.ImportURL, fURL, true}
		}
	default: // mock name
		if r0.dot && isDot(s.r(1)) {
			if r2 := s.r(2); r2.c == tWord {
				if (r2.w == wJest && jestName(w)) || (r2.w == wVi && viName(w)) {
					return candidate{model.ImportMock, fNormal, true}
				}
			}
		}
	}
	return candidate{}
}

func memberStart(r rec) bool {
	switch r.c {
	case tPunct:
		switch r.ch {
		case '{', ',', ';', '}', '*':
			return true
		}
	case tWord:
		return !r.dot && r.w == wMod
	}
	return false
}

// callee determines whether the upcoming `(` is an import-like call (called before pushing `(` into the ring).
func (s *scanner) callee() (kind model.ImportKind, flavor uint8, method, ok bool) {
	r0 := s.r(0)
	if r0.c == tWord {
		if r0.w == wImport || r0.w == wRequire {
			if r0.dot {
				return
			}
			r1 := s.r(1)
			if r1.c == tWord && !r1.dot && r1.w == wFunction {
				return
			}
			ms := memberStart(r1)
			if n := len(s.stack); n > 0 && s.stack[n-1].kind == kClass {
				if ms {
					return // method named import/require inside a class/interface
				}
				ms = true // other position (e.g. type `: import("x")`): keep, but cancel if it turns out to be a method declaration
			}
			kind = model.ImportDynamic
			if r0.w == wRequire {
				kind = model.ImportRequire
			}
			return kind, fNormal, ms, true
		}
		if s.cand.ok && s.candIdx == s.rn {
			return s.cand.kind, s.cand.flavor, false, true
		}
		return
	}
	if r0.c == tPunct && r0.ch == '>' && s.cand.ok && s.genEnd == s.rn {
		return s.cand.kind, s.cand.flavor, false, true
	}
	return
}

func (s *scanner) afterWord(w uint8) {
	switch w {
	case wImport:
		s.stmt, s.stN, s.stType = 1, 0, false
		s.watch = true
	case wExport:
		s.stmt, s.stN, s.stType = 2, 0, false
		s.watch = true
	case wRequire:
		s.checkRequire()
	case wClass, wInterface, wEnum:
		s.clsPending = len(s.stack) + 1
	default:
		if w >= wFirstCallee && w <= wLastCallee {
			s.armCand(w)
		}
	}
}

func (s *scanner) enterSub() bool {
	if s.depth++; s.depth > maxDepth {
		s.fail("too_deep", "nesting too deep")
		return false
	}
	s.push(entry{kind: kSub})
	s.ringPush(tPunct, '{', 0, false)
	s.rx = true
	return !s.stop
}

func (s *scanner) leaveSub() {
	s.depth--
	if n := len(s.stack); n > 0 && s.stack[n-1].kind == kSub {
		s.stack = s.stack[:n-1]
	}
}

// lex runs the main lexer loop. base is the stack depth of this call. sub=true: finishes (successfully) on a '}'
// matching the kSub sentinel; returns false on end of file / an earlier error.
func (s *scanner) lex(base int, sub bool) bool {
	src := s.src
	n := len(src)
	for s.i < n {
		if s.stop {
			return false
		}
		c := src[s.i]
		switch c {
		case ' ', '\t', '\r', '\v', '\f':
			s.i++
		case '\n':
			s.line++
			s.i++
		case '"', '\'':
			s.str(c)
		case '`':
			s.template()
		case '/':
			s.slash()
		case '(':
			s.openParen()
		case ')':
			s.closeParen(base)
		case '[':
			s.arrCap = 0
			s.tok(tPunct, '[', 0, false)
			s.push(entry{kind: kBracket, cap: s.arrCap})
			s.arrCap = 0
			s.i++
		case ']':
			s.closeBracket(base)
		case '{':
			kind := s.braceKind()
			s.tok(tPunct, '{', 0, false)
			s.push(entry{kind: kind})
			s.i++
		case '}':
			if len(s.stack) <= base {
				if sub {
					s.i++
					return true
				}
				s.fail("unbalanced_brackets", "unexpected '}' at line "+strconv.Itoa(s.line))
				return false
			}
			s.closeBrace()
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			s.number()
		default:
			if identStart[c] {
				if c >= 0x80 {
					if l := uniSpaceLen(src[s.i:]); l > 0 {
						s.i += l
						continue
					}
				}
				s.word()
			} else {
				s.punct(c)
			}
		}
	}
	return false
}

func (s *scanner) word() {
	src := s.src
	n := len(src)
	j := s.i + 1
	for j < n && identPart[src[j]] {
		if c := src[j]; c >= 0xC2 && c <= 0xEF && uniSpaceLen(src[j:]) > 0 {
			break
		}
		j++
	}
	w := wordID(src[s.i:j])
	s.i = j
	dot := isDot(s.r(0))
	s.tok(tWord, 0, w, dot)
	if w != wOther {
		if !dot {
			s.afterWord(w)
		} else if w >= wFirstCallee && w <= wLastCallee {
			s.armCand(w)
		}
	}
}

func (s *scanner) number() {
	src := s.src
	n := len(src)
	j := s.i
	seenDot := false
	hex := false
	if src[j] == '.' {
		seenDot = true
		j++
	} else if src[j] == '0' && j+1 < n {
		switch src[j+1] | 0x20 {
		case 'x', 'o', 'b':
			hex = true
		}
	}
	for j < n {
		c := src[j]
		if isAlnum(c) || c == '_' {
			j++
			continue
		}
		if c == '.' && !seenDot && !hex {
			seenDot = true
			j++
			continue
		}
		if (c == '+' || c == '-') && !hex && j > s.i && src[j-1]|0x20 == 'e' {
			j++
			continue
		}
		break
	}
	s.i = j
	s.tok(tNum, 0, 0, false)
}

func (s *scanner) punct(c uint8) {
	src := s.src
	n := len(src)
	i := s.i
	var next byte
	if i+1 < n {
		next = src[i+1]
	}
	switch c {
	case '.':
		switch {
		case next >= '0' && next <= '9':
			s.number()
		case next == '.' && i+2 < n && src[i+2] == '.':
			s.tok(tPunct, pSpread, 0, false)
			s.i += 3
		default:
			s.tok(tPunct, '.', 0, false)
			s.i++
		}
	case '?':
		if next == '.' && !(i+2 < n && src[i+2] >= '0' && src[i+2] <= '9') {
			s.tok(tPunct, pQDot, 0, false)
			s.i += 2
		} else {
			s.tok(tPunct, '?', 0, false)
			s.i++
		}
	case '=':
		if next == '>' {
			s.tok(tPunct, pArrow, 0, false)
			s.i += 2
		} else {
			s.tok(tPunct, '=', 0, false)
			s.i++
		}
	case '+', '-':
		if next == c {
			s.tok(tPunct, pIncDec, 0, false)
			s.i += 2
		} else {
			s.tok(tPunct, c, 0, false)
			s.i++
		}
	case '<':
		if next == '<' { // bit-shift operator `<<`, not JSX
			s.tok(tPunct, '<', 0, false)
			s.i += 2
			return
		}
		if s.jsx && s.rx && s.jsxStart() {
			s.jsxElement()
			if s.stop {
				return
			}
			s.tok(tJSX, 0, 0, false)
			return
		}
		s.tok(tPunct, '<', 0, false)
		s.i++
	case '!':
		// TS non-null assertion `x! / y`: a `!` right after a value (not followed by '=') still leaves `/` as a division.
		prevValue := !s.rx
		s.tok(tPunct, '!', 0, false)
		s.i++
		if prevValue && next != '=' {
			s.rx = false
		}
	case '#':
		if identStart[next] {
			j := i + 2
			for j < n && identPart[src[j]] {
				j++
			}
			s.i = j
			s.tok(tWord, 0, wOther, true)
			return
		}
		s.tok(tPunct, '#', 0, false)
		s.i++
	case '\\':
		s.flag("ident_escape", "escape sequence in identifier at line "+strconv.Itoa(s.line))
		s.i++
	case ':', ';':
		if s.clsPending == len(s.stack)+1 {
			s.clsPending = 0
		}
		s.tok(tPunct, c, 0, false)
		s.i++
	default:
		if c < 0x20 || c == 0x7f {
			s.i++
			return
		}
		s.tok(tPunct, c, 0, false)
		s.i++
	}
}

func (s *scanner) braceKind() uint8 {
	if s.clsPending == len(s.stack)+1 {
		s.clsPending = 0
		return kClass
	}
	if s.stmt != 0 {
		return kBlock
	}
	r := s.r(0)
	switch r.c {
	case tPunct:
		switch r.ch {
		case ')', ';', '{', '}', pArrow, '>', ']':
			return kBlock
		}
		return kObj
	case tWord:
		if !r.dot && r.w != 0 && r.w <= wLastRx && r.w != wElse && r.w != wDo && r.w != wVoid {
			return kObj
		}
	}
	return kBlock
}

func (s *scanner) openParen() {
	kind := kParen
	r0 := s.r(0)
	if r0.c == tWord && !r0.dot {
		switch r0.w {
		case wCond, wFor:
			kind = kParenCond
		case wAwait:
			if r1 := s.r(1); r1.c == tWord && r1.w == wFor && !r1.dot {
				kind = kParenCond
			}
		}
	}
	ck, fl, method, ok := s.callee()
	line := s.line
	s.tok(tPunct, '(', 0, false)
	e := entry{kind: kind}
	if ok {
		e.cap = s.newCap(ck, fl, method, line)
	}
	s.push(e)
	s.i++
}

func (s *scanner) closeParen(base int) {
	n := len(s.stack)
	if n <= base || (s.stack[n-1].kind != kParen && s.stack[n-1].kind != kParenCond) {
		s.fail("unbalanced_brackets", "unexpected ')' at line "+strconv.Itoa(s.line))
		return
	}
	s.tok(tPunct, ')', 0, false)
	top := s.stack[n-1]
	s.stack = s.stack[:n-1]
	s.rx = top.kind == kParenCond
	if top.bare && top.kind == kParen {
		s.pc = pcState{kind: pcBare}
		s.watch = true
	}
	s.i++
}

func (s *scanner) closeBracket(base int) {
	n := len(s.stack)
	if n <= base || s.stack[n-1].kind != kBracket {
		s.fail("unbalanced_brackets", "unexpected ']' at line "+strconv.Itoa(s.line))
		return
	}
	s.tok(tPunct, ']', 0, false)
	s.stack = s.stack[:n-1]
	s.rx = false
	s.i++
}

func (s *scanner) closeBrace() {
	n := len(s.stack)
	top := s.stack[n-1]
	switch top.kind {
	case kBlock, kObj, kClass:
	default:
		s.fail("unbalanced_brackets", "mismatched '}' at line "+strconv.Itoa(s.line))
		return
	}
	s.tok(tPunct, '}', 0, false)
	s.stack = s.stack[:n-1]
	s.rx = top.kind != kObj
	s.i++
}

func (s *scanner) slash() {
	src := s.src
	i := s.i
	if i+1 < len(src) {
		switch src[i+1] {
		case '/':
			s.lineComment()
			return
		case '*':
			s.blockComment()
			return
		}
	}
	if s.rx {
		s.regex()
		return
	}
	s.tok(tPunct, '/', 0, false)
	s.i++
}

func (s *scanner) lineComment() {
	src := s.src
	start := s.i + 2
	end := len(src)
	if k := bytes.IndexByte(src[start:], '\n'); k >= 0 {
		end = start + k
	}
	if end > start && src[start] == '/' {
		s.tripleSlash(src[start+1 : end])
	}
	s.i = end
}

func (s *scanner) blockComment() {
	src := s.src
	k := bytes.Index(src[s.i+2:], []byte("*/"))
	if k < 0 {
		s.fail("unterminated_comment", "block comment opened at line "+strconv.Itoa(s.line))
		return
	}
	s.line += bytes.Count(src[s.i+2:s.i+2+k], []byte{'\n'})
	s.i += k + 4
}

// tripleSlash: `/// <reference path="x" />`; rest is the part after "///".
func (s *scanner) tripleSlash(rest []byte) {
	i := 0
	skip := func() {
		for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\r') {
			i++
		}
	}
	skip()
	if !bytes.HasPrefix(rest[i:], []byte("<reference")) {
		return
	}
	i += len("<reference")
	for i < len(rest) {
		skip()
		if i >= len(rest) || rest[i] == '/' || rest[i] == '>' {
			return
		}
		a := i
		for i < len(rest) && rest[i] != '=' && rest[i] != ' ' && rest[i] != '\t' && rest[i] != '/' && rest[i] != '>' {
			i++
		}
		name := rest[a:i]
		skip()
		if i >= len(rest) || rest[i] != '=' {
			continue
		}
		i++
		skip()
		if i >= len(rest) || (rest[i] != '"' && rest[i] != '\'') {
			return
		}
		q := rest[i]
		k := bytes.IndexByte(rest[i+1:], q)
		if k < 0 {
			return
		}
		val := rest[i+1 : i+1+k]
		i += k + 2
		if string(name) == "path" {
			v, ok := decode(val)
			if !ok {
				s.flag("bad_escape", "undecodable escape in reference path at line "+strconv.Itoa(s.line))
			}
			s.imports = append(s.imports, model.ImportRef{Spec: v, Kind: model.ImportTripleSlash, Line: s.line})
			return
		}
	}
}

func (s *scanner) regex() {
	src := s.src
	n := len(src)
	j := s.i + 1
	inClass := false
	for {
		for j < n && !rxSpecial[src[j]] {
			j++
		}
		if j >= n {
			break
		}
		c := src[j]
		if c == '\\' {
			if j+1 >= n || src[j+1] == '\n' || src[j+1] == '\r' {
				break
			}
			j += 2
			continue
		}
		if c == '\n' || c == '\r' {
			break
		}
		if c == '[' {
			inClass = true
		} else if c == ']' {
			inClass = false
		} else if !inClass { // '/'
			j++
			for j < n && identPart[src[j]] {
				j++
			}
			s.i = j
			s.tok(tRegex, 0, 0, false)
			return
		}
		j++
	}
	s.fail("unterminated_regex", "regex literal starting at line "+strconv.Itoa(s.line))
}

func (s *scanner) str(q byte) {
	src := s.src
	n := len(src)
	ln := s.line
	j := s.i + 1
	for {
		for j < n && !strSpecial[src[j]] {
			j++
		}
		if j >= n {
			s.fail("unterminated_string", "string starting at line "+strconv.Itoa(ln))
			return
		}
		c := src[j]
		if c == q {
			break
		}
		if c == '\\' {
			if j+1 >= n {
				s.fail("unterminated_string", "string starting at line "+strconv.Itoa(ln))
				return
			}
			if src[j+1] == '\r' && j+2 < n && src[j+2] == '\n' {
				j += 3
				s.line++
			} else {
				if src[j+1] == '\n' {
					s.line++
				}
				j += 2
			}
			continue
		}
		if c == '\n' || c == '\r' {
			s.fail("unterminated_string", "string starting at line "+strconv.Itoa(ln))
			return
		}
		j++ // other quote kind
	}
	raw := src[s.i+1 : j]
	s.i = j + 1

	rec := false
	if s.stmt != 0 {
		r0 := s.r(0)
		rec = r0.c == tWord && !r0.dot && (r0.w == wFrom || (r0.w == wImport && s.stmt == 1))
	}
	if !rec && s.topCap() == 0 {
		s.tok(tStr, 0, 0, false)
		return
	}
	val, ok := decode(raw)
	if !ok {
		s.flag("bad_escape", "undecodable escape in module specifier at line "+strconv.Itoa(ln))
	}
	s.tokLit(tStr, val, "", false, ln, ok)
	if rec {
		kind := model.ImportStatic
		if s.stmt == 2 {
			kind = model.ImportReExport
		}
		if s.stType && s.stN != 3 {
			kind = model.ImportTypeOnly
		}
		s.imports = append(s.imports, model.ImportRef{Spec: val, Kind: kind, Line: ln})
		s.stmt = 0
	}
}

func (s *scanner) template() {
	src := s.src
	n := len(src)
	startLine := s.line
	s.i++
	want := s.topCap() != 0
	segStart := s.i
	var head, tail string
	hasSub := false
	allOK := true
	for {
		j := s.i
		for j < n && !tplSpecial[src[j]] {
			j++
		}
		s.i = j
		if j >= n {
			s.fail("unterminated_template", "template starting at line "+strconv.Itoa(startLine))
			return
		}
		switch src[j] {
		case '\\':
			if j+1 < n && src[j+1] == '\n' {
				s.line++
			}
			s.i = j + 2
		case '\n':
			s.line++
			s.i = j + 1
		case '`':
			if want {
				v, ok := decode(src[segStart:j])
				allOK = allOK && ok
				if hasSub {
					tail = v
				} else {
					head = v
				}
			}
			s.i = j + 1
			if want {
				if !allOK {
					s.flag("bad_escape", "undecodable escape in template at line "+strconv.Itoa(startLine))
				}
				s.tokLit(tTmpl, head, tail, hasSub, startLine, allOK)
			} else {
				s.tok(tTmpl, 0, 0, false)
			}
			return
		case '$':
			if j+1 < n && src[j+1] == '{' {
				if want && !hasSub {
					v, ok := decode(src[segStart:j])
					allOK = allOK && ok
					head = v
				}
				hasSub = true
				s.i = j + 2
				if !s.enterSub() {
					return
				}
				ok := s.lex(len(s.stack), true)
				s.leaveSub()
				if !ok {
					if !s.stop {
						s.fail("unterminated_template", "unterminated substitution in template starting at line "+strconv.Itoa(startLine))
					}
					return
				}
				segStart = s.i
			} else {
				s.i = j + 1
			}
		}
	}
}

func (s *scanner) armCand(w uint8) {
	s.cand = s.candAt(w)
	if s.cand.ok {
		s.candIdx = s.rn
		s.candArm = true
		s.watch = true
	}
}

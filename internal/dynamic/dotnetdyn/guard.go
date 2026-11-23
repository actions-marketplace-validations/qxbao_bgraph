package dotnetdyn

// Body guard of the .NET per-test-class layer (docs/dynamic-layer.md §9). A record lists the source files that had a line
// executed while a test class ran (code coverage). A change can only narrow when such a record sees its effect: code inside a
// method body (or an expression body, or a non-const field initializer) runs only when a test executes it, so a test that
// never executed it is unaffected. Anything the compiler copies into other assemblies or exposes through metadata (signatures,
// default parameter values, const fields, enum members, attributes, member and type declarations) is invisible to coverage,
// so the guard refuses it. The guard is lexical and fail-closed: a file it cannot lex or place refuses.

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Span is a member of the base file: lines From..To (1-based, inclusive) and a description ("method Foo", "accessor get of
// Bar", "initializer of field _x").
type Span struct {
	From, To int
	What     string
}

// BodyGuard returns "" when every change from base to head of C# file p lies inside code that only runs when a test executes
// it; otherwise the reason (one line, starts with p and names the line). members receives, for each allowed change, the base
// line span of the enclosing member (so the caller can check that coverage instrumented it), once per member; changes to
// comments, whitespace or using directives add none.
//
// Changes are computed over the token streams without comments and whitespace (a refinement of a line diff: a change
// confined to comments or layout is no change at all).
func BodyGuard(p string, base, head []byte) (members []Span, why string) {
	if strings.ToLower(path.Ext(p)) != ".cs" {
		return nil, p + ": not a C# source"
	}
	if bytes.Equal(base, head) {
		return nil, ""
	}
	fb, why := analyze(base)
	if why != "" {
		return nil, p + ": cannot be lexed reliably at the base (" + why + ")"
	}
	fh, why := analyze(head)
	if why != "" {
		return nil, p + ": cannot be lexed reliably at the head (" + why + ")"
	}
	a, b := keys(fb.t), keys(fh.t)
	hunks := diffHunks(a, b)
	match := make([]int, len(a)) // base token -> head token, -1 when changed
	bi, hi := 0, 0
	for _, h := range hunks {
		for ; bi < h.b0; bi, hi = bi+1, hi+1 {
			match[bi] = hi
		}
		for ; bi < h.b1; bi++ {
			match[bi] = -1
		}
		hi = h.h1
	}
	for ; bi < len(a); bi, hi = bi+1, hi+1 {
		match[bi] = hi
	}
	for _, h := range hunks {
		sp, why := check(p, fb, fh, match, h)
		if why != "" {
			return nil, why
		}
		if sp != nil && !containsSpan(members, *sp) {
			members = append(members, *sp)
		}
	}
	return members, ""
}

func containsSpan(s []Span, x Span) bool {
	for _, y := range s {
		if y == x {
			return true
		}
	}
	return false
}

func keys(t []tok) []string {
	out := make([]string, len(t))
	for i, x := range t {
		out[i] = string(rune(x.k)) + x.s
	}
	return out
}

// check decides one change: base tokens [h.b0, h.b1) replaced by head tokens [h.h0, h.h1).
func check(p string, fb, fh *file, match []int, h hunk) (*Span, string) {
	refuse := func(what string) string {
		if h.h1 > h.h0 {
			return fmt.Sprintf("%s:%d: change touches %s", p, fh.t[h.h0].line, what)
		}
		if h.b0 < len(fb.t) {
			return fmt.Sprintf("%s:%d (base): change touches %s", p, fb.t[h.b0].line, what)
		}
		return fmt.Sprintf("%s: change at the end of the file touches %s", p, what)
	}
	if fb.inCond(h.b0, h.b1) || fh.inCond(h.h0, h.h1) {
		return nil, refuse("an #if region")
	}
	pragmasOnly := true
	for _, x := range append(append([]tok(nil), fb.t[h.b0:h.b1]...), fh.t[h.h0:h.h1]...) {
		pragma := x.k == 'd' && strings.HasPrefix(x.s, "#pragma")
		if x.k == 'd' && !pragma {
			return nil, refuse("preprocessor directive " + strings.Fields(x.s)[0])
		}
		pragmasOnly = pragmasOnly && pragma
	}
	if pragmasOnly {
		return nil, "" // #pragma only changes compiler diagnostics, and the build is never narrowed
	}
	if zb := fb.zoneOf(h.b0, h.b1); zb >= 0 {
		if zh := fh.zoneOf(h.h0, h.h1); zh >= 0 && fb.zones[zb].ord == fh.zones[zh].ord {
			return nil, ""
		}
	}
	if rb := fb.regionOf(h.b0, h.b1); rb >= 0 {
		if rh := fh.regionOf(h.h0, h.h1); rh >= 0 {
			r, q := fb.regions[rb], fh.regions[rh]
			if match[r.s-1] == q.s-1 && r.what == q.what {
				d := fb.decls[r.member]
				return &Span{From: fb.t[d.s].line, To: fb.t[d.e-1].endLine, What: r.what}, ""
			}
		}
	}
	if h.h1 > h.h0 {
		return nil, refuse(fh.describe(h.h0, h.h1))
	}
	return nil, refuse(fb.describe(h.b0, h.b1))
}

// TopLevelTypes returns the namespace-qualified names ("Ns.Sub.Name", no generic arity) of the top-level types declared in
// src: classes, structs, records, interfaces, enums and delegates. ok=false when the file cannot be lexed or placed, or when
// an inactive preprocessor branch outside any type may declare a type.
func TopLevelTypes(p string, src []byte) ([]string, bool) {
	if strings.ToLower(path.Ext(p)) != ".cs" {
		return nil, false
	}
	f, why := analyze(src)
	if why != "" || f.unsure {
		return nil, false
	}
	return f.types, true
}

// ---------------------------------------------------------------------------------------------------------------------------
// Lexing

// tok is one lexical token. k: 'i' identifier or keyword, 's' string or char literal, 'n' number, 'p' punctuation,
// 'd' preprocessor directive (normalized text), 'x' inactive preprocessor section (raw text).
type tok struct {
	k             byte
	s             string
	line, endLine int // 1-based lines of the first and last character
}

type lexer struct {
	b     []byte
	i     int
	line  int
	out   []tok
	open  []int    // token index of each open #if
	conds [][2]int // closed conditionals: token indices of the #if and its #endif
}

func (l *lexer) emit(k byte, start, line int) {
	s := string(l.b[start:l.i])
	l.line = line + strings.Count(s, "\n")
	l.out = append(l.out, tok{k: k, s: s, line: line, endLine: l.line})
}

// lex tokenizes C# source. Of each #if/#elif/#else chain the first branch is lexed; the other branches up to the #endif are
// one 'x' token. why != "" when the source cannot be lexed reliably.
func lex(src []byte) (*lexer, string) {
	b := bytes.TrimPrefix(src, []byte("\xef\xbb\xbf"))
	l := &lexer{b: b, line: 1}
	bol := true // only whitespace since the start of the line
	for l.i < len(b) {
		c := b[l.i]
		switch c {
		case '\n':
			l.line++
			l.i++
			bol = true
			continue
		case ' ', '\t', '\r', '\f', '\v':
			l.i++
			continue
		case '#':
			if bol {
				if why := l.directive(); why != "" {
					return nil, why
				}
				continue
			}
		}
		bol = false
		start, line := l.i, l.line
		next := byte(0)
		if l.i+1 < len(b) {
			next = b[l.i+1]
		}
		switch {
		case c == '/' && next == '/':
			for l.i < len(b) && b[l.i] != '\n' {
				l.i++
			}
		case c == '/' && next == '*':
			j := bytes.Index(b[l.i+2:], []byte("*/"))
			if j < 0 {
				return nil, fmt.Sprintf("unterminated comment on line %d", line)
			}
			l.line += bytes.Count(b[l.i:l.i+2+j], []byte("\n"))
			l.i += j + 4
		case stringStart(b, l.i):
			end := scanString(b, l.i)
			if end < 0 {
				return nil, fmt.Sprintf("unterminated or malformed string literal on line %d", line)
			}
			l.i = end
			l.emit('s', start, line)
		case c == '\'':
			end := scanChar(b, l.i)
			if end < 0 {
				return nil, fmt.Sprintf("malformed character literal on line %d", line)
			}
			l.i = end
			l.emit('s', start, line)
		case c == '@' && l.i+1 < len(b) && (identStartAt(b, l.i+1)):
			l.i++
			l.ident()
			l.emit('i', start, line)
		default:
			if !l.common() {
				return nil, fmt.Sprintf("unexpected character on line %d", line)
			}
		}
	}
	if len(l.open) > 0 {
		return nil, fmt.Sprintf("#if on line %d without #endif", l.out[l.open[len(l.open)-1]].line)
	}
	return l, ""
}

var directiveName = regexp.MustCompile(`^#\s*([a-z]*)`)

// directive consumes the preprocessor directive line at l.i (the '#'), up to but excluding the line break.
func (l *lexer) directive() string {
	b := l.b
	start, line := l.i, l.line
	end := start
	for end < len(b) && b[end] != '\n' {
		end++
	}
	text := strings.TrimRight(string(b[start:end]), "\r")
	name := directiveName.FindStringSubmatch(text)[1]
	rest := strings.TrimSpace(text[len(directiveName.FindString(text)):])
	if name != "error" && name != "warning" && name != "region" && name != "endregion" {
		if i := strings.Index(rest, "//"); i >= 0 {
			rest = strings.TrimSpace(rest[:i])
		}
	}
	norm := "#" + name
	if f := strings.Fields(rest); len(f) > 0 {
		norm += " " + strings.Join(f, " ")
	}
	l.i = end
	add := func() { l.out = append(l.out, tok{k: 'd', s: norm, line: line, endLine: line}) }
	switch name {
	case "region", "endregion":
	case "if":
		add()
		l.open = append(l.open, len(l.out)-1)
	case "elif", "else":
		if len(l.open) == 0 {
			return fmt.Sprintf("#%s without #if on line %d", name, line)
		}
		// Skip the remaining branches up to the matching #endif; preprocessing of skipped lines only looks at directives.
		j, depth, lines := start, 0, 0
		for {
			lineEnd := j
			for lineEnd < len(b) && b[lineEnd] != '\n' {
				lineEnd++
			}
			if m := directiveName.FindStringSubmatch(strings.TrimSpace(string(b[j:lineEnd]))); m != nil && j > start {
				if m[1] == "if" {
					depth++
				} else if m[1] == "endif" {
					if depth == 0 {
						break
					}
					depth--
				}
			}
			if lineEnd >= len(b) {
				return fmt.Sprintf("#if without #endif before line %d", line+lines)
			}
			j = lineEnd + 1
			lines++
		}
		l.out = append(l.out, tok{k: 'x', s: string(b[start:j]), line: line, endLine: line + lines - 1})
		l.line = line + lines
		l.i = j
	case "endif":
		if len(l.open) == 0 {
			return fmt.Sprintf("#endif without #if on line %d", line)
		}
		add()
		l.conds = append(l.conds, [2]int{l.open[len(l.open)-1], len(l.out) - 1})
		l.open = l.open[:len(l.open)-1]
	case "pragma", "nullable", "define", "undef", "line", "error", "warning":
		add()
	default:
		return fmt.Sprintf("unknown preprocessor directive on line %d", line)
	}
	return ""
}

// stringStart reports whether a string literal starts at i: '"', or '$'s and at most one '@' followed by '"'.
func stringStart(b []byte, i int) bool {
	at := false
	for ; i < len(b); i++ {
		switch b[i] {
		case '"':
			return true
		case '$':
		case '@':
			if at {
				return false
			}
			at = true
		default:
			return false
		}
	}
	return false
}

// scanString scans the string literal at i (see stringStart) and returns the index after it (and after a u8 suffix), or -1.
func scanString(b []byte, i int) int {
	dollars, verbatim := 0, false
	for b[i] != '"' {
		if b[i] == '$' {
			dollars++
		} else {
			verbatim = true
		}
		i++
	}
	q := 0
	for i+q < len(b) && b[i+q] == '"' {
		q++
	}
	var end int
	switch {
	case q >= 3 && !verbatim:
		end = scanRaw(b, i, q, dollars)
	case dollars > 1:
		return -1
	case verbatim:
		end = scanVerbatim(b, i+1, dollars == 1)
	default:
		end = scanRegular(b, i+1, dollars == 1)
	}
	if end > 0 && end+1 < len(b) && (b[end] == 'u' || b[end] == 'U') && b[end+1] == '8' && !identPartAt(b, end+2) {
		end += 2
	}
	return end
}

func scanRegular(b []byte, j int, interp bool) int {
	for j < len(b) {
		switch b[j] {
		case '\\':
			j += 2
		case '\n':
			return -1
		case '"':
			return j + 1
		case '{':
			if !interp {
				j++
			} else if j+1 < len(b) && b[j+1] == '{' {
				j += 2
			} else if j = scanHole(b, j+1, 1); j < 0 {
				return -1
			}
		case '}':
			if interp && j+1 < len(b) && b[j+1] == '}' {
				j += 2
			} else {
				j++
			}
		default:
			j++
		}
	}
	return -1
}

func scanVerbatim(b []byte, j int, interp bool) int {
	for j < len(b) {
		switch b[j] {
		case '"':
			if j+1 < len(b) && b[j+1] == '"' {
				j += 2
			} else {
				return j + 1
			}
		case '{':
			if !interp {
				j++
			} else if j+1 < len(b) && b[j+1] == '{' {
				j += 2
			} else if j = scanHole(b, j+1, 1); j < 0 {
				return -1
			}
		case '}':
			if interp && j+1 < len(b) && b[j+1] == '}' {
				j += 2
			} else {
				j++
			}
		default:
			j++
		}
	}
	return -1
}

// scanRaw scans a raw string literal whose q opening quotes start at j; n > 0 for an interpolated raw string with n '$'.
func scanRaw(b []byte, j, q, n int) int {
	run := func(k int, c byte) int {
		r := 0
		for k+r < len(b) && b[k+r] == c {
			r++
		}
		return r
	}
	k := j + q
	for k < len(b) {
		switch {
		case b[k] == '"':
			r := run(k, '"')
			if r > q {
				return -1
			}
			if r == q {
				return k + q
			}
			k += r
		case n > 0 && b[k] == '{':
			r := run(k, '{')
			if r < n {
				k += r
			} else if k = scanHole(b, k+r, n); k < 0 {
				return -1
			}
		default:
			k++
		}
	}
	return -1
}

// scanHole scans the code of an interpolation hole starting at i up to its closing run of n '}' and returns the index after
// it, or -1. A ':' at depth 0 starts the format clause.
func scanHole(b []byte, i, n int) int {
	closeRun := func(i int) int {
		for k := 0; k < n; k++ {
			if i+k >= len(b) || b[i+k] != '}' {
				return -1
			}
		}
		return i + n
	}
	depth := 0
	for i < len(b) {
		c := b[i]
		switch {
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			j := bytes.Index(b[i+2:], []byte("*/"))
			if j < 0 {
				return -1
			}
			i += j + 4
		case stringStart(b, i):
			if i = scanString(b, i); i < 0 {
				return -1
			}
		case c == '\'':
			if i = scanChar(b, i); i < 0 {
				return -1
			}
		case c == '(' || c == '[' || c == '{':
			depth++
			i++
		case c == ')' || c == ']':
			if depth--; depth < 0 {
				return -1
			}
			i++
		case c == '}':
			if depth == 0 {
				return closeRun(i)
			}
			depth--
			i++
		case c == ':' && depth == 0:
			if i+1 < len(b) && b[i+1] == ':' {
				i += 2
				continue
			}
			for i < len(b) && b[i] != '}' {
				if b[i] == '\n' {
					return -1
				}
				i++
			}
			return closeRun(i)
		default:
			i++
		}
	}
	return -1
}

// scanChar scans the character literal at i and returns the index after it, or -1.
func scanChar(b []byte, i int) int {
	j := i + 1
	if j >= len(b) {
		return -1
	}
	if b[j] == '\\' {
		j += 2
		for j < len(b) && b[j] != '\'' && b[j] != '\n' && j-i < 12 {
			j++
		}
	} else {
		r, n := utf8.DecodeRune(b[j:])
		if r == '\n' || r == '\'' {
			return -1
		}
		j += n
	}
	if j < len(b) && b[j] == '\'' {
		return j + 1
	}
	return -1
}

func identStart(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.Is(unicode.Nl, r) }
func identPart(r rune) bool {
	return identStart(r) || unicode.IsDigit(r) || unicode.In(r, unicode.Mn, unicode.Mc, unicode.Pc, unicode.Cf)
}

func identStartAt(b []byte, i int) bool {
	if i >= len(b) {
		return false
	}
	if b[i] == '\\' && i+1 < len(b) && (b[i+1] == 'u' || b[i+1] == 'U') {
		return true
	}
	r, _ := utf8.DecodeRune(b[i:])
	return identStart(r)
}

func identPartAt(b []byte, i int) bool {
	if i >= len(b) {
		return false
	}
	r, _ := utf8.DecodeRune(b[i:])
	return identPart(r) || b[i] == '\\'
}

// ident consumes an identifier at l.i (unicode escapes included).
func (l *lexer) ident() {
	for l.i < len(l.b) {
		if l.b[l.i] == '\\' && l.i+1 < len(l.b) && l.b[l.i+1] == 'u' {
			l.i += 6
			continue
		}
		if l.b[l.i] == '\\' && l.i+1 < len(l.b) && l.b[l.i+1] == 'U' {
			l.i += 10
			continue
		}
		r, n := utf8.DecodeRune(l.b[l.i:])
		if !identPart(r) {
			break
		}
		l.i += n
	}
	if l.i > len(l.b) {
		l.i = len(l.b)
	}
}

var puncts = []string{"??=", "<<=", "=>", "==", "!=", "<=", ">=", "&&", "||", "++", "--", "+=", "-=", "*=", "/=", "%=",
	"&=", "|=", "^=", "??", "?.", "->", "::", ".."}

// common lexes identifiers, numbers and punctuation at l.i; it returns false for any other character. Non-ASCII whitespace
// is skipped.
func (l *lexer) common() bool {
	b := l.b
	c := b[l.i]
	start, line := l.i, l.line
	switch {
	case identStartAt(b, l.i):
		l.ident()
		l.emit('i', start, line)
	case c >= '0' && c <= '9' || c == '.' && l.i+1 < len(b) && b[l.i+1] >= '0' && b[l.i+1] <= '9':
		l.i++
		for l.i < len(b) {
			d := b[l.i]
			if d >= '0' && d <= '9' || d >= 'a' && d <= 'z' || d >= 'A' && d <= 'Z' || d == '_' {
				l.i++
				continue
			}
			if d == '.' && l.i+1 < len(b) && b[l.i+1] >= '0' && b[l.i+1] <= '9' {
				l.i++
				continue
			}
			if (d == '+' || d == '-') && strings.ContainsRune("eE", rune(b[l.i-1])) && !strings.HasPrefix(string(b[start:l.i]), "0x") {
				l.i++
				continue
			}
			break
		}
		l.emit('n', start, line)
	case c < 0x80:
		for _, p := range puncts {
			if bytes.HasPrefix(b[l.i:], []byte(p)) {
				if p == "?." && l.i+2 < len(b) && b[l.i+2] >= '0' && b[l.i+2] <= '9' {
					continue
				}
				l.i += len(p)
				l.emit('p', start, line)
				return true
			}
		}
		l.i++
		l.emit('p', start, line)
	default:
		r, n := utf8.DecodeRune(b[l.i:])
		if r == utf8.RuneError || !unicode.IsSpace(r) && r != 0xfeff {
			return false
		}
		l.i += n
	}
	return true
}

// ---------------------------------------------------------------------------------------------------------------------------
// Placement

// region is free code: tokens [s, e) inside the body of member decls[member].
type region struct {
	s, e   int
	member int
	what   string
}

// decl is a declaration over tokens [s, e), for refusal reasons and member spans. kind: 't' type, 'm' member, 'a' attribute,
// 'o' other. body is the token index of a type's '{' (-1 when none).
type decl struct {
	s, e int
	what string
	kind byte
	body int
}

// zone is where non-global using directives are (or would be) at the start of the compilation unit or of a namespace body.
type zone struct{ s, e, ord int }

type file struct {
	t       []tok
	conds   [][2]int
	regions []region
	decls   []decl
	zones   []zone
	types   []string
	unsure  bool
}

// inCond reports whether tokens [s, e) (or, when empty, the insertion point s) touch an #if ... #endif region.
func (f *file) inCond(s, e int) bool {
	for _, c := range f.conds {
		if e > s && s <= c[1] && e-1 >= c[0] || e == s && c[0] < s && s <= c[1] {
			return true
		}
	}
	return false
}

func (f *file) zoneOf(s, e int) int {
	for i, z := range f.zones {
		if z.s <= s && e <= z.e {
			return i
		}
	}
	return -1
}

func (f *file) regionOf(s, e int) int {
	for i, r := range f.regions {
		if r.s <= s && e <= r.e {
			return i
		}
	}
	return -1
}

// describe names the innermost declaration containing tokens [s, e) (or the insertion point s).
func (f *file) describe(s, e int) string {
	best := -1
	for i, d := range f.decls {
		in := d.s <= s && e <= d.e
		if s == e {
			in = d.s < s && s < d.e
		}
		if in && (best < 0 || d.e-d.s < f.decls[best].e-f.decls[best].s) {
			best = i
		}
	}
	if best < 0 {
		return "the compilation unit outside any declaration"
	}
	d := f.decls[best]
	switch {
	case d.kind != 'o' && s <= d.s && d.e <= e:
		return d.what + " (added or removed)"
	case d.kind == 'm' && strings.HasPrefix(d.what, "const "):
		return d.what
	case d.kind == 'm':
		return "the declaration of " + d.what + " outside its body"
	case d.kind == 't' && strings.HasPrefix(d.what, "enum "):
		return d.what
	case d.kind == 't' && d.body >= 0 && s > d.body:
		return "the members of " + d.what + " (a member added, removed or changed outside a body)"
	case d.kind == 't':
		return "the declaration of " + d.what
	}
	return d.what
}

type parser struct {
	t        []tok
	c        []int // token indices of code tokens (not directives or inactive sections)
	pair     []int // code index -> code index of the matching bracket, -1 otherwise
	k        int
	f        *file
	declared bool // a namespace or type was declared (top-level statements must precede them)
	bad      int  // code index where placement failed, -1 when fine
}

// analyze lexes and places src; why != "" when it cannot.
func analyze(src []byte) (*file, string) {
	l, why := lex(src)
	if why != "" {
		return nil, why
	}
	f := &file{t: l.out, conds: l.conds}
	p := &parser{t: l.out, f: f, bad: -1}
	for i, x := range l.out {
		if x.k != 'd' && x.k != 'x' {
			p.c = append(p.c, i)
		}
	}
	if k := p.pairs(); k >= 0 {
		return nil, fmt.Sprintf("unbalanced bracket on line %d", p.line(k))
	}
	p.namespaceBody("", false, true)
	if p.bad >= 0 {
		return nil, fmt.Sprintf("cannot place the declaration on line %d", p.line(p.bad))
	}
	for i, x := range f.t {
		if x.k == 'x' && typeWord.MatchString(x.s) && !f.inType(i) {
			f.unsure = true
		}
	}
	return f, ""
}

var typeWord = regexp.MustCompile(`\b(class|struct|interface|enum|record|delegate)\b`)

func (f *file) inType(i int) bool {
	for _, d := range f.decls {
		if d.kind == 't' && d.body >= 0 && d.body < i && i < d.e {
			return true
		}
	}
	return false
}

func (p *parser) line(k int) int {
	if k < len(p.c) {
		return p.t[p.c[k]].line
	}
	if len(p.t) > 0 {
		return p.t[len(p.t)-1].endLine
	}
	return 1
}

// pairs matches brackets; it returns the code index of the first unbalanced one, or -1.
func (p *parser) pairs() int {
	p.pair = make([]int, len(p.c))
	var stack []int
	for k := range p.c {
		p.pair[k] = -1
		s := p.s(k)
		switch s {
		case "(", "[", "{":
			stack = append(stack, k)
		case ")", "]", "}":
			if len(stack) == 0 {
				return k
			}
			o := stack[len(stack)-1]
			if map[string]string{")": "(", "]": "[", "}": "{"}[s] != p.s(o) {
				return o
			}
			stack = stack[:len(stack)-1]
			p.pair[k], p.pair[o] = o, k
		}
	}
	if len(stack) > 0 {
		return stack[len(stack)-1]
	}
	return -1
}

// s returns the text of code token k when it is an identifier, keyword or punctuation; "" otherwise.
func (p *parser) s(k int) string {
	if k >= 0 && k < len(p.c) {
		if x := p.t[p.c[k]]; x.k == 'i' || x.k == 'p' {
			return x.s
		}
	}
	return ""
}

func (p *parser) isIdent(k int) bool { return k >= 0 && k < len(p.c) && p.t[p.c[k]].k == 'i' }
func (p *parser) name(k int) string  { return strings.TrimPrefix(p.t[p.c[k]].s, "@") }

// orig is the token index of code token k (len(t) at the end).
func (p *parser) orig(k int) int {
	if k < len(p.c) {
		return p.c[k]
	}
	return len(p.t)
}

func (p *parser) fail(k int) {
	if p.bad < 0 {
		p.bad = k
	}
}

// decl records a declaration from code token k0 to the token before p.k.
func (p *parser) decl(k0 int, what string, kind byte) int {
	p.f.decls = append(p.f.decls, decl{s: p.orig(k0), e: p.c[p.k-1] + 1, what: what, kind: kind, body: -1})
	return len(p.f.decls) - 1
}

// region records free code strictly between code tokens ko and kc.
func (p *parser) region(ko, kc, member int, what string) {
	p.f.regions = append(p.f.regions, region{s: p.c[ko] + 1, e: p.c[kc], member: member, what: what})
}

// toSemi advances past the next ';' at bracket depth 0.
func (p *parser) toSemi() {
	for p.k < len(p.c) {
		s := p.s(p.k)
		switch {
		case s == ";":
			p.k++
			return
		case p.pair[p.k] > p.k:
			p.k = p.pair[p.k] + 1
		case s == ")" || s == "]" || s == "}":
			p.fail(p.k)
			return
		default:
			p.k++
		}
	}
	p.fail(p.k)
}

// exprEnd returns the code index of the ';' (or, with declarators, the ',' starting the next variable declarator) that ends
// the expression starting at p.k; -1 when there is none.
func (p *parser) exprEnd(declarators bool) int {
	for k := p.k; k < len(p.c); k++ {
		s := p.s(k)
		switch {
		case s == ";":
			return k
		case s == "," && declarators && p.isIdent(k+1) && (p.s(k+2) == "=" || p.s(k+2) == "," || p.s(k+2) == ";"):
			return k
		case p.pair[k] > k:
			k = p.pair[k]
		case s == ")" || s == "]" || s == "}":
			return -1
		}
	}
	return -1
}

func (p *parser) attributes() {
	for p.s(p.k) == "[" {
		k0 := p.k
		p.k = p.pair[p.k] + 1
		p.decl(k0, "an attribute", 'a')
	}
}

var modifiers = map[string]bool{"public": true, "private": true, "protected": true, "internal": true, "static": true,
	"readonly": true, "const": true, "volatile": true, "virtual": true, "override": true, "abstract": true, "sealed": true,
	"extern": true, "unsafe": true, "new": true, "partial": true, "async": true, "required": true, "file": true, "ref": true,
	"event": true, "fixed": true}

func (p *parser) modifiers() map[string]bool {
	m := map[string]bool{}
	for modifiers[p.s(p.k)] && p.s(p.k+1) != "(" && p.s(p.k+1) != "=" && p.s(p.k+1) != ";" {
		m[p.s(p.k)] = true
		p.k++
	}
	return m
}

// typeStart reports whether a type declaration keyword is at code index k.
func (p *parser) typeStart(k int) bool {
	switch p.s(k) {
	case "class", "struct", "interface", "enum", "delegate":
		return true
	case "record":
		return p.isIdent(k + 1)
	}
	return false
}

// declAhead reports whether a namespace or type declaration starts at k (after attributes and modifiers).
func (p *parser) declAhead(k int) bool {
	for p.s(k) == "[" {
		k = p.pair[k] + 1
	}
	for modifiers[p.s(k)] {
		k++
	}
	return p.s(k) == "namespace" || p.typeStart(k) && p.s(k) != "delegate"
}

func qualify(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "." + name
}

// namespaceBody parses the members of the compilation unit or of a namespace up to its closing '}' (block) or the end.
func (p *parser) namespaceBody(ns string, block, top bool) {
	z := len(p.f.zones)
	p.f.zones = append(p.f.zones, zone{s: p.orig(p.k), e: p.orig(p.k), ord: z})
	open := true // the using zone is still being extended
	for p.bad < 0 {
		if p.k >= len(p.c) {
			if block {
				p.fail(p.k)
			}
			return
		}
		if block && p.s(p.k) == "}" {
			return
		}
		k0 := p.k
		switch {
		case p.s(k0) == ";":
			p.k++
			continue
		case p.s(k0) == "extern" && p.s(k0+1) == "alias", p.s(k0) == "global" && p.s(k0+1) == "using":
			p.toSemi()
			what := "an extern alias"
			if p.s(k0) == "global" {
				what = "a global using directive"
			}
			p.decl(k0, what, 'o')
			if open && p.f.zones[z].s == p.f.zones[z].e {
				p.f.zones[z].s, p.f.zones[z].e = p.orig(p.k), p.orig(p.k)
			} else {
				open = false
			}
			continue
		case p.s(k0) == "using" && !(top && !p.declared && p.usingStatement(k0)):
			p.toSemi()
			// Using directives are never free: a namespace import or an alias decides which type a name in this file's
			// signatures, constants and attributes binds to (metadata no record sees), not only what bodies call.
			what := "a using directive"
			if p.usingAlias(k0) {
				what = "a using alias directive"
			}
			p.decl(k0, what, 'o')
			open = false
			continue
		case p.s(k0) == "[" && (p.s(k0+1) == "assembly" || p.s(k0+1) == "module") && p.s(k0+2) == ":":
			p.k = p.pair[k0] + 1
			p.decl(k0, "an assembly attribute", 'o')
			open = false
			continue
		}
		open = false
		p.attributes()
		mods := p.modifiers()
		switch {
		case p.s(p.k) == "namespace":
			if len(mods) > 0 || p.k != k0 {
				p.fail(p.k)
				return
			}
			p.k++
			name := p.dotted()
			full := qualify(ns, name)
			if name == "" {
				p.fail(p.k)
				return
			}
			p.declared = true
			if p.s(p.k) == ";" {
				p.k++
				p.namespaceBody(full, false, false)
				if p.bad < 0 {
					p.decl(k0, "namespace "+full, 'o')
				}
				return
			}
			if p.s(p.k) != "{" {
				p.fail(p.k)
				return
			}
			p.k++
			p.namespaceBody(full, true, false)
			if p.bad >= 0 {
				return
			}
			p.k++
			p.decl(k0, "namespace "+full, 'o')
		case p.typeStart(p.k):
			p.declared = true
			p.typeDecl(k0, ns, true)
		case top && !p.declared:
			p.k = k0
			p.statements()
			p.decl(k0, "top-level statements", 'o')
		default:
			p.fail(k0)
		}
	}
}

// usingStatement reports whether the 'using' at k starts a statement (top-level statements: "using (...)" or
// "using Type name = ...") rather than a using directive ("using A.B;", "using static A.B;", "using X = A.B;").
func (p *parser) usingStatement(k int) bool {
	if p.s(k+1) == "(" {
		return true
	}
	if p.s(k+1) == "static" || p.s(k+1) == "unsafe" || p.s(k+1) == "global" {
		return false
	}
	return p.isIdent(k+1) && p.s(k+2) != "=" && p.s(k+2) != ";" && p.s(k+2) != "." && p.s(k+2) != "::"
}

// usingAlias reports whether the using directive at k is an alias ("using X = A.B;", "using unsafe X = ...;"): an '='
// before the directive's ';' at bracket depth 0. Call it after p.toSemi() moved past the directive.
func (p *parser) usingAlias(k int) bool {
	for i := k + 1; i < p.k && p.s(i) != ";"; i++ {
		if p.s(i) == "=" {
			return true
		}
		if p.pair[i] > i {
			i = p.pair[i]
		}
	}
	return false
}

func (p *parser) dotted() string {
	var parts []string
	for p.isIdent(p.k) {
		parts = append(parts, p.name(p.k))
		p.k++
		if p.s(p.k) != "." {
			break
		}
		p.k++
	}
	return strings.Join(parts, ".")
}

// statements skips top-level statements up to the first namespace or type declaration.
func (p *parser) statements() {
	k0 := p.k
	for p.k < len(p.c) {
		if p.k > k0 && (p.s(p.k-1) == ";" || p.s(p.k-1) == "}") && p.declAhead(p.k) {
			return
		}
		if p.pair[p.k] > p.k {
			p.k = p.pair[p.k] + 1
		} else {
			p.k++
		}
	}
}

// typeDecl parses a type declaration whose keyword is at p.k (attributes and modifiers from k0).
func (p *parser) typeDecl(k0 int, ns string, top bool) {
	kw := p.s(p.k)
	p.k++
	if kw == "delegate" {
		h0 := p.k
		p.toSemi()
		if p.bad >= 0 {
			return
		}
		name, _ := p.nameBeforeParen(h0, p.k-1)
		if name == "" {
			p.fail(h0)
			return
		}
		p.decl(k0, "delegate "+name, 't')
		if top {
			p.f.types = append(p.f.types, qualify(ns, name))
		}
		return
	}
	if kw == "record" && (p.s(p.k) == "class" || p.s(p.k) == "struct") {
		kw += " " + p.s(p.k)
		p.k++
	}
	if !p.isIdent(p.k) {
		p.fail(p.k)
		return
	}
	name := p.name(p.k)
	for p.k++; ; {
		if p.k >= len(p.c) {
			p.fail(p.k)
			return
		}
		s := p.s(p.k)
		if s == "{" || s == ";" {
			break
		}
		if s == ")" || s == "]" || s == "}" {
			p.fail(p.k)
			return
		}
		if p.pair[p.k] > p.k {
			p.k = p.pair[p.k] + 1
		} else {
			p.k++
		}
	}
	body := -1
	if p.s(p.k) == ";" {
		p.k++
	} else {
		body = p.c[p.k]
		if kw == "enum" {
			p.k = p.pair[p.k] + 1
		} else {
			p.k++
			p.typeBody(name)
			if p.bad >= 0 {
				return
			}
			p.k++
		}
		if p.s(p.k) == ";" {
			p.k++
		}
	}
	d := p.decl(k0, kw+" "+name, 't')
	p.f.decls[d].body = body
	if top {
		p.f.types = append(p.f.types, qualify(ns, name))
	}
}

// typeBody parses the members of a class, struct, record, interface or extension block up to its closing '}'.
func (p *parser) typeBody(typeName string) {
	for p.bad < 0 {
		if p.k >= len(p.c) {
			p.fail(p.k)
			return
		}
		switch p.s(p.k) {
		case "}":
			return
		case ";":
			p.k++
			continue
		}
		k0 := p.k
		p.attributes()
		mods := p.modifiers()
		switch {
		case p.typeStart(p.k):
			p.typeDecl(k0, "", false)
		case p.s(p.k) == "extension" && (p.s(p.k+1) == "(" || p.s(p.k+1) == "<"):
			for p.k < len(p.c) && p.s(p.k) != "{" {
				if p.pair[p.k] > p.k {
					p.k = p.pair[p.k]
				}
				p.k++
			}
			if p.k >= len(p.c) {
				p.fail(p.k)
				return
			}
			body := p.c[p.k]
			p.k++
			p.typeBody(typeName)
			if p.bad >= 0 {
				return
			}
			p.k++
			d := p.decl(k0, "extension block", 't')
			p.f.decls[d].body = body
		default:
			p.member(k0, mods, typeName)
		}
	}
}

// nameBeforeParen returns the identifier before the first parameter list in code tokens [h0, h1), skipping a type
// parameter list, and its code index.
func (p *parser) nameBeforeParen(h0, h1 int) (string, int) {
	for k := h0; k < h1; k++ {
		switch p.s(k) {
		case "(":
			j := k - 1
			if j >= h0 && p.s(j) == ">" {
				for d := 0; j >= h0; j-- {
					if p.s(j) == ">" {
						d++
					} else if p.s(j) == "<" {
						if d--; d == 0 {
							break
						}
					}
				}
				j--
			}
			if j >= h0 && p.isIdent(j) {
				return p.name(j), j
			}
			k = p.pair[k]
		case "[":
			k = p.pair[k]
		}
	}
	return "", -1
}

// memberKind classifies the member whose header is code tokens [h0, h1) ending with terminator term.
func (p *parser) memberKind(h0, h1 int, mods map[string]bool, term, typeName string) (kind, name string, methodLike bool) {
	methodLike = p.s(h1-1) == ")"
	lastIdent := ""
	for k := h0; k < h1; k++ {
		switch {
		case p.s(k) == "where" && k > h0:
			methodLike = true
		case p.s(k) == "operator":
			kind, name = "operator", "operator "+p.t[p.orig(k+1)].s
		case p.s(k) == "this" && p.s(k+1) == "[":
			if kind == "" {
				kind, name = "indexer", "this[]"
			}
		case p.pair[k] > k:
			k = p.pair[k]
		case p.isIdent(k):
			lastIdent = p.name(k)
		}
	}
	switch {
	case mods["const"] || mods["fixed"]:
		return "const field", lastIdent, false
	case kind == "operator":
		return kind, name, true
	case kind == "indexer":
		return kind, name, false
	case methodLike:
		n, j := p.nameBeforeParen(h0, h1)
		switch {
		case j > h0 && p.s(j-1) == "~":
			return "finalizer", "~" + n, true
		case n == typeName:
			return "constructor", n, true
		}
		return "method", n, true
	case mods["event"]:
		return "event", lastIdent, false
	case term == "=" || term == ";":
		return "field", lastIdent, false
	}
	return "property", lastIdent, false
}

// member parses a member whose header starts at p.k (attributes and modifiers from k0).
func (p *parser) member(k0 int, mods map[string]bool, typeName string) {
	h0 := p.k
	for {
		if p.k >= len(p.c) {
			p.fail(p.k)
			return
		}
		s := p.s(p.k)
		if s == "{" || s == ";" || s == "=>" || s == "=" {
			break
		}
		if s == "}" || s == ")" || s == "]" {
			p.fail(p.k)
			return
		}
		if p.pair[p.k] > p.k {
			p.k = p.pair[p.k] + 1
		} else {
			p.k++
		}
	}
	if p.k == h0 {
		p.fail(p.k)
		return
	}
	term := p.s(p.k)
	kind, name, methodLike := p.memberKind(h0, p.k, mods, term, typeName)
	what := kind + " " + name
	m := len(p.f.decls)
	p.f.decls = append(p.f.decls, decl{what: what, kind: 'm', body: -1})
	switch term {
	case ";":
		p.k++
	case "=>":
		ko := p.k
		p.k++
		kc := p.exprEnd(false)
		if kc < 0 {
			p.fail(ko)
			return
		}
		p.region(ko, kc, m, what)
		p.k = kc + 1
	case "=":
		if kind == "const field" {
			p.toSemi()
			break
		}
		for p.bad < 0 {
			ko := p.k
			p.k++
			kc := p.exprEnd(true)
			if kc < 0 {
				p.fail(ko)
				return
			}
			p.region(ko, kc, m, "initializer of "+kind+" "+name)
			p.k = kc + 1
			if p.s(kc) == ";" {
				break
			}
			name = p.name(p.k)
			p.k++
			for p.s(p.k) == "," && p.isIdent(p.k+1) {
				name = p.name(p.k + 1)
				p.k += 2
			}
			if p.s(p.k) == ";" {
				p.k++
				break
			}
			if p.s(p.k) != "=" {
				p.fail(p.k)
				return
			}
		}
	case "{":
		if methodLike {
			ko := p.k
			p.region(ko, p.pair[ko], m, what)
			p.k = p.pair[ko] + 1
			break
		}
		p.accessors(m, name)
		if p.s(p.k) == "=" {
			ko := p.k
			p.k++
			kc := p.exprEnd(false)
			if kc < 0 {
				p.fail(ko)
				return
			}
			p.region(ko, kc, m, "initializer of "+kind+" "+name)
			p.k = kc + 1
		}
	}
	if p.bad >= 0 {
		return
	}
	p.f.decls[m].s, p.f.decls[m].e = p.orig(k0), p.c[p.k-1]+1
}

var accessorMods = map[string]bool{"public": true, "private": true, "protected": true, "internal": true, "readonly": true}

// accessors parses the accessor list of a property, indexer or event at p.k ('{'). An accessor list it does not understand
// has no free code.
func (p *parser) accessors(m int, name string) {
	closing := p.pair[p.k]
	mark, dmark := len(p.f.regions), len(p.f.decls)
	defer func() { p.k = closing + 1 }()
	p.k++
	for p.k < closing {
		p.attributes()
		for accessorMods[p.s(p.k)] {
			p.k++
		}
		acc := p.s(p.k)
		switch acc {
		case "get", "set", "init", "add", "remove":
		default:
			p.f.regions, p.f.decls = p.f.regions[:mark], p.f.decls[:dmark]
			return
		}
		p.k++
		what := "accessor " + acc + " of " + name
		switch p.s(p.k) {
		case ";":
			p.k++
		case "=>":
			ko := p.k
			p.k++
			kc := p.exprEnd(false)
			if kc < 0 || kc > closing {
				p.f.regions, p.f.decls = p.f.regions[:mark], p.f.decls[:dmark]
				return
			}
			p.region(ko, kc, m, what)
			p.k = kc + 1
		case "{":
			p.region(p.k, p.pair[p.k], m, what)
			p.k = p.pair[p.k] + 1
		default:
			p.f.regions, p.f.decls = p.f.regions[:mark], p.f.decls[:dmark]
			return
		}
	}
}

// ---------------------------------------------------------------------------------------------------------------------------
// Diff (copied from internal/dynamic/jvmdyn; here applied to token keys)

type hunk struct{ b0, b1, h0, h1 int } // base items [b0,b1), head items [h0,h1), 0-based

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

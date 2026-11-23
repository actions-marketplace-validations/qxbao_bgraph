package rbdyn

// Body guard of the Ruby method layer (docs/dynamic-layer.md §10.2). A Ruby record lists, per loaded repository file, the
// methods defined by `def` and how many times each ran (Ruby's Coverage, methods mode). A change can only narrow when such a
// record sees its effect: the body of a method defined by `def` runs only when the method is called, so a test that never
// called it is unaffected. Everything else in a file (class and module bodies, constants, attr_*, include, alias,
// define_method and DSL blocks, signatures and default values, BEGIN/END, the __END__ data, magic comments) runs or matters
// when the file is loaded, so the guard refuses it. The guard is lexical (internal/scan/ruby's lexer) and fail-closed: a file
// the lexer cannot read with certainty, or whose block structure it cannot follow, refuses.
//
// Comparison: each line is keyed by its tokens (kind, text, preceding white space; comments and layout dropped; a literal
// spanning lines and a heredoc with its body belong to their first line). The "skeleton" (every keyed line that is not
// strictly inside the body of a `def ... end`) must be identical at base and head; the outermost methods are then paired in
// order and each pair whose body keys differ is a changed member, identified by its base line span (the `def` line and the
// `end` line, which is how Coverage names it).

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	rbscan "github.com/qxbao/bgraph/internal/scan/ruby"
)

// Span is a changed method of the base file: lines From (the `def` line) to To (the `end` line), and its description.
type Span struct {
	From, To int
	What     string
}

type frameKind uint8

const (
	fClass frameKind = iota + 1
	fModule
	fSClass
	fDef
	fDo    // do ... end block
	fKw    // if / unless / while / until / case / begin / for ... end
	fParen // ( or [
	fBrace // {
)

type frame struct {
	kind  frameKind
	label string
	m     *method // fDef
}

// method is one `def` of a file.
type method struct {
	id                 string // nesting path, singleton-ness and name
	disp               string // Module::Class#name, Module.name
	start, sigEnd, end int    // lines: the def keyword, the last line of the signature, the matching end (0: endless)
	endless            bool
	nested             bool // contains a def, or lies inside one
	outer              bool // not inside another def
}

// rbFile is the guard's view of one version of a file.
type rbFile struct {
	lines   []int          // keyed lines, ascending
	keys    map[int]string // line -> key
	methods []*method      // in source order
	magic   []string       // magic comment lines and the shebang
	data    []byte         // the __END__ line and everything after it (nil: none)
}

var magicComment = regexp.MustCompile(`(?i)^\s*#.*\b(?:frozen[-_]string[-_]literal|(?:en)?coding|warn[-_]indent|warn[-_]past[-_]scope|shareable[-_]constant[-_]value)\s*[:=]`)

// keywords that open a block closed by `end` or that cannot appear inside a signature.
var blockKeywords = map[string]bool{"class": true, "module": true, "def": true, "if": true, "unless": true, "while": true,
	"until": true, "case": true, "begin": true, "for": true, "do": true, "end": true}

// openerAfter: after these keywords an `if` / `unless` / `while` / `until` starts a statement (it is not a modifier).
var openerAfter = map[string]bool{"then": true, "else": true, "elsif": true, "do": true, "begin": true, "ensure": true, "and": true,
	"or": true, "not": true, "when": true, "in": true, "if": true, "unless": true, "while": true, "until": true, "case": true}

func parseRuby(src []byte) (*rbFile, string) {
	toks, data, why := rbscan.Tokens(src)
	if why != "" {
		return nil, "the lexer cannot read the file with certainty (" + why + ")"
	}
	f := &rbFile{keys: map[int]string{}}
	if data >= 0 {
		f.data = src[data:]
	}
	for i, l := range strings.Split(string(src), "\n") {
		if i == 0 && strings.HasPrefix(l, "#!") || magicComment.MatchString(l) {
			f.magic = append(f.magic, strings.TrimSpace(l))
		}
	}
	var sb strings.Builder
	for _, t := range toks {
		if t.Kind == rbscan.TokNL {
			continue
		}
		k := f.keys[t.Line]
		sb.Reset()
		sb.WriteString(k)
		if k != "" {
			sb.WriteByte(1)
		}
		sb.WriteByte(byte(t.Kind) + '0')
		if t.Space {
			sb.WriteByte('_')
		}
		sb.WriteString(t.Text)
		if k == "" {
			f.lines = append(f.lines, t.Line)
		}
		f.keys[t.Line] = sb.String()
	}
	if why := f.structure(toks); why != "" {
		return nil, why
	}
	return f, ""
}

func isOp(t rbscan.Token, s string) bool { return t.Kind == rbscan.TokOp && t.Text == s }

// keywordAt reports whether toks[i] is a keyword use of an identifier: not a method name after `.`, `&.` or `::`, not a label
// (`if:`).
func keywordAt(toks []rbscan.Token, i int) bool {
	t := toks[i]
	if t.Kind != rbscan.TokIdent {
		return false
	}
	if i > 0 && (isOp(toks[i-1], ".") || isOp(toks[i-1], "&.") || isOp(toks[i-1], "::")) {
		return false
	}
	if i+1 < len(toks) && isOp(toks[i+1], ":") && !toks[i+1].Space {
		return false
	}
	return true
}

// statementStart reports whether toks[i] (if / unless / while / until) starts a statement rather than being a modifier.
func statementStart(toks []rbscan.Token, i int) bool {
	if i == 0 {
		return true
	}
	p := toks[i-1]
	switch p.Kind {
	case rbscan.TokNL:
		return true
	case rbscan.TokOp:
		return p.Text != ")" && p.Text != "]" && p.Text != "}"
	case rbscan.TokIdent:
		return openerAfter[p.Text] && keywordAt(toks, i-1)
	}
	return false
}

// structure follows the block structure of the file and fills f.methods.
func (f *rbFile) structure(toks []rbscan.Token) string {
	var stack []frame
	pendingDo := -1 // stack depth at which a `do` belongs to an open while / until / for
	path := func() string {
		var parts []string
		for _, fr := range stack {
			parts = append(parts, fr.label)
		}
		return strings.Join(parts, " ")
	}
	singletonScope := func() bool {
		for i := len(stack) - 1; i >= 0; i-- {
			switch stack[i].kind {
			case fSClass:
				return true
			case fClass, fModule, fDef:
				return false
			}
		}
		return false
	}
	inDef := func() *method {
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].kind == fDef {
				return stack[i].m
			}
		}
		return nil
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.Kind == rbscan.TokNL || isOp(t, ";") {
			pendingDo = -1
			continue
		}
		if t.Kind == rbscan.TokOp {
			switch t.Text {
			case "(", "[":
				stack = append(stack, frame{kind: fParen, label: "("})
			case "{":
				stack = append(stack, frame{kind: fBrace, label: "{"})
			case ")", "]":
				if len(stack) == 0 || stack[len(stack)-1].kind != fParen {
					return fmt.Sprintf("line %d: unbalanced %s", t.Line, t.Text)
				}
				stack = stack[:len(stack)-1]
			case "}":
				if len(stack) == 0 || stack[len(stack)-1].kind != fBrace {
					return fmt.Sprintf("line %d: unbalanced }", t.Line)
				}
				stack = stack[:len(stack)-1]
			}
			continue
		}
		if !keywordAt(toks, i) || !blockKeywords[t.Text] {
			continue
		}
		switch t.Text {
		case "class", "module":
			kind, label := fClass, "class "
			if t.Text == "module" {
				kind, label = fModule, "module "
			}
			j := i + 1
			if t.Text == "class" && j < len(toks) && isOp(toks[j], "<<") {
				kind, label = fSClass, "class<<"
				j++
			}
			for ; j < len(toks) && toks[j].Kind != rbscan.TokNL && !isOp(toks[j], ";") && !isOp(toks[j], "<"); j++ {
				label += toks[j].Text
			}
			stack = append(stack, frame{kind: kind, label: label})
		case "def":
			m, next, why := signature(toks, i)
			if why != "" {
				return fmt.Sprintf("line %d: %s", t.Line, why)
			}
			if outer := inDef(); outer != nil {
				outer.nested, m.nested = true, true
				for _, fr := range stack {
					if fr.kind == fDef {
						fr.m.nested = true
					}
				}
			} else {
				m.outer = true
			}
			kind := "#"
			if singletonScope() || strings.Contains(m.id, ".") {
				kind = "."
			}
			var names []string
			for _, fr := range stack {
				if fr.kind == fClass || fr.kind == fModule {
					names = append(names, strings.TrimSpace(fr.label[strings.IndexByte(fr.label, ' ')+1:]))
				}
			}
			m.disp = strings.Join(names, "::") + kind + strings.TrimPrefix(m.id, "self.")
			if recv, _, ok := strings.Cut(m.id, "."); ok && recv != "self" {
				m.disp = m.id
			}
			m.id = path() + " " + kind + m.id
			f.methods = append(f.methods, m)
			if !m.endless {
				stack = append(stack, frame{kind: fDef, label: "def " + m.id, m: m})
			}
			i = next - 1
		case "end":
			if len(stack) == 0 {
				return fmt.Sprintf("line %d: end without a block", t.Line)
			}
			top := stack[len(stack)-1]
			switch top.kind {
			case fParen, fBrace:
				return fmt.Sprintf("line %d: end inside an open bracket", t.Line)
			case fDef:
				top.m.end = t.Line
			}
			stack = stack[:len(stack)-1]
		case "do":
			if pendingDo == len(stack) {
				pendingDo = -1
				continue
			}
			stack = append(stack, frame{kind: fDo, label: "do"})
		case "if", "unless", "while", "until":
			if !statementStart(toks, i) {
				continue
			}
			stack = append(stack, frame{kind: fKw, label: t.Text})
			if t.Text == "while" || t.Text == "until" {
				pendingDo = len(stack)
			}
		case "for":
			stack = append(stack, frame{kind: fKw, label: "for"})
			pendingDo = len(stack)
		case "case", "begin":
			stack = append(stack, frame{kind: fKw, label: t.Text})
		}
	}
	if len(stack) > 0 {
		return fmt.Sprintf("unbalanced block structure (%d blocks open at the end of the file)", len(stack))
	}
	return ""
}

// signature reads the signature of the def at toks[i]: name (with its receiver), parameters, endless form. next is the index of
// the first token after the signature.
func signature(toks []rbscan.Token, i int) (m *method, next int, why string) {
	m = &method{start: toks[i].Line}
	j := i + 1
	if j >= len(toks) || toks[j].Kind == rbscan.TokNL {
		return nil, 0, "def without a name"
	}
	if isOp(toks[j], "(") {
		return nil, 0, "def on a parenthesized receiver (unsupported)"
	}
	name := toks[j]
	if j+2 < len(toks) && (isOp(toks[j+1], ".") || isOp(toks[j+1], "::")) {
		m.id = toks[j].Text + "."
		j += 2
		name = toks[j]
	}
	if name.Kind != rbscan.TokIdent {
		return nil, 0, "def with an unexpected method name " + name.Text
	}
	m.id += name.Text
	m.sigEnd = name.Line
	k := j + 1
	// A setter: `def x=(v)` (no space before = or before the parenthesis).
	if k+1 < len(toks) && isOp(toks[k], "=") && !toks[k].Space && isOp(toks[k+1], "(") && !toks[k+1].Space {
		m.id += "="
		k++
	}
	params := func(k int, paren bool) (int, string) {
		depth := 0
		for ; k < len(toks); k++ {
			t := toks[k]
			if !paren && depth == 0 && (t.Kind == rbscan.TokNL || isOp(t, ";")) {
				return k, ""
			}
			if t.Kind == rbscan.TokNL {
				continue
			}
			m.sigEnd = t.Line
			if t.Kind == rbscan.TokOp {
				switch t.Text {
				case "(", "[", "{":
					depth++
				case ")", "]", "}":
					depth--
					if paren && depth == 0 {
						return k + 1, ""
					}
					if depth < 0 {
						return k, "unbalanced parameter list"
					}
				}
				continue
			}
			if keywordAt(toks, k) && blockKeywords[t.Text] {
				return k, "keyword " + t.Text + " inside a parameter list"
			}
		}
		return k, "unterminated parameter list"
	}
	switch {
	case k >= len(toks):
		return m, k, ""
	case isOp(toks[k], "("):
		var w string
		if k, w = params(k, true); w != "" {
			return nil, 0, w
		}
	case toks[k].Kind == rbscan.TokNL || isOp(toks[k], ";"):
	case isOp(toks[k], "="):
	default:
		var w string
		if k, w = params(k, false); w != "" {
			return nil, 0, w
		}
	}
	if k < len(toks) && isOp(toks[k], "=") {
		m.endless = true
		return m, k + 1, ""
	}
	return m, k, ""
}

// body returns the keys of the lines strictly inside m's body.
func (f *rbFile) body(m *method) []string {
	var out []string
	for _, l := range f.lines {
		if l > m.sigEnd && l < m.end {
			out = append(out, f.keys[l])
		}
	}
	return out
}

// skeleton returns the keyed lines that are not strictly inside a method body.
func (f *rbFile) skeleton() []int {
	var out []int
	for _, l := range f.lines {
		in := false
		for _, m := range f.methods {
			if !m.endless && m.outer && l > m.sigEnd && l < m.end {
				in = true
				break
			}
		}
		if !in {
			out = append(out, l)
		}
	}
	return out
}

func (f *rbFile) outerMethods() []*method {
	var out []*method
	for _, m := range f.methods {
		if m.outer && !m.endless {
			out = append(out, m)
		}
	}
	return out
}

// BodyGuard returns "" when every change from base to head of Ruby file p lies strictly inside the body of a method defined by
// `def ... end` (or only touches comments and layout); otherwise the reason (one line, starting with p). members are the
// changed methods (base line spans), once each.
func BodyGuard(p string, base, head []byte) (members []Span, why string) {
	if bytes.Equal(base, head) {
		return nil, ""
	}
	fb, w := parseRuby(base)
	if w != "" {
		return nil, p + ": base: " + w
	}
	fh, w := parseRuby(head)
	if w != "" {
		return nil, p + ": head: " + w
	}
	if !bytes.Equal(fb.data, fh.data) {
		return nil, p + ": the __END__ data section changed (DATA is read when the file runs)"
	}
	if strings.Join(fb.magic, "\n") != strings.Join(fh.magic, "\n") {
		return nil, p + ": a magic comment or the shebang changed (it applies to the whole file)"
	}
	sb, sh := fb.skeleton(), fh.skeleton()
	for i := 0; i < len(sb) || i < len(sh); i++ {
		switch {
		case i >= len(sb):
			return nil, fmt.Sprintf("%s:%d: code outside method bodies added (class or module level code, a signature or a def/end line)", p, sh[i])
		case i >= len(sh):
			return nil, fmt.Sprintf("%s:%d: code outside method bodies removed (class or module level code, a signature or a def/end line)", p, sb[i])
		case fb.keys[sb[i]] != fh.keys[sh[i]]:
			return nil, fmt.Sprintf("%s:%d: code outside method bodies changed (class or module level code, a signature, a default value, a def/end line or a heredoc starting outside a method)", p, sb[i])
		}
	}
	mb, mh := fb.outerMethods(), fh.outerMethods()
	if len(mb) != len(mh) {
		return nil, fmt.Sprintf("%s: %d methods at the base, %d at the head", p, len(mb), len(mh))
	}
	for i, b := range mb {
		h := mh[i]
		if b.id != h.id {
			return nil, fmt.Sprintf("%s:%d: method %s became %s", p, b.start, b.id, h.id)
		}
		if strings.Join(fb.body(b), "\n") == strings.Join(fh.body(h), "\n") {
			continue
		}
		if b.nested || h.nested {
			return nil, fmt.Sprintf("%s:%d: the changed method %s contains a def (an inner def runs when the outer method runs)", p, b.start, b.disp)
		}
		members = append(members, Span{From: b.start, To: b.end, What: "method " + b.disp})
	}
	return members, ""
}

package ruby

import (
	"path"
	"strings"
)

// base says what a path value is relative to.
type base uint8

const (
	bStr  base = iota + 1 // a plain string: a feature name, or a path relative to an unknown working directory
	bRel                  // relative to the directory of the scanned file
	bRoot                 // relative to the project root (Rails.root)
	bEnv                  // ENV[...]: only meaningful as the left side of ||
)

// selfMark stands for the scanned file itself inside a bRel path (__FILE__): File.expand_path("../x", __FILE__) is "x".
const selfMark = "\x00file"

// pval is a value of the path evaluator.
type pval struct {
	b   base
	s   string
	pn  bool   // a Pathname: + and / join
	dyn bool   // only the static prefix s is known (s is then a string prefix, not necessarily a whole segment)
	suf string // with dyn: the static text known to follow the unknown part ("" when nothing is known)
}

// fullyDynamic: nothing static is known about the path.
func (v pval) fullyDynamic() bool { return v.dyn && v.s == "" && v.b == bStr && v.suf == "" }

// holeMark separates the static prefix and suffix of a partly computed path in a reference's Spec.
const holeMark = "#{}"

// spec renders a value for a reference: the path, or for a partly computed one its prefix, and when a static suffix is known,
// holeMark and the suffix.
func (v pval) spec() string {
	if v.dyn && v.suf != "" {
		return v.s + holeMark + v.suf
	}
	return v.s
}

func (v pval) usable() bool { return v.b != bEnv && !strings.Contains(v.s, "\x00") }

// pp is the evaluator over tokens [pos, hi).
type pp struct {
	s     *scanner
	ts    []token
	pos   int
	hi    int
	depth int
}

// evalExact evaluates ts[lo:hi] as one path expression.
func (s *scanner) evalExact(ts []token, lo, hi int) (pval, bool) {
	if lo >= hi {
		return pval{}, false
	}
	p := &pp{s: s, ts: ts, pos: lo, hi: hi}
	v, ok := p.expr()
	if !ok || p.pos != hi {
		return pval{}, false
	}
	return v, true
}

// evalGreedy evaluates the longest path expression starting at lo; returns the index after it.
func (s *scanner) evalGreedy(ts []token, lo, hi int) (pval, int, bool) {
	p := &pp{s: s, ts: ts, pos: lo, hi: hi}
	v, ok := p.expr()
	return v, p.pos, ok
}

func (p *pp) op(i int, o string) bool {
	return i < p.hi && p.ts[i].kind == tOp && p.s.text(p.ts[i]) == o
}

func (p *pp) ident(i int, o string) bool {
	return i < p.hi && p.ts[i].kind == tIdent && p.s.text(p.ts[i]) == o
}

func (p *pp) expr() (pval, bool) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > 32 {
		return pval{}, false
	}
	v, ok := p.term()
	if !ok {
		return pval{}, false
	}
	for p.pos < p.hi {
		switch {
		case p.op(p.pos, "+"), p.op(p.pos, "/"):
			o := p.s.text(p.ts[p.pos])
			save := p.pos
			p.pos++
			r, ok := p.term()
			if !ok {
				if o == "/" && !v.pn {
					p.pos = save
					return v, true
				}
				// x + <unknown>: the static prefix is still known.
				p.pos = p.skipOperand()
				if !v.dyn {
					v.dyn = true
					if v.pn {
						v.s = joinPrefix(v.s)
					}
				}
				v.suf = ""
				continue
			}
			if v, ok = combine(o, v, r); !ok {
				return pval{}, false
			}
		case p.op(p.pos, "||"):
			if v.b != bEnv {
				return v, true
			}
			p.pos++
			r, ok := p.term()
			if !ok {
				return pval{}, false
			}
			v = r
		default:
			return v, v.b != bEnv
		}
	}
	return v, v.b != bEnv
}

// skipOperand skips one operand of a binary operator (up to the next + / , ) at depth 0).
func (p *pp) skipOperand() int {
	d := 0
	for i := p.pos; i < p.hi; i++ {
		t := p.ts[i]
		if t.kind == tNL && d == 0 {
			return i
		}
		if t.kind != tOp {
			continue
		}
		switch p.s.text(t) {
		case "(", "[", "{":
			d++
		case ")", "]", "}":
			if d == 0 {
				return i
			}
			d--
		case ",", "+", "||":
			if d == 0 {
				return i
			}
		}
	}
	return p.hi
}

// joinPrefix: the static prefix of path.Join(v, <unknown>).
func joinPrefix(s string) string {
	if s == "." || s == "" {
		return ""
	}
	return strings.TrimSuffix(s, "/") + "/"
}

func combine(o string, l, r pval) (pval, bool) {
	if !l.usable() && !strings.Contains(l.s, selfMark) || r.b == bEnv {
		return pval{}, false
	}
	if l.dyn {
		if r.b != bStr {
			return pval{}, false
		}
		if o == "/" || l.pn {
			return join(l, r, false)
		}
		return concat(l, r), true
	}
	if o == "/" || l.pn {
		if !l.pn || r.b != bStr {
			return pval{}, false
		}
		return join(l, r, false)
	}
	// String concatenation.
	if r.b != bStr {
		if l.b == bStr && l.s == "" {
			return r, true
		}
		return pval{}, false
	}
	return concat(l, r), true
}

// concat appends a plain string to a value as text.
func concat(l, r pval) pval {
	out := l
	if l.dyn {
		if r.dyn {
			out.suf = r.suf
		} else {
			out.suf = l.suf + r.s
		}
		return out
	}
	out.dyn = r.dyn
	out.suf = r.suf
	if l.b == bStr {
		out.s = l.s + r.s
		return out
	}
	switch {
	case strings.HasPrefix(r.s, "/"):
		if r.dyn {
			out.s = joinPrefix(l.s) + strings.TrimLeft(r.s, "/")
		} else {
			out.s = path.Join(l.s, r.s[1:])
			if strings.HasSuffix(r.s, "/") && out.s != "." {
				out.s += "/"
			}
		}
	case r.s == "" && r.dyn:
		out.s = l.s
		if out.s == "." {
			out.s = ""
		}
	case r.s == "":
	case l.s == "." || l.s == "":
		// "<dir>" + "x": a sibling of the directory, not a path inside it.
		if r.dyn {
			return pval{b: l.b, s: "../", dyn: true, suf: r.suf}
		}
		return pval{b: bStr, s: "\x00"} // unusable
	default:
		out.s = l.s + r.s
		if !r.dyn {
			out.s = path.Clean(out.s)
		}
	}
	return out
}

// join is File.join / Pathname#join of one more element.
func join(l, r pval, fileJoin bool) (pval, bool) {
	if l.dyn {
		if r.b != bStr {
			return pval{}, false
		}
		if strings.HasPrefix(r.s, "/") && !fileJoin {
			return r, true
		}
		out := l
		if r.dyn {
			out.suf = r.suf
		} else {
			out.suf = strings.TrimSuffix(l.suf, "/") + "/" + strings.TrimLeft(r.s, "/")
		}
		return out, true
	}
	if r.b != bStr {
		if l.b == bStr && l.s == "" {
			return r, true
		}
		return pval{}, false
	}
	s := r.s
	if strings.HasPrefix(s, "/") {
		if !fileJoin {
			return pval{b: bStr, s: s, dyn: r.dyn}, true // Pathname#join with an absolute path replaces it
		}
		s = strings.TrimLeft(s, "/")
	}
	out := l
	if r.dyn {
		out.dyn = true
		out.suf = r.suf
		out.s = joinPrefix(l.s) + s
		if l.b == bStr && l.s == "" {
			out.s = s
		}
		return out, true
	}
	if l.b == bStr && l.s == "" {
		out.s = s
		return out, true
	}
	out.s = path.Join(l.s, s)
	return out, true
}

func (p *pp) term() (pval, bool) {
	v, ok := p.primary()
	if !ok {
		return pval{}, false
	}
	// Postfix method calls.
	for p.pos+1 < p.hi && (p.op(p.pos, ".") || p.op(p.pos, "&.")) && p.ts[p.pos+1].kind == tIdent {
		m := p.s.text(p.ts[p.pos+1])
		switch m {
		case "to_s", "to_path", "to_str", "freeze", "dup", "expand_path", "realpath", "realdirpath", "cleanpath", "itself":
			p.pos += 2
			if p.op(p.pos, "(") && p.op(p.pos+1, ")") {
				p.pos += 2
			}
			if m != "to_s" && m != "to_str" && m != "freeze" && m != "dup" && m != "itself" {
				v.pn = true
			}
			if m == "to_s" || m == "to_str" {
				v.pn = false
			}
		case "parent", "dirname":
			if v.dyn {
				return pval{}, false
			}
			p.pos += 2
			v.s = dirOf(v.s)
		case "join":
			p.pos += 2
			args, ok := p.callArgs()
			if !ok {
				return pval{}, false
			}
			for _, a := range args {
				var ok bool
				if v, ok = join(v, a, false); !ok {
					return pval{}, false
				}
			}
			v.pn = true
		default:
			return v, true
		}
	}
	return v, true
}

func dirOf(s string) string {
	if s == "" {
		return "."
	}
	return path.Dir(s)
}

// callArgs parses "(a, b)" at pos (or a parenless argument list up to hi) into values; an argument that cannot be evaluated is a
// dynamic empty string (only its position matters).
func (p *pp) callArgs() ([]pval, bool) {
	var ranges [][2]int
	if p.op(p.pos, "(") && !p.ts[p.pos].sp {
		c := p.s.closeOf(p.ts, p.pos, p.hi)
		if c < 0 {
			return nil, false
		}
		ranges = p.s.split(p.ts, p.pos+1, c)
		p.pos = c + 1
	} else if p.pos < p.hi && p.ts[p.pos].sp && p.ts[p.pos].kind != tNL && p.ts[p.pos].kind != tOp {
		ranges = p.s.split(p.ts, p.pos, p.hi)
		p.pos = p.hi
	} else {
		return nil, false
	}
	out := make([]pval, 0, len(ranges))
	for _, r := range ranges {
		if v, ok := p.s.evalExact(p.ts, r[0], r[1]); ok {
			out = append(out, v)
		} else {
			out = append(out, pval{b: bStr, dyn: true})
		}
	}
	return out, true
}

func (p *pp) primary() (pval, bool) {
	if p.pos >= p.hi {
		return pval{}, false
	}
	t := p.ts[p.pos]
	switch t.kind {
	case tStr:
		p.pos++
		return p.s.strValue(t)
	case tOp:
		switch p.s.text(t) {
		case "(":
			c := p.s.closeOf(p.ts, p.pos, p.hi)
			if c < 0 {
				return pval{}, false
			}
			v, ok := p.s.evalExact(p.ts, p.pos+1, c)
			if !ok {
				return pval{}, false
			}
			p.pos = c + 1
			return v, true
		case "::":
			if p.pos+1 < p.hi && p.ts[p.pos+1].kind == tIdent {
				p.pos++
				return p.primary()
			}
		}
		return pval{}, false
	case tIdent:
	default:
		return pval{}, false
	}
	name := p.s.text(t)
	switch name {
	case "__dir__":
		p.pos++
		if p.op(p.pos, "(") && p.op(p.pos+1, ")") {
			p.pos += 2
		}
		return pval{b: bRel, s: "."}, true
	case "__FILE__":
		p.pos++
		return pval{b: bRel, s: selfMark}, true
	case "ENV":
		if p.op(p.pos+1, "[") {
			c := p.s.closeOf(p.ts, p.pos+1, p.hi)
			if c < 0 {
				return pval{}, false
			}
			p.pos = c + 1
			return pval{b: bEnv}, true
		}
		if p.op(p.pos+1, ".") && p.ident(p.pos+2, "fetch") {
			p.pos += 3
			args, ok := p.callArgs()
			if !ok || len(args) != 2 {
				return pval{}, false
			}
			return args[1], !args[1].dyn || args[1].s != ""
		}
		return pval{}, false
	case "Dir":
		// Dir.pwd: the working directory, i.e. a plain relative path.
		if p.op(p.pos+1, ".") && (p.ident(p.pos+2, "pwd") || p.ident(p.pos+2, "getwd")) {
			p.pos += 3
			return pval{b: bStr}, true
		}
		return pval{}, false
	case "Rails":
		if p.op(p.pos+1, ".") && p.ident(p.pos+2, "root") {
			p.pos += 3
			return pval{b: bRoot, s: ".", pn: true}, true
		}
		return pval{}, false
	case "Pathname":
		switch {
		case p.op(p.pos+1, "(") && !p.ts[p.pos+1].sp:
			p.pos++
		case p.op(p.pos+1, ".") && p.ident(p.pos+2, "new"):
			p.pos += 3
		default:
			return pval{}, false
		}
		args, ok := p.callArgs()
		if !ok || len(args) != 1 || args[0].dyn && args[0].s == "" {
			return pval{}, false
		}
		v := args[0]
		v.pn = true
		return v, true
	case "File":
		if p.op(p.pos+1, "::") && p.ident(p.pos+2, "SEPARATOR") {
			p.pos += 3
			return pval{b: bStr, s: "/"}, true
		}
		if !p.op(p.pos+1, ".") || p.pos+2 >= p.hi || p.ts[p.pos+2].kind != tIdent {
			return pval{}, false
		}
		m := p.s.text(p.ts[p.pos+2])
		p.pos += 3
		args, ok := p.callArgs()
		if !ok || len(args) == 0 {
			return pval{}, false
		}
		return fileCall(m, args)
	}
	// A single-assignment variable or constant holding a path (vars.go).
	if !p.op(p.pos+1, "(") || p.ts[p.pos+1].sp {
		at := p.pos
		if !p.s.sameList(p.ts) {
			at = p.s.ctx // an interpolation: the position of its string in the top-level list
		}
		if v, ok := p.s.varValue(name, at, p.ts); ok {
			p.pos++
			return v, true
		}
	}
	return pval{}, false
}

func fileCall(m string, args []pval) (pval, bool) {
	switch m {
	case "join":
		v := args[0]
		for _, a := range args[1:] {
			var ok bool
			if v, ok = join(v, a, true); !ok {
				return pval{}, false
			}
		}
		v.pn = false
		return v, !v.fullyDynamic()
	case "expand_path", "absolute_path":
		a := args[0]
		if len(args) == 1 {
			if a.fullyDynamic() {
				return pval{}, false
			}
			return a, true
		}
		b := args[1]
		if b.dyn || len(args) > 2 {
			return pval{}, false
		}
		if a.b == bStr && (strings.HasPrefix(a.s, "/") || strings.HasPrefix(a.s, "~")) {
			return a, !a.dyn
		}
		if a.b != bStr {
			return a, true // an absolute path ignores the base
		}
		v, ok := join(b, a, true)
		v.pn = false
		return v, ok
	case "dirname":
		v := args[0]
		if v.dyn {
			return pval{}, false
		}
		n := 1
		if len(args) == 2 {
			// File.dirname(x, 2): the level must be a literal number, which callArgs cannot evaluate: not supported.
			return pval{}, false
		}
		for i := 0; i < n; i++ {
			v.s = dirOf(v.s)
		}
		v.pn = false
		return v, true
	case "realpath", "realdirpath":
		v := args[0]
		v.pn = false
		return v, !v.fullyDynamic()
	}
	return pval{}, false
}

// strValue evaluates a string literal: static text, or interpolations that are path expressions (a non-evaluable
// interpolation leaves a dynamic prefix).
func (s *scanner) strValue(t token) (pval, bool) {
	si := &s.strs[t.aux]
	switch si.kind {
	case sSingle, sDouble, sHeredoc:
	default:
		return pval{}, false
	}
	save := s.ctx
	defer func() { s.ctx = save }()
	if s.sameList(s.top) {
		s.ctx = s.topIndex(t)
	}
	acc := pval{b: bStr}
	first := true
	for _, part := range si.parts {
		if acc.dyn {
			// After a computed part only the static tail is tracked.
			if !part.isCode {
				acc.suf += part.lit
				continue
			}
			if v, ok := s.evalExact(part.code, 0, len(part.code)); ok && !v.dyn && v.b == bStr {
				acc.suf += v.s
			} else {
				acc.suf = ""
			}
			continue
		}
		if !part.isCode {
			acc = concat(acc, pval{b: bStr, s: part.lit})
			if !acc.usable() && !strings.Contains(acc.s, selfMark) {
				return pval{}, false
			}
			first = false
			continue
		}
		v, ok := s.evalExact(part.code, 0, len(part.code))
		if !ok || v.dyn {
			if ok && first {
				acc = v
				acc.pn = false
				first = false
				continue
			}
			if ok && v.b == bStr {
				acc = concat(acc, v) // "../#{x}" with x = "<unknown>/spec/helper": the suffix is kept
				first = false
				continue
			}
			acc.dyn, acc.suf = true, ""
			if acc.b != bStr {
				acc.s = strings.TrimSuffix(acc.s, ".")
			}
			first = false
			continue
		}
		if first || acc.b == bStr && acc.s == "" {
			acc = v
			acc.pn = false
		} else if v.b == bStr {
			acc = concat(acc, v)
		} else {
			acc.dyn, acc.suf = true, ""
		}
		first = false
	}
	return acc, true
}

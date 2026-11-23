package ruby

import (
	"path"
	"strings"
)

// The literal tail of a computed path: the literal components written after its last computed part (or `..`). Whatever the
// computed part evaluates to, the file loaded ends with that tail, so a suffix match on it is sound even when the evaluator
// gets the computed part wrong (or an ENV variable overrides it). `File.join(X, "lib", "foo")` → "lib/foo";
// `"#{dir}/x"` → "x"; `File.expand_path("../lib/foo", __dir__)` → "lib/foo".

const maxTailDepth = 16

// litTails returns the possible literal tails of ts[lo:hi] (one per alternative of `a || b`); "" stands for an alternative
// with no literal tail. at is the top-level token index used to resolve local variables (-1: none).
func (s *scanner) litTails(ts []token, lo, hi, at, depth int) []string {
	if lo >= hi || depth > maxTailDepth {
		return []string{""}
	}
	depth++
	// ( expr )
	if s.isOp(ts, lo, "(") && s.closeOf(ts, lo, hi) == hi-1 {
		return s.litTails(ts, lo+1, hi-1, at, depth)
	}
	// a || b
	if parts := s.splitTop(ts, lo, hi, "||"); len(parts) > 1 {
		var out []string
		for _, r := range parts {
			out = append(out, s.litTails(ts, r[0], r[1], at, depth)...)
		}
		return out
	}
	// a + b + "lit": the literal operands at the end.
	if parts := s.splitTop(ts, lo, hi, "+"); len(parts) > 1 {
		lit := ""
		k := len(parts) - 1
		for ; k >= 0; k-- {
			v, ok := s.staticStr(ts, parts[k][0], parts[k][1])
			if !ok {
				break
			}
			lit = v + lit
		}
		if k < 0 {
			return []string{normTail(lit, true)}
		}
		if lit == "" {
			return []string{""}
		}
		return []string{normTail(lit, strings.HasPrefix(lit, "/"))}
	}
	if hi-lo == 1 {
		t := ts[lo]
		switch t.kind {
		case tStr:
			return s.strTails(t, depth)
		case tIdent:
			if r, rat, ok := s.varRange(s.text(t), at, ts); ok {
				return s.litTails(s.top, r[0], r[1], rat, depth)
			}
		}
		return []string{""}
	}
	// ENV[...], ENV.fetch(k, default)
	if s.isIdent(ts, lo, "ENV") {
		if s.isOp(ts, lo+1, ".") && s.isIdent(ts, lo+2, "fetch") {
			if args, end := s.argRanges(ts, lo+2, hi); end == hi && len(args) == 2 {
				return append(s.litTails(ts, args[1][0], args[1][1], at, depth), "")
			}
		}
		return []string{""}
	}
	// A method call at the end: recv.m(args) / recv.m args / File.m(args) / Pathname(x)
	if s.isIdent(ts, lo, "Pathname") && s.isOp(ts, lo+1, "(") && s.closeOf(ts, lo+1, hi) == hi-1 {
		return s.litTails(ts, lo+2, hi-1, at, depth)
	}
	dot, m := s.lastCall(ts, lo, hi)
	if dot < 0 {
		return []string{""}
	}
	recv := [2]int{lo, dot}
	args, end := s.argRanges(ts, dot+1, hi)
	if end != hi {
		return []string{""}
	}
	isFile := dot-lo == 1 && s.isIdent(ts, lo, "File") || dot-lo == 2 && s.isOp(ts, lo, "::") && s.isIdent(ts, lo+1, "File")
	switch {
	case m == "join":
		// File.join(a, b, ...) or recv.join(a, ...): the literal arguments at the end.
		if !isFile {
			args = append([][2]int{recv}, args...)
		}
		if len(args) == 0 {
			return []string{""}
		}
		lit := ""
		k := len(args) - 1
		for ; k >= 0; k-- {
			v, ok := s.staticStr(ts, args[k][0], args[k][1])
			if !ok {
				break
			}
			v = strings.Trim(v, "/")
			if lit == "" {
				lit = v
			} else if v != "" {
				lit = v + "/" + lit
			}
		}
		if k == len(args)-1 {
			return s.litTails(ts, args[k][0], args[k][1], at, depth) // the last element is itself computed
		}
		return []string{normTail(lit, true)}
	case isFile && (m == "expand_path" || m == "absolute_path" || m == "realpath" || m == "realdirpath"):
		if len(args) == 0 {
			return []string{""}
		}
		return s.litTails(ts, args[0][0], args[0][1], at, depth)
	case !isFile && (m == "to_s" || m == "to_path" || m == "to_str" || m == "freeze" || m == "dup" || m == "itself" ||
		m == "expand_path" || m == "realpath" || m == "cleanpath") && len(args) == 0:
		return s.litTails(ts, recv[0], recv[1], at, depth)
	case !isFile && m == "new" && dot-lo == 1 && s.isIdent(ts, lo, "Pathname") && len(args) == 1:
		return s.litTails(ts, args[0][0], args[0][1], at, depth)
	}
	return []string{""}
}

// strTails: the literal text after the last interpolation of a string (or its whole text).
func (s *scanner) strTails(t token, depth int) []string {
	si := &s.strs[t.aux]
	switch si.kind {
	case sSingle, sDouble, sHeredoc:
	default:
		return []string{""}
	}
	if v, ok := si.static(); ok {
		return []string{normTail(v, true)}
	}
	lit := ""
	k := len(si.parts) - 1
	for ; k >= 0 && !si.parts[k].isCode; k-- {
		lit = si.parts[k].lit + lit
	}
	if lit != "" {
		return []string{normTail(lit, strings.HasPrefix(lit, "/"))}
	}
	// The string ends with an interpolation: its own tail (in the context of this string).
	code := si.parts[k].code
	save := s.ctx
	if idx := s.topIndex(t); idx >= 0 {
		s.ctx = idx
	}
	out := s.litTails(code, 0, len(code), s.ctx, depth)
	s.ctx = save
	return out
}

// normTail keeps the components of a literal after the last "..": exact=false when its first component may be glued to a
// computed part before it.
func normTail(lit string, exact bool) string {
	lit = strings.ReplaceAll(lit, "\\", "/")
	comps := strings.Split(lit, "/")
	if !exact && len(comps) > 0 {
		comps = comps[1:]
	}
	var out []string
	for _, c := range comps {
		switch c {
		case "", ".":
		case "..":
			out = out[:0]
		default:
			if strings.ContainsAny(c, "*?[]{}") || strings.Contains(c, "~") && len(out) == 0 {
				out = out[:0] // a glob or a home directory is not a literal component
				continue
			}
			out = append(out, c)
		}
	}
	return path.Join(out...)
}

// staticStr: ts[lo:hi] is one string literal without interpolation.
func (s *scanner) staticStr(ts []token, lo, hi int) (string, bool) {
	if hi-lo != 1 || ts[lo].kind != tStr {
		return "", false
	}
	si := &s.strs[ts[lo].aux]
	if si.kind != sSingle && si.kind != sDouble && si.kind != sHeredoc {
		return "", false
	}
	return si.static()
}

// splitTop splits ts[lo:hi] at the top-level occurrences of operator op.
func (s *scanner) splitTop(ts []token, lo, hi int, op string) [][2]int {
	var out [][2]int
	d, start := 0, lo
	for j := lo; j < hi; j++ {
		if ts[j].kind != tOp {
			continue
		}
		switch o := s.text(ts[j]); o {
		case "(", "[", "{":
			d++
		case ")", "]", "}":
			d--
		default:
			if o == op && d == 0 {
				out = append(out, [2]int{start, j})
				start = j + 1
			}
		}
	}
	return append(out, [2]int{start, hi})
}

// lastCall finds `recv.m(...)` / `recv.m args` / `recv.m` spanning ts[lo:hi]: the index of the last top-level "." and the method.
func (s *scanner) lastCall(ts []token, lo, hi int) (int, string) {
	d := 0
	for j := lo; j < hi; j++ {
		if ts[j].kind != tOp {
			continue
		}
		switch s.text(ts[j]) {
		case "(", "[", "{":
			d++
		case ")", "]", "}":
			d--
		case ".", "&.":
			if d == 0 && j+1 < hi && ts[j+1].kind == tIdent {
				// the last top-level dot whose method call reaches hi
				if _, end := s.argRanges(ts, j+1, hi); end == hi {
					return j, s.text(ts[j+1])
				}
			}
		}
	}
	return -1, ""
}

// argRanges parses the arguments of the method name at i: (a, b) or a parenless list up to hi; returns the index after them.
func (s *scanner) argRanges(ts []token, i, hi int) ([][2]int, int) {
	j := i + 1
	if j >= hi {
		return nil, hi
	}
	if s.isOp(ts, j, "(") && !ts[j].sp {
		c := s.closeOf(ts, j, hi)
		if c < 0 {
			return nil, -1
		}
		return s.split(ts, j+1, c), c + 1
	}
	if ts[j].sp && ts[j].kind != tOp {
		return s.split(ts, j, hi), hi
	}
	return nil, j
}

// varRange returns the right-hand side (in the top-level list) of a single-assignment variable visible at index at.
func (s *scanner) varRange(n string, at int, ts []token) ([2]int, int, bool) {
	d := s.vars[n]
	if d == nil || d.bad || d.n != 1 || d.lo >= d.hi {
		return [2]int{}, 0, false
	}
	if isLocalName(n) {
		if !s.sameList(ts) {
			at = s.ctx
		}
		if s.scopes == nil || at < 0 || at >= len(s.scopes) || s.scopes[at] != d.scope || at < d.lo {
			return [2]int{}, 0, false
		}
	}
	return [2]int{d.lo, d.hi}, d.lo, true
}

// nestedCommand: ts[lo:hi] holds a parenless method call (`File.join DIR, "x"`), whose arguments run on past the commas.
func (s *scanner) nestedCommand(ts []token, lo, hi int) bool {
	for j := lo; j+1 < hi; j++ {
		t, n := ts[j], ts[j+1]
		if t.kind != tIdent || !n.sp {
			continue
		}
		name := s.text(t)
		if !(s.isOp(ts, j-1, ".") || s.isOp(ts, j-1, "&.")) && !(isLocalName(name) && !stmtEnders[name]) {
			continue
		}
		switch n.kind {
		case tStr, tIdent, tIVar, tGVar, tSym, tNum:
			if n.kind == tIdent && stmtEnders[s.text(n)] {
				continue
			}
			return true
		case tOp:
			if o := s.text(n); o == "::" || o == "(" || o == "[" {
				return true
			}
		}
	}
	return false
}

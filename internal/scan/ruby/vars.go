package ruby

import "sort"

// Single-assignment variables and constants that hold a path expression: `glob = File.join(__dir__, path, "*.rb")` then
// `Dir[glob]`, `FIXTURES = File.expand_path("fixtures", __dir__)` then `File.join(FIXTURES, "a.json")`.
//
// A name is usable only when it is assigned exactly once in the file and never rebound (+=, <<, ||=, a mutating method, a
// method or block parameter, a multiple assignment, a for variable). A local variable is visible only inside the scope (def,
// class or module body, or the top level) that assigns it; when the scopes of the file cannot be delimited, no local is used.
// Constants are file-wide.

type varDef struct {
	lo, hi int // right-hand side tokens
	scope  int // index of the scope that assigns it (locals)
	n      int // assignments
	bad    bool
	state  uint8 // 0 not evaluated, 1 evaluating, 2 done
	val    pval
	ok     bool
}

// mutators rebind or modify a string in place.
var mutators = map[string]bool{"sub!": true, "gsub!": true, "concat": true, "replace": true, "prepend": true, "insert": true,
	"<<": true, "clear": true, "chomp!": true, "squeeze!": true, "tr!": true, "delete!": true, "strip!": true, "freeze": false,
	"slice!": true, "chop!": true, "downcase!": true, "upcase!": true}

func isConstName(n string) bool { return n != "" && n[0] >= 'A' && n[0] <= 'Z' }

func isLocalName(n string) bool { return n != "" && (n[0] >= 'a' && n[0] <= 'z' || n[0] == '_') }

// collectVars finds the single-assignment names of the top-level token list.
func (s *scanner) collectVars(ts []token) {
	s.vars = map[string]*varDef{}
	s.scopes = s.scopeOf(ts)
	get := func(n string) *varDef {
		d := s.vars[n]
		if d == nil {
			d = &varDef{}
			s.vars[n] = d
		}
		return d
	}
	inPipes := false
	for i := 0; i < len(ts); i++ {
		t := ts[i]
		if t.kind == tOp && s.text(t) == "|" && i > 0 && (s.isOp(ts, i-1, "{") || s.isIdent(ts, i-1, "do")) {
			inPipes = true
			continue
		}
		if inPipes {
			if t.kind == tOp && s.text(t) == "|" || t.kind == tNL {
				inPipes = false
			} else if t.kind == tIdent {
				get(s.text(t)).bad = true
			}
			continue
		}
		if t.kind != tIdent {
			continue
		}
		n := s.text(t)
		if s.isOp(ts, i-1, ".") || s.isOp(ts, i-1, "&.") || s.isOp(ts, i-1, "::") {
			continue
		}
		switch {
		case n == "def":
			if s.isIdent(ts, i+1, "load") || s.isIdent(ts, i+1, "self") && s.isOp(ts, i+2, ".") && s.isIdent(ts, i+3, "load") {
				s.defsLoad = true
			}
			// Parameters: `def name(a, b)` or `def name a, b` (the name may be self.name / Const.name).
			k := i + 1
			if s.isOp(ts, k+1, ".") {
				k += 2
			}
			var lo, hi int
			switch {
			case s.isOp(ts, k+1, "(") && !ts[k+1].sp:
				lo, hi = k+2, s.closeOf(ts, k+1, len(ts))
				if hi < 0 {
					hi = len(ts)
				}
			case k+1 < len(ts) && ts[k+1].kind != tNL && !s.isOp(ts, k+1, "=") && !s.isOp(ts, k+1, ";"):
				lo, hi = k+1, k+1
				for hi < len(ts) && ts[hi].kind != tNL && !s.isOp(ts, hi, ";") {
					hi++
				}
			}
			for q := lo; q < hi; q++ {
				if ts[q].kind == tIdent {
					get(s.text(ts[q])).bad = true
				}
			}
		case n == "for":
			for k := i + 1; k < len(ts) && !s.isIdent(ts, k, "in") && ts[k].kind != tNL; k++ {
				if ts[k].kind == tIdent {
					get(s.text(ts[k])).bad = true
				}
			}
		case s.isOp(ts, i+1, "=") && (isLocalName(n) || isConstName(n)):
			d := get(n)
			d.n++
			if !s.stmtStart(ts, i) || s.isOp(ts, i-1, ",") {
				d.bad = true
				continue
			}
			d.lo = i + 2
			d.hi = s.cmdEnd(ts, i+2)
			if s.scopes != nil {
				d.scope = s.scopes[i]
			}
		case s.isOp(ts, i+1, ","):
			// a, b = ... (only when this is a multiple assignment: an = follows on the same statement)
			for k := i + 1; k < len(ts) && ts[k].kind != tNL; k++ {
				if s.isOp(ts, k, "=") {
					get(n).bad = true
					break
				}
				if !(ts[k].kind == tIdent || s.isOp(ts, k, ",") || s.isOp(ts, k, "*")) {
					break
				}
			}
		case i+1 < len(ts) && ts[i+1].kind == tOp && (s.text(ts[i+1]) == "+=" || s.text(ts[i+1]) == "<<" || s.text(ts[i+1]) == "||=" ||
			s.text(ts[i+1]) == "<<=" || s.text(ts[i+1]) == "-=" || s.text(ts[i+1]) == "&&="):
			get(n).bad = true
		case s.isOp(ts, i+1, ".") && i+2 < len(ts) && mutators[s.text(ts[i+2])]:
			get(n).bad = true
		}
	}
}

// varValue returns the path value of a single-assignment name used at token index at (of the top-level list; -1: from a nested
// token list such as an interpolation, where only constants are used).
func (s *scanner) varValue(n string, at int, ts []token) (pval, bool) {
	d := s.vars[n]
	if d == nil || d.bad || d.n != 1 || d.lo >= d.hi {
		return pval{}, false
	}
	if isLocalName(n) {
		if s.scopes == nil || at < 0 || at >= len(s.scopes) || s.scopes[at] != d.scope || at < d.lo {
			return pval{}, false
		}
	}
	switch d.state {
	case 1:
		return pval{}, false // a cycle
	case 2:
		return d.val, d.ok
	}
	d.state = 1
	d.val, d.ok = s.evalExact(s.top, d.lo, d.hi)
	d.state = 2
	return d.val, d.ok
}

// topIndex returns the index of t in the top-level list, or the context of the enclosing string when t is nested (-1: none).
func (s *scanner) topIndex(t token) int {
	i := sort.Search(len(s.top), func(k int) bool { return s.top[k].start >= t.start })
	if i < len(s.top) && s.top[i].start == t.start && s.top[i].kind == t.kind {
		return i
	}
	return s.ctx
}

func (s *scanner) sameList(ts []token) bool {
	return len(ts) == len(s.top) && (len(ts) == 0 || &ts[0] == &s.top[0])
}

// scopeOf assigns each token of the top-level list the index of its innermost def/class/module (0: the top level). nil when
// the bodies cannot be delimited.
func (s *scanner) scopeOf(ts []token) []int {
	out := make([]int, len(ts))
	type frame struct{ scope, end int }
	var stack []frame
	next := 0
	for i := 0; i < len(ts); i++ {
		for len(stack) > 0 && i > stack[len(stack)-1].end {
			stack = stack[:len(stack)-1]
		}
		cur := 0
		if len(stack) > 0 {
			cur = stack[len(stack)-1].scope
		}
		out[i] = cur
		t := ts[i]
		if t.kind != tIdent || s.isOp(ts, i-1, ".") || s.isOp(ts, i-1, "&.") || s.isOp(ts, i+1, ":") && !ts[i+1].sp {
			continue
		}
		switch s.text(t) {
		case "def", "class", "module":
			if s.text(t) == "def" && s.endlessDef(ts, i) {
				continue
			}
			e := s.doEnd(ts, i)
			if e < 0 {
				return nil
			}
			next++
			stack = append(stack, frame{next, e})
		}
	}
	return out
}

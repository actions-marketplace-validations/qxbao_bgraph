package python

import (
	"path"
	"strconv"
	"strings"
)

type pvKind uint8

const (
	pvStr  pvKind = iota + 1 // a plain string / path relative to the working directory
	pvFile                   // a path relative to the directory of the scanned file ("." is that directory)
)

// pval is a value of the tiny path-expression evaluator behind sys.path handling.
type pval struct {
	kind pvKind
	s    string
	self bool // the value is __file__ itself (not a directory)
}

// pathExpr evaluates the tokens [lo,hi) as a path built from literals and __file__ with the os.path / pathlib idioms that
// conftest files use to extend sys.path. ok=false: anything else.
func (s *scanner) pathExpr(lo, hi int) (pval, bool) {
	p := &pparser{s: s, pos: lo, hi: hi}
	v, ok := p.expr(0)
	if !ok || p.pos != hi {
		return pval{}, false
	}
	return v, true
}

type pparser struct {
	s       *scanner
	pos, hi int
}

const maxPathDepth = 32

func (p *pparser) expr(depth int) (pval, bool) {
	if depth > maxPathDepth {
		return pval{}, false
	}
	v, ok := p.term(depth)
	if !ok {
		return pval{}, false
	}
	for p.pos < p.hi && (p.s.punct(p.pos, "/") || p.s.punct(p.pos, "+")) {
		op := p.s.text(p.pos)
		p.pos++
		r, ok := p.term(depth)
		if !ok {
			return pval{}, false
		}
		if v, ok = combine(op, v, r); !ok {
			return pval{}, false
		}
	}
	return v, true
}

func combine(op string, l, r pval) (pval, bool) {
	if r.kind != pvStr || l.self {
		return pval{}, false
	}
	if op == "+" {
		if l.kind == pvStr {
			return pval{kind: pvStr, s: l.s + r.s}, true
		}
		r.s = strings.TrimLeft(r.s, "/")
	} else if strings.HasPrefix(r.s, "/") {
		return pval{}, false
	}
	if isAbs(r.s) {
		return pval{}, false
	}
	if l.kind == pvStr && isAbs(l.s) {
		return pval{}, false
	}
	return pval{kind: l.kind, s: path.Join(l.s, r.s)}, true
}

func isAbs(p string) bool {
	return strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") || len(p) > 1 && p[1] == ':'
}

func parent(v pval) (pval, bool) {
	if v.kind == pvStr && isAbs(v.s) {
		return pval{}, false
	}
	if v.self {
		return pval{kind: pvFile, s: "."}, true
	}
	return pval{kind: v.kind, s: path.Join(v.s, "..")}, true
}

func (p *pparser) term(depth int) (pval, bool) {
	v, ok := p.atom(depth)
	if !ok {
		return pval{}, false
	}
	for p.pos+1 < p.hi && p.s.punct(p.pos, ".") && p.s.toks[p.pos+1].kind == tName {
		m := p.s.text(p.pos + 1)
		p.pos += 2
		switch m {
		case "parent":
			if v, ok = parent(v); !ok {
				return pval{}, false
			}
		case "parents":
			if !(p.pos+2 < p.hi && p.s.punct(p.pos, "[") && p.s.toks[p.pos+1].kind == tNum && p.s.punct(p.pos+2, "]")) {
				return pval{}, false
			}
			n, err := strconv.Atoi(p.s.text(p.pos + 1))
			if err != nil || n < 0 || n > 64 {
				return pval{}, false
			}
			p.pos += 3
			for k := 0; k <= n; k++ {
				if v, ok = parent(v); !ok {
					return pval{}, false
				}
			}
		case "resolve", "absolute", "as_posix", "expanduser":
			if m == "expanduser" || !(p.pos+1 < p.hi && p.s.punct(p.pos, "(") && p.s.punct(p.pos+1, ")")) {
				return pval{}, false
			}
			p.pos += 2
		case "joinpath":
			if p.pos >= p.hi || !p.s.punct(p.pos, "(") {
				return pval{}, false
			}
			args, next := p.s.callArgs(p.pos)
			if next > p.hi {
				return pval{}, false
			}
			p.pos = next
			for _, a := range args {
				r, ok := p.sub(a, depth)
				if !ok || a.kw != "" {
					return pval{}, false
				}
				if v, ok = combine("/", v, r); !ok {
					return pval{}, false
				}
			}
		default:
			return pval{}, false
		}
	}
	return v, true
}

func (p *pparser) sub(a arg, depth int) (pval, bool) {
	q := &pparser{s: p.s, pos: a.lo, hi: a.hi}
	v, ok := q.expr(depth + 1)
	if !ok || q.pos != a.hi {
		return pval{}, false
	}
	return v, true
}

func (p *pparser) atom(depth int) (pval, bool) {
	if p.pos >= p.hi {
		return pval{}, false
	}
	t := p.s.toks[p.pos]
	switch t.kind {
	case tStr:
		si := p.s.lx.strs[t.aux]
		if si.field || si.cut || si.bytes {
			return pval{}, false
		}
		p.pos++
		return pval{kind: pvStr, s: si.val}, true
	case tPunct:
		if p.s.text(p.pos) != "(" {
			return pval{}, false
		}
		p.pos++
		v, ok := p.expr(depth + 1)
		if !ok || p.pos >= p.hi || !p.s.punct(p.pos, ")") {
			return pval{}, false
		}
		p.pos++
		return v, true
	case tName:
	default:
		return pval{}, false
	}
	// a dotted chain, optionally called
	var chain []string
	chain = append(chain, p.s.text(p.pos))
	p.pos++
	for p.pos+1 < p.hi && p.s.punct(p.pos, ".") && p.s.toks[p.pos+1].kind == tName && !isPostfix(p.s.text(p.pos+1)) {
		chain = append(chain, p.s.text(p.pos+1))
		p.pos += 2
	}
	last := chain[len(chain)-1]
	if p.pos >= p.hi || !p.s.punct(p.pos, "(") {
		switch {
		case len(chain) == 1 && last == "__file__":
			return pval{kind: pvFile, s: "", self: true}, true
		case len(chain) > 1 && last == "pardir":
			return pval{kind: pvStr, s: ".."}, true
		case len(chain) > 1 && last == "curdir":
			return pval{kind: pvStr, s: "."}, true
		case len(chain) > 1 && last == "sep":
			return pval{kind: pvStr, s: "/"}, true
		}
		return pval{}, false
	}
	args, next := p.s.callArgs(p.pos)
	if next > p.hi {
		return pval{}, false
	}
	p.pos = next
	vals := make([]pval, 0, len(args))
	for _, a := range args {
		if a.kw != "" {
			return pval{}, false
		}
		v, ok := p.sub(a, depth)
		if !ok {
			return pval{}, false
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return pval{}, false
	}
	switch last {
	case "dirname":
		if len(vals) != 1 {
			return pval{}, false
		}
		return parent(vals[0])
	case "abspath", "realpath", "normpath", "str", "fspath":
		if len(vals) != 1 {
			return pval{}, false
		}
		return vals[0], true
	case "Path", "PurePath", "PosixPath", "WindowsPath", "join":
		v := vals[0]
		for _, r := range vals[1:] {
			var ok bool
			if v, ok = combine("/", v, r); !ok {
				return pval{}, false
			}
		}
		return v, true
	}
	return pval{}, false
}

func isPostfix(m string) bool {
	switch m {
	case "parent", "parents", "resolve", "absolute", "as_posix", "joinpath", "expanduser":
		return true
	}
	return false
}

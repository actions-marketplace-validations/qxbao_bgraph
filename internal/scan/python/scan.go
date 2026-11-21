// Package python is bgraph's Python import scanner (docs/plan.md §5, §6.2). It is a lexer plus a token-level extractor, not a parser:
// it finds what the resolver needs, and reports Uncertain only when the token stream cannot be trusted (unterminated string,
// unbalanced brackets).
//
// Recognized (every occurrence counts, wherever it sits: function bodies, try/except, `if TYPE_CHECKING:`):
//
//   - `import a.b.c [as x], d`: one ImportRef per module, Kind ImportStatic, Spec "a.b.c".
//   - `from a.b import c, d` / `from . import x` / `from ..a import b` / `from x import *`: Spec is the module as written (leading
//     dots for a relative import), Suffix the comma-separated imported names ("*" for a star import).
//   - importlib.import_module("m") / __import__("m") / runpy.run_module("m"): Kind ImportDynamic with a constant Spec;
//     pytest.importorskip("m"): ImportRequire. A non-constant argument is Dynamic=true; Spec then holds the static head of a
//     concatenation or f-string ("pkg.plugins." + name), empty when nothing is known.
//   - pkgutil.walk_packages / iter_modules and importlib.util.spec_from_file_location: Dynamic ImportDynamic with an empty Spec.
//   - mock.patch("a.b.c"), patch.object/multiple/dict("a.b"), monkeypatch.setattr("a.b.c", ...): Kind ImportMock (heuristic, add-only).
//   - `pytest_plugins = ["a.b"]`: Kind ImportPlugin (Dynamic when an element is not a constant string).
//   - INSTALLED_APPS / MIDDLEWARE / ROOT_URLCONF / AUTHENTICATION_BACKENDS assignments: every string in the right-hand side is an
//     ImportMock (Django settings).
//   - `>>> import x` / `>>> from x import y` lines inside string literals (doctests): ImportMock.
//   - sys.path.insert/append/extend(...) and site.addsitedir(...): Kind ImportSysPath. The argument is evaluated when it is a
//     literal or an os.path/pathlib expression built from __file__: Spec is then a path relative to the file's directory ("." is the
//     directory itself), or "cwd:<path>" for a relative literal. Anything else, and any assignment to sys.path, is Dynamic.
//
// The result does not depend on the file's path, so it can be cached by content.
package python

import (
	"path"
	"strings"

	"github.com/qxbao/bgraph/internal/model"
)

const maxImports = 200000

// Scan scans one Python file.
func Scan(_ string, src []byte) model.ScanResult {
	lx, why := lex(src)
	if why != "" {
		return model.ScanResult{Imports: []model.ImportRef{}, Uncertain: true, UncertainReason: why}
	}
	s := &scanner{lx: lx, toks: lx.toks, src: src}
	s.run()
	if s.res == nil {
		s.res = []model.ImportRef{}
	}
	return model.ScanResult{Imports: s.res}
}

type scanner struct {
	lx   *lexed
	toks []token
	src  []byte
	res  []model.ImportRef
}

func (s *scanner) text(i int) string { return string(s.src[s.toks[i].start:s.toks[i].end]) }

func (s *scanner) is(i int, kind tokKind, text string) bool {
	if i < 0 || i >= len(s.toks) || s.toks[i].kind != kind {
		return false
	}
	t := s.toks[i]
	return string(s.src[t.start:t.end]) == text
}

func (s *scanner) punct(i int, text string) bool { return s.is(i, tPunct, text) }
func (s *scanner) name(i int, text string) bool  { return s.is(i, tName, text) }

func (s *scanner) add(r model.ImportRef) {
	if len(s.res) < maxImports {
		s.res = append(s.res, r)
	}
}

func (s *scanner) run() {
	n := len(s.toks)
	for i := 0; i < n; i++ {
		t := s.toks[i]
		switch t.kind {
		case tStr:
			si := s.lx.strs[t.aux]
			if !si.bytes && !si.field && strings.Contains(si.val, ">>>") {
				s.docstring(si.val, int(t.line))
			}
		case tName:
			s.nameToken(i)
		}
	}
}

func (s *scanner) nameToken(i int) {
	t := s.toks[i]
	txt := s.text(i)
	switch txt {
	case "import":
		if t.sstart || s.name(i-1, "lazy") && s.toks[i-1].sstart {
			s.parseImport(i)
		}
		return
	case "from":
		if t.sstart || s.name(i-1, "lazy") && s.toks[i-1].sstart {
			s.parseFrom(i)
		}
		return
	}
	next := i + 1
	if t.sstart {
		switch txt {
		case "pytest_plugins":
			s.collectAssign(i, model.ImportPlugin, true)
			return
		case "INSTALLED_APPS", "MIDDLEWARE", "ROOT_URLCONF", "AUTHENTICATION_BACKENDS":
			s.collectAssign(i, model.ImportMock, false)
			return
		case "sys":
			// sys.path = ... / sys.path += ... / sys.path[...] = ...
			if s.punct(i+1, ".") && s.name(i+2, "path") && (s.punct(i+3, "=") || s.punct(i+3, "+=") || s.punct(i+3, "[")) {
				s.add(model.ImportRef{Kind: model.ImportSysPath, Dynamic: true, Line: int(t.line)})
			}
		}
	}
	if next >= len(s.toks) || !s.punct(next, "(") {
		return
	}
	s.call(i, txt)
}

// qualifier returns the identifiers of the dotted chain that precedes the name at i ("sys.path.insert" → ["sys", "path"]).
func (s *scanner) qualifier(i int) []string {
	var q []string
	for j := i; j >= 2 && s.punct(j-1, ".") && s.toks[j-2].kind == tName; j -= 2 {
		q = append([]string{s.text(j - 2)}, q...)
		if len(q) > 6 {
			break
		}
	}
	return q
}

func hasSuffix(q []string, suffix ...string) bool {
	if len(q) < len(suffix) {
		return false
	}
	for i, x := range suffix {
		if q[len(q)-len(suffix)+i] != x {
			return false
		}
	}
	return true
}

type arg struct {
	kw     string
	lo, hi int // token range of the value, hi exclusive
}

// callArgs parses the arguments of the call whose `(` is at token i. next is the index just past the `)`.
func (s *scanner) callArgs(i int) (args []arg, next int) {
	depth := 0
	start := i + 1
	flush := func(end int) {
		if end <= start {
			return
		}
		a := arg{lo: start, hi: end}
		if s.toks[start].kind == tName && s.punct(start+1, "=") && start+1 < end {
			a.kw = s.text(start)
			a.lo = start + 2
		}
		args = append(args, a)
	}
	for j := i; j < len(s.toks); j++ {
		if s.toks[j].kind != tPunct {
			continue
		}
		switch s.text(j) {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
			if depth == 0 {
				flush(j)
				return args, j + 1
			}
		case ",":
			if depth == 1 {
				flush(j)
				start = j + 1
			}
		}
	}
	return args, len(s.toks)
}

func (s *scanner) call(i int, name string) {
	line := int(s.toks[i].line)
	switch name {
	case "import_module", "__import__", "run_module", "importorskip":
		args, _ := s.callArgs(i + 1)
		kind := model.ImportDynamic
		if name == "importorskip" {
			kind = model.ImportRequire
		}
		var a *arg
		for k := range args {
			if kw := args[k].kw; kw == "" || kw == "name" || kw == "modname" || kw == "mod_name" || kw == "modulename" {
				a = &args[k]
				break
			}
		}
		if a == nil {
			s.add(model.ImportRef{Kind: kind, Line: line, Dynamic: true})
			return
		}
		k, v := s.strExpr(a.lo, a.hi)
		if k != exConst {
			s.add(model.ImportRef{Spec: v, Kind: kind, Line: line, Dynamic: true})
			return
		}
		r := model.ImportRef{Spec: v, Kind: kind, Line: line}
		if name == "import_module" && strings.HasPrefix(v, ".") {
			// importlib.import_module(".x", package): relative to `package`
			for _, pa := range args {
				if pa == *a || pa.kw != "" && pa.kw != "package" {
					continue
				}
				if pk, pv := s.strExpr(pa.lo, pa.hi); pk == exConst {
					r.Spec = absoluteName(v, pv)
				} else if !(pa.hi == pa.lo+1 && (s.name(pa.lo, "__package__") || s.name(pa.lo, "__name__"))) {
					r.Dynamic, r.Spec = true, ""
				}
				break
			}
		}
		if name == "__import__" && s.hasLevel(args) {
			r.Dynamic, r.Spec = true, ""
		}
		s.add(r)
	case "walk_packages", "iter_modules", "spec_from_file_location":
		s.add(model.ImportRef{Kind: model.ImportDynamic, Line: line, Dynamic: true})
	case "patch", "object", "multiple", "dict", "setattr":
		q := s.qualifier(i)
		switch name {
		case "patch":
		case "setattr":
			// monkeypatch.setattr("a.b.c", value): only the string-target form
		default:
			if !hasSuffix(q, "patch") {
				return
			}
		}
		args, _ := s.callArgs(i + 1)
		if len(args) == 0 || args[0].kw != "" && args[0].kw != "target" {
			return
		}
		if k, v := s.strExpr(args[0].lo, args[0].hi); k == exConst && isDottedName(v) && (name != "setattr" || strings.Contains(v, ".")) {
			s.add(model.ImportRef{Spec: v, Kind: model.ImportMock, Line: line})
		}
	case "insert", "append", "extend", "addsitedir":
		q := s.qualifier(i)
		if name == "addsitedir" {
			if !hasSuffix(q, "site") && len(q) != 0 {
				return
			}
		} else if !hasSuffix(q, "sys", "path") {
			return
		}
		args, _ := s.callArgs(i + 1)
		idx := 0
		if name == "insert" {
			idx = 1
		}
		if len(args) <= idx {
			s.add(model.ImportRef{Kind: model.ImportSysPath, Line: line, Dynamic: true})
			return
		}
		a := args[idx]
		if name == "extend" && a.hi > a.lo && s.punct(a.lo, "[") && s.punct(a.hi-1, "]") {
			s.listPaths(a.lo+1, a.hi-1, line)
			return
		}
		s.sysPath(a.lo, a.hi, line)
	}
}

// hasLevel: a __import__ call with a level argument other than a literal 0 (relative import by number).
func (s *scanner) hasLevel(args []arg) bool {
	for i, a := range args {
		if a.kw == "level" || a.kw == "" && i == 4 {
			return !(a.hi == a.lo+1 && s.toks[a.lo].kind == tNum && s.text(a.lo) == "0")
		}
	}
	return false
}

// absoluteName resolves a relative module name against a package name.
func absoluteName(rel, pkg string) string {
	level := 0
	for level < len(rel) && rel[level] == '.' {
		level++
	}
	parts := strings.Split(pkg, ".")
	if level-1 > len(parts) || pkg == "" {
		return rel
	}
	parts = parts[:len(parts)-(level-1)]
	if rest := rel[level:]; rest != "" {
		parts = append(parts, rest)
	}
	return strings.Join(parts, ".")
}

func isDottedName(v string) bool {
	if v == "" {
		return false
	}
	start := true
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '.':
			if start {
				return false
			}
			start = true
		case isIdentStart(c):
			start = false
		case c >= '0' && c <= '9':
			if start {
				return false
			}
		default:
			return false
		}
	}
	return !start
}

type exprKind uint8

const (
	exConst exprKind = iota // a constant string
	exHead                  // starts with a known string, then something computed
	exDyn                   // nothing known (head is empty)
)

// strExpr evaluates the tokens [lo,hi) as a string expression: a constant (adjacent literals, `+` of literals), a known head
// ("pkg." + x, f"pkg.{x}", "pkg.%s" % x, "pkg.{}".format(x)), or unknown.
func (s *scanner) strExpr(lo, hi int) (exprKind, string) {
	var sb strings.Builder
	pos := lo
	for {
		got := false
		for pos < hi && s.toks[pos].kind == tStr {
			si := s.lx.strs[s.toks[pos].aux]
			if si.cut {
				return exDyn, ""
			}
			sb.WriteString(si.val)
			pos++
			got = true
			if si.field {
				return exHead, sb.String()
			}
		}
		if !got {
			if pos == lo {
				return exDyn, ""
			}
			return exHead, sb.String()
		}
		if pos >= hi {
			return exConst, sb.String()
		}
		switch {
		case s.punct(pos, "+"):
			pos++
			continue
		case s.punct(pos, "%"):
			v := sb.String()
			if i := strings.IndexByte(v, '%'); i >= 0 {
				return exHead, v[:i]
			}
			return exConst, v
		case s.punct(pos, ".") && s.name(pos+1, "format"):
			v := sb.String()
			if i := strings.IndexByte(v, '{'); i >= 0 {
				return exHead, v[:i]
			}
			return exConst, v
		}
		return exDyn, ""
	}
}

func (s *scanner) parseImport(i int) {
	line := int(s.toks[i].line)
	j := i + 1
	for {
		mod, e := s.dotted(j)
		if mod == "" {
			return
		}
		s.add(model.ImportRef{Spec: mod, Kind: model.ImportStatic, Line: line})
		j = e
		if s.name(j, "as") && j+1 < len(s.toks) && s.toks[j+1].kind == tName {
			j += 2
		}
		if !s.punct(j, ",") {
			return
		}
		j++
	}
}

// dotted reads `a.b.c` starting at token j; returns the name and the index after it ("" if there is none).
func (s *scanner) dotted(j int) (string, int) {
	if j >= len(s.toks) || s.toks[j].kind != tName {
		return "", j
	}
	var sb strings.Builder
	sb.WriteString(s.text(j))
	j++
	for s.punct(j, ".") && j+1 < len(s.toks) && s.toks[j+1].kind == tName {
		sb.WriteByte('.')
		sb.WriteString(s.text(j + 1))
		j += 2
	}
	return sb.String(), j
}

func (s *scanner) parseFrom(i int) {
	line := int(s.toks[i].line)
	j := i + 1
	dots := 0
	for s.punct(j, ".") {
		dots++
		j++
	}
	mod := ""
	if s.name(j, "import") && dots > 0 {
		// from . import x
	} else {
		m, e := s.dotted(j)
		if m == "" {
			return
		}
		mod, j = m, e
	}
	if !s.name(j, "import") {
		return // `yield from x`, `raise A from B`
	}
	j++
	paren := s.punct(j, "(")
	if paren {
		j++
	}
	var names []string
	for j < len(s.toks) {
		switch {
		case s.punct(j, "*"):
			names = append(names, "*")
			j++
		case s.toks[j].kind == tName:
			names = append(names, s.text(j))
			j++
			if s.name(j, "as") && j+1 < len(s.toks) && s.toks[j+1].kind == tName {
				j += 2
			}
		default:
			goto done
		}
		if s.punct(j, ",") {
			j++
			continue
		}
		break
	}
done:
	if len(names) == 0 {
		return
	}
	s.add(model.ImportRef{Spec: strings.Repeat(".", dots) + mod, Suffix: strings.Join(names, ","), Kind: model.ImportStatic, Line: line})
}

// collectAssign handles `NAME = <expr>`, `NAME += <expr>` and `NAME: T = <expr>` where the expression is made of string literals.
func (s *scanner) collectAssign(i int, kind model.ImportKind, strict bool) {
	j := i + 1
	if s.punct(j, ":") {
		for j < len(s.toks) && !s.punct(j, "=") && s.toks[j].kind != tNewline {
			j++
		}
	}
	if !(s.punct(j, "=") || s.punct(j, "+=")) {
		return
	}
	line := int(s.toks[i].line)
	dynamic := false
	for j++; j < len(s.toks) && s.toks[j].kind != tNewline && !s.punct(j, ";"); j++ {
		switch s.toks[j].kind {
		case tStr:
			si := s.lx.strs[s.toks[j].aux]
			if si.field || si.cut {
				dynamic = true
				continue
			}
			if si.val != "" && !si.bytes {
				s.add(model.ImportRef{Spec: si.val, Kind: kind, Line: int(s.toks[j].line)})
			}
		case tName:
			dynamic = true
		}
	}
	if strict && dynamic {
		s.add(model.ImportRef{Kind: kind, Line: line, Dynamic: true})
	}
}

// docstring extracts `>>> import x` / `>>> from x import y` lines of a doctest.
func (s *scanner) docstring(val string, line int) {
	for k, l := range strings.Split(val, "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, ">>>") {
			continue
		}
		l = strings.TrimSpace(strings.TrimPrefix(l, ">>>"))
		l = strings.TrimSpace(strings.SplitN(l, ";", 2)[0])
		ln := line + k
		switch {
		case strings.HasPrefix(l, "import "):
			for _, part := range strings.Split(strings.TrimPrefix(l, "import "), ",") {
				if f := strings.Fields(part); len(f) > 0 && isDottedName(f[0]) {
					s.add(model.ImportRef{Spec: f[0], Kind: model.ImportMock, Line: ln})
				}
			}
		case strings.HasPrefix(l, "from "):
			f := strings.Fields(l)
			if len(f) >= 4 && f[2] == "import" {
				mod := strings.TrimLeft(f[1], ".")
				if mod == "" || isDottedName(mod) {
					names := strings.NewReplacer("(", " ", ")", " ", ",", " ").Replace(strings.Join(f[3:], " "))
					var out []string
					for _, nm := range strings.Fields(names) {
						if nm == "as" {
							break
						}
						out = append(out, nm)
					}
					if len(out) > 0 {
						s.add(model.ImportRef{Spec: f[1], Suffix: strings.Join(out, ","), Kind: model.ImportMock, Line: ln})
					}
				}
			}
		}
	}
}

// listPaths handles sys.path.extend([a, b]).
func (s *scanner) listPaths(lo, hi, line int) {
	start, depth := lo, 0
	for j := lo; j <= hi; j++ {
		if j < hi && s.toks[j].kind == tPunct {
			switch s.text(j) {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			}
		}
		if j == hi || depth == 0 && s.punct(j, ",") {
			if j > start {
				s.sysPath(start, j, line)
			}
			start = j + 1
		}
	}
}

// sysPath records one sys.path entry.
func (s *scanner) sysPath(lo, hi, line int) {
	pv, ok := s.pathExpr(lo, hi)
	spec := ""
	if ok {
		switch {
		case pv.kind == pvStr && !strings.HasPrefix(pv.s, "/") && !strings.Contains(pv.s, ":") && !strings.Contains(pv.s, "\\"):
			spec = "cwd:" + path.Clean(pv.s)
		case pv.kind == pvFile && !pv.self:
			spec = path.Clean(pv.s)
		}
	}
	if spec == "" {
		s.add(model.ImportRef{Kind: model.ImportSysPath, Line: line, Dynamic: true})
		return
	}
	s.add(model.ImportRef{Spec: spec, Kind: model.ImportSysPath, Line: line})
}

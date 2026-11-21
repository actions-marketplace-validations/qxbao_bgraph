package evalkit

import (
	"path"
	"strings"
)

// Python mutation generator for T2: a small tokenizer (comments and strings, f-strings included, are single tokens) finds one-token
// operator flips in non-test code. Like the Rust generator it does not parse, so it is conservative; candidates that do not compile are
// filtered out later by the oracle (compile check before the mutant runs).

type pyTokKind int

const (
	ptName pyTokKind = iota
	ptNum
	ptStr
	ptOp
	ptNewline // end of a logical line
)

type pyTok struct {
	kind       pyTokKind
	start, end int
}

func (t pyTok) text(src []byte) string { return string(src[t.start:t.end]) }

var pyOps3 = map[string]bool{"**=": true, "//=": true, ">>=": true, "<<=": true, "...": true}
var pyOps2 = map[string]bool{"==": true, "!=": true, "<=": true, ">=": true, "->": true, "+=": true, "-=": true, "*=": true, "/=": true, "%=": true,
	"&=": true, "|=": true, "^=": true, "**": true, "//": true, "<<": true, ">>": true, ":=": true, "@=": true}

func pyIsStrPrefix(w string) bool {
	switch strings.ToLower(w) {
	case "r", "u", "b", "f", "br", "rb", "fr", "rf":
		return true
	}
	return false
}

// pySkipString returns the end of the string literal whose opening quote is at src[i]; f is true for an f-string (nested quotes inside
// replacement fields are skipped).
func pySkipString(src []byte, i int, f bool) int {
	n := len(src)
	q := src[i]
	triple := i+2 < n && src[i+1] == q && src[i+2] == q
	if triple {
		i += 3
	} else {
		i++
	}
	for i < n {
		c := src[i]
		switch {
		case c == '\\':
			i += 2
			continue
		case f && c == '{':
			if i+1 < n && src[i+1] == '{' {
				i += 2
				continue
			}
			// replacement field: skip to the matching brace, honouring nested strings
			depth := 1
			i++
			for i < n && depth > 0 {
				switch src[i] {
				case '{':
					depth++
				case '}':
					depth--
				case '\'', '"':
					if depth > 0 {
						i = pySkipString(src, i, false)
						continue
					}
				case '\n':
					if !triple {
						return i // unterminated; give up on this string
					}
				}
				i++
			}
			continue
		case c == q:
			if !triple {
				return i + 1
			}
			if i+2 < n && src[i+1] == q && src[i+2] == q {
				return i + 3
			}
		case c == '\n' && !triple:
			return i
		}
		i++
	}
	return n
}

// lexPython returns the significant tokens of src: comments and insignificant newlines are dropped, a logical line ends with a ptNewline
// token.
func lexPython(src []byte) []pyTok {
	var out []pyTok
	n := len(src)
	depth := 0
	for i := 0; i < n; {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\f':
			i++
		case c == '\\' && i+1 < n && (src[i+1] == '\n' || src[i+1] == '\r'):
			i++
			for i < n && (src[i] == '\r' || src[i] == '\n') {
				i++
			}
		case c == '\n':
			if depth == 0 && len(out) > 0 && out[len(out)-1].kind != ptNewline {
				out = append(out, pyTok{ptNewline, i, i})
			}
			i++
		case c == '#':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '"' || c == '\'':
			e := pySkipString(src, i, false)
			out = append(out, pyTok{ptStr, i, e})
			i = e
		case isIdentStart(c):
			j := i
			for j < n && isIdentChar(src[j]) {
				j++
			}
			if j < n && (src[j] == '"' || src[j] == '\'') && pyIsStrPrefix(string(src[i:j])) {
				e := pySkipString(src, j, strings.ContainsAny(string(src[i:j]), "fF"))
				out = append(out, pyTok{ptStr, i, e})
				i = e
				continue
			}
			out = append(out, pyTok{ptName, i, j})
			i = j
		case c >= '0' && c <= '9' || c == '.' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9':
			j := i + 1
			for j < n && (isIdentChar(src[j]) || src[j] == '.' ||
				(src[j] == '+' || src[j] == '-') && (src[j-1] == 'e' || src[j-1] == 'E') && !(j > i+1 && (src[i+1] == 'x' || src[i+1] == 'X'))) {
				j++
			}
			out = append(out, pyTok{ptNum, i, j})
			i = j
		default:
			l := 1
			if i+3 <= n && pyOps3[string(src[i:i+3])] {
				l = 3
			} else if i+2 <= n && pyOps2[string(src[i:i+2])] {
				l = 2
			}
			switch c {
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				if depth > 0 {
					depth--
				}
			}
			out = append(out, pyTok{ptOp, i, i + l})
			i += l
		}
	}
	return out
}

var pyKeywords = map[string]bool{"and": true, "or": true, "not": true, "in": true, "is": true, "if": true, "else": true, "elif": true, "for": true, "while": true,
	"return": true, "yield": true, "lambda": true, "await": true, "async": true, "def": true, "class": true, "import": true, "from": true, "as": true,
	"with": true, "try": true, "except": true, "finally": true, "raise": true, "assert": true, "del": true, "pass": true, "break": true, "continue": true,
	"global": true, "nonlocal": true}

// pyScan returns the mutation candidates of one Python source file.
func pyScan(src []byte) []rustCand {
	toks := lexPython(src)
	txt := func(k int) string {
		if k < 0 || k >= len(toks) {
			return ""
		}
		return toks[k].text(src)
	}
	var cands []rustCand
	add := func(s, e int, repl, op string) { cands = append(cands, rustCand{Start: s, End: e, Repl: repl, Op: op}) }
	for ls := 0; ls < len(toks); {
		le := ls
		for le < len(toks) && toks[le].kind != ptNewline {
			le++
		}
		pyScanLine(src, toks, ls, le, txt, add)
		ls = le + 1
	}
	return cands
}

// pyScanLine scans the tokens [ls,le) of one logical line.
func pyScanLine(src []byte, toks []pyTok, ls, le int, txt func(int) string, add func(s, e int, repl, op string)) {
	if ls >= le {
		return
	}
	first := txt(ls)
	if first == "import" || first == "from" || first == "@" {
		return
	}
	sig := first == "def" || first == "class" || (first == "async" && txt(ls+1) == "def")
	operandBefore := func(k int) bool {
		if k <= ls {
			return false
		}
		p := toks[k-1]
		switch p.kind {
		case ptNum, ptStr:
			return true
		case ptName:
			w := p.text(src)
			return !pyKeywords[w] || w == "None"
		}
		t := p.text(src)
		return t == ")" || t == "]" || t == "}"
	}
	spaced := func(t pyTok) bool {
		return t.start > 0 && t.end < len(src) && src[t.start-1] == ' ' && src[t.end] == ' '
	}
	depth := 0
	var forDepths []int
	for k := ls; k < le; k++ {
		t := toks[k]
		s := txt(k)
		if t.kind == ptOp {
			switch s {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			}
		}
		if s == "for" && t.kind == ptName {
			forDepths = append(forDepths, depth)
		}
		if t.kind == ptName && (s == "True" || s == "False") && txt(k-1) != "." {
			repl := "False"
			if s == "False" {
				repl = "True"
			}
			add(t.start, t.end, repl, strings.ToLower(s)+"-"+strings.ToLower(repl))
			continue
		}
		if sig {
			continue
		}
		switch {
		case t.kind == ptOp && s == "==":
			add(t.start, t.end, "!=", "eq-ne")
		case t.kind == ptOp && s == "!=":
			add(t.start, t.end, "==", "ne-eq")
		case t.kind == ptOp && s == "<":
			add(t.start, t.end, ">=", "lt-ge")
		case t.kind == ptOp && s == ">":
			add(t.start, t.end, "<=", "gt-le")
		case t.kind == ptOp && s == "<=":
			add(t.start, t.end, ">", "le-gt")
		case t.kind == ptOp && s == ">=":
			add(t.start, t.end, "<", "ge-lt")
		case t.kind == ptOp && (s == "+" || s == "-") && spaced(t) && operandBefore(k) && k+1 < le:
			if s == "+" {
				add(t.start, t.end, "-", "add-sub")
			} else {
				add(t.start, t.end, "+", "sub-add")
			}
		case t.kind == ptName && s == "and":
			add(t.start, t.end, "or", "and-or")
		case t.kind == ptName && s == "or":
			add(t.start, t.end, "and", "or-and")
		case t.kind == ptName && s == "is":
			if txt(k+1) == "not" && k+1 < le {
				add(t.start, toks[k+1].end, "is", "isnot-is")
			} else {
				add(t.start, t.end, "is not", "is-isnot")
			}
		case t.kind == ptName && s == "in":
			switch {
			case txt(k-1) == "not" && k-1 >= ls:
				add(toks[k-1].start, t.end, "in", "notin-in")
			case len(forDepths) > 0 && forDepths[len(forDepths)-1] == depth:
				forDepths = forDepths[:len(forDepths)-1] // the `in` of a for header / comprehension
			default:
				add(t.start, t.end, "not in", "in-notin")
			}
		case t.kind == ptName && s == "return" && k+1 < le:
			simple := true
			for j := k + 1; j < le; j++ {
				w := txt(j)
				switch toks[j].kind {
				case ptNum, ptStr:
				case ptName:
					simple = simple && !pyKeywords[w]
				default:
					simple = simple && w == "."
				}
			}
			if simple && !(k+2 == le && txt(k+1) == "None") && toks[k+1].kind != ptOp {
				add(toks[k+1].start, toks[le-1].end, "None", "return-none")
			}
		}
	}
}

// pyCandidateFile reports whether a repo path is non-test Python source that T2 may mutate: no tests/docs/examples/benchmark directories, no
// conftest/setup files, no test modules.
func pyCandidateFile(f string) bool {
	if !strings.HasSuffix(f, ".py") {
		return false
	}
	base := path.Base(f)
	if base == "conftest.py" || base == "setup.py" || strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") || base == "noxfile.py" {
		return false
	}
	for _, seg := range strings.Split(path.Dir(f), "/") {
		switch seg {
		case "tests", "test", "testing", "docs", "doc", "examples", "example", "bench", "benchmarks", "performance", "typing_tests", "scripts",
			"venv", ".venv", "node_modules", "site-packages", "build", "dist", ".tox", ".git", ".github":
			return false
		}
	}
	return true
}

// GenPyMutants deterministically picks up to n Python mutants spread over the files (see spreadMutants).
func GenPyMutants(files []RustMutantSource, seed, idPrefix string, n int) []T2Mutant {
	cands := map[string][]rustCand{}
	for _, f := range files {
		cands[f.Path] = pyScan(f.Data)
	}
	return spreadMutants(files, cands, nil, seed, idPrefix, n)
}

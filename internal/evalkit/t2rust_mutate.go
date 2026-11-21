package evalkit

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Rust mutation generator for T2: a lexical pass (comments, strings, chars and lifetimes are skipped) that finds one-token operator
// and literal flips in non-test code. It does not parse Rust, so it is conservative: binary operators are only mutated when they are
// space-delimited and follow an operand, which keeps generics (`Vec<u8>`), closures (`|| x`), references (`&&T`) and unary minus out.

type rtokKind int

const (
	rtIdent rtokKind = iota
	rtNum
	rtStr
	rtPunct
)

type rtok struct {
	kind       rtokKind
	start, end int
}

func (t rtok) text(src []byte) string { return string(src[t.start:t.end]) }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentChar(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

var rustPuncts3 = map[string]bool{"<<=": true, ">>=": true, "...": true, "..=": true}
var rustPuncts2 = map[string]bool{"==": true, "!=": true, "<=": true, ">=": true, "&&": true, "||": true, "+=": true, "-=": true, "->": true, "=>": true,
	"<<": true, ">>": true, "::": true, "..": true, "|=": true, "&=": true, "*=": true, "/=": true, "%=": true, "^=": true}

// lexRust returns the significant tokens of src (comments dropped).
func lexRust(src []byte) []rtok {
	var out []rtok
	n := len(src)
	i := 0
	// rawString handles r"..", r#".."# (and b/c prefixes): src[i] is the first quote-or-hash after the prefix. Returns the end or -1.
	rawString := func(i int) int {
		h := 0
		for i < n && src[i] == '#' {
			h++
			i++
		}
		if i >= n || src[i] != '"' {
			return -1
		}
		i++
		for i < n {
			if src[i] == '"' {
				j, k := i+1, 0
				for j < n && src[j] == '#' && k < h {
					j++
					k++
				}
				if k == h {
					return j
				}
			}
			i++
		}
		return n
	}
	quoted := func(i int, q byte) int { // src[i] is the opening quote
		i++
		for i < n {
			switch src[i] {
			case '\\':
				i += 2
				continue
			case q:
				return i + 1
			}
			i++
		}
		return n
	}
	for i < n {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			depth := 1
			i += 2
			for i < n && depth > 0 {
				switch {
				case src[i] == '/' && i+1 < n && src[i+1] == '*':
					depth++
					i += 2
				case src[i] == '*' && i+1 < n && src[i+1] == '/':
					depth--
					i += 2
				default:
					i++
				}
			}
		case c == '"':
			e := quoted(i, '"')
			out = append(out, rtok{rtStr, i, e})
			i = e
		case isIdentStart(c):
			j := i
			for j < n && isIdentChar(src[j]) {
				j++
			}
			word := string(src[i:j])
			// string prefixes: r, b, br, c, cr
			if j < n && (src[j] == '"' || src[j] == '#' || src[j] == '\'') && (word == "r" || word == "b" || word == "br" || word == "c" || word == "cr") {
				switch {
				case src[j] == '"' && (word == "b" || word == "c"):
					e := quoted(j, '"')
					out = append(out, rtok{rtStr, i, e})
					i = e
					continue
				case (src[j] == '"' || src[j] == '#') && strings.HasSuffix(word, "r"):
					if e := rawString(j); e >= 0 {
						out = append(out, rtok{rtStr, i, e})
						i = e
						continue
					}
				case src[j] == '\'' && word == "b":
					e := quoted(j, '\'')
					out = append(out, rtok{rtStr, i, e})
					i = e
					continue
				}
			}
			if word == "r" && j+1 < n && src[j] == '#' && isIdentStart(src[j+1]) { // raw identifier r#name
				j++
				for j < n && isIdentChar(src[j]) {
					j++
				}
			}
			out = append(out, rtok{rtIdent, i, j})
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < n && (isIdentChar(src[j]) || (src[j] == '.' && j+1 < n && src[j+1] >= '0' && src[j+1] <= '9')) {
				j++
			}
			out = append(out, rtok{rtNum, i, j})
			i = j
		case c == '\'':
			// char literal 'x' / '\n' versus lifetime 'a
			if i+1 < n && src[i+1] == '\\' {
				e := quoted(i, '\'')
				out = append(out, rtok{rtStr, i, e})
				i = e
				continue
			}
			// one (possibly multi-byte) character then a closing quote
			j := i + 1
			if j < n {
				j++
				for j < n && src[j]&0xC0 == 0x80 {
					j++
				}
			}
			if j < n && src[j] == '\'' {
				out = append(out, rtok{rtStr, i, j + 1})
				i = j + 1
				continue
			}
			j = i + 1
			for j < n && isIdentChar(src[j]) {
				j++
			}
			out = append(out, rtok{rtIdent, i, j}) // lifetime
			i = j
		default:
			l := 1
			if i+3 <= n && rustPuncts3[string(src[i:i+3])] {
				l = 3
			} else if i+2 <= n && rustPuncts2[string(src[i:i+2])] {
				l = 2
			}
			out = append(out, rtok{rtPunct, i, i + l})
			i += l
		}
	}
	return out
}

// rustCand is a mutation candidate: replace src[Start:End] by Repl.
type rustCand struct {
	Start, End int
	Repl, Op   string
}

// rustScan returns the mutation candidates of a Rust source file plus the names of `#[cfg(test)] mod name;` declarations (whose files are test code).
func rustScan(src []byte) (cands []rustCand, testMods []string) {
	toks := lexRust(src)
	txt := func(k int) string {
		if k < 0 || k >= len(toks) {
			return ""
		}
		return toks[k].text(src)
	}
	// skipItem returns the index after the item starting at token k: a braced block, or up to a top-level `;`.
	skipItem := func(k int) int {
		depth := 0
		for ; k < len(toks); k++ {
			switch txt(k) {
			case "(", "[":
				depth++
			case ")", "]":
				depth--
			case ";":
				if depth <= 0 {
					return k + 1
				}
			case "{":
				if depth <= 0 {
					d := 0
					for ; k < len(toks); k++ {
						switch txt(k) {
						case "{":
							d++
						case "}":
							d--
							if d == 0 {
								return k + 1
							}
						}
					}
					return k
				}
			}
		}
		return k
	}
	pendingSkip := false
	for k := 0; k < len(toks); {
		t := toks[k]
		s := txt(k)
		// attributes
		if s == "#" && (txt(k+1) == "[" || (txt(k+1) == "!" && txt(k+2) == "[")) {
			j := k + 1
			if txt(j) == "!" {
				j++
			}
			d := 0
			end := j
			for ; end < len(toks); end++ {
				if txt(end) == "[" {
					d++
				} else if txt(end) == "]" {
					d--
					if d == 0 {
						break
					}
				}
			}
			if testOnlyAttr(toks, src, j, end) {
				pendingSkip = true
			}
			k = end + 1
			continue
		}
		if pendingSkip {
			if txt(k) == "mod" && txt(k+2) == ";" && toks[k+1].kind == rtIdent {
				testMods = append(testMods, txt(k+1))
			}
			pendingSkip = false
			k = skipItem(k)
			continue
		}
		if s == "macro_rules" && txt(k+1) == "!" {
			k = skipItem(k + 2)
			continue
		}
		if c, ok := rustMutation(src, toks, k); ok {
			cands = append(cands, c)
		}
		_ = t
		k++
	}
	return cands, testMods
}

// testOnlyAttr reports whether the attribute `[ ... ]` (tokens j..end) marks test-only code: #[test], #[bench], #[tokio::test],
// #[cfg(test)] and #[cfg(all(test, ...))] without any not/any.
func testOnlyAttr(toks []rtok, src []byte, j, end int) bool {
	var ids []string
	for k := j + 1; k < end; k++ {
		if toks[k].kind == rtIdent {
			ids = append(ids, toks[k].text(src))
		}
	}
	if len(ids) == 0 {
		return false
	}
	last := ids[len(ids)-1]
	if ids[0] != "cfg" && ids[0] != "cfg_attr" {
		return last == "test" || last == "bench"
	}
	if ids[0] == "cfg_attr" {
		return false
	}
	has := false
	for _, id := range ids[1:] {
		switch id {
		case "not", "any":
			return false
		case "test":
			has = true
		}
	}
	return has
}

var rustNonOperand = map[string]bool{"return": true, "in": true, "if": true, "else": true, "match": true, "while": true, "move": true, "mut": true,
	"ref": true, "let": true, "for": true, "loop": true, "break": true, "continue": true, "unsafe": true, "async": true, "await": false, "impl": true,
	"dyn": true, "where": true, "pub": true, "fn": true, "as": true, "use": true, "mod": true, "const": true, "static": true, "struct": true, "enum": true,
	"trait": true, "type": true}

// rustMutation returns the mutation for token k, if it is a mutable operator in an operand context.
func rustMutation(src []byte, toks []rtok, k int) (rustCand, bool) {
	t := toks[k]
	s := t.text(src)
	spaced := t.start > 0 && t.end < len(src) && src[t.start-1] == ' ' && src[t.end] == ' '
	// operand before / after on the same line
	operandBefore := func() bool {
		if k == 0 {
			return false
		}
		p := toks[k-1]
		if p.end > t.start || strings.Contains(string(src[p.end:t.start]), "\n") {
			return false
		}
		switch p.kind {
		case rtNum, rtStr:
			return true
		case rtIdent:
			w := p.text(src)
			return !rustNonOperand[w] && !strings.HasPrefix(w, "'")
		}
		pt := p.text(src)
		return pt == ")" || pt == "]" || pt == "?"
	}
	operandAfter := func() bool {
		if k+1 >= len(toks) {
			return false
		}
		n := toks[k+1]
		if strings.Contains(string(src[t.end:n.start]), "\n") {
			return false
		}
		switch n.kind {
		case rtNum, rtStr:
			return true
		case rtIdent:
			return !strings.HasPrefix(n.text(src), "'")
		}
		nt := n.text(src)
		return nt == "(" || nt == "!" || nt == "&" || nt == "*"
	}
	mk := func(repl, op string) (rustCand, bool) { return rustCand{t.start, t.end, repl, op}, true }
	if inSignatureLine(src, t.start) {
		return rustCand{}, false
	}
	switch s {
	case "true":
		if t.kind == rtIdent {
			return mk("false", "true-false")
		}
	case "false":
		if t.kind == rtIdent {
			return mk("true", "false-true")
		}
	case "==":
		return mk("!=", "eq-ne")
	case "!=":
		return mk("==", "ne-eq")
	case "<=":
		if spaced {
			return mk(">", "le-gt")
		}
	case ">=":
		if spaced {
			return mk("<", "ge-lt")
		}
	case "<":
		if spaced && operandBefore() && operandAfter() {
			return mk(">=", "lt-ge")
		}
	case ">":
		if spaced && operandBefore() && operandAfter() {
			return mk("<=", "gt-le")
		}
	case "+":
		if spaced && operandBefore() && operandAfter() {
			return mk("-", "add-sub")
		}
	case "-":
		if spaced && operandBefore() && operandAfter() {
			return mk("+", "sub-add")
		}
	case "&&":
		if spaced && operandBefore() && operandAfter() {
			return mk("||", "and-or")
		}
	case "||":
		if spaced && operandBefore() && operandAfter() {
			return mk("&&", "or-and")
		}
	}
	return rustCand{}, false
}

// RustMutantSource is one file at R handed to the generator.
type RustMutantSource struct {
	Path string
	Blob string // blob OID
	Data []byte
}

func seedHash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

// lineOf returns the 1-based line of offset off and the text of that line.
func lineOf(src []byte, off int) (int, string) {
	line := 1 + strings.Count(string(src[:off]), "\n")
	s := off
	for s > 0 && src[s-1] != '\n' {
		s--
	}
	e := off
	for e < len(src) && src[e] != '\n' {
		e++
	}
	return line, strings.TrimSpace(strings.TrimSuffix(string(src[s:e]), "\r"))
}

// GenRustMutants deterministically picks up to n mutants spread over the files: candidates are ranked per file by a seeded hash, one per line,
// and files are visited round-robin in seeded order. Files that are test modules of a parent (`#[cfg(test)] mod x;`) are excluded.
func GenRustMutants(files []RustMutantSource, seed, idPrefix string, n int) []T2Mutant {
	cands := map[string][]rustCand{}
	excluded := map[string]bool{}
	for _, f := range files {
		cs, tm := rustScan(f.Data)
		cands[f.Path] = cs
		dir := path.Dir(f.Path)
		base := strings.TrimSuffix(path.Base(f.Path), ".rs")
		sub := dir
		if base != "mod" && base != "lib" && base != "main" {
			sub = path.Join(dir, base)
		}
		for _, m := range tm {
			excluded[path.Join(sub, m+".rs")] = true
			excluded[path.Join(sub, m, "mod.rs")] = true
		}
	}
	return spreadMutants(files, cands, excluded, seed, idPrefix, n)
}

// spreadMutants picks up to n mutants from the per-file candidates (keyed by path), skipping the files in skip: candidates are ranked per
// file by a seeded hash, one per line, and files are visited round-robin in seeded order. Shared by the Rust and Python generators.
func spreadMutants(files []RustMutantSource, candsByPath map[string][]rustCand, skip map[string]bool, seed, idPrefix string, n int) []T2Mutant {
	type fc struct {
		src   RustMutantSource
		cands []rustCand
	}
	var order []*fc
	for _, f := range files {
		if skip[f.Path] || len(candsByPath[f.Path]) == 0 {
			continue
		}
		order = append(order, &fc{src: f, cands: append([]rustCand(nil), candsByPath[f.Path]...)})
	}
	sort.Slice(order, func(i, j int) bool {
		return seedHash(seed, "file", order[i].src.Path) < seedHash(seed, "file", order[j].src.Path)
	})
	for _, f := range order {
		seen := map[int]bool{}
		sort.Slice(f.cands, func(i, j int) bool {
			return seedHash(seed, f.src.Path, strconv.Itoa(f.cands[i].Start)) < seedHash(seed, f.src.Path, strconv.Itoa(f.cands[j].Start))
		})
		var keep []rustCand
		for _, c := range f.cands {
			l, _ := lineOf(f.src.Data, c.Start)
			if !seen[l] {
				seen[l] = true
				keep = append(keep, c)
			}
		}
		f.cands = keep
	}
	var out []T2Mutant
	for round := 0; len(out) < n; round++ {
		any := false
		for _, f := range order {
			if round >= len(f.cands) || len(out) >= n {
				continue
			}
			any = true
			c := f.cands[round]
			line, before := lineOf(f.src.Data, c.Start)
			mutated := append(append(append([]byte(nil), f.src.Data[:c.Start]...), c.Repl...), f.src.Data[c.End:]...)
			_, after := lineOf(mutated, c.Start)
			out = append(out, T2Mutant{File: f.src.Path, Line: line, Op: c.Op, Start: c.Start, End: c.End, Repl: c.Repl, Before: before, After: after, Blob: f.src.Blob})
		}
		if !any {
			break
		}
	}
	for i := range out {
		out[i].ID = idPrefix + "-" + pad2(i+1)
	}
	return out
}

func pad2(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	s := ""
	for ; i > 0; i /= 10 {
		s = string(rune('0'+i%10)) + s
	}
	return s
}

// reSigLine matches lines that start an item signature; `+` and `<`/`>` there are trait bounds and generics, not arithmetic.
var reSigLine = regexp.MustCompile(`^(pub(\([^)]*\))?\s+)?((const|async|unsafe|extern\s+"[^"]*")\s+)*(fn|impl|trait|struct|enum|type|where)\b`)

func inSignatureLine(src []byte, off int) bool {
	s := off
	for s > 0 && src[s-1] != '\n' {
		s--
	}
	head := strings.TrimSpace(string(src[s:off])) // the part of the line before the token
	return !strings.Contains(head, "{") && reSigLine.MatchString(head)
}

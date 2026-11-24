package phpdyn

// Body guard of the PHP function-level refinement (docs/dynamic-layer.md §11.2). A PHP file record lists, per included
// repository file, the lines Xdebug saw executed and the instrumentable lines it saw not executed. A change can be judged by
// those lines only when it lies inside the body of a named function or method: the body runs only when the function is called,
// so a test that executed no line of it is unaffected. Everything else in a file (namespace, use, declare, top-level
// statements, class constants, properties, trait uses, enum cases, signatures, attributes, doc comments read by reflection,
// conditional declarations, functions declared inside functions, anonymous classes and closures outside a named function body)
// matters when the file is loaded or a declaration is reflected, so the guard refuses it and the file is judged at file level.
//
// The guard reads both versions with internal/scan/php's full tokenizer and is fail-closed. A "body" is the lines strictly
// inside an outermost named function or method of a class declared at file or namespace level, unconditionally: after the
// line of its opening brace and before the line of its closing brace. The "skeleton" of a file is every token (comments
// dropped, doc comments kept) that does not start on a body line; it must be identical at base and head. Bodies are then
// paired in order, and each pair whose tokens differ is a changed function, identified by its base span (the line of the
// `function` keyword to the line of its closing brace).

import (
	"fmt"
	"strings"

	phpscan "github.com/qxbao/bgraph/internal/scan/php"
)

// Span is a changed function of the base file: lines From (the `function` keyword) to To (the closing brace).
type Span struct {
	From, To int
	What     string // namespace, class path and name, for messages
}

// FuncSpan is a named function with a body, at any depth (for the cross-check against PHP's own tokenizer).
type FuncSpan struct{ Start, End int }

type frameKind uint8

const (
	fBlock     frameKind = iota + 1 // any other brace
	fNamespace                      // namespace X { ... }
	fClass                          // a named class, interface, trait or enum
	fAnonClass                      // new class { ... }
	fFunc                           // a named function or method
	fClosure                        // function () { ... }
)

type frame struct {
	kind frameKind
	name string
	fn   *function // fFunc
}

// function is one named function or method with a body.
type function struct {
	id         string
	start      int // line of the `function` keyword
	open       int // line of the opening brace
	end        int // line of the closing brace
	body       bool
	declares   string // a declaration inside the body that makes a change unjudgeable ("" when none)
	tokenStart int    // index of the first token after the opening brace
	tokenEnd   int    // index of the closing brace
}

// phpFile is the guard's view of one version of a file.
type phpFile struct {
	toks   []phpscan.Token
	funcs  []*function // every named function with a body, in source order
	bodies []*function // the outermost judgeable bodies, in source order
	lines  int
}

// significant reports whether a token takes part in the structure (not a comment).
func significant(t phpscan.Token) bool {
	return t.Kind != phpscan.TokComment && t.Kind != phpscan.TokDocComment
}

func isPunct(t phpscan.Token, s string) bool { return t.Kind == phpscan.TokPunct && t.Text == s }

func isKeyword(t phpscan.Token, s string) bool {
	return t.Kind == phpscan.TokIdent && strings.EqualFold(t.Text, s)
}

// parsePHP reads the structure of src. It returns a reason when the file cannot be followed with certainty.
func parsePHP(src []byte) (*phpFile, string) {
	toks, why := phpscan.Tokens(src)
	if why != "" {
		return nil, "the lexer cannot read the file with certainty (" + why + ")"
	}
	f := &phpFile{toks: toks, lines: strings.Count(string(src), "\n") + 1}
	// sig: indexes of significant tokens, for looking around without comments.
	var sig []int
	for i, t := range toks {
		if significant(t) {
			sig = append(sig, i)
		}
	}
	at := func(k int) phpscan.Token {
		if k < 0 || k >= len(sig) {
			return phpscan.Token{}
		}
		return toks[sig[k]]
	}
	member := func(k int) bool { // the significant token k is a member name (after ->, ?->, ::)
		p, q := at(k-1), at(k-2)
		return isPunct(p, ">") && isPunct(q, "-") || isPunct(p, ":") && isPunct(q, ":")
	}
	// skipParens returns the significant index after the parenthesized group starting at k ("(" expected).
	skipParens := func(k int) (int, bool) {
		if !isPunct(at(k), "(") {
			return k, false
		}
		depth := 0
		for ; k < len(sig); k++ {
			switch t := at(k); {
			case isPunct(t, "("):
				depth++
			case isPunct(t, ")"):
				depth--
				if depth == 0 {
					return k + 1, true
				}
			}
		}
		return k, false
	}
	var stack []frame
	ns := ""
	anon := 0
	pendingClass := "" // name of the class-like whose opening brace comes next ("" none)
	pendingAnon := false
	var pendingFn *function
	pendingClosure := false
	inFunc := func() *function { // the innermost enclosing named function, if any
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].kind == fFunc {
				return stack[i].fn
			}
		}
		return nil
	}
	classPath := func() []string {
		var p []string
		for _, fr := range stack {
			if fr.kind == fClass || fr.kind == fAnonClass {
				p = append(p, fr.name)
			}
		}
		return p
	}
	// judgeable: a named function whose body is a body of the guard (outermost, in a named class or at file level, declared
	// unconditionally, not inside an anonymous class).
	judgeable := func() bool {
		for i, fr := range stack {
			switch fr.kind {
			case fNamespace:
				if i != 0 {
					return false
				}
			case fClass:
				if i != len(stack)-1 || i > 1 || i == 1 && stack[0].kind != fNamespace {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	for k := 0; k < len(sig); k++ {
		t := at(k)
		switch {
		case t.Kind == phpscan.TokIdent && !member(k):
			switch strings.ToLower(t.Text) {
			case "function":
				j := k + 1
				if isPunct(at(j), "&") {
					j++
				}
				switch n := at(j); {
				case n.Kind == phpscan.TokIdent && isPunct(at(j+1), "("):
					name := n.Text
					e, ok := skipParens(j + 1)
					if !ok {
						return nil, fmt.Sprintf("line %d: unbalanced parameter list", t.Line)
					}
					// return type: up to the body's brace or the declaration's semicolon
					for e < len(sig) && !isPunct(at(e), "{") && !isPunct(at(e), ";") {
						if isPunct(at(e), "(") {
							if e, ok = skipParens(e); !ok {
								return nil, fmt.Sprintf("line %d: unbalanced return type", t.Line)
							}
							continue
						}
						e++
					}
					if e >= len(sig) {
						return nil, fmt.Sprintf("line %d: function %s has no body or semicolon", t.Line, name)
					}
					if isPunct(at(e), ";") {
						k = e // abstract or interface method
						continue
					}
					id := strings.Join(append(classPath(), name), "::")
					if ns != "" {
						id = ns + "\\" + id
					}
					fn := &function{id: id, start: t.Line, body: judgeable()}
					inAnon := len(stack) > 0 && stack[len(stack)-1].kind == fAnonClass
					if outer := inFunc(); outer != nil && outer.declares == "" && !inAnon {
						// a method of an anonymous class runs only through the object the body creates
						outer.declares = fmt.Sprintf("declares the function %s inside its body (line %d)", name, t.Line)
					}
					pendingFn = fn
					k = e - 1 // the brace is handled below
				case isPunct(n, "("):
					e, ok := skipParens(j)
					if !ok {
						return nil, fmt.Sprintf("line %d: unbalanced closure parameters", t.Line)
					}
					if isKeyword(at(e), "use") {
						if e, ok = skipParens(e + 1); !ok {
							return nil, fmt.Sprintf("line %d: unbalanced closure use list", t.Line)
						}
					}
					for e < len(sig) && !isPunct(at(e), "{") {
						if isPunct(at(e), "(") {
							if e, ok = skipParens(e); !ok {
								return nil, fmt.Sprintf("line %d: unbalanced return type", t.Line)
							}
							continue
						}
						if isPunct(at(e), ";") {
							return nil, fmt.Sprintf("line %d: closure without a body", t.Line)
						}
						e++
					}
					pendingClosure = true
					k = e - 1
				default:
					if p := at(k - 1); isKeyword(p, "use") || isPunct(p, "{") || isPunct(p, ",") {
						continue // use function A\b; use A\{function b}
					}
					return nil, fmt.Sprintf("line %d: unexpected token after function", t.Line)
				}
			case "class", "interface", "trait", "enum":
				kw := strings.ToLower(t.Text)
				n := at(k + 1)
				if isPunct(n, ":") && !isPunct(at(k+2), ":") {
					continue // a named argument label
				}
				if kw == "enum" && !(n.Kind == phpscan.TokIdent && (isPunct(at(k+2), "{") || isPunct(at(k+2), ":"))) {
					continue // enum used as a name
				}
				if kw == "class" && isKeyword(at(k-1), "new") {
					// An anonymous class: its methods are bodies only as part of an enclosing named function's body.
					pendingAnon = true
				} else if n.Kind == phpscan.TokIdent {
					pendingClass = n.Text
					if outer := inFunc(); outer != nil && outer.declares == "" {
						outer.declares = fmt.Sprintf("declares the %s %s inside its body (line %d)", kw, n.Text, t.Line)
					}
				} else {
					return nil, fmt.Sprintf("line %d: unexpected token after %s", t.Line, kw)
				}
				// up to the opening brace (anonymous class arguments in parentheses)
				e := k + 1
				for e < len(sig) && !isPunct(at(e), "{") {
					if isPunct(at(e), "(") {
						var ok bool
						if e, ok = skipParens(e); !ok {
							return nil, fmt.Sprintf("line %d: unbalanced parentheses", t.Line)
						}
						continue
					}
					if isPunct(at(e), ";") || isPunct(at(e), "}") {
						return nil, fmt.Sprintf("line %d: %s without a body", t.Line, kw)
					}
					e++
				}
				k = e - 1
			case "namespace":
				n := at(k + 1)
				if isPunct(n, "\\") {
					continue // namespace\name(): a relative name
				}
				if len(stack) != 0 {
					return nil, fmt.Sprintf("line %d: namespace declaration inside a block", t.Line)
				}
				var name strings.Builder
				e := k + 1
				for ; e < len(sig); e++ {
					x := at(e)
					if x.Kind == phpscan.TokIdent || isPunct(x, "\\") {
						name.WriteString(x.Text)
						continue
					}
					break
				}
				switch {
				case isPunct(at(e), ";"):
					ns = name.String()
					k = e
				case isPunct(at(e), "{"):
					ns = name.String()
					stack = append(stack, frame{kind: fNamespace, name: ns})
					k = e
				default:
					return nil, fmt.Sprintf("line %d: malformed namespace declaration", t.Line)
				}
			case "endif", "endwhile", "endfor", "endforeach", "endswitch", "enddeclare":
				if inFunc() == nil {
					return nil, fmt.Sprintf("line %d: alternative control syntax outside function bodies (declarations may be conditional)", t.Line)
				}
			}
		case isPunct(t, "{"):
			fr := frame{kind: fBlock}
			switch {
			case pendingFn != nil:
				fr = frame{kind: fFunc, fn: pendingFn}
				pendingFn.open = t.Line
				pendingFn.tokenStart = sig[k] + 1
				pendingFn = nil
			case pendingClosure:
				fr.kind = fClosure
				pendingClosure = false
			case pendingClass != "":
				fr = frame{kind: fClass, name: pendingClass}
				pendingClass = ""
			case pendingAnon:
				anon++
				fr = frame{kind: fAnonClass, name: fmt.Sprintf("class@anonymous#%d", anon)}
				pendingAnon = false
			}
			stack = append(stack, fr)
		case isPunct(t, "}"):
			if len(stack) == 0 {
				return nil, fmt.Sprintf("line %d: unbalanced closing brace", t.Line)
			}
			fr := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if fr.kind == fNamespace {
				ns = ""
			}
			if fr.kind == fFunc {
				fr.fn.end = t.Line
				fr.fn.tokenEnd = sig[k]
				f.funcs = append(f.funcs, fr.fn)
			}
		}
	}
	if len(stack) != 0 || pendingFn != nil || pendingClass != "" || pendingAnon || pendingClosure {
		return nil, "unbalanced braces at the end of the file"
	}
	// funcs were appended at their closing brace: put them in source order.
	sortFuncs(f.funcs)
	for _, fn := range f.funcs {
		if fn.body {
			f.bodies = append(f.bodies, fn)
		}
	}
	return f, ""
}

func sortFuncs(fs []*function) {
	for i := 1; i < len(fs); i++ {
		for j := i; j > 0 && fs[j].start < fs[j-1].start; j-- {
			fs[j], fs[j-1] = fs[j-1], fs[j]
		}
	}
}

// inBody reports whether line lies strictly inside the body of fn.
func (fn *function) inBody(line int) bool { return line > fn.open && line < fn.end }

// key returns the comparison key of a token (kind and text; plain comments are not keyed).
func key(t phpscan.Token) string { return string(t.Kind) + t.Text }

// skeleton returns the keys of every token that does not start on a body line, comments dropped, doc comments kept.
func (f *phpFile) skeleton() []string {
	var out []string
	b := 0
	for _, t := range f.toks {
		if t.Kind == phpscan.TokComment {
			continue
		}
		for b < len(f.bodies) && t.Line >= f.bodies[b].end {
			b++
		}
		if b < len(f.bodies) && f.bodies[b].inBody(t.Line) {
			continue
		}
		out = append(out, key(t))
	}
	return out
}

// bodyKeys returns the keys of the tokens that start on body lines of fn (comments dropped, doc comments kept).
func (f *phpFile) bodyKeys(fn *function) []string {
	var out []string
	for _, t := range f.toks {
		if t.Kind == phpscan.TokComment || !fn.inBody(t.Line) {
			continue
		}
		out = append(out, key(t))
	}
	return out
}

// Spans returns every named function with a body (any depth) of src, for the cross-check against PHP's tokenizer; ok is false
// when the file cannot be parsed.
func Spans(src []byte) ([]FuncSpan, bool) {
	f, why := parsePHP(src)
	if why != "" {
		return nil, false
	}
	out := make([]FuncSpan, 0, len(f.funcs))
	for _, fn := range f.funcs {
		out = append(out, FuncSpan{fn.start, fn.end})
	}
	return out, true
}

// BodyGuard compares the base and head versions of a changed PHP file (docs/dynamic-layer.md §11.2). It returns the changed
// functions (base spans; empty when only comments, blank lines or whitespace changed), whether the line count changed and the
// first changed base line (for the line-shift rule), or why the change cannot be judged at function level.
func BodyGuard(name string, base, head []byte) (spans []Span, shift bool, first int, why string) {
	b, w := parsePHP(base)
	if w != "" {
		return nil, false, 0, "base: " + w
	}
	h, w := parsePHP(head)
	if w != "" {
		return nil, false, 0, "head: " + w
	}
	bs, hs := b.skeleton(), h.skeleton()
	if len(bs) != len(hs) {
		return nil, false, 0, "the change touches code outside function bodies (declarations, signatures, attributes, doc comments, class-level or file-level code)"
	}
	for i := range bs {
		if bs[i] != hs[i] {
			return nil, false, 0, "the change touches code outside function bodies (declarations, signatures, attributes, doc comments, class-level or file-level code)"
		}
	}
	if len(b.bodies) != len(h.bodies) {
		return nil, false, 0, "the functions differ between base and head"
	}
	for i, bf := range b.bodies {
		hf := h.bodies[i]
		if bf.id != hf.id {
			return nil, false, 0, fmt.Sprintf("the functions differ between base and head (%s / %s)", bf.id, hf.id)
		}
		bk, hk := b.bodyKeys(bf), h.bodyKeys(hf)
		same := len(bk) == len(hk)
		for j := 0; same && j < len(bk); j++ {
			same = bk[j] == hk[j]
		}
		if same {
			continue
		}
		if bf.declares != "" || hf.declares != "" {
			d := bf.declares
			if d == "" {
				d = hf.declares
			}
			return nil, false, 0, fmt.Sprintf("the changed function %s %s", bf.id, d)
		}
		spans = append(spans, Span{From: bf.start, To: bf.end, What: bf.id})
	}
	bl, hl := strings.Split(string(base), "\n"), strings.Split(string(head), "\n")
	shift = len(bl) != len(hl)
	first = 0
	for i := 0; i < len(bl) && i < len(hl); i++ {
		if bl[i] != hl[i] {
			first = i + 1
			break
		}
	}
	if first == 0 && len(bl) != len(hl) {
		first = min(len(bl), len(hl)) + 1
	}
	return spans, shift, first, ""
}

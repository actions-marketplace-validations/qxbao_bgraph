package godyn

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"regexp"
	"strings"
)

// Span is a changed function: its base lines (the line of the func keyword to the line of the closing brace).
type Span struct {
	From, To int
	What     string // "Recv.Name" or "Name"
}

// generated matches the marker of a generated file (https://go.dev/s/generatedcode).
var generated = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// lineDirective reports whether a comment is a //line or /*line directive (anywhere, as the compiler accepts it at the start
// of a line or inside a line for the /* form).
func lineDirective(text string) bool {
	return strings.HasPrefix(text, "//line ") || strings.HasPrefix(text, "/*line ")
}

// directive reports whether a comment is kept in the skeleton: compiler and tool directives change what is built even when
// nothing else does.
func directive(text string) bool {
	for _, p := range []string{"//go:", "//line ", "/*line ", "//export ", "// +build", "//extern ", "//sys "} {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// tok is one token of a file: kind and literal (the literal of an automatically inserted semicolon is "\n").
type tok struct {
	t   token.Token
	lit string
}

// parsed is one side of a changed file.
type parsed struct {
	fset   *token.FileSet
	file   *ast.File
	funcs  []*ast.FuncDecl // with a body, in source order
	skel   []tok           // tokens outside function bodies (bodies replaced by a placeholder), directive comments kept
	skelAt []int           // line of each skeleton token
	bodies [][]tok         // tokens strictly inside each body (comments dropped)
	head   []string        // lines before the package clause
	prefix []byte          // the raw bytes before the package clause (build constraints, package documentation)
}

const bodyPlaceholder = token.Token(-1)

func parseSide(name string, src []byte) (*parsed, string) {
	p := &parsed{fset: token.NewFileSet()}
	f, err := parser.ParseFile(p.fset, name, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, "does not parse: " + firstN(err.Error(), 200)
	}
	p.file = f
	tf := p.fset.File(f.Pos())
	if tf == nil {
		return nil, "does not parse"
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			p.funcs = append(p.funcs, fd)
		}
	}
	p.prefix = src[:tf.Offset(f.Package)]
	pkgLine := p.fset.Position(f.Package).Line
	for i, l := range bytes.Split(src, []byte("\n")) {
		if i+1 >= pkgLine {
			break
		}
		p.head = append(p.head, strings.TrimRight(string(l), "\r"))
	}
	// Body ranges as file offsets (strictly between the braces).
	type rng struct{ lo, hi int }
	rs := make([]rng, len(p.funcs))
	for i, fd := range p.funcs {
		rs[i] = rng{tf.Offset(fd.Body.Lbrace), tf.Offset(fd.Body.Rbrace)}
	}
	p.bodies = make([][]tok, len(p.funcs))
	var sc scanner.Scanner
	sfs := token.NewFileSet()
	sf := sfs.AddFile(name, -1, len(src))
	var scanErr error
	sc.Init(sf, src, func(pos token.Position, msg string) {
		if scanErr == nil {
			scanErr = fmt.Errorf("%s: %s", pos, msg)
		}
	}, scanner.ScanComments)
	cur := 0 // index of the next body range
	for {
		pos, t, lit := sc.Scan()
		if t == token.EOF {
			break
		}
		off := sf.Offset(pos)
		for cur < len(rs) && off >= rs[cur].hi {
			cur++
		}
		inBody := -1
		if cur < len(rs) && off > rs[cur].lo && off < rs[cur].hi {
			inBody = cur
		}
		if inBody >= 0 {
			if t != token.COMMENT {
				p.bodies[inBody] = append(p.bodies[inBody], tok{t, lit})
			}
			continue
		}
		if t == token.COMMENT && !directive(lit) {
			continue
		}
		p.skel = append(p.skel, tok{t, lit})
		p.skelAt = append(p.skelAt, sf.Line(pos))
		if cur < len(rs) && off == rs[cur].lo {
			// The opening brace of a body: the body itself is one placeholder in the skeleton.
			p.skel = append(p.skel, tok{bodyPlaceholder, ""})
			p.skelAt = append(p.skelAt, sf.Line(pos))
		}
	}
	if scanErr != nil {
		return nil, "does not scan: " + firstN(scanErr.Error(), 200)
	}
	return p, ""
}

// cgo reports whether the file imports "C".
func (p *parsed) cgo() bool {
	for _, im := range p.file.Imports {
		if im.Path != nil && im.Path.Value == `"C"` {
			return true
		}
	}
	return false
}

// generated reports whether the file carries the generated-code marker before its package clause.
func (p *parsed) generated() bool {
	for _, l := range p.head {
		if generated.MatchString(l) {
			return true
		}
	}
	return false
}

// lineDirectiveAt returns the line of a //line or /*line directive in the file, 0 when there is none.
func (p *parsed) lineDirectiveAt() int {
	for _, cg := range p.file.Comments {
		for _, c := range cg.List {
			if lineDirective(c.Text) {
				return p.fset.Position(c.Slash).Line
			}
		}
	}
	return 0
}

// funcName returns "Recv.Name" or "Name" of a function declaration.
func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
			continue
		case *ast.IndexExpr:
			t = x.X
			continue
		case *ast.IndexListExpr:
			t = x.X
			continue
		case *ast.ParenExpr:
			t = x.X
			continue
		}
		break
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func sameToks(a, b []tok) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// BodyGuard checks a changed non-test Go source file (both versions) and returns the changed functions (base spans), whether
// the change alters the line count of the file below some point (shift) and the first base line from which lines move (first).
// why is non-empty when the change cannot be judged per function: a parse error at either side, a cgo file, a generated file,
// a changed build constraint, a //line directive anywhere in the file, or any change outside function bodies besides plain
// comments (signatures, receivers, type parameters, constants, variables, types, imports, directives such as //go:embed,
// //go:linkname or //go:noinline).
func BodyGuard(name string, base, head []byte) (spans []Span, shift bool, first int, why string) {
	b, w := parseSide(name, base)
	if w != "" {
		return nil, false, 0, "base " + w
	}
	h, w := parseSide(name, head)
	if w != "" {
		return nil, false, 0, "head " + w
	}
	switch {
	case b.cgo() || h.cgo():
		return nil, false, 0, "a cgo file (imports \"C\")"
	case b.generated() || h.generated():
		return nil, false, 0, "a generated file (Code generated ... DO NOT EDIT)"
	case !bytes.Equal(b.prefix, h.prefix):
		// Build constraints (//go:build, // +build and the blank line that ends them) live there: compare it byte for byte.
		return nil, false, 0, "the text before the package clause changed (build constraints or package documentation)"
	}
	if l := b.lineDirectiveAt(); l > 0 {
		return nil, false, 0, fmt.Sprintf("a //line directive at base line %d (coverage lines would not be the file's own)", l)
	}
	if l := h.lineDirectiveAt(); l > 0 {
		return nil, false, 0, fmt.Sprintf("a //line directive at head line %d (coverage lines would not be the file's own)", l)
	}
	if !sameToks(b.skel, h.skel) || len(b.funcs) != len(h.funcs) {
		line := 0
		for i := 0; i < len(b.skel) && i < len(h.skel); i++ {
			if b.skel[i] != h.skel[i] {
				line = b.skelAt[i]
				break
			}
		}
		if line == 0 && len(b.skelAt) > 0 {
			line = b.skelAt[min(len(b.skelAt), len(h.skelAt))-1]
		}
		return nil, false, 0, fmt.Sprintf("a change outside function bodies near base line %d (declaration, signature, import or directive)", line)
	}
	for i, fb := range b.funcs {
		fh := h.funcs[i]
		if funcName(fb) != funcName(fh) {
			return nil, false, 0, fmt.Sprintf("function %s at base line %d does not pair with %s", funcName(fb), b.fset.Position(fb.Pos()).Line, funcName(fh))
		}
		if sameToks(b.bodies[i], h.bodies[i]) {
			continue
		}
		spans = append(spans, Span{From: b.fset.Position(fb.Pos()).Line, To: b.fset.Position(fb.Body.Rbrace).Line, What: funcName(fb)})
	}
	bl := strings.Split(string(base), "\n")
	hl := strings.Split(string(head), "\n")
	hunks, exact := diffHunks(bl, hl)
	for _, hk := range hunks {
		if hk.b1-hk.b0 != hk.h1-hk.h0 || !exact {
			// An approximate diff (one hunk over the whole changed region) cannot tell where lines move: treat it as a shift.
			return spans, true, hk.b0 + 1, ""
		}
	}
	return spans, false, 0, ""
}

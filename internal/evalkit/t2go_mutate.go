package evalkit

import (
	"bytes"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"path"
	"strings"
)

// Go mutation generator for T2: parses each non-test, non-generated source file with go/parser and flips one binary operator or boolean
// literal per candidate (==/!=, </>=, >/<=, <=/>, >=/<, &&/||, +/-, true/false). `+` with a string literal operand is skipped (a `-` there
// never compiles); other type errors surface as nocompile mutants.

// goMutationOps maps an operator to its replacement and the operator name used in the report.
var goMutationOps = map[token.Token][2]string{
	token.EQL:  {"!=", "eq-ne"},
	token.NEQ:  {"==", "ne-eq"},
	token.LSS:  {">=", "lt-ge"},
	token.GTR:  {"<=", "gt-le"},
	token.LEQ:  {">", "le-gt"},
	token.GEQ:  {"<", "ge-lt"},
	token.LAND: {"||", "and-or"},
	token.LOR:  {"&&", "or-and"},
	token.ADD:  {"-", "add-sub"},
	token.SUB:  {"+", "sub-add"},
}

func isStringLit(e ast.Expr) bool {
	b, ok := e.(*ast.BasicLit)
	return ok && b.Kind == token.STRING
}

// goScan returns the mutation candidates of one Go source file. ok=false when the file does not parse or is generated.
func goScan(src []byte) (cands []rustCand, ok bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, parser.ParseComments)
	if err != nil || ast.IsGenerated(f) {
		return nil, false
	}
	off := func(p token.Pos) int { return fset.Position(p).Offset }
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BinaryExpr:
			rep, ok := goMutationOps[x.Op]
			if !ok {
				return true
			}
			if x.Op == token.ADD && (isStringLit(x.X) || isStringLit(x.Y)) {
				return true
			}
			s := off(x.OpPos)
			cands = append(cands, rustCand{Start: s, End: s + len(x.Op.String()), Repl: rep[0], Op: rep[1]})
		case *ast.Ident:
			switch x.Name {
			case "true":
				s := off(x.Pos())
				cands = append(cands, rustCand{Start: s, End: s + 4, Repl: "false", Op: "true-false"})
			case "false":
				s := off(x.Pos())
				cands = append(cands, rustCand{Start: s, End: s + 5, Repl: "true", Op: "false-true"})
			}
		}
		return true
	})
	return cands, true
}

// goMutableFile reports whether a repo file may receive a mutant: a .go file that is not a test file, not under vendor/ or testdata/ or a
// directory the go tool ignores (`.x`, `_x`), and whose name suffix / build constraint matches the host build context (bctx).
func goMutableFile(f string, data []byte, bctx *build.Context) bool {
	if !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") {
		return false
	}
	for _, seg := range strings.Split(path.Dir(f), "/") {
		if seg == "vendor" || seg == "testdata" || strings.HasPrefix(seg, ".") && seg != "." || strings.HasPrefix(seg, "_") {
			return false
		}
	}
	if strings.HasPrefix(path.Base(f), ".") || strings.HasPrefix(path.Base(f), "_") {
		return false
	}
	if bctx == nil {
		return true
	}
	c := *bctx
	c.OpenFile = func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	c.JoinPath = path.Join
	ok, err := c.MatchFile(path.Dir(f), path.Base(f))
	return err == nil && ok
}

// GenGoMutants deterministically picks up to n mutants spread over the files (see spreadMutants).
func GenGoMutants(files []RustMutantSource, seed, idPrefix string, n int) []T2Mutant {
	cands := map[string][]rustCand{}
	for _, f := range files {
		if cs, ok := goScan(f.Data); ok {
			cands[f.Path] = cs
		}
	}
	return spreadMutants(files, cands, nil, seed, idPrefix, n)
}

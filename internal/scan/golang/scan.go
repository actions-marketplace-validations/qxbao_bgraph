// Package golang scans Go source for what the Go ecosystem needs (docs/plan.md §6.6): the import paths (from go/parser with
// ImportsOnly, so build constraints are ignored and every file contributes: the union of every configuration), the //go:embed
// patterns, and, in _test.go files, the string literals that may name a fixture path.
//
// FAIL-CLOSED principle: a file whose header (package clause and imports) does not parse is reported Uncertain; the caller then
// cannot trust its import list. A //go:embed directive that cannot be parsed is reported with Unknown set; the caller treats the
// whole package directory as embedded.
package golang

import (
	"go/parser"
	"go/scanner"
	"go/token"
	"path"
	"strconv"
	"strings"

	"github.com/qxbao/bgraph/internal/model"
)

// MaxFileSize: files larger than this threshold (bytes) are treated as uncertain (not scanned).
const MaxFileSize = 8 << 20

// maxString bounds the length of a string literal kept as a candidate path.
const maxString = 1024

// Scan scans one .go file. p decides whether the file is a test file (_test.go) and is used for diagnostics.
func Scan(p string, src []byte) model.ScanResult {
	if len(src) > MaxFileSize {
		return uncertain("file_too_large: " + p)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, p, src, parser.ImportsOnly|parser.ParseComments)
	if err != nil {
		return uncertain("parse: " + firstLine(err.Error()))
	}
	r := model.ScanResult{Go: &model.GoScan{}}
	if f.Name != nil {
		r.Go.Package = f.Name.Name
	}
	for _, im := range f.Imports {
		v, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			return uncertain("parse: import path " + im.Path.Value)
		}
		r.Imports = append(r.Imports, model.ImportRef{Spec: v, Kind: model.ImportStatic, Line: fset.Position(im.Path.Pos()).Line})
	}
	isTest := strings.HasSuffix(path.Base(p), "_test.go")
	if !isTest && !strings.Contains(string(src), "go:embed") {
		return r
	}
	r.Go.Embeds, r.Go.Strings = tokens(src, isTest)
	return r
}

func uncertain(reason string) model.ScanResult {
	return model.ScanResult{Uncertain: true, UncertainReason: reason}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

type tok struct {
	t   token.Token
	lit string
}

// tokens runs go/scanner over the whole file: //go:embed directives always, string literals only when strs is set. Lexical errors
// in the body are ignored (the header parsed; a body that does not lex cannot compile, so its embeds and literals do not matter).
func tokens(src []byte, strs bool) ([]model.GoEmbed, []string) {
	fset := token.NewFileSet()
	file := fset.AddFile("x.go", -1, len(src))
	var s scanner.Scanner
	s.Init(file, src, func(token.Position, string) {}, scanner.ScanComments)
	var embeds []model.GoEmbed
	var toks []tok
	for {
		pos, t, lit := s.Scan()
		if t == token.EOF {
			break
		}
		if t == token.COMMENT {
			if rest, ok := strings.CutPrefix(lit, "//go:embed"); ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
				embeds = append(embeds, parseEmbed(strings.TrimRight(rest, "\r"), fset.Position(pos).Line)...)
			}
			continue
		}
		if strs && t != token.SEMICOLON {
			toks = append(toks, tok{t, lit})
		}
	}
	if !strs {
		return embeds, nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v == "" || len(v) > maxString || seen[v] || strings.ContainsAny(v, "\n\r\x00") || !strings.ContainsAny(v, "/\\.") {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for i, tk := range toks {
		if tk.t == token.STRING {
			if v, err := strconv.Unquote(tk.lit); err == nil {
				add(v)
			}
			continue
		}
		// X.Join("a", "b", ...): the literal leading arguments joined.
		if tk.t == token.IDENT && tk.lit == "Join" && i >= 2 && toks[i-1].t == token.PERIOD && toks[i-2].t == token.IDENT &&
			i+2 < len(toks) && toks[i+1].t == token.LPAREN {
			var segs []string
			for j := i + 2; j < len(toks) && toks[j].t == token.STRING; j += 2 {
				v, err := strconv.Unquote(toks[j].lit)
				if err != nil {
					break
				}
				segs = append(segs, strings.ReplaceAll(v, "\\", "/"))
				if j+1 >= len(toks) || toks[j+1].t != token.COMMA {
					break
				}
			}
			if len(segs) > 1 {
				add(strings.Join(segs, "/"))
			}
		}
	}
	return embeds, out
}

// parseEmbed splits the arguments of a //go:embed directive the way cmd/go does: space-separated patterns, each either bare or a
// Go string literal ("..." or `...`).
func parseEmbed(args string, line int) []model.GoEmbed {
	var out []model.GoEmbed
	bad := func(why string) []model.GoEmbed {
		return []model.GoEmbed{{Line: line, Unknown: why}}
	}
	s := strings.TrimLeft(args, " \t")
	for s != "" {
		var pat string
		switch s[0] {
		case '`':
			i := strings.IndexByte(s[1:], '`')
			if i < 0 {
				return bad("unterminated quoted pattern")
			}
			pat, s = s[1:1+i], s[2+i:]
		case '"':
			i := 1
			for ; i < len(s); i++ {
				if s[i] == '\\' {
					i++
					continue
				}
				if s[i] == '"' {
					break
				}
			}
			if i >= len(s) {
				return bad("unterminated quoted pattern")
			}
			v, err := strconv.Unquote(s[:i+1])
			if err != nil {
				return bad("invalid quoted pattern")
			}
			pat, s = v, s[i+1:]
		default:
			i := strings.IndexAny(s, " \t")
			if i < 0 {
				i = len(s)
			}
			pat, s = s[:i], s[i:]
		}
		if s != "" && s[0] != ' ' && s[0] != '\t' {
			return bad("invalid quoted pattern")
		}
		s = strings.TrimLeft(s, " \t")
		if pat == "" {
			return bad("empty pattern")
		}
		out = append(out, model.GoEmbed{Pattern: pat, Line: line})
	}
	if len(out) == 0 {
		return bad("no pattern")
	}
	return out
}

package evalkit

import (
	"path"
	"slices"
	"strings"

	dartlex "github.com/qxbao/bgraph/internal/scan/dart"
)

// Dart mutation generator for the T2 oracle: one-token operator and literal flips in non-test package code (lib/, bin/).
// The bgraph Dart lexer splits the source (comments skipped, string literals and their interpolations separated), adjacent
// punctuation bytes are joined into one operator, and a candidate is kept only when re-lexing the mutated source changes
// exactly that operator or literal. `dart format` puts spaces around binary operators, which tells `a < b` from a type
// argument list `List<int>`. A candidate that still does not compile (`+` on strings flipped to `-`) is caught by the
// oracle run (a compile error while loading a suite) and recorded as "nocompile".

// dartItem is one significant token: a word, number or string, or a run of adjacent punctuation bytes (one operator).
type dartItem struct {
	kind       dartlex.Kind
	text       string
	start, end int // byte range; end is -1 for a string literal (its length is not kept by the lexer)
	line       int
}

// dartItems joins the punctuation bytes of one lexer segment into operator runs.
func dartItems(seg []dartlex.Token) []dartItem {
	var out []dartItem
	for _, t := range seg {
		end := t.Start + len(t.Text)
		if t.Kind == dartlex.String {
			end = -1
		}
		if t.Kind == dartlex.Punct && len(out) > 0 {
			p := &out[len(out)-1]
			if p.kind == dartlex.Punct && p.end == t.Start && t.Text != "(" && t.Text != ")" && t.Text != "[" && t.Text != "]" &&
				t.Text != "{" && t.Text != "}" && t.Text != ";" && t.Text != "," && !strings.ContainsAny(p.text, "()[]{};,") {
				p.text += t.Text
				p.end = end
				continue
			}
		}
		out = append(out, dartItem{kind: t.Kind, text: t.Text, start: t.Start, end: end, line: t.Line})
	}
	return out
}

// dartNonOperand: words after which an operator is unary or part of a pattern, not a binary operator.
var dartNonOperand = map[string]bool{"return": true, "case": true, "throw": true, "in": true, "is": true, "as": true, "new": true, "else": true,
	"await": true, "yield": true, "when": true, "assert": true, "if": true, "while": true, "for": true, "switch": true, "const": true, "final": true,
	"var": true, "late": true}

// dartDirective: a line whose first token is one of these is a directive or declaration head and is never mutated.
var dartDirective = map[string]bool{"import": true, "export": true, "part": true, "library": true, "@": true, "typedef": true}

var dartFlips = map[string][2]string{
	"==": {"!=", "eq-ne"}, "!=": {"==", "ne-eq"}, "<=": {">", "le-gt"}, ">=": {"<", "ge-lt"}, "<": {">=", "lt-ge"}, ">": {"<=", "gt-le"},
	"&&": {"||", "and-or"}, "||": {"&&", "or-and"}, "+": {"-", "add-sub"}, "-": {"+", "sub-add"},
}

// dartScan returns the mutation candidates of one Dart source file (none when the file does not lex cleanly).
func dartScan(src []byte) []rustCand {
	lexed := dartlex.Lex(src)
	if lexed.Problem != "" {
		return nil
	}
	spaced := func(it dartItem) bool {
		return it.start > 0 && it.end < len(src) && src[it.start-1] == ' ' && src[it.end] == ' '
	}
	var out []rustCand
	for _, seg := range lexed.Segments {
		items := dartItems(seg)
		skipLine, line := false, -1
		operandBefore := func(k int) bool {
			if k == 0 {
				return false
			}
			p := items[k-1]
			switch p.kind {
			case dartlex.Number, dartlex.String:
				return true
			case dartlex.Word:
				return !dartNonOperand[p.text]
			}
			return p.text == ")" || p.text == "]" || p.text == "!"
		}
		operandAfter := func(k int) bool {
			if k+1 >= len(items) {
				return false
			}
			n := items[k+1]
			if n.kind != dartlex.Punct {
				return true
			}
			return n.text == "(" || n.text == "!" || n.text == "[" || n.text == "-"
		}
		for k, it := range items {
			if it.line != line {
				line = it.line
				skipLine = it.kind == dartlex.Word && dartDirective[it.text] || it.kind == dartlex.Punct && strings.HasPrefix(it.text, "@")
			}
			if skipLine {
				continue
			}
			add := func(repl, op string) {
				out = append(out, rustCand{Start: it.start, End: it.end, Repl: repl, Op: op})
			}
			switch it.kind {
			case dartlex.Word:
				if k > 0 && items[k-1].kind == dartlex.Punct && strings.HasSuffix(items[k-1].text, ".") {
					continue
				}
				switch it.text {
				case "true":
					add("false", "true-false")
				case "false":
					add("true", "false-true")
				}
			case dartlex.Number:
				switch it.text {
				case "0":
					add("1", "zero-one")
				case "1":
					add("0", "one-zero")
				}
			case dartlex.Punct:
				f, ok := dartFlips[it.text]
				if !ok {
					continue
				}
				switch it.text {
				case "==", "!=", "<=", ">=", "&&", "||":
					if !spaced(it) || !operandBefore(k) {
						continue
					}
				case "<", ">":
					if !spaced(it) || !operandBefore(k) || !operandAfter(k) {
						continue
					}
				case "+", "-":
					// Binary arithmetic only; `'a' + b` concatenates strings and would not compile as `-`.
					if !spaced(it) || !operandBefore(k) || !operandAfter(k) || items[k-1].kind == dartlex.String ||
						k+1 < len(items) && items[k+1].kind == dartlex.String {
						continue
					}
				}
				add(f[0], f[1])
			}
		}
	}
	var keep []rustCand
	for _, c := range out {
		if dartSingleFlip(src, c) {
			keep = append(keep, c)
		}
	}
	return keep
}

// dartSingleFlip re-lexes the mutated source and checks that exactly one token changed, from the original text to Repl.
func dartSingleFlip(src []byte, c rustCand) bool {
	mutated, err := ApplyEdit(src, T2Mutant{Start: c.Start, End: c.End, Repl: c.Repl})
	if err != nil {
		return false
	}
	a, b := dartlex.Lex(src), dartlex.Lex(mutated)
	if b.Problem != "" || len(a.Segments) != len(b.Segments) {
		return false
	}
	diff := 0
	for i := range a.Segments {
		x, y := dartItems(a.Segments[i]), dartItems(b.Segments[i])
		if len(x) != len(y) {
			return false
		}
		for j := range x {
			if x[j].kind != y[j].kind || x[j].text != y[j].text {
				if x[j].start != c.Start || x[j].text != string(src[c.Start:c.End]) || y[j].text != c.Repl {
					return false
				}
				diff++
			}
		}
	}
	return diff == 1
}

// dartCandidateFile reports whether f is mutable package code: a .dart file under <pkg>/lib/ or <pkg>/bin/ of one of the
// packages (repo-relative directories, "." for the root), outside generated files.
func dartCandidateFile(f string, packages []string) bool {
	if !strings.HasSuffix(f, ".dart") {
		return false
	}
	base := path.Base(f)
	for _, g := range []string{".g.dart", ".freezed.dart", ".mocks.dart", ".pb.dart", ".pbenum.dart", ".pbjson.dart", ".pbserver.dart"} {
		if strings.HasSuffix(base, g) {
			return false
		}
	}
	return slices.ContainsFunc(packages, func(p string) bool {
		pre := ""
		if p != "." && p != "" {
			pre = p + "/"
		}
		return strings.HasPrefix(f, pre+"lib/") || strings.HasPrefix(f, pre+"bin/")
	})
}

// dartPackageOf returns the package (from packages) that contains f: the longest matching directory.
func dartPackageOf(f string, packages []string) string {
	best := ""
	found := false
	for _, p := range packages {
		if p == "." || p == "" {
			if !found {
				best, found = ".", true
			}
			continue
		}
		if strings.HasPrefix(f, p+"/") && (!found || len(p) > len(best) || best == ".") {
			best, found = p, true
		}
	}
	return best
}

// GenDartMutants picks up to n mutants, split evenly over the packages that have candidates (a package with fewer
// candidates than its share leaves the rest to the others), spread over each package's files by spreadMutants.
func GenDartMutants(files []RustMutantSource, packages []string, seed, idPrefix string, n int) []T2Mutant {
	byPkg := map[string][]RustMutantSource{}
	cands := map[string][]rustCand{}
	count := map[string]int{}
	for _, f := range files {
		cs := dartScan(f.Data)
		if len(cs) == 0 {
			continue
		}
		p := dartPackageOf(f.Path, packages)
		byPkg[p] = append(byPkg[p], f)
		cands[f.Path] = cs
		lines := map[int]bool{} // spreadMutants keeps one candidate per line
		for _, c := range cs {
			l, _ := lineOf(f.Data, c.Start)
			lines[l] = true
		}
		count[p] += len(lines)
	}
	var pkgs []string
	for _, p := range packages {
		if p == "" {
			p = "."
		}
		if len(byPkg[p]) > 0 && !slices.Contains(pkgs, p) {
			pkgs = append(pkgs, p)
		}
	}
	// Fill the smallest packages first so that their unused share moves to the larger ones.
	slices.SortStableFunc(pkgs, func(a, b string) int { return count[a] - count[b] })
	share := map[string]int{}
	left := n
	for i, p := range pkgs {
		q := left / (len(pkgs) - i)
		if left%(len(pkgs)-i) != 0 {
			q++
		}
		q = min(q, count[p])
		share[p] = q
		left -= q
	}
	var out []T2Mutant
	for _, p := range packages {
		if p == "" {
			p = "."
		}
		if share[p] == 0 {
			continue
		}
		out = append(out, spreadMutants(byPkg[p], cands, nil, seed, "x", share[p])...)
		share[p] = 0
	}
	for i := range out {
		out[i].ID = idPrefix + "-" + pad2(i+1)
	}
	return out
}

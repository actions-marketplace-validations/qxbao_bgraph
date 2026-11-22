package evalkit

import (
	"path"
	"strings"
)

// C-family mutation generator for the JVM and .NET T2 (Java, Kotlin and C#): a small tokenizer (comments, string and character literals
// are single tokens) finds one-token operator flips in non-test code. Like the Rust and Python generators it does not parse, so it is
// conservative; a candidate that does not compile is detected by the oracle run (the build fails) and recorded as "nocompile".

type cfTokKind int

const (
	cfName cfTokKind = iota
	cfNum
	cfStr
	cfOp
)

type cfTok struct {
	kind       cfTokKind
	start, end int
}

func (t cfTok) text(src []byte) string { return string(src[t.start:t.end]) }

var cfOps3 = map[string]bool{">>>": true, "<<=": true, ">>=": true, "...": true, "??=": true}
var cfOps2 = map[string]bool{"==": true, "!=": true, "<=": true, ">=": true, "&&": true, "||": true, "++": true, "--": true, "+=": true,
	"-=": true, "*=": true, "/=": true, "%=": true, "&=": true, "|=": true, "^=": true, "->": true, "=>": true, "<<": true, ">>": true,
	"::": true, "??": true, "?.": true, "..": true}

// lexCFamily returns the significant tokens of a Java/Kotlin/C# source. cs enables C# literal forms (@"..", $"..", raw strings).
func lexCFamily(src []byte, cs bool) (toks []cfTok) {
	n := len(src)
	for i := 0; i < n; {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f':
			i++
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			i += 2
			for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i += 2
		case c == '"' || (cs && (c == '@' || c == '$') && cfStringStart(src, i)):
			s := i
			i = cfSkipString(src, i, cs)
			toks = append(toks, cfTok{cfStr, s, i})
		case c == '\'':
			s := i
			i++
			for i < n && src[i] != '\'' && src[i] != '\n' {
				if src[i] == '\\' {
					i++
				}
				i++
			}
			i++
			if i > n {
				i = n
			}
			toks = append(toks, cfTok{cfStr, s, i})
		case isIdentStart(c) || c == '@' && !cs:
			s := i
			i++
			for i < n && isIdentChar(src[i]) {
				i++
			}
			toks = append(toks, cfTok{cfName, s, i})
		case c >= '0' && c <= '9':
			s := i
			for i < n && (isIdentChar(src[i]) || src[i] == '.' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9') {
				i++
			}
			toks = append(toks, cfTok{cfNum, s, i})
		default:
			s := i
			switch {
			case i+2 < n && cfOps3[string(src[i:i+3])]:
				i += 3
			case i+1 < n && cfOps2[string(src[i:i+2])]:
				i += 2
			default:
				i++
			}
			toks = append(toks, cfTok{cfOp, s, i})
		}
	}
	return toks
}

// cfStringStart: at src[i] ('@' or '$') a C# string literal begins ("@\"", "$\"", "$@\"", "@$\"").
func cfStringStart(src []byte, i int) bool {
	j := i
	for j < len(src) && (src[j] == '@' || src[j] == '$') && j-i < 2 {
		j++
	}
	return j < len(src) && src[j] == '"'
}

// cfSkipString returns the end of the string literal starting at src[i].
func cfSkipString(src []byte, i int, cs bool) int {
	n := len(src)
	verbatim := false
	for i < n && (src[i] == '@' || src[i] == '$') {
		verbatim = verbatim || src[i] == '@'
		i++
	}
	if i+2 < n && src[i+1] == '"' && src[i+2] == '"' { // triple-quoted: Java text block, Kotlin raw string, C# raw string
		j := i + 3
		for j+2 < n && !(src[j] == '"' && src[j+1] == '"' && src[j+2] == '"') {
			if src[j] == '\\' && !cs {
				j++
			}
			j++
		}
		return min(j+3, n)
	}
	i++
	for i < n {
		switch {
		case verbatim && src[i] == '"' && i+1 < n && src[i+1] == '"':
			i += 2
		case !verbatim && src[i] == '\\':
			i += 2
		case src[i] == '"':
			return i + 1
		case src[i] == '\n' && !verbatim:
			return i
		default:
			i++
		}
	}
	return n
}

// cfScan returns the mutation candidates of one source file.
func cfScan(src []byte, cs bool) []rustCand {
	toks := lexCFamily(src, cs)
	var out []rustCand
	txt := func(k int) string {
		if k < 0 || k >= len(toks) {
			return ""
		}
		return toks[k].text(src)
	}
	add := func(t cfTok, repl, op string) {
		out = append(out, rustCand{Start: t.start, End: t.end, Repl: repl, Op: op})
	}
	spaced := func(t cfTok) bool {
		return t.start > 0 && t.end < len(src) && src[t.start-1] == ' ' && src[t.end] == ' '
	}
	operandBefore := func(k int) bool {
		if k == 0 {
			return false
		}
		p := toks[k-1]
		switch p.kind {
		case cfNum, cfStr:
			return true
		case cfName:
			switch p.text(src) {
			case "return", "case", "throw", "in", "is", "as", "new", "else":
				return false
			}
			return true
		}
		t := p.text(src)
		return t == ")" || t == "]"
	}
	line, lastOff, curLine := -1, 0, 1
	skipLine := false
	for k, t := range toks {
		// A line that starts with a declaration or directive head is not mutated (imports, packages, attributes, annotations, signatures).
		curLine += strings.Count(string(src[lastOff:t.start]), "\n")
		lastOff = t.start
		if l := curLine; l != line {
			line = l
			switch txt(k) {
			case "import", "package", "using", "namespace", "@", "[", "#", "class", "interface", "enum", "record", "struct", "fun", "typealias", "annotation":
				skipLine = true
			default:
				skipLine = false
			}
			if t.kind == cfName && strings.HasPrefix(txt(k), "@") {
				skipLine = true
			}
		}
		if skipLine {
			continue
		}
		s := t.text(src)
		switch {
		case t.kind == cfName && (s == "true" || s == "false") && txt(k-1) != ".":
			if s == "true" {
				add(t, "false", "true-false")
			} else {
				add(t, "true", "false-true")
			}
		case t.kind != cfOp:
		case s == "==":
			add(t, "!=", "eq-ne")
		case s == "!=":
			add(t, "==", "ne-eq")
		case s == "<=":
			add(t, ">", "le-gt")
		case s == ">=":
			add(t, "<", "ge-lt")
		case s == "<" && spaced(t):
			add(t, ">=", "lt-ge")
		case s == ">" && spaced(t):
			add(t, "<=", "gt-le")
		case s == "&&":
			add(t, "||", "and-or")
		case s == "||":
			add(t, "&&", "or-and")
		case (s == "+" || s == "-") && spaced(t) && operandBefore(k) && k+1 < len(toks):
			if s == "+" {
				add(t, "-", "add-sub")
			} else {
				add(t, "+", "sub-add")
			}
		}
	}
	return out
}

// jvmCandidateFile reports whether a repo path is mutable non-test Java/Kotlin source.
func jvmCandidateFile(f string) bool {
	if !(strings.HasSuffix(f, ".java") || strings.HasSuffix(f, ".kt")) {
		return false
	}
	base := path.Base(f)
	if base == "module-info.java" || base == "package-info.java" || strings.HasSuffix(base, "Test.java") || strings.HasSuffix(base, "Test.kt") {
		return false
	}
	return cfSourceDir(f, "src/main/")
}

// dotnetCandidateFile reports whether a repo path is mutable non-test C# source (under src/, outside test, generated and legacy support code).
func dotnetCandidateFile(f string) bool {
	if !strings.HasSuffix(f, ".cs") || strings.HasSuffix(f, ".Designer.cs") || strings.HasSuffix(f, ".g.cs") || strings.HasSuffix(f, ".Tests.cs") {
		return false
	}
	return cfSourceDir(f, "src/")
}

// cfSourceDir: f lies under a directory starting with prefix (anywhere in the path) and under no test, build-output or sample directory.
func cfSourceDir(f, prefix string) bool {
	if !strings.HasPrefix(f, prefix) && !strings.Contains(f, "/"+prefix) {
		return false
	}
	for _, seg := range strings.Split(path.Dir(f), "/") {
		l := strings.ToLower(seg)
		switch l {
		case "test", "tests", "obj", "bin", "target", "build", "generated", "samples", "sample", "examples", "docs", "bench", "benchmarks", "snippets",
			"legacysupport", "japicmp", "node_modules":
			return false
		}
		if strings.HasSuffix(l, ".tests") || strings.HasSuffix(l, ".test") || strings.HasSuffix(l, "tests") && l != "tests" {
			return false
		}
	}
	return true
}

// GenCFamilyMutants deterministically picks up to n mutants spread over the files (see spreadMutants).
func GenCFamilyMutants(files []RustMutantSource, cs bool, seed, idPrefix string, n int) []T2Mutant {
	cands := map[string][]rustCand{}
	for _, f := range files {
		cands[f.Path] = cfScan(f.Data, cs)
	}
	return spreadMutants(files, cands, nil, seed, idPrefix, n)
}

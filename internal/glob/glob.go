// Package glob is a path glob matcher that does not use gitignore syntax.
//
// Syntax: `*`, `?`, `**` (a whole segment), `[abc]`, `[a-z]`, `[!a]`/`[^a]`, `{a,b}` (nestable, may contain `/`),
// `\` escapes the next character. Case-sensitive. Follows the minimatch matchBase=false convention: a pattern without `/` matches only at the root.
//
// Implementation: `{}` is expanded at compile time into multiple variants; each variant is a list of segments. Segment matching and
// `**` matching both use a two-pointer algorithm (backtracking to the nearest star), so time is polynomial with no blowup.
package glob

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxAlternatives caps the number of variants after `{}` expansion to avoid combinatorial explosion.
const maxAlternatives = 1024

type tokKind uint8

const (
	tLit   tokKind = iota
	tAny           // ?
	tStar          // *
	tClass         // [...]
)

type rng struct{ lo, hi rune }

type token struct {
	kind   tokKind
	r      rune
	neg    bool
	ranges []rng
}

type segment struct {
	doubleStar bool
	toks       []token
	literal    string // valid when isLit
	isLit      bool
}

type variant struct{ segs []segment }

// Pattern is a compiled pattern; safe for concurrent use.
type Pattern struct {
	raw      string
	variants []variant
	prefix   string
}

// Compile compiles a pattern.
func Compile(pat string) (*Pattern, error) {
	orig := pat
	for strings.HasPrefix(pat, "./") {
		pat = pat[2:]
	}
	if pat == "" {
		return nil, errors.New("glob: empty pattern")
	}
	alts, err := expandBraces(pat)
	if err != nil {
		return nil, fmt.Errorf("glob %q: %w", orig, err)
	}
	p := &Pattern{raw: orig}
	for _, a := range alts {
		v, err := compileVariant(a)
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", orig, err)
		}
		p.variants = append(p.variants, v)
	}
	p.prefix = staticPrefix(p.variants)
	return p, nil
}

// MustCompile is like Compile but panics on error (for constant tables).
func MustCompile(pat string) *Pattern {
	p, err := Compile(pat)
	if err != nil {
		panic(err)
	}
	return p
}

// String returns the original pattern.
func (p *Pattern) String() string { return p.raw }

// StaticPrefix returns the longest literal directory prefix common to all variants ("src/a/**/*.ts" → "src/a").
// Empty means it cannot be narrowed.
func (p *Pattern) StaticPrefix() string { return p.prefix }

// Match matches a path (forward slashes, relative, no leading "./").
func (p *Pattern) Match(path string) bool {
	if path == "" {
		return false
	}
	segs := strings.Split(path, "/")
	for i := range p.variants {
		if matchSegs(p.variants[i].segs, segs) {
			return true
		}
	}
	return false
}

// Match is a convenience; an invalid pattern → false.
func Match(pat, path string) bool {
	p, err := Compile(pat)
	if err != nil {
		return false
	}
	return p.Match(path)
}

// expandBraces expands `{a,b}` (nestable) into a list of strings; leaves `\x` and `[...]` unchanged.
func expandBraces(s string) ([]string, error) {
	open := -1
scan:
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '[':
			if j := classEnd(s, i); j > 0 {
				i = j
			}
		case '{':
			open = i
			break scan
		}
	}
	if open < 0 {
		return []string{s}, nil
	}
	depth := 0
	var commas []int
	closeIdx := -1
	for i := open; i < len(s) && closeIdx < 0; i++ {
		switch s[i] {
		case '\\':
			i++
		case '[':
			if j := classEnd(s, i); j > 0 {
				i = j
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				closeIdx = i
			}
		case ',':
			if depth == 1 {
				commas = append(commas, i)
			}
		}
	}
	if closeIdx < 0 {
		return nil, errors.New("missing '}'")
	}
	pre, post := s[:open], s[closeIdx+1:]
	var parts []string
	start := open + 1
	for _, c := range commas {
		parts = append(parts, s[start:c])
		start = c + 1
	}
	parts = append(parts, s[start:closeIdx])
	postAlts, err := expandBraces(post)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, part := range parts {
		partAlts, err := expandBraces(part)
		if err != nil {
			return nil, err
		}
		for _, pa := range partAlts {
			for _, po := range postAlts {
				out = append(out, pre+pa+po)
				if len(out) > maxAlternatives {
					return nil, errors.New("too many {} variants")
				}
			}
		}
	}
	return out, nil
}

// classEnd returns the index of the `]` closing the class that starts at s[i]=='[', or -1 if there is none.
func classEnd(s string, i int) int {
	j := i + 1
	if j < len(s) && (s[j] == '!' || s[j] == '^') {
		j++
	}
	if j < len(s) && s[j] == ']' { // a leading ']' is an ordinary character
		j++
	}
	for ; j < len(s); j++ {
		if s[j] == '\\' {
			j++
			continue
		}
		if s[j] == ']' {
			return j
		}
	}
	return -1
}

func compileVariant(s string) (variant, error) {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '[':
			if j := classEnd(s, i); j > 0 {
				i = j
			}
		case '/':
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	var v variant
	for _, part := range parts {
		if part == "" {
			continue // "a//b" ≡ "a/b"; leading/trailing "/" ignored
		}
		if part == "**" {
			if n := len(v.segs); n > 0 && v.segs[n-1].doubleStar {
				continue // merge consecutive **
			}
			v.segs = append(v.segs, segment{doubleStar: true})
			continue
		}
		seg, err := compileSegment(part)
		if err != nil {
			return v, err
		}
		v.segs = append(v.segs, seg)
	}
	if len(v.segs) == 0 {
		return v, errors.New("empty pattern")
	}
	return v, nil
}

func compileSegment(s string) (segment, error) {
	var seg segment
	var lit strings.Builder
	isLit := true
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case '\\':
			if i+1 >= len(s) {
				return seg, errors.New("'\\' at end of pattern")
			}
			r, n := utf8.DecodeRuneInString(s[i+1:])
			seg.toks = append(seg.toks, token{kind: tLit, r: r})
			lit.WriteRune(r)
			i += 1 + n
		case '*':
			for i < len(s) && s[i] == '*' {
				i++
			}
			seg.toks = append(seg.toks, token{kind: tStar})
			isLit = false
		case '?':
			seg.toks = append(seg.toks, token{kind: tAny})
			isLit = false
			i++
		case '[':
			end := classEnd(s, i)
			if end < 0 {
				return seg, errors.New("missing ']'")
			}
			t, err := parseClass(s[i+1 : end])
			if err != nil {
				return seg, err
			}
			seg.toks = append(seg.toks, t)
			isLit = false
			i = end + 1
		default:
			r, n := utf8.DecodeRuneInString(s[i:])
			seg.toks = append(seg.toks, token{kind: tLit, r: r})
			lit.WriteRune(r)
			i += n
		}
	}
	seg.isLit = isLit
	if isLit {
		seg.literal = lit.String()
	}
	return seg, nil
}

// parseClass parses the body of a character class (excluding the brackets).
func parseClass(body string) (token, error) {
	t := token{kind: tClass}
	if body != "" && (body[0] == '!' || body[0] == '^') {
		t.neg = true
		body = body[1:]
	}
	type item struct {
		r       rune
		escaped bool
	}
	var items []item
	for i := 0; i < len(body); {
		r, n := utf8.DecodeRuneInString(body[i:])
		if r == '\\' && i+n < len(body) {
			r2, n2 := utf8.DecodeRuneInString(body[i+n:])
			items = append(items, item{r2, true})
			i += n + n2
			continue
		}
		items = append(items, item{r, false})
		i += n
	}
	for i := 0; i < len(items); i++ {
		lo := items[i].r
		if i+2 < len(items) && items[i+1].r == '-' && !items[i+1].escaped {
			hi := items[i+2].r
			if hi < lo {
				return t, fmt.Errorf("reversed range [%c-%c]", lo, hi)
			}
			t.ranges = append(t.ranges, rng{lo, hi})
			i += 2
			continue
		}
		t.ranges = append(t.ranges, rng{lo, lo})
	}
	if len(t.ranges) == 0 {
		return t, errors.New("empty character class")
	}
	return t, nil
}

func (t *token) classMatch(r rune) bool {
	in := false
	for _, g := range t.ranges {
		if r >= g.lo && r <= g.hi {
			in = true
			break
		}
	}
	return in != t.neg
}

// matchSegs: matches the list of pattern segments against path segments; `**` uses two pointers.
// A trailing `**` requires at least one remaining segment ("a/**" does not match "a").
func matchSegs(pat []segment, path []string) bool {
	pi, si := 0, 0
	starP, starS := -1, 0
	for si < len(path) {
		if pi < len(pat) && pat[pi].doubleStar {
			starP, starS = pi, si
			pi++
			if pi == len(pat) {
				return true
			}
			continue
		}
		if pi < len(pat) && matchSegment(&pat[pi], path[si]) {
			pi++
			si++
			continue
		}
		if starP >= 0 {
			starS++
			si = starS
			pi = starP + 1
			continue
		}
		return false
	}
	if pi == len(pat)-1 && pat[pi].doubleStar {
		return false // a trailing `**` must swallow ≥1 segment
	}
	return pi == len(pat)
}

func matchSegment(seg *segment, s string) bool {
	if seg.isLit {
		return seg.literal == s
	}
	toks := seg.toks
	ti, si := 0, 0
	starT, starS := -1, 0
	for si < len(s) {
		if ti < len(toks) {
			t := &toks[ti]
			switch t.kind {
			case tStar:
				starT, starS = ti, si
				ti++
				continue
			case tLit:
				r, n := utf8.DecodeRuneInString(s[si:])
				if r == t.r {
					ti++
					si += n
					continue
				}
			case tAny:
				_, n := utf8.DecodeRuneInString(s[si:])
				ti++
				si += n
				continue
			case tClass:
				r, n := utf8.DecodeRuneInString(s[si:])
				if t.classMatch(r) {
					ti++
					si += n
					continue
				}
			}
		}
		if starT >= 0 {
			_, n := utf8.DecodeRuneInString(s[starS:])
			starS += n
			si = starS
			ti = starT + 1
			continue
		}
		return false
	}
	for ti < len(toks) && toks[ti].kind == tStar {
		ti++
	}
	return ti == len(toks)
}

func staticPrefix(vs []variant) string {
	var common []string
	for i, v := range vs {
		var lits []string
		for j, s := range v.segs {
			if !s.isLit || j == len(v.segs)-1 { // the last segment is the file, not counted
				break
			}
			lits = append(lits, s.literal)
		}
		if i == 0 {
			common = lits
			continue
		}
		n := 0
		for n < len(common) && n < len(lits) && common[n] == lits[n] {
			n++
		}
		common = common[:n]
	}
	return strings.Join(common, "/")
}

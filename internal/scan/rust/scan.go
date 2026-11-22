// Package rust scans Rust source for what decides Cargo-target ownership (docs/ecosystems/rust.md): `mod name;` declarations (with their
// #[path] attributes and the context they appear in) and include!/include_str!/include_bytes! invocations.
//
// It is a lexer plus a flat token pass, not a parser. FAIL-CLOSED principle: whenever the token pass sees something it cannot interpret
// lexically (a module declared inside a macro, #[cfg_attr(.., path = ..)], a non-literal #[path]), the declaration is reported with
// Unknown set and the caller treats the whole package as the owner. A malformed file (unterminated comment/string, unbalanced
// delimiters) is reported Uncertain.
package rust

import (
	"strings"

	"github.com/qxbao/bgraph/internal/model"
)

// MaxFileSize: files larger than this threshold (bytes) are treated as uncertain (not scanned).
const MaxFileSize = 8 << 20

// Scan scans one .rs file. path is only used for diagnostics.
func Scan(path string, src []byte) model.ScanResult {
	if len(src) > MaxFileSize {
		return uncertain("file_too_large: " + path)
	}
	toks, reason := lex(src)
	if reason != "" {
		return uncertain("lex: " + reason)
	}
	p := &parser{src: src, toks: toks}
	if r := p.matchDelims(); r != "" {
		return uncertain("lex: " + r)
	}
	rs := p.run()
	return model.ScanResult{Rust: rs}
}

// Strings returns the value of every string literal in src, in order (used for build scripts, whose literal paths are inputs).
// A malformed file returns ok=false.
func Strings(src []byte) (out []string, ok bool) {
	if len(src) > MaxFileSize {
		return nil, false
	}
	toks, reason := lex(src)
	if reason != "" {
		return nil, false
	}
	for _, t := range toks {
		if t.kind == tStr {
			out = append(out, strValue(src, t))
		}
	}
	return out, true
}

func uncertain(reason string) model.ScanResult {
	return model.ScanResult{Uncertain: true, UncertainReason: reason}
}

type parser struct {
	src   []byte
	toks  []token
	match []int32 // for an opening delimiter its closing index and vice versa; -1 otherwise
}

func (p *parser) ch(i int) byte {
	t := p.toks[i]
	if t.kind != tPunct {
		return 0
	}
	return p.src[t.start]
}

func (p *parser) ident(i int) string {
	if i < 0 || i >= len(p.toks) || p.toks[i].kind != tIdent {
		return ""
	}
	t := p.toks[i]
	return string(p.src[t.start:t.end])
}

func (p *parser) isPunct(i int, c byte) bool {
	return i >= 0 && i < len(p.toks) && p.toks[i].kind == tPunct && p.toks[i].end == p.toks[i].start+1 && p.src[p.toks[i].start] == c
}

func isOpen(c byte) bool  { return c == '(' || c == '[' || c == '{' }
func isClose(c byte) bool { return c == ')' || c == ']' || c == '}' }
func closerOf(c byte) byte {
	switch c {
	case '(':
		return ')'
	case '[':
		return ']'
	}
	return '}'
}

// matchDelims pairs the delimiters; a mismatch means the lexical structure cannot be trusted.
func (p *parser) matchDelims() string {
	p.match = make([]int32, len(p.toks))
	var stack []int32
	for i, t := range p.toks {
		p.match[i] = -1
		if t.kind != tPunct || t.end != t.start+1 {
			continue
		}
		c := p.src[t.start]
		switch {
		case isOpen(c):
			stack = append(stack, int32(i))
		case isClose(c):
			if len(stack) == 0 {
				return "unbalanced delimiters"
			}
			o := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if closerOf(p.src[p.toks[o].start]) != c {
				return "mismatched delimiters"
			}
			p.match[o], p.match[i] = int32(i), o
		}
	}
	if len(stack) > 0 {
		return "unbalanced delimiters"
	}
	return ""
}

type frameKind uint8

const (
	fOther frameKind = iota + 1 // block, call, impl body...
	fMod                        // inline module
	fMacro                      // macro_rules! definition or macro invocation
)

type frame struct {
	kind    frameKind
	name    string
	unknown string // fMod: the module carries a path attribute we do not interpret
}

func (p *parser) run() *model.RustScan {
	rs := &model.RustScan{}
	var frames []frame
	pendingOpen := -1 // index of the `{` that opens an inline module
	var pendingMod frame
	for i := 0; i < len(p.toks); i++ {
		t := p.toks[i]
		switch t.kind {
		case tPunct:
			c := p.ch(i)
			if isOpen(c) {
				switch {
				case i == pendingOpen:
					frames = append(frames, pendingMod)
					pendingOpen = -1
				case i >= 2 && p.isPunct(i-1, '!') && p.ident(i-2) != "":
					frames = append(frames, frame{kind: fMacro}) // name!{..} / name!(..) / name![..]
				case i >= 3 && p.ident(i-1) != "" && p.isPunct(i-2, '!') && p.ident(i-3) == "macro_rules":
					frames = append(frames, frame{kind: fMacro})
				default:
					frames = append(frames, frame{kind: fOther})
				}
			} else if isClose(c) && len(frames) > 0 {
				frames = frames[:len(frames)-1]
			}
		case tIdent:
			switch name := p.ident(i); name {
			case "mod":
				if i > 0 && (p.isPunct(i-1, '.') || p.isPunct(i-1, ':')) {
					continue
				}
				p.handleMod(i, frames, rs, &pendingOpen, &pendingMod)
			case "automod":
				if p.isPunct(i+1, ':') && p.isPunct(i+2, ':') && p.ident(i+3) == "dir" && p.isPunct(i+4, '!') && i+5 < len(p.toks) &&
					p.toks[i+5].kind == tPunct && isOpen(p.ch(i+5)) {
					p.handleAutomod(i, i+5, rs)
				}
			case "include", "include_str", "include_bytes":
				if p.isPunct(i+1, '!') && i+2 < len(p.toks) && p.toks[i+2].kind == tPunct && isOpen(p.ch(i+2)) {
					p.handleInclude(i, name, rs)
				}
			}
		}
	}
	if len(rs.Mods) == 0 {
		rs.Mods = nil
	}
	if len(rs.Includes) == 0 {
		rs.Includes = nil
	}
	return rs
}

// modAttrs inspects the outer attributes directly in front of the `mod` keyword at index i (walking backwards over `pub(..)`).
func (p *parser) modAttrs(i int) (path string, hasPath, pathDyn, cfgAttrPath bool) {
	k := i
	if k > 0 && p.isPunct(k-1, ')') {
		if m := int(p.match[k-1]); m > 0 && p.ident(m-1) == "pub" {
			k = m - 1
		}
	} else if k > 0 && p.ident(k-1) == "pub" {
		k--
	}
	for k > 0 && p.isPunct(k-1, ']') {
		m := int(p.match[k-1])
		if m < 1 || !p.isPunct(m-1, '#') {
			break
		}
		// attribute tokens: m+1 .. k-2
		lo, hi := m+1, k-2
		switch p.ident(lo) {
		case "path":
			hasPath = true
			if hi == lo+2 && p.isPunct(lo+1, '=') && p.toks[lo+2].kind == tStr {
				path = strValue(p.src, p.toks[lo+2])
			} else {
				pathDyn = true
			}
		case "cfg_attr":
			for j := lo + 1; j <= hi; j++ {
				if p.ident(j) == "path" {
					cfgAttrPath = true
				}
			}
		}
		k = m - 1
	}
	return
}

func (p *parser) handleMod(i int, frames []frame, rs *model.RustScan, pendingOpen *int, pendingMod *frame) {
	j := i + 1
	dollar := false
	if p.isPunct(j, '$') {
		dollar = true
		j++
	}
	name := p.ident(j)
	if name == "" {
		return
	}
	if dollar {
		name = "$" + name
	}
	path, hasPath, pathDyn, cfgAttrPath := p.modAttrs(i)
	switch {
	case p.isPunct(j+1, '{'):
		// Inline module: remember it as a frame; a path attribute on it changes where its children live.
		f := frame{kind: fMod, name: name}
		if hasPath || cfgAttrPath {
			f.unknown = "enclosing inline module has a path attribute"
		}
		*pendingOpen, *pendingMod = j+1, f
	case p.isPunct(j+1, ';'):
		m := model.RustMod{Name: name, Line: int(p.toks[i].line), Path: path}
		var nest []string
		for _, f := range frames {
			switch f.kind {
			case fMacro:
				if m.Unknown == "" {
					m.Unknown = "module declared inside a macro"
				}
			case fOther:
				if m.Unknown == "" {
					m.Unknown = "module declared inside a block"
				}
			case fMod:
				nest = append(nest, f.name)
				if f.unknown != "" && m.Unknown == "" {
					m.Unknown = f.unknown
				}
			}
		}
		if len(nest) > 0 {
			m.Nest = nest
		}
		switch {
		case m.Unknown != "":
		case dollar:
			m.Unknown = "macro-generated module name"
		case cfgAttrPath:
			m.Unknown = "cfg_attr with a path attribute"
		case pathDyn:
			m.Unknown = "path attribute is not a string literal"
		}
		rs.Mods = append(rs.Mods, m)
	}
}

// handleInclude records the include macro at index i (the identifier); the argument group opens at i+2.
func (p *parser) handleInclude(i int, macro string, rs *model.RustScan) {
	open := i + 2
	closeIdx := int(p.match[open])
	inc := model.RustInclude{Macro: macro, Kind: model.RustIncludeDynamic, Line: int(p.toks[i].line)}
	lo, hi := open+1, closeIdx-1 // inclusive argument token range
	if hi >= lo && p.isPunct(hi, ',') {
		hi--
	}
	if lo == hi && p.toks[lo].kind == tStr {
		inc.Kind, inc.Path = model.RustIncludeLiteral, strValue(p.src, p.toks[lo])
	} else if kind, path, ok := p.concatPath(lo, hi); ok {
		inc.Kind, inc.Path = kind, path
	}
	rs.Includes = append(rs.Includes, inc)
}

// handleAutomod records automod::dir!("path") (the automod crate generates a `mod` per file of the directory, relative to the
// manifest directory); any other argument form is dynamic.
func (p *parser) handleAutomod(i, open int, rs *model.RustScan) {
	inc := model.RustInclude{Macro: "automod::dir", Kind: model.RustIncludeDynamic, Line: int(p.toks[i].line)}
	lo, hi := open+1, int(p.match[open])-1
	if lo <= hi && p.ident(lo) == "pub" {
		lo++
		if lo <= hi && p.isPunct(lo, '(') {
			lo = int(p.match[lo]) + 1
		}
	}
	if lo == hi && p.toks[lo].kind == tStr {
		inc.Kind, inc.Path = model.RustIncludeDir, strValue(p.src, p.toks[lo])
	}
	rs.Includes = append(rs.Includes, inc)
}

// concatPath evaluates concat!(parts...) where each part is a string literal or env!("OUT_DIR"/"CARGO_MANIFEST_DIR").
func (p *parser) concatPath(lo, hi int) (kind, path string, ok bool) {
	i := p.skipPathPrefix(lo)
	if p.ident(i) != "concat" || !p.isPunct(i+1, '!') || !p.isPunct(i+2, '(') || int(p.match[i+2]) != hi {
		return "", "", false
	}
	var b strings.Builder
	manifest, outDir := false, false
	start := i + 3
	for start < hi {
		end := start
		for end < hi && !p.isPunct(end, ',') {
			if o := p.ch(end); isOpen(o) && p.toks[end].kind == tPunct {
				end = int(p.match[end])
			}
			end++
		}
		part := [2]int{start, end - 1}
		switch {
		case part[0] == part[1] && p.toks[part[0]].kind == tStr:
			b.WriteString(strValue(p.src, p.toks[part[0]]))
		default:
			q := p.skipPathPrefix(part[0])
			if p.ident(q) == "env" && p.isPunct(q+1, '!') && p.isPunct(q+2, '(') && q+4 == part[1] && p.toks[q+3].kind == tStr {
				switch strValue(p.src, p.toks[q+3]) {
				case "OUT_DIR":
					outDir = true
				case "CARGO_MANIFEST_DIR":
					manifest = true
				default:
					return "", "", false
				}
			} else {
				return "", "", false
			}
		}
		start = end + 1
	}
	switch {
	case outDir:
		return model.RustIncludeOutDir, "", true
	case manifest:
		return model.RustIncludeManifest, strings.TrimLeft(b.String(), "/\\"), true
	}
	return model.RustIncludeLiteral, b.String(), true
}

// skipPathPrefix skips a leading `std ::` / `core ::` / `::` of a path-qualified macro name.
func (p *parser) skipPathPrefix(i int) int {
	for i+2 < len(p.toks) && p.isPunct(i, ':') && p.isPunct(i+1, ':') {
		i += 2
	}
	for i+3 < len(p.toks) && p.ident(i) != "" && p.isPunct(i+1, ':') && p.isPunct(i+2, ':') {
		i += 3
	}
	return i
}

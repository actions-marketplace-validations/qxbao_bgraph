// Package sfc extracts embedded code from Single-File-Component files (.vue, .svelte, .astro) and .mdx and hands it to the JS/TS scanner
// (bgraph/internal/scan/js) and the CSS @import scanner.
//
// FAIL-CLOSED principle (same as the JS scanner): if it is not sure the blocks were split correctly (unclosed block, unclosed comment,
// stray closing tag, duplicate script/setup, unknown lang...) it returns Uncertain=true with a reason "block-kind: code: detail", and never
// silently returns a partial list. The Line of every ImportRef is the line in the ORIGINAL FILE (block offset already added).
package sfc

import (
	"bytes"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scan/js"
)

// ReasonUnsupported is the reason code for a file extension not handled by this package.
const ReasonUnsupported = "unsupported_sfc"

// Scan scans a .vue/.svelte/.astro/.mdx file (chosen by extension, case-insensitive). Any other extension → Uncertain.
func Scan(path string, src []byte) model.ScanResult {
	ext := extOf(path)
	switch ext {
	case ".vue", ".svelte", ".astro", ".mdx":
	default:
		return unc(ReasonUnsupported + ": " + ext)
	}
	if len(src) > js.MaxFileSize {
		return unc("file_too_large: " + strconv.Itoa(len(src)) + " bytes")
	}
	if bytes.IndexByte(src, 0) >= 0 {
		return unc("binary_content: NUL byte in source")
	}
	if !utf8.Valid(src) {
		return unc("invalid_utf8: source is not valid UTF-8")
	}
	src = bytes.TrimPrefix(src, []byte{0xEF, 0xBB, 0xBF})
	a := &acc{src: src, res: model.ScanResult{Imports: []model.ImportRef{}}}
	for i, c := range src {
		if c == '\n' {
			a.nl = append(a.nl, i)
		}
	}
	switch ext {
	case ".vue":
		scanVue(a)
	case ".svelte":
		scanSvelte(a)
	case ".astro":
		scanAstro(a)
	case ".mdx":
		scanMDX(a)
	}
	return a.res
}

func unc(reason string) model.ScanResult {
	return model.ScanResult{Imports: []model.ImportRef{}, Uncertain: true, UncertainReason: reason}
}

func extOf(p string) string {
	k := len(p)
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '.' {
			k = i
			break
		}
		if p[i] == '/' || p[i] == '\\' {
			break
		}
	}
	return string(asciiLower([]byte(p[k:])))
}

// asciiLower lowercases ASCII letters, preserving length (bytes.ToLower can change the length for Unicode).
func asciiLower(b []byte) []byte {
	o := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		o[i] = c
	}
	return o
}

// acc accumulates the results for one file.
type acc struct {
	src []byte
	nl  []int // positions of the '\n' characters
	res model.ScanResult
}

// line returns the (1-based) line number of position pos in the original file.
func (a *acc) line(pos int) int {
	return 1 + sort.SearchInts(a.nl, pos)
}

// stopped: an uncertain reason is already set, loops should stop.
func (a *acc) stopped() bool { return a.res.Uncertain }

func (a *acc) fail(kind, reason string) {
	if a.res.Uncertain {
		return
	}
	a.res.Uncertain = true
	a.res.UncertainReason = kind + ": " + reason
}

func (a *acc) ref(spec string, line int) {
	a.res.Imports = append(a.res.Imports, model.ImportRef{Spec: spec, Kind: model.ImportStatic, Line: line})
}

// scanJS hands the content of one block to the JS scanner with virtual extension ext; pos is the start position of the content in the original file.
func (a *acc) scanJS(kind string, content []byte, ext string, pos int) {
	if len(bytes.TrimSpace(content)) == 0 {
		return
	}
	r := js.Scan("block"+ext, content)
	off := a.line(pos) - 1
	for _, im := range r.Imports {
		im.Line += off
		a.res.Imports = append(a.res.Imports, im)
	}
	if r.Uncertain {
		a.fail(kind, r.UncertainReason)
	}
}

// scanStyle scans @import/@use/@forward/@plugin in the content of one style block.
func (a *acc) scanStyle(kind string, ext string, content []byte, pos int) {
	off := a.line(pos) - 1
	imps, reason := scanCSS(content, ext)
	for _, im := range imps {
		a.ref(im.spec, im.line+off)
	}
	if reason != "" {
		a.fail(kind, reason)
	}
}

// remote: external URL, not a file dependency.
func remote(s string) bool {
	return hasPrefixFold(s, "http://") || hasPrefixFold(s, "https://") || hasPrefixFold(s, "//") || hasPrefixFold(s, "data:")
}

func hasPrefixFold(s, p string) bool {
	if len(s) < len(p) {
		return false
	}
	return string(asciiLower([]byte(s[:len(p)]))) == p
}

func relative(s string) bool {
	return len(s) > 1 && s[0] == '.' && (s[1] == '/' || (s[1] == '.' && len(s) > 2 && s[2] == '/'))
}

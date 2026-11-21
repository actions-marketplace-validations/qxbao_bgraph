// Package sfc trích mã nhúng từ tệp Single-File-Component (.vue, .svelte, .astro) và .mdx rồi đưa cho bộ quét JS/TS
// (bgraph/internal/scan/js) và bộ quét @import của CSS (mục 5 của docs/plan.md).
//
// Nguyên tắc FAIL-CLOSED (giống bộ quét JS): nếu không chắc đã tách đúng các khối (khối không đóng, comment không đóng,
// thẻ đóng lạc, script/setup trùng, lang lạ...) thì trả Uncertain=true kèm lý do "loại-khối: mã: chi tiết", không bao giờ
// lặng lẽ trả danh sách một phần. Dòng (Line) của mọi ImportRef là dòng trong TỆP GỐC (đã cộng độ lệch của khối).
package sfc

import (
	"bytes"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scan/js"
)

// ReasonUnsupported là mã lý do cho đuôi tệp không thuộc gói này.
const ReasonUnsupported = "unsupported_sfc"

// Scan quét một tệp .vue/.svelte/.astro/.mdx (chọn theo đuôi, không phân biệt hoa thường). Đuôi khác → Uncertain.
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

// asciiLower hạ chữ hoa ASCII, giữ nguyên độ dài (bytes.ToLower có thể đổi độ dài với Unicode).
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

// acc gom kết quả của một tệp.
type acc struct {
	src []byte
	nl  []int // vị trí các '\n'
	res model.ScanResult
}

// line trả số dòng (1-based) của vị trí pos trong tệp gốc.
func (a *acc) line(pos int) int {
	return 1 + sort.SearchInts(a.nl, pos)
}

// stopped: đã có lý do uncertain, các vòng lặp nên dừng.
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

// scanJS đưa nội dung một khối cho bộ quét JS với đuôi ảo ext; pos là vị trí đầu nội dung trong tệp gốc.
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

// scanStyle quét @import/@use/@forward/@plugin trong nội dung một khối style.
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

// remote: URL ngoài, không phải phụ thuộc tệp.
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

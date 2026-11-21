package js

import (
	"strings"

	"bgraph/internal/model"
)

// Kiểu đối số đặc biệt của lời gọi.
const (
	fNormal uint8 = iota
	fGlob         // import.meta.glob / globEager
	fCtx          // require.context
	fURL          // new URL(x, import.meta.url)
)

// capture theo dõi đối số của một lời gọi giống import: `(` mở ra capture, các token ở đúng mức ngoặc đó được đưa vào
// feed*, `)` đóng lại và ghi kết quả vào slot đã giữ chỗ trong s.imports (để giữ thứ tự nguồn khi có lời gọi lồng nhau).
type capture struct {
	kind   model.ImportKind
	flavor uint8
	slot   int
	line   int
	argN   int // số dấu phẩy ở mức này
	argTok int // số token trong đối số hiện tại

	head   string // phần tĩnh đầu
	litAcc string // nối các hằng chuỗi khi mọi toán hạng đều là hằng
	suf    string // phần tĩnh đuôi (của toán hạng cuối)
	allLit bool
	bad    bool // đối số đầu không hiểu được => Spec rỗng, dynamic
	expOp  bool // vừa gặp '+', đang chờ toán hạng
	cancel bool
	method bool // có thể là khai báo method (xem slow/pcMethod)
	done   bool

	// glob mảng
	arr     bool
	arrDyn  bool
	expElem bool

	// require.context
	ctxSet bool
	ctxRec bool

	// new URL
	urlBad bool
	urlOK  bool
}

type candidate struct {
	kind   model.ImportKind
	flavor uint8
	ok     bool
}

func (s *scanner) newCap(kind model.ImportKind, flavor uint8, method bool, line int) int32 {
	slot := len(s.imports)
	s.imports = append(s.imports, model.ImportRef{Kind: kind, Line: line, Dynamic: true})
	s.caps = append(s.caps, capture{kind: kind, flavor: flavor, slot: slot, line: line, method: method})
	return int32(len(s.caps))
}

func (c *capture) nonLit() {
	if c.allLit {
		c.head = c.litAcc
		c.allLit = false
	}
}

func (s *scanner) topCap() int32 {
	if n := len(s.stack); n > 0 {
		return s.stack[n-1].cap
	}
	return 0
}

// feed nhận token không phải literal ở mức ngoặc của capture.
func (s *scanner) feed(cp int32, tk, ch, w uint8) {
	c := &s.caps[cp-1]
	if c.done {
		return
	}
	if s.stack[len(s.stack)-1].kind == kBracket {
		if tk == tPunct {
			if ch == ',' {
				c.expElem = true
				return
			}
			if ch == ']' {
				return
			}
		}
		s.globDyn(c)
		c.expElem = false
		return
	}
	if tk == tPunct {
		if ch == ')' {
			s.finish(c)
			return
		}
		if ch == ',' {
			c.argN++
			c.argTok = 0
			return
		}
	}
	switch c.argN {
	case 0:
		s.arg0(cp, c, tk, ch, w)
	case 1:
		switch c.flavor {
		case fCtx:
			if c.argTok == 0 {
				c.ctxSet = true
				c.ctxRec = !(tk == tWord && w == wFalse)
			}
		case fURL:
			idx := c.argTok
			if idx <= 4 {
				ok := false
				switch idx {
				case 0:
					ok = tk == tWord && w == wImport
				case 1, 3:
					ok = tk == tPunct && ch == '.'
				case 2:
					ok = tk == tWord && w == wMeta
				case 4:
					ok = tk == tWord && w == wUrl
				}
				if !ok {
					c.urlBad = true
				} else if idx == 4 && !c.urlBad {
					c.urlOK = true
				}
			}
		}
	}
	c.argTok++
}

func (s *scanner) arg0(cp int32, c *capture, tk, ch, w uint8) {
	if c.argTok == 0 {
		c.line = s.line
		if tk == tPunct && ch == '[' && c.flavor == fGlob {
			c.arr = true
			c.expElem = true
			s.arrCap = cp
			return
		}
		if tk == tWord && w == wImport && c.kind == model.ImportMock {
			c.cancel = true // vi.mock(import('./a')): import() lồng bên trong là tham chiếu thật
		}
		c.bad = true
		return
	}
	if c.arr {
		s.globDyn(c)
		return
	}
	if c.bad {
		return
	}
	if tk == tPunct {
		switch ch {
		case '+':
			c.expOp = true
			c.suf = ""
		case '.', pQDot, '(', '[':
			c.nonLit()
			c.suf = ""
		default:
			c.bad = true
		}
		return
	}
	c.nonLit()
	c.suf = ""
	c.expOp = false
}

// feedLit nhận literal chuỗi / template. tail và hasSub chỉ có nghĩa với template.
func (s *scanner) feedLit(cp int32, val, tail string, hasSub bool, ln int, ok bool) {
	c := &s.caps[cp-1]
	if c.done {
		return
	}
	if s.stack[len(s.stack)-1].kind == kBracket {
		if c.expElem && !hasSub && ok {
			s.imports = append(s.imports, model.ImportRef{Spec: val, Kind: model.ImportGlob, Line: ln})
		} else {
			s.globDyn(c)
		}
		c.expElem = false
		return
	}
	if c.argN != 0 {
		if c.argN == 1 {
			if c.flavor == fURL {
				c.urlBad = true
			} else if c.flavor == fCtx && c.argTok == 0 {
				c.ctxSet = true
				c.ctxRec = true
			}
		}
		c.argTok++
		return
	}
	switch {
	case c.argTok == 0:
		c.line = ln
		switch {
		case !ok:
			c.bad = true
		case !hasSub:
			c.head, c.litAcc, c.suf, c.allLit = val, val, val, true
		default:
			c.head, c.suf, c.allLit = val, tail, false
		}
	case c.arr:
		s.globDyn(c)
	case c.bad:
	case c.expOp && ok:
		c.expOp = false
		if hasSub {
			c.nonLit()
			c.suf = tail
		} else {
			c.suf = val
			if c.allLit {
				c.litAcc += val
			}
		}
	default:
		c.bad = true
	}
	c.argTok++
}

func (s *scanner) globDyn(c *capture) {
	if c.arrDyn {
		return
	}
	c.arrDyn = true
	s.imports = append(s.imports, model.ImportRef{Kind: model.ImportGlob, Line: s.line, Dynamic: true})
}

// finish đóng capture khi gặp ')'.
func (s *scanner) finish(c *capture) {
	c.done = true
	ref := &s.imports[c.slot]
	if c.cancel || (c.flavor == fURL && !c.urlOK) || (c.flavor == fGlob && c.arr) {
		ref.Line = -1
		return
	}
	if c.method {
		s.pc = pcState{kind: pcMethod, slot: c.slot}
		s.watch = true
	}
	if c.argN == 0 && c.argTok == 0 {
		ref.Line = s.line
		ref.Spec, ref.Suffix, ref.Dynamic = "", "", true
		return
	}
	ref.Line = c.line
	if c.bad {
		ref.Spec, ref.Suffix, ref.Dynamic = "", "", true
		return
	}
	if c.allLit {
		ref.Spec, ref.Suffix, ref.Dynamic = c.litAcc, "", false
		if c.flavor == fCtx {
			if c.ctxSet && c.ctxRec {
				ref.Spec = strings.TrimSuffix(ref.Spec, "/") + "/**"
			} else {
				ref.Spec = strings.TrimSuffix(ref.Spec, "/") + "/*"
			}
		}
		return
	}
	ref.Spec, ref.Suffix, ref.Dynamic = c.head, c.suf, true
}

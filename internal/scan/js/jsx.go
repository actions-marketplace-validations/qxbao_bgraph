package js

import (
	"bytes"
	"strconv"
)

const (
	tagOpen = iota + 1
	tagSelf
	tagClose
)

type nameRange struct{ a, b int }

// jsxStart quyết định `<` tại s.i có mở phần tử JSX hay không (chỉ gọi khi ở vị trí biểu thức và bật JSX).
func (s *scanner) jsxStart() bool {
	src := s.src
	n := len(src)
	j := s.i + 1
	if j >= n {
		return false
	}
	c := src[j]
	if c == '>' {
		return true
	}
	if !identStart[c] {
		return false
	}
	k := j
	for k < n && (identPart[src[k]] || src[k] == '-' || src[k] == '.' || src[k] == ':') {
		k++
	}
	m := k
	for m < n && (src[m] == ' ' || src[m] == '\t' || src[m] == '\n' || src[m] == '\r') {
		m++
	}
	if m < n {
		switch src[m] {
		case ',':
			return false // <T,>(x) => ...
		case '=':
			return false // <T = unknown>(...)
		}
		if bytes.HasPrefix(src[m:], []byte("extends")) && (m+7 >= n || !identPart[src[m+7]]) {
			return false // <T extends U>(...)
		}
	}
	if string(src[j:k]) == "const" && m > k {
		// <const T,>(...)
		q := m
		for q < n && identPart[src[q]] {
			q++
		}
		for q < n && (src[q] == ' ' || src[q] == '\t' || src[q] == '\n' || src[q] == '\r') {
			q++
		}
		if q < n && (src[q] == ',' || bytes.HasPrefix(src[q:], []byte("extends"))) {
			return false
		}
	}
	return true
}

func (s *scanner) jsxFail(detail string) {
	s.fail("jsx", detail+" at line "+strconv.Itoa(s.line))
}

// jsxWS bỏ khoảng trắng và comment trong thẻ, bắt đầu từ i; cập nhật s.line.
func (s *scanner) jsxWS(i int) int {
	src := s.src
	n := len(src)
	for i < n {
		switch src[i] {
		case ' ', '\t', '\r':
			i++
		case '\n':
			s.line++
			i++
		case '/':
			if i+1 < n && src[i+1] == '*' {
				k := bytes.Index(src[i+2:], []byte("*/"))
				if k < 0 {
					s.jsxFail("unterminated comment in tag")
					return n
				}
				s.line += bytes.Count(src[i+2:i+2+k], []byte{'\n'})
				i += k + 4
			} else if i+1 < n && src[i+1] == '/' {
				k := bytes.IndexByte(src[i:], '\n')
				if k < 0 {
					return n
				}
				i += k
			} else {
				return i
			}
		default:
			if c := src[i]; c >= 0x80 {
				if l := uniSpaceLen(src[i:]); l > 0 {
					i += l
					continue
				}
			}
			return i
		}
	}
	return i
}

// jsxExpr: s.i ở '{'; chạy lexer JS tới '}' tương ứng.
func (s *scanner) jsxExpr() bool {
	s.i++
	if !s.enterSub() {
		return false
	}
	ok := s.lex(len(s.stack), true)
	s.leaveSub()
	if !ok {
		if !s.stop {
			s.jsxFail("unterminated expression container")
		}
		return false
	}
	return true
}

// jsxTag phân tích một thẻ bắt đầu tại s.i ('<').
func (s *scanner) jsxTag() (int, nameRange) {
	src := s.src
	n := len(src)
	i := s.jsxWS(s.i + 1)
	closing := false
	if i < n && src[i] == '/' {
		closing = true
		i = s.jsxWS(i + 1)
	}
	a := i
	for i < n && (identPart[src[i]] || src[i] == '-' || src[i] == '.' || src[i] == ':') {
		i++
	}
	nm := nameRange{a, i}
	if s.stop {
		return 0, nm
	}
	if closing {
		i = s.jsxWS(i)
		if i >= n || src[i] != '>' {
			s.jsxFail("malformed closing tag")
			return 0, nm
		}
		s.i = i + 1
		return tagClose, nm
	}
	i = s.jsxWS(i)
	if i < n && src[i] == '<' { // TSX: <Foo<Bar> prop />
		d := 0
		for i < n {
			if src[i] == '<' {
				d++
			} else if src[i] == '>' {
				d--
				if d == 0 {
					i++
					break
				}
			} else if src[i] == '\n' {
				s.line++
			}
			i++
		}
	}
	for {
		i = s.jsxWS(i)
		if s.stop {
			return 0, nm
		}
		if i >= n {
			s.jsxFail("unexpected end of file in tag")
			return 0, nm
		}
		c := src[i]
		switch {
		case c == '>':
			s.i = i + 1
			return tagOpen, nm
		case c == '/':
			if i+1 < n && src[i+1] == '>' {
				s.i = i + 2
				return tagSelf, nm
			}
			s.jsxFail("stray '/' in tag")
			return 0, nm
		case c == '{':
			s.i = i
			if !s.jsxExpr() {
				return 0, nm
			}
			i = s.i
		case identStart[c]:
			j := i
			for j < n && (identPart[src[j]] || src[j] == '-' || src[j] == ':' || src[j] == '.') {
				j++
			}
			i = s.jsxWS(j)
			if i < n && src[i] == '=' {
				i = s.jsxWS(i + 1)
				if i >= n {
					s.jsxFail("unexpected end of file in attribute")
					return 0, nm
				}
				switch q := src[i]; q {
				case '"', '\'':
					k := bytes.IndexByte(src[i+1:], q)
					if k < 0 {
						s.jsxFail("unterminated attribute string")
						return 0, nm
					}
					s.line += bytes.Count(src[i+1:i+1+k], []byte{'\n'})
					i += k + 2
				case '{':
					s.i = i
					if !s.jsxExpr() {
						return 0, nm
					}
					i = s.i
				case '<':
					s.i = i
					s.jsxElement()
					if s.stop {
						return 0, nm
					}
					i = s.i
				default:
					s.jsxFail("bad attribute value")
					return 0, nm
				}
			}
		default:
			s.jsxFail("unexpected character in tag")
			return 0, nm
		}
	}
}

// jsxElement phân tích trọn một phần tử JSX (cả con) bắt đầu tại s.i ('<'). Lỗi => fail("jsx").
func (s *scanner) jsxElement() {
	src := s.src
	n := len(src)
	if s.depth++; s.depth > maxDepth {
		s.fail("too_deep", "JSX nesting too deep")
		return
	}
	var buf [8]nameRange
	names := buf[:0]
	for {
		kind, nm := s.jsxTag()
		if s.stop {
			break
		}
		switch kind {
		case tagOpen:
			names = append(names, nm)
		case tagSelf:
			if len(names) == 0 {
				s.depth--
				return
			}
		case tagClose:
			if len(names) == 0 || !bytes.Equal(src[names[len(names)-1].a:names[len(names)-1].b], src[nm.a:nm.b]) {
				s.jsxFail("mismatched closing tag </" + string(src[nm.a:nm.b]) + ">")
				s.depth--
				return
			}
			names = names[:len(names)-1]
			if len(names) == 0 {
				s.depth--
				return
			}
		}
		// phần con: văn bản thuần tới '<' hoặc '{'
		for {
			if s.i >= n {
				s.jsxFail("unclosed element <" + string(src[names[len(names)-1].a:names[len(names)-1].b]) + ">")
				s.depth--
				return
			}
			c := src[s.i]
			if c == '<' {
				break
			}
			if c == '{' {
				if !s.jsxExpr() {
					s.depth--
					return
				}
				continue
			}
			if c == '\n' {
				s.line++
			}
			s.i++
		}
	}
	s.depth--
}

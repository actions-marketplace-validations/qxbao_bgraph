package python

// lexString lexes the string literal whose opening quote is at src[q]; the prefix flags come from the caller. It advances *line past
// embedded newlines and appends a strInfo. For an f-string the value is only the static head before the first replacement field.
func (lx *lexed) lexString(q int, raw, f, b bool, line *int32) (end, si int, ok bool, why string) {
	src := lx.src
	n := len(src)
	quote := src[q]
	i := q + 1
	triple := false
	if i+1 < n && src[i] == quote && src[i+1] == quote {
		triple = true
		i += 2
	}
	info := strInfo{bytes: b}
	if f {
		e, w := lx.scanF(i, quote, triple, raw, line, &info, 0, true)
		if w != "" {
			return 0, 0, false, w
		}
		lx.strs = append(lx.strs, info)
		return e, len(lx.strs) - 1, true, ""
	}
	var val []byte
	keep := true
	for {
		if i >= n {
			return 0, 0, false, "unterminated string"
		}
		c := src[i]
		switch {
		case c == '\\':
			if i+1 >= n {
				return 0, 0, false, "unterminated string"
			}
			d := src[i+1]
			switch {
			case d == '\r' && i+2 < n && src[i+2] == '\n':
				*line++
				i += 3
			case d == '\n':
				*line++
				i += 2
			default:
				if keep {
					if raw {
						val = append(val, c, d)
					} else {
						val = appendEscape(val, d)
					}
				}
				i += 2
			}
		case c == quote && (!triple || i+2 < n && src[i+1] == quote && src[i+2] == quote):
			if triple {
				i += 3
			} else {
				i++
			}
			if keep {
				info.val = string(val)
			} else {
				info.cut = true
			}
			lx.strs = append(lx.strs, info)
			return i, len(lx.strs) - 1, true, ""
		case c == '\n':
			if !triple {
				return 0, 0, false, "unterminated string"
			}
			*line++
			if keep {
				val = append(val, c)
			}
			i++
		default:
			if keep {
				val = append(val, c)
			}
			i++
		}
		if keep && len(val) > maxStrKeep {
			keep, val = false, nil
		}
	}
}

// appendEscape handles the escapes that can appear in a module name or a path; anything else is kept verbatim (backslash included).
func appendEscape(val []byte, d byte) []byte {
	switch d {
	case '\\', '\'', '"':
		return append(val, d)
	case 'n':
		return append(val, '\n')
	case 't':
		return append(val, '\t')
	case 'r':
		return append(val, '\r')
	}
	return append(val, '\\', d)
}

// scanF scans the body of an f-string starting at i (just after the opening quote) and returns the offset just past the closing quote.
// Replacement fields may contain nested strings (including f-strings and, since Python 3.12, the same quote), brackets and format
// specs with nested fields. top: record the static head and the field flag in info.
func (lx *lexed) scanF(i int, quote byte, triple, raw bool, line *int32, info *strInfo, depth int, top bool) (int, string) {
	if depth > maxFDepth {
		return 0, "f-string nested too deeply"
	}
	src := lx.src
	n := len(src)
	var head []byte
	headOpen := top
	keep := true
	for {
		if i >= n {
			return 0, "unterminated f-string"
		}
		c := src[i]
		switch {
		case c == '\\':
			if i+1 >= n {
				return 0, "unterminated f-string"
			}
			d := src[i+1]
			switch {
			case d == '{' || d == '}':
				// not an escape: the backslash is a literal character and the brace keeps its meaning
				if headOpen && keep {
					head = append(head, '\\')
				}
				i++
			case d == '\r' && i+2 < n && src[i+2] == '\n':
				*line++
				i += 3
			case d == '\n':
				*line++
				i += 2
			default:
				if headOpen && keep {
					if raw {
						head = append(head, c, d)
					} else {
						head = appendEscape(head, d)
					}
				}
				i += 2
			}
		case c == quote && (!triple || i+2 < n && src[i+1] == quote && src[i+2] == quote):
			if triple {
				i += 3
			} else {
				i++
			}
			if top {
				if keep {
					info.val = string(head)
				} else {
					info.cut = true
				}
			}
			return i, ""
		case c == '{':
			if i+1 < n && src[i+1] == '{' {
				if headOpen && keep {
					head = append(head, '{')
				}
				i += 2
				continue
			}
			if top {
				info.field = true
				headOpen = false
			}
			e, why := lx.scanField(i+1, triple, line, depth)
			if why != "" {
				return 0, why
			}
			i = e
		case c == '}':
			if headOpen && keep {
				head = append(head, '}')
			}
			if i+1 < n && src[i+1] == '}' {
				i += 2
			} else {
				i++
			}
		case c == '\n':
			if !triple {
				return 0, "unterminated f-string"
			}
			*line++
			if headOpen && keep {
				head = append(head, c)
			}
			i++
		default:
			if headOpen && keep {
				head = append(head, c)
			}
			i++
		}
		if keep && len(head) > maxStrKeep {
			keep, head = false, nil
		}
	}
}

// scanField scans a replacement field starting just after its `{` and returns the offset just past its `}`.
func (lx *lexed) scanField(i int, outerTriple bool, line *int32, depth int) (int, string) {
	if depth > maxFDepth {
		return 0, "f-string nested too deeply"
	}
	src := lx.src
	n := len(src)
	br := 0
	for i < n {
		c := src[i]
		switch {
		case c == '"' || c == '\'':
			// A nested string. Its prefix letters (at most two) sit right before the quote.
			j := i
			for j > 0 && j > i-2 && isIdentCont(src[j-1]) {
				j--
			}
			ok, raw, f, _ := stringPrefix(src[j:i])
			if !ok {
				raw, f = false, false
			}
			if f {
				e, why := lx.scanFNested(i, raw, line, depth+1)
				if why != "" {
					return 0, why
				}
				i = e
			} else {
				e, _, ok, why := lx.lexString(i, raw, false, false, line)
				if !ok {
					return 0, why
				}
				lx.strs = lx.strs[:len(lx.strs)-1] // a string inside a field is not a token of its own
				i = e
			}
		case c == '(' || c == '[' || c == '{':
			br++
			i++
		case c == ')' || c == ']':
			if br > 0 {
				br--
			}
			i++
		case c == '}':
			if br == 0 {
				return i + 1, ""
			}
			br--
			i++
		case c == ':' && br == 0:
			// Format spec: literal text with nested replacement fields, up to the closing brace.
			i++
			for i < n {
				d := src[i]
				switch {
				case d == '{':
					e, why := lx.scanField(i+1, outerTriple, line, depth+1)
					if why != "" {
						return 0, why
					}
					i = e
				case d == '}':
					return i + 1, ""
				case d == '\n':
					*line++
					i++
					if !outerTriple {
						return 0, "unterminated f-string"
					}
				default:
					i++
				}
			}
			return 0, "unterminated f-string"
		case c == '!' && i+1 < n && src[i+1] == '=':
			i += 2
		case c == '\\':
			if i+1 < n && src[i+1] == '\n' {
				*line++
			}
			i += 2
		case c == '\n':
			*line++
			i++
		default:
			i++
		}
	}
	return 0, "unterminated f-string"
}

// scanFNested scans an f-string literal nested inside a replacement field; i is its opening quote.
func (lx *lexed) scanFNested(i int, raw bool, line *int32, depth int) (int, string) {
	src := lx.src
	q := src[i]
	triple := false
	i++
	if i+1 < len(src) && src[i] == q && src[i+1] == q {
		triple = true
		i += 2
	}
	var tmp strInfo
	return lx.scanF(i, q, triple, raw, line, &tmp, depth, false)
}

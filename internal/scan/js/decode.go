package js

import (
	"bytes"
	"unicode/utf8"
)

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexVal(c byte) rune {
	switch {
	case c >= '0' && c <= '9':
		return rune(c - '0')
	case c >= 'a' && c <= 'f':
		return rune(c-'a') + 10
	}
	return rune(c-'A') + 10
}

func isAlnum(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// parseU reads \u... (raw[i] is the character after 'u'); returns the rune and the number of bytes consumed.
func parseU(raw []byte, i int) (rune, int, bool) {
	if i < len(raw) && raw[i] == '{' {
		j := i + 1
		var r rune
		d := 0
		for j < len(raw) && isHex(raw[j]) {
			r = r<<4 | hexVal(raw[j])
			if r > 0x10FFFF {
				return 0, 0, false
			}
			j++
			d++
		}
		if d == 0 || j >= len(raw) || raw[j] != '}' {
			return 0, 0, false
		}
		return r, j + 1 - i, true
	}
	if i+4 > len(raw) {
		return 0, 0, false
	}
	var r rune
	for k := 0; k < 4; k++ {
		if !isHex(raw[i+k]) {
			return 0, 0, false
		}
		r = r<<4 | hexVal(raw[i+k])
	}
	return r, 4, true
}

// decode decodes string/template content (without the quotes). ok=false on an unknown escape (cannot be decoded with certainty).
func decode(raw []byte) (string, bool) {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw), true
	}
	b := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); {
		c := raw[i]
		if c != '\\' {
			b = append(b, c)
			i++
			continue
		}
		i++
		if i >= len(raw) {
			return "", false
		}
		e := raw[i]
		i++
		switch e {
		case 'n':
			b = append(b, '\n')
		case 't':
			b = append(b, '\t')
		case 'r':
			b = append(b, '\r')
		case 'b':
			b = append(b, '\b')
		case 'f':
			b = append(b, '\f')
		case 'v':
			b = append(b, '\v')
		case '0':
			if i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
				return "", false
			}
			b = append(b, 0)
		case '1', '2', '3', '4', '5', '6', '7', '8', '9':
			return "", false
		case 'x':
			if i+2 > len(raw) || !isHex(raw[i]) || !isHex(raw[i+1]) {
				return "", false
			}
			b = utf8.AppendRune(b, hexVal(raw[i])<<4|hexVal(raw[i+1]))
			i += 2
		case 'u':
			r, k, ok := parseU(raw, i)
			if !ok {
				return "", false
			}
			i += k
			if r >= 0xD800 && r <= 0xDBFF {
				// surrogate pair: must be followed by \uDC00-DFFF
				if i+2 <= len(raw) && raw[i] == '\\' && raw[i+1] == 'u' {
					r2, k2, ok2 := parseU(raw, i+2)
					if ok2 && r2 >= 0xDC00 && r2 <= 0xDFFF {
						i += 2 + k2
						r = 0x10000 + (r-0xD800)<<10 + (r2 - 0xDC00)
						b = utf8.AppendRune(b, r)
						continue
					}
				}
				return "", false
			}
			if r >= 0xDC00 && r <= 0xDFFF {
				return "", false
			}
			b = utf8.AppendRune(b, r)
		case '\n':
			// line continuation
		case '\r':
			if i < len(raw) && raw[i] == '\n' {
				i++
			}
		default:
			if isAlnum(e) {
				return "", false
			}
			b = append(b, e)
		}
	}
	return string(b), true
}

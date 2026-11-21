package sfc

import "bytes"

// scanMDX: top-level ESM blocks. A line starting at column 0 (outside a code fence) with import/export begins a block,
// which extends to the next blank line; each block is scanned as JS with JSX. YAML/TOML front matter at the start of the file is skipped.
func scanMDX(a *acc) {
	src := a.src
	n := len(src)
	pos := 0
	first := true
	fence := byte(0)
	fenceLen := 0
	for pos < n && !a.stopped() {
		le := bytes.IndexByte(src[pos:], '\n')
		end, next := n, n
		if le >= 0 {
			end, next = pos+le, pos+le+1
		}
		ln := src[pos:end]
		trim := bytes.TrimRight(ln, " \t\r")

		if first {
			first = false
			if string(trim) == "---" || string(trim) == "+++" {
				mark := string(trim)
				j := next
				found := false
				for j < n {
					e := bytes.IndexByte(src[j:], '\n')
					le2, nx := n, n
					if e >= 0 {
						le2, nx = j+e, j+e+1
					}
					if string(bytes.TrimRight(src[j:le2], " \t\r")) == mark {
						found = true
						next = nx
						break
					}
					j = nx
				}
				if !found {
					a.fail("mdx", "unclosed_frontmatter: front matter opened at line 1 has no closing "+mark)
					return
				}
				pos = next
				continue
			}
		}

		if fence != 0 {
			// inside a code fence: find the closing line (at most 3 spaces of indent, same character, enough length, no other text)
			if c, l, rest := fenceStart(trim); c == fence && l >= fenceLen && len(bytes.TrimSpace(rest)) == 0 {
				fence = 0
			}
			pos = next
			continue
		}
		if c, l, _ := fenceStart(trim); c != 0 {
			fence, fenceLen = c, l
			pos = next
			continue
		}
		if esmStart(trim) {
			start := pos
			bend := end
			p := next
			for p < n {
				e := bytes.IndexByte(src[p:], '\n')
				le2, nx := n, n
				if e >= 0 {
					le2, nx = p+e, p+e+1
				}
				if len(bytes.TrimSpace(src[p:le2])) == 0 {
					break
				}
				bend = le2
				p = nx
			}
			a.scanJS("mdx esm", src[start:bend], ".jsx", start)
			pos = p
			continue
		}
		pos = next
	}
	// unclosed code fence up to end of file: per CommonMark it is still code to the end of the document, so no import is missed.
}

// fenceStart recognizes an opening/closing code fence line: at most 3 spaces of indent then >=3 ` or ~ characters.
func fenceStart(l []byte) (byte, int, []byte) {
	i := 0
	for i < len(l) && i < 4 && l[i] == ' ' {
		i++
	}
	if i > 3 || i >= len(l) || (l[i] != '`' && l[i] != '~') {
		return 0, 0, nil
	}
	c := l[i]
	j := i
	for j < len(l) && l[j] == c {
		j++
	}
	if j-i < 3 {
		return 0, 0, nil
	}
	if c == '`' && bytes.IndexByte(l[j:], '`') >= 0 { // the info string of a ` fence must not contain `
		return 0, 0, nil
	}
	return c, j - i, l[j:]
}

// esmStart: a line starting at column 0 with "import"/"export" followed by whitespace, '{', '*' or a quote.
func esmStart(l []byte) bool {
	for _, kw := range []string{"import", "export"} {
		if len(l) > len(kw) && string(l[:len(kw)]) == kw {
			switch l[len(kw)] {
			case ' ', '\t', '{', '*', '"', '\'':
				return true
			}
		}
	}
	return false
}

package ruby

import (
	"strconv"
	"strings"
)

// harmlessGemspec: Gem::Specification attributes that cannot change what a test run loads.
var harmlessGemspec = map[string]bool{"summary": true, "description": true, "homepage": true, "authors": true, "author": true,
	"email": true, "metadata": true, "license": true, "licenses": true, "post_install_message": true, "rdoc_options": true,
	"extra_rdoc_files": true}

// GemspecFingerprint returns a normalized form of a gemspec: its tokens (comments and layout dropped), without the statements
// that assign a harmless attribute (`s.summary = ...`, `s.metadata["x"] = ...`, `s.authors << ...`). Two gemspecs with the same
// fingerprint differ only in harmless attributes. ok=false when the file cannot be lexed reliably.
func GemspecFingerprint(src []byte) (string, bool) {
	lx, why := lex(src)
	if why != "" {
		return "", false
	}
	s := &scanner{src: src, strs: lx.strs}
	var b strings.Builder
	s.fingerprint(&b, lx.toks, true)
	return b.String(), true
}

func (s *scanner) fingerprint(b *strings.Builder, ts []token, top bool) {
	for i := 0; i < len(ts); i++ {
		if top && ts[i].kind == tIdent && s.stmtStart(ts, i) {
			if e := s.harmlessAssign(ts, i); e > i {
				i = e - 1
				continue
			}
		}
		t := ts[i]
		switch t.kind {
		case tNL:
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
				b.WriteString("\n")
			}
			continue
		case tStr:
			s.writeStr(b, t)
		case tSym:
			if t.aux >= 0 {
				b.WriteString(":")
				s.writeStr(b, t)
			} else {
				b.WriteString(s.text(t))
			}
		default:
			b.WriteString(s.text(t))
		}
		b.WriteString(" ")
	}
}

func (s *scanner) writeStr(b *strings.Builder, t token) {
	si := &s.strs[t.aux]
	b.WriteString("<s" + strconv.Itoa(int(si.kind)) + " ")
	for _, p := range si.parts {
		if p.isCode {
			b.WriteString("#{")
			s.fingerprint(b, p.code, false)
			b.WriteString("}")
			continue
		}
		b.WriteString(strconv.Quote(p.lit))
	}
	b.WriteString(">")
}

// harmlessAssign returns the end of a statement `recv.attr = ...` / `recv.attr << ...` / `recv.metadata[k] = ...` assigning a
// harmless attribute at i, or i.
func (s *scanner) harmlessAssign(ts []token, i int) int {
	if !s.isOp(ts, i+1, ".") || i+2 >= len(ts) || ts[i+2].kind != tIdent || !harmlessGemspec[s.text(ts[i+2])] {
		return i
	}
	j := i + 3
	if s.text(ts[i+2]) == "metadata" && s.isOp(ts, j, "[") {
		c := s.closeOf(ts, j, len(ts))
		if c < 0 {
			return i
		}
		j = c + 1
	}
	if !(s.isOp(ts, j, "=") || s.isOp(ts, j, "<<") || s.isOp(ts, j, "+=") || s.isOp(ts, j, "||=")) {
		return i
	}
	e := s.cmdEnd(ts, j+1)
	// A `{` right after the operator is a hash literal, not a block.
	if e == j+1 && s.isOp(ts, j+1, "{") {
		c := s.closeOf(ts, j+1, len(ts))
		if c < 0 {
			return i
		}
		e = s.cmdEnd(ts, c+1)
		if e < c+1 {
			e = c + 1
		}
	}
	// Only a whole statement is dropped: the next token must end it.
	if e < len(ts) && ts[e].kind != tNL && !s.isOp(ts, e, ";") && !s.isIdent(ts, e, "end") {
		return i
	}
	return e
}

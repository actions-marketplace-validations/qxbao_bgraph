package ruby

// TokenKind is the kind of a Token.
type TokenKind uint8

// Token kinds (the lexer's own, see tokKind).
const (
	TokNL    TokenKind = TokenKind(tNL)
	TokIdent TokenKind = TokenKind(tIdent) // identifier, keyword, constant or method name (operator names after def / . / :: too)
	TokIVar  TokenKind = TokenKind(tIVar)
	TokGVar  TokenKind = TokenKind(tGVar)
	TokSym   TokenKind = TokenKind(tSym)
	TokStr   TokenKind = TokenKind(tStr) // any string-like literal (strings, regexes, %-literals, heredocs, ?c)
	TokNum   TokenKind = TokenKind(tNum)
	TokOp    TokenKind = TokenKind(tOp)
)

// Token is one top-level token of a Ruby file, as the dynamic layer's body guard (internal/dynamic/rbdyn) reads it. Tokens
// inside string interpolations are part of their literal's Text, not separate tokens.
type Token struct {
	Kind TokenKind
	// Text is the token's source text. A literal's text spans its whole source (interpolations included); a heredoc's is its
	// opener (<<~ID) followed by "\x00" and the raw body, so that a change to the body changes the token.
	Text  string
	Line  int  // line of the token's first byte (1-based)
	Space bool // white space (or a line continuation) right before the token
	// Heredoc: the token opens a heredoc whose body lies on the lines after it.
	Heredoc bool
}

// Tokens lexes src with the scanner's lexer. data is the byte offset of the __END__ line (-1 when the file has none: nothing
// after it is code). why is non-empty when the lexer cannot read the file with certainty (the reason Scan marks it uncertain);
// the tokens are then nil.
func Tokens(src []byte) (toks []Token, data int, why string) {
	lx, why := lex(src)
	if why != "" {
		return nil, -1, why
	}
	toks = make([]Token, 0, len(lx.toks))
	for _, t := range lx.toks {
		tk := Token{Kind: TokenKind(t.kind), Text: string(src[t.start:t.end]), Line: int(t.line), Space: t.sp}
		if t.kind == tStr && t.aux >= 0 && int(t.aux) < len(lx.strs) && lx.strs[t.aux].kind == sHeredoc {
			b := lx.strs[t.aux].body
			tk.Heredoc = true
			tk.Text += "\x00" + string(src[b[0]:b[1]])
		}
		toks = append(toks, tk)
	}
	return toks, lx.data, ""
}

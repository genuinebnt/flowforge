package jsparse

// Automatic semicolon insertion (ECMA-262 12.10) and error recovery.
//
// The restricted productions ([no LineTerminator here]) are checked where
// they occur: postfix ++/-- (parsePostfix), continue/break labels
// (parseBreakContinue), return and throw (parseReturn, parseThrow), yield
// (parseYield), arrow `=>` (parsePrimary, parseParenOrArrow) and `async`
// (parseAsyncPrimary, parseStatementListItem). The do-while rule of
// 12.10.1 is in parseDoWhile.

// canInsertSemicolon reports whether a semicolon may be inserted before
// the current token: it is `}`, the end of input, or is preceded by a
// LineTerminator.
func (p *parser) canInsertSemicolon() bool {
	return p.at(TSemicolon) || p.at(TRBrace) || p.at(TEOF) || p.newlineBefore()
}

// semicolon consumes the `;` that ends a statement, or inserts one.
func (p *parser) semicolon() {
	if p.eat(TSemicolon) {
		return
	}
	if p.at(TRBrace) || p.at(TEOF) || p.newlineBefore() {
		return
	}
	p.syntaxError(CodeUnexpectedToken, "missing semicolon before "+p.tokText())
}

// startsStatement reports whether t can begin a statement; resync stops
// before such a token at the start of a line (where ASI would also end the
// damaged statement). Operators and closing brackets do not count, so a
// continuation line is still skipped.
func startsStatement(t *Token) bool {
	switch t.Kind {
	case TIdentifier:
		if t.Flags&FlagEscaped != 0 {
			return true
		}
		switch t.Kw {
		case KwIn, KwInstanceof, KwElse, KwCase, KwDefault, KwCatch, KwFinally, KwExtends:
			return false
		}
		return true
	case TString, TNumeric, TBigInt, TNoSubstitutionTemplate, TTemplateHead, TPrivateName, TLBrace, TLBracket, TLParen:
		return true
	}
	return false
}

// resync is tolerant-mode recovery after a halt: it re-lexes from the
// token where the error occurred and skips tokens, balancing brackets and
// template substitutions, until a statement boundary: a `;` (consumed), a
// `}` that closes the enclosing block (not consumed), or a token that can
// start a statement at the start of a line. A boundary at stmtStart itself (the
// start of the statement that failed) does not count, so every recovery
// makes progress.
func (p *parser) resync(stmtStart int32) {
	p.halted = false
	p.havePeek = false
	p.lex.rewindTo(&p.haltTok)
	p.skipping = true
	defer func() { p.skipping = false }()
	p.tok = p.lex.Next(GoalDiv)
	p.checkLexError()
	depth := 0
	var tmpl []int
	for i := 0; i < p.haltTmpl; i++ {
		tmpl = append(tmpl, 0)
	}
	for !p.at(TEOF) {
		t := &p.tok
		if depth == 0 && t.Start > stmtStart {
			switch {
			case t.Kind == TSemicolon:
				p.next()
				return
			case t.Kind == TRBrace && len(tmpl) == 0:
				return
			case t.Flags&FlagNewlineBefore != 0 && len(tmpl) == 0 && startsStatement(t):
				return
			}
		}
		switch t.Kind {
		case TLBrace, TLParen, TLBracket:
			depth++
		case TTemplateHead:
			tmpl = append(tmpl, depth)
		case TRBrace:
			if n := len(tmpl); n > 0 && tmpl[n-1] == depth {
				p.regoalTemplateTail()
				if p.at(TTemplateTail) {
					tmpl = tmpl[:n-1]
				}
				p.next()
				continue
			}
			if depth > 0 {
				depth--
			}
		case TRParen, TRBracket:
			if depth > 0 {
				depth--
			}
		}
		p.next()
	}
}

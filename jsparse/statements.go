package jsparse

// Statements and declarations (ECMA-262 clause 14).

// parseDirectives parses the Directive Prologue (11.2.1) of a function
// body or the program, pushing its statements, and switches to strict
// mode on a Use Strict Directive. It reports whether one was present.
func (p *parser) parseDirectives(info *fnInfo) bool {
	useStrict := false
	octal := Span{-1, -1}
	for p.at(TString) && !p.halted {
		t := p.tok
		stmt := p.parseStatementListItem(false)
		p.push(stmt)
		n := p.nd(stmt)
		if n.Kind != KExpressionStatement || n.A == NoNode {
			break
		}
		e := p.nd(n.A)
		if e.Kind != KLiteral || e.Op != LitString || e.Flags&NParenthesized != 0 || e.Span.Start != t.Start || e.Span.End != t.End {
			break
		}
		raw := p.src[t.Start+1 : t.End-1]
		n = p.nd(stmt)
		n.Flags |= NDirective
		n.Str = raw
		if t.Flags&FlagLegacyOctal != 0 && octal.Start < 0 {
			octal = t.Span()
		}
		if raw == "use strict" {
			useStrict = true
			if !p.strict() {
				p.cur().strict = true
				if info != nil {
					p.strictParamChecks(info)
				}
				if octal.Start >= 0 {
					p.early(CodeStrictOctal, octal, "octal escape sequences are not allowed in strict mode")
				}
			}
		}
	}
	return useStrict
}

// parseStatementList parses StatementListItems until end, recovering at
// statement boundaries in tolerant mode.
func (p *parser) parseStatementList(end TokenKind, topLevel bool) {
	last := int32(-1)
	for {
		if p.halted {
			if !p.opts.Tolerant {
				return
			}
			p.resync(last)
		}
		if p.at(end) || p.at(TEOF) {
			return
		}
		if p.tok.Start == last {
			// No progress since the last item (tolerant mode): drop a token.
			p.next()
			continue
		}
		last = p.tok.Start
		p.push(p.parseStatementListItem(topLevel))
	}
}

// parseStatementListItem parses StatementListItem (or ModuleItem when
// topLevel in a module).
func (p *parser) parseStatementListItem(topLevel bool) NodeID {
	start := p.start()
	if p.tok.Kind == TIdentifier && p.tok.Flags&FlagEscaped == 0 {
		switch p.tok.Kw {
		case KwFunction:
			p.clearDirectLabels()
			return p.parseFunction(start, false, true, false)
		case KwClass:
			p.clearDirectLabels()
			return p.parseClass(start, true, false)
		case KwConst:
			return p.parseLexicalStatement(OpConst)
		case KwLet:
			if p.isLetDeclaration() {
				return p.parseLexicalStatement(OpLet)
			}
		case KwAsync:
			if pk := p.peek(); isKwTok(pk, KwFunction) && pk.Flags&FlagNewlineBefore == 0 {
				p.clearDirectLabels()
				p.next()
				return p.parseFunction(start, true, true, false)
			}
		case KwImport:
			if pk := p.peek(); pk.Kind != TLParen && pk.Kind != TDot {
				if !topLevel || !p.module {
					p.syntaxError(CodeModuleItem, "import declarations may only appear at the top level of a module")
					return p.errNode()
				}
				return p.parseImportDeclaration()
			}
		case KwExport:
			if !topLevel || !p.module {
				p.syntaxError(CodeModuleItem, "export declarations may only appear at the top level of a module")
				return p.errNode()
			}
			return p.parseExportDeclaration()
		}
	}
	return p.parseStatement()
}

// isLetDeclaration decides whether `let` starts a LexicalDeclaration: it
// does when followed by an identifier, `[` or `{`.
func (p *parser) isLetDeclaration() bool {
	pk := p.peek()
	switch pk.Kind {
	case TLBracket, TLBrace:
		return true
	case TIdentifier:
		// `let in x`, `let instanceof y`: let is an identifier.
		return !pk.Kw.isReserved() || pk.Kw == KwYield || pk.Kw == KwAwait
	}
	return false
}

func (p *parser) parseLexicalStatement(kind Op) NodeID {
	p.clearDirectLabels()
	id := p.parseVarDecl(kind, false, false)
	p.semicolon()
	return p.done(id)
}

// parseStatement parses Statement (no declarations except where Annex B
// allows functions).
func (p *parser) parseStatement() NodeID {
	if !p.enter() {
		p.leave()
		return p.errNode()
	}
	defer p.leave()
	start := p.start()
	t := &p.tok
	if t.Kind == TIdentifier && t.Flags&FlagEscaped == 0 {
		switch t.Kw {
		case KwVar:
			p.clearDirectLabels()
			id := p.parseVarDecl(OpVar, false, false)
			p.semicolon()
			return p.done(id)
		case KwIf:
			p.clearDirectLabels()
			return p.parseIf()
		case KwFor:
			p.markLoopLabels()
			p.clearDirectLabels()
			return p.parseFor()
		case KwWhile:
			p.markLoopLabels()
			p.clearDirectLabels()
			return p.parseWhile()
		case KwDo:
			p.markLoopLabels()
			p.clearDirectLabels()
			return p.parseDoWhile()
		case KwContinue, KwBreak:
			p.clearDirectLabels()
			return p.parseBreakContinue()
		case KwReturn:
			p.clearDirectLabels()
			return p.parseReturn()
		case KwWith:
			p.clearDirectLabels()
			return p.parseWith()
		case KwSwitch:
			p.clearDirectLabels()
			return p.parseSwitch()
		case KwThrow:
			p.clearDirectLabels()
			return p.parseThrow()
		case KwTry:
			p.clearDirectLabels()
			return p.parseTry()
		case KwDebugger:
			p.clearDirectLabels()
			p.next()
			p.semicolon()
			return p.done(p.newNode(KDebuggerStatement, start))
		case KwFunction:
			p.syntaxError(CodeFunctionPosition, "function declarations are not allowed here")
			return p.errNode()
		case KwClass, KwConst:
			p.syntaxError(CodeUnexpectedToken, "declarations are not allowed here")
			return p.errNode()
		case KwLet:
			if pk := p.peek(); pk.Kind == TLBracket {
				p.syntaxError(CodeUnexpectedToken, "lexical declarations are not allowed here")
				return p.errNode()
			}
		case KwAsync:
			if pk := p.peek(); isKwTok(pk, KwFunction) && pk.Flags&FlagNewlineBefore == 0 {
				p.syntaxError(CodeFunctionPosition, "async function declarations are not allowed here")
				return p.errNode()
			}
		case KwImport:
			if pk := p.peek(); pk.Kind != TLParen && pk.Kind != TDot {
				p.syntaxError(CodeModuleItem, "import declarations may only appear at the top level of a module")
				return p.errNode()
			}
		case KwExport:
			p.syntaxError(CodeModuleItem, "export declarations may only appear at the top level of a module")
			return p.errNode()
		}
	}
	switch t.Kind {
	case TLBrace:
		p.clearDirectLabels()
		return p.parseBlock(true)
	case TSemicolon:
		p.clearDirectLabels()
		p.next()
		return p.done(p.newNode(KEmptyStatement, start))
	case TIdentifier:
		if isIdentifierToken(t) {
			if pk := p.peek(); pk.Kind == TColon {
				return p.parseLabelled()
			}
		}
	}
	p.clearDirectLabels()
	expr := p.parseExpression(false)
	p.semicolon()
	id := p.newNode(KExpressionStatement, start)
	p.nd(id).A = expr
	return p.done(id)
}

// parseBlock parses Block; withScope is false for catch bodies, which
// share the catch parameter's scope.
func (p *parser) parseBlock(withScope bool) NodeID {
	start := p.start()
	p.expect(TLBrace)
	if withScope {
		p.pushScope(0)
	}
	mark := len(p.scratch)
	p.parseStatementList(TRBrace, false)
	p.expect(TRBrace)
	if withScope {
		p.popScope()
	}
	id := p.newNode(KBlockStatement, start)
	p.nd(id).List = p.list(mark)
	return p.done(id)
}

// parseVarDecl parses VariableStatement / LexicalDeclaration / the
// declaration of a for head (forHead: initializers are checked by the
// caller).
func (p *parser) parseVarDecl(kind Op, noIn, forHead bool) NodeID {
	start := p.start()
	p.next()
	bk := bindVar
	if kind != OpVar {
		bk = bindLexical
	}
	mark := len(p.scratch)
	for !p.halted {
		dstart := p.start()
		target := p.parseBindingTarget(bk)
		init := NoNode
		if p.eat(TEq) {
			init = p.parseAssignment(noIn, false)
		} else if !forHead || !(p.isKw(KwIn) || p.isKw(KwOf)) {
			if kind == OpConst {
				p.syntaxError(CodeExpectedToken, "missing initializer in const declaration")
			} else if p.nd(target).Kind != KIdentifier {
				p.syntaxError(CodeExpectedToken, "missing initializer in destructuring declaration")
			}
		}
		d := p.newNode(KVariableDeclarator, dstart)
		n := p.nd(d)
		n.A, n.B = target, init
		p.push(p.done(d))
		if !p.eat(TComma) {
			break
		}
	}
	id := p.newNode(KVariableDeclaration, start)
	n := p.nd(id)
	n.Op = kind
	n.List = p.list(mark)
	return p.done(id)
}

func (p *parser) parseIf() NodeID {
	start := p.start()
	p.next()
	p.expect(TLParen)
	test := p.parseExpression(false)
	p.expect(TRParen)
	cons := p.parseIfBody()
	alt := NoNode
	if p.isKw(KwElse) {
		p.next()
		alt = p.parseIfBody()
	}
	id := p.newNode(KIfStatement, start)
	n := p.nd(id)
	n.A, n.B, n.C = test, cons, alt
	return p.done(id)
}

// parseIfBody parses the Statement of an if; Annex B.3.4 allows a plain
// function declaration there in sloppy mode.
func (p *parser) parseIfBody() NodeID {
	if p.isKw(KwFunction) && !p.strict() {
		pk := p.peek()
		if pk.Kind == TStar {
			p.syntaxError(CodeFunctionPosition, "generator declarations are not allowed here")
			return p.errNode()
		}
		p.pushScope(0)
		id := p.parseFunction(p.start(), false, true, false)
		p.popScope()
		return id
	}
	body := p.parseStatement()
	p.checkLabelledFunction(body)
	return body
}

// checkLabelledFunction reports IsLabelledFunction(stmt) for bodies of if
// and iteration statements.
func (p *parser) checkLabelledFunction(stmt NodeID) {
	for stmt != NoNode && p.nd(stmt).Kind == KLabeledStatement {
		stmt = p.nd(stmt).B
		if stmt != NoNode && p.nd(stmt).Kind == KFunctionDeclaration {
			p.early(CodeLabelledFunction, p.nd(stmt).Span, "labelled function declarations are not allowed here")
			return
		}
	}
}

// parseLoopBody parses the Statement of an iteration statement.
func (p *parser) parseLoopBody() NodeID {
	c := p.cur()
	c.loopDepth++
	c.brkDepth++
	body := p.parseStatement()
	c = p.cur()
	c.loopDepth--
	c.brkDepth--
	p.checkLabelledFunction(body)
	return body
}

func (p *parser) parseWhile() NodeID {
	start := p.start()
	p.next()
	p.expect(TLParen)
	test := p.parseExpression(false)
	p.expect(TRParen)
	body := p.parseLoopBody()
	id := p.newNode(KWhileStatement, start)
	n := p.nd(id)
	n.A, n.B = test, body
	return p.done(id)
}

func (p *parser) parseDoWhile() NodeID {
	start := p.start()
	p.next()
	body := p.parseLoopBody()
	if !p.isKw(KwWhile) {
		p.unexpectedExpected(TIdentifier)
	}
	p.next()
	p.expect(TLParen)
	test := p.parseExpression(false)
	p.expect(TRParen)
	// 12.10.1: a semicolon is inserted after do-while if missing.
	p.eat(TSemicolon)
	id := p.newNode(KDoWhileStatement, start)
	n := p.nd(id)
	n.A, n.B = body, test
	return p.done(id)
}

// parseFor parses for, for-in, for-of and for-await-of (14.7.4-14.7.5).
func (p *parser) parseFor() NodeID {
	start := p.start()
	p.next()
	isAwait := false
	if p.isKw(KwAwait) && p.cur().async && !p.cur().awaitIdent {
		if p.inStaticBlock() {
			p.early(CodeAwaitInStaticBlock, p.tok.Span(), "await in class static block")
		}
		isAwait = true
		p.next()
	}
	p.expect(TLParen)
	p.pushScope(0)
	defer p.popScope()
	init := NoNode
	if p.at(TSemicolon) {
		if isAwait {
			p.unexpected()
		}
		return p.parseForRest(start, NoNode)
	}
	declKind := OpNone
	switch {
	case p.isKw(KwVar):
		declKind = OpVar
	case p.isKw(KwConst):
		declKind = OpConst
	case p.isKw(KwLet):
		if pk := p.peek(); pk.Kind == TLBracket || pk.Kind == TLBrace || (pk.Kind == TIdentifier && (!pk.Kw.isReserved() || pk.Kw == KwYield || pk.Kw == KwAwait)) {
			declKind = OpLet
		}
	}
	if declKind != OpNone {
		init = p.parseVarDecl(declKind, true, true)
		decls := p.ids(p.nd(init).List)
		if p.isKw(KwOf) || p.isKw(KwIn) {
			isOf := p.isKw(KwOf)
			if len(decls) != 1 {
				p.syntaxError(CodeForInOfHead, "only one binding is allowed in a for-in/of head")
			} else if d := p.nd(decls[0]); d.B != NoNode {
				// B.3.5: for (var x = e in o) in sloppy mode with a simple binding.
				if isOf || declKind != OpVar || p.strict() || p.nd(d.A).Kind != KIdentifier {
					p.syntaxError(CodeForInOfHead, "for-in/of declarations may not have initializers")
				}
			}
			if isOf && declKind == OpVar {
				// var bindings of for-of are not exempt from the catch rule (B.3.5).
				for _, b := range p.boundNamesOf(nil, init) {
					p.declare(b.name, bindVarForOf, b.span)
				}
			}
			return p.parseForInOf(start, init, isOf, isAwait)
		}
		for _, d := range decls {
			dn := p.nd(d)
			if dn.B == NoNode && (declKind == OpConst || p.nd(dn.A).Kind != KIdentifier) {
				p.syntaxErrorAt(CodeExpectedToken, dn.Span, "missing initializer")
			}
		}
		if isAwait {
			p.unexpected()
		}
		return p.parseForRest(start, init)
	}
	initStart := p.start()
	letIdent := p.isKw(KwLet)
	asyncIdent := p.isKw(KwAsync)
	init = p.parseExpressionCover(true, true)
	if p.isKw(KwOf) || p.isKw(KwIn) {
		isOf := p.isKw(KwOf)
		in := p.nd(init)
		if isOf && letIdent {
			p.syntaxError(CodeForInOfHead, "for-of head may not start with let")
		}
		if isOf && asyncIdent && !isAwait && in.Kind == KIdentifier && in.Str == "async" && in.Flags&NParenthesized == 0 {
			p.syntaxError(CodeForInOfHead, "for-of head may not start with async of")
		}
		p.toAssignTarget(init)
		return p.parseForInOf(start, init, isOf, isAwait)
	}
	p.flushCover(initStart)
	if isAwait {
		p.unexpected()
	}
	return p.parseForRest(start, init)
}

// parseForRest parses `; test ; update ) body` of a C-style for.
func (p *parser) parseForRest(start int32, init NodeID) NodeID {
	p.expect(TSemicolon)
	test, update := NoNode, NoNode
	if !p.at(TSemicolon) {
		test = p.parseExpression(false)
	}
	p.expect(TSemicolon)
	if !p.at(TRParen) {
		update = p.parseExpression(false)
	}
	p.expect(TRParen)
	body := p.parseLoopBody()
	id := p.newNode(KForStatement, start)
	n := p.nd(id)
	n.A, n.B, n.C, n.D = init, test, update, body
	return p.done(id)
}

func (p *parser) parseForInOf(start int32, left NodeID, isOf, isAwait bool) NodeID {
	p.next() // in / of
	if isAwait && !isOf {
		p.syntaxError(CodeForInOfHead, "for await requires of")
	}
	var right NodeID
	if isOf {
		right = p.parseAssignment(false, false)
	} else {
		right = p.parseExpression(false)
	}
	p.expect(TRParen)
	body := p.parseLoopBody()
	kind := KForInStatement
	if isOf {
		kind = KForOfStatement
	}
	id := p.newNode(kind, start)
	n := p.nd(id)
	n.A, n.B, n.C = left, right, body
	if isAwait {
		n.Flags |= NAwait
	}
	return p.done(id)
}

func (p *parser) parseBreakContinue() NodeID {
	start := p.start()
	isBreak := p.isKw(KwBreak)
	kwSpan := p.tok.Span()
	p.next()
	label := NoNode
	if p.at(TIdentifier) && !p.newlineBefore() && isIdentifierToken(&p.tok) {
		t := p.tok
		p.checkIdentifierReference(&t)
		label = p.ident(t.Value, t.Span())
		p.next()
		l := p.findLabel(t.Value)
		switch {
		case l == nil:
			p.early(CodeIllegalBreak, t.Span(), "undefined label '"+t.Value+"'")
		case !isBreak && !l.isLoop:
			p.early(CodeIllegalContinue, t.Span(), "continue target is not an iteration statement")
		}
	} else {
		c := p.cur()
		if isBreak && c.brkDepth == 0 {
			p.early(CodeIllegalBreak, kwSpan, "illegal break statement")
		}
		if !isBreak && c.loopDepth == 0 {
			p.early(CodeIllegalContinue, kwSpan, "illegal continue statement")
		}
	}
	p.semicolon()
	kind := KContinueStatement
	if isBreak {
		kind = KBreakStatement
	}
	id := p.newNode(kind, start)
	p.nd(id).A = label
	return p.done(id)
}

func (p *parser) parseReturn() NodeID {
	start := p.start()
	if !p.cur().returnOK {
		p.early(CodeIllegalReturn, p.tok.Span(), "return outside of a function")
	}
	p.next()
	arg := NoNode
	if !p.canInsertSemicolon() {
		arg = p.parseExpression(false)
	}
	p.semicolon()
	id := p.newNode(KReturnStatement, start)
	p.nd(id).A = arg
	return p.done(id)
}

func (p *parser) parseWith() NodeID {
	start := p.start()
	if p.strict() {
		p.early(CodeStrictWith, p.tok.Span(), "with statement in strict mode")
	}
	p.next()
	p.expect(TLParen)
	obj := p.parseExpression(false)
	p.expect(TRParen)
	body := p.parseStatement()
	p.checkLabelledFunction(body)
	id := p.newNode(KWithStatement, start)
	n := p.nd(id)
	n.A, n.B = obj, body
	return p.done(id)
}

func (p *parser) parseSwitch() NodeID {
	start := p.start()
	p.next()
	p.expect(TLParen)
	disc := p.parseExpression(false)
	p.expect(TRParen)
	p.expect(TLBrace)
	p.pushScope(0)
	p.cur().brkDepth++
	mark := len(p.scratch)
	sawDefault := false
	for !p.at(TRBrace) && !p.at(TEOF) {
		cstart := p.start()
		test := NoNode
		switch {
		case p.isKw(KwCase):
			p.next()
			test = p.parseExpression(false)
		case p.isKw(KwDefault):
			if sawDefault {
				p.syntaxError(CodeUnexpectedToken, "multiple default clauses")
			}
			sawDefault = true
			p.next()
		default:
			p.unexpected()
		}
		p.expect(TColon)
		cmark := len(p.scratch)
		for !p.at(TRBrace) && !p.at(TEOF) && !p.isKw(KwCase) && !p.isKw(KwDefault) {
			p.push(p.parseStatementListItem(false))
		}
		cs := p.newNode(KSwitchCase, cstart)
		n := p.nd(cs)
		n.A = test
		n.List = p.list(cmark)
		p.push(p.done(cs))
	}
	p.expect(TRBrace)
	p.cur().brkDepth--
	p.popScope()
	id := p.newNode(KSwitchStatement, start)
	n := p.nd(id)
	n.A = disc
	n.List = p.list(mark)
	return p.done(id)
}

func (p *parser) parseThrow() NodeID {
	start := p.start()
	p.next()
	if p.newlineBefore() {
		p.syntaxError(CodeNewlineForbidden, "illegal newline after throw")
	}
	arg := p.parseExpression(false)
	p.semicolon()
	id := p.newNode(KThrowStatement, start)
	p.nd(id).A = arg
	return p.done(id)
}

func (p *parser) parseTry() NodeID {
	start := p.start()
	p.next()
	block := p.parseBlock(true)
	handler, finalizer := NoNode, NoNode
	if p.isKw(KwCatch) {
		cstart := p.start()
		p.next()
		param := NoNode
		var body NodeID
		if p.eat(TLParen) {
			if p.at(TIdentifier) {
				p.pushScope(scopeSimpleCatch)
				param = p.parseBindingIdentifier(bindSimpleCatch)
			} else {
				p.pushScope(0)
				param = p.parseBindingTarget(bindLexical)
			}
			p.expect(TRParen)
			body = p.parseBlock(false)
			p.popScope()
		} else {
			body = p.parseBlock(true)
		}
		h := p.newNode(KCatchClause, cstart)
		n := p.nd(h)
		n.A, n.B = param, body
		handler = p.done(h)
	}
	if p.isKw(KwFinally) {
		p.next()
		finalizer = p.parseBlock(true)
	}
	if handler == NoNode && finalizer == NoNode {
		p.syntaxError(CodeExpectedToken, "missing catch or finally after try")
	}
	id := p.newNode(KTryStatement, start)
	n := p.nd(id)
	n.A, n.B, n.C = block, handler, finalizer
	return p.done(id)
}

// parseLabelled parses LabelledStatement (14.13).
func (p *parser) parseLabelled() NodeID {
	start := p.start()
	t := p.tok
	p.checkIdentifierReference(&t)
	if p.findLabel(t.Value) != nil {
		p.early(CodeDuplicateLabel, t.Span(), "label '"+t.Value+"' has already been declared")
	}
	labelID := p.ident(t.Value, t.Span())
	p.next()
	p.expect(TColon)
	c := p.cur()
	c.labels = append(c.labels, label{name: t.Value, direct: true})
	var body NodeID
	if p.isKw(KwFunction) {
		p.clearDirectLabels()
		if p.strict() {
			p.early(CodeLabelledFunction, p.tok.Span(), "labelled function declarations are not allowed in strict mode")
		}
		if pk := p.peek(); pk.Kind == TStar {
			p.syntaxError(CodeFunctionPosition, "labelled generator declarations are not allowed")
		}
		body = p.parseFunction(p.start(), false, true, false)
	} else {
		body = p.parseStatement()
	}
	c = p.cur()
	c.labels = c.labels[:len(c.labels)-1]
	id := p.newNode(KLabeledStatement, start)
	n := p.nd(id)
	n.A, n.B = labelID, body
	return p.done(id)
}

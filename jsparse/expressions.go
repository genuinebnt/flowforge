package jsparse

// Expressions (ECMA-262 clause 13). Binary operators use precedence
// climbing; everything else is recursive descent. noIn carries the [~In]
// grammar parameter of for-statement heads; nested brackets reset it.

// errNode returns a placeholder for a production that failed: an empty
// Identifier at the current position.
func (p *parser) errNode() NodeID {
	return p.ident("", Span{p.tok.Start, p.tok.Start})
}

func (p *parser) isBareArrow(id NodeID) bool {
	n := p.nd(id)
	return n.Kind == KArrowFunctionExpression && n.Flags&NParenthesized == 0
}

// parseExpression parses Expression: AssignmentExpression ( ,
// AssignmentExpression )*.
func (p *parser) parseExpression(noIn bool) NodeID {
	return p.parseExpressionCover(noIn, false)
}

// parseExpressionCover parses an Expression whose first item may still be
// reinterpreted as a pattern by the caller (for-in/of heads) when cover.
func (p *parser) parseExpressionCover(noIn, cover bool) NodeID {
	start := p.start()
	e := p.parseAssignment(noIn, cover)
	if !p.at(TComma) {
		return e
	}
	p.flushCover(start)
	mark := len(p.scratch)
	p.push(e)
	for p.eat(TComma) {
		p.push(p.parseAssignment(noIn, false))
	}
	seq := p.newNode(KSequenceExpression, p.nd(e).Span.Start)
	p.nd(seq).List = p.list(mark)
	return p.done(seq)
}

// parseAssignment parses AssignmentExpression. With cover, CoverInitializedName
// errors inside the result stay pending because the caller may still turn
// it into a pattern.
func (p *parser) parseAssignment(noIn, cover bool) NodeID {
	if !p.enter() {
		p.leave()
		return p.errNode()
	}
	defer p.leave()
	if p.isKw(KwYield) && p.cur().generator {
		return p.parseYield(noIn)
	}
	start := p.start()
	left := p.parseConditional(noIn)
	if k := p.tok.Kind; k.isAssignOp() {
		if k == TEq {
			p.toAssignTarget(left)
		} else {
			p.checkSimpleTarget(left)
		}
		p.next()
		right := p.parseAssignment(noIn, false)
		id := p.newNode(KAssignmentExpression, start)
		n := p.nd(id)
		n.Op = assignOps[k-TEq]
		n.A, n.B = left, right
		return p.done(id)
	}
	if !cover {
		p.flushCover(start)
	}
	return left
}

var assignOps = [...]Op{
	TEq - TEq: OpAssign, TPlusEq - TEq: OpAddAssign, TMinusEq - TEq: OpSubAssign, TStarEq - TEq: OpMulAssign,
	TSlashEq - TEq: OpDivAssign, TPercentEq - TEq: OpModAssign, TStarStarEq - TEq: OpExpAssign,
	TShlEq - TEq: OpShlAssign, TShrEq - TEq: OpShrAssign, TUShrEq - TEq: OpUShrAssign, TAmpEq - TEq: OpBitAndAssign,
	TPipeEq - TEq: OpBitOrAssign, TCaretEq - TEq: OpBitXorAssign, TAmpAmpEq - TEq: OpAndAssign,
	TPipePipeEq - TEq: OpOrAssign, TQuestionQuestionEq - TEq: OpCoalesceAssign,
}

// parseYield parses YieldExpression (15.5).
func (p *parser) parseYield(noIn bool) NodeID {
	start := p.start()
	c := p.cur()
	if c.inParams {
		p.early(CodeYieldAwaitInParams, p.tok.Span(), "yield expression in formal parameters")
	}
	c.yieldPos = start
	p.next()
	id := p.newNode(KYieldExpression, start)
	if !p.newlineBefore() {
		if p.at(TStar) {
			p.next()
			p.nd(id).Flags |= NDelegate
			arg := p.parseAssignment(noIn, false)
			p.nd(id).A = arg
			return p.done(id)
		}
		if p.startsExpression() {
			arg := p.parseAssignment(noIn, false)
			p.nd(id).A = arg
		}
	}
	return p.done(id)
}

// startsExpression reports whether the current token can begin an
// AssignmentExpression (used after yield).
func (p *parser) startsExpression() bool {
	switch p.tok.Kind {
	case TIdentifier:
		switch p.tok.Kw {
		case KwIn, KwInstanceof, KwOf:
			return p.tok.Kw == KwOf
		}
		return true
	case TPrivateName, TNumeric, TBigInt, TString, TNoSubstitutionTemplate, TTemplateHead, TRegExp,
		TLParen, TLBracket, TLBrace, TPlus, TMinus, TBang, TTilde, TPlusPlus, TMinusMinus, TSlash, TSlashEq, TLt:
		return true
	}
	return false
}

// parseConditional parses ConditionalExpression.
func (p *parser) parseConditional(noIn bool) NodeID {
	start := p.start()
	test := p.parseBinary(0, noIn)
	if !p.at(TQuestion) || p.isBareArrow(test) {
		return test
	}
	p.flushCover(start)
	p.next()
	cons := p.parseAssignment(false, false)
	p.expect(TColon)
	alt := p.parseAssignment(noIn, false)
	id := p.newNode(KConditionalExpression, start)
	n := p.nd(id)
	n.A, n.B, n.C = test, cons, alt
	return p.done(id)
}

// binaryPrec returns the precedence and operator of the current token as a
// binary operator, or 0.
func (p *parser) binaryPrec(noIn bool) (int, Op) {
	switch p.tok.Kind {
	case TQuestionQuestion:
		return 1, OpCoalesce
	case TPipePipe:
		return 2, OpOr
	case TAmpAmp:
		return 3, OpAnd
	case TPipe:
		return 4, OpBitOr
	case TCaret:
		return 5, OpBitXor
	case TAmp:
		return 6, OpBitAnd
	case TEqEq:
		return 7, OpEq
	case TNotEq:
		return 7, OpNotEq
	case TEqEqEq:
		return 7, OpStrictEq
	case TNotEqEq:
		return 7, OpStrictNe
	case TLt:
		return 8, OpLt
	case TGt:
		return 8, OpGt
	case TLtEq:
		return 8, OpLe
	case TGtEq:
		return 8, OpGe
	case TShl:
		return 9, OpShl
	case TShr:
		return 9, OpShr
	case TUShr:
		return 9, OpUShr
	case TPlus:
		return 10, OpAdd
	case TMinus:
		return 10, OpSub
	case TStar:
		return 11, OpMul
	case TSlash:
		return 11, OpDiv
	case TPercent:
		return 11, OpMod
	case TStarStar:
		return 12, OpExp
	case TIdentifier:
		if p.tok.Flags&FlagEscaped != 0 {
			return 0, OpNone
		}
		switch p.tok.Kw {
		case KwInstanceof:
			return 8, OpInstanceof
		case KwIn:
			if !noIn {
				return 8, OpIn
			}
		}
	}
	return 0, OpNone
}

const precRelational = 8

// parseBinary parses the binary-operator levels from ShortCircuitExpression
// down to ExponentiationExpression by precedence climbing.
func (p *parser) parseBinary(minPrec int, noIn bool) NodeID {
	if p.opts.Algorithms.Binary == BinaryCascade {
		return p.parseBinaryCascade(max(minPrec, 1), minPrec, noIn)
	}
	start := p.start()
	var left NodeID
	if p.at(TPrivateName) {
		// RelationalExpression : PrivateIdentifier in ShiftExpression
		name, span := p.tok.Value, p.tok.Span()
		p.next()
		if !p.isKw(KwIn) || noIn || precRelational < minPrec {
			p.unexpected()
			return p.errNode()
		}
		left = p.newNode(KPrivateIdentifier, span.Start)
		p.nd(left).Str = p.str(name)
		p.done(left)
		p.usePrivate(name, span)
	} else {
		left = p.parseUnary(noIn)
		if p.isBareArrow(left) {
			return left
		}
	}
	added := 0
	for {
		prec, op := p.binaryPrec(noIn)
		if prec == 0 || prec < minPrec {
			break
		}
		if !p.binaryOperatorOK(left, op) {
			break
		}
		p.flushCover(start)
		p.next()
		var right NodeID
		if op == OpExp {
			right = p.parseBinary(prec, noIn)
		} else {
			right = p.parseBinary(prec+1, noIn)
		}
		left = p.binaryNode(start, left, op, right)
		added++
		if !p.enter() {
			break
		}
	}
	p.depth -= added
	return left
}

// binaryOperatorOK applies the checks that depend on the left operand
// before op is consumed: ExponentiationExpression's left side is an
// UpdateExpression, never a bare UnaryExpression (13.6), and a
// PrivateIdentifier may only be the left operand of `in` (13.10).
func (p *parser) binaryOperatorOK(left NodeID, op Op) bool {
	ln := p.nd(left)
	if op == OpExp && ln.Flags&NParenthesized == 0 && (ln.Kind == KUnaryExpression || ln.Kind == KAwaitExpression) {
		p.syntaxError(CodeExponentUnary, "unary expression before ** must be parenthesized")
		return false
	}
	if ln.Kind == KPrivateIdentifier && op != OpIn {
		p.unexpected()
		return false
	}
	return true
}

// binaryNode builds `left op right`, rejecting arrow-function operands and
// the ?? mixing forbidden by CoalesceExpressionHead (13.13).
func (p *parser) binaryNode(start int32, left NodeID, op Op, right NodeID) NodeID {
	if p.isBareArrow(right) {
		p.syntaxErrorAt(CodeUnexpectedToken, p.nd(right).Span, "arrow function is not allowed as an operand")
	}
	kind := KBinaryExpression
	if op == OpCoalesce || op == OpOr || op == OpAnd {
		kind = KLogicalExpression
		if p.mixesCoalesce(op, left) || p.mixesCoalesce(op, right) {
			p.syntaxError(CodeMixedCoalesce, "?? cannot be mixed with && or || without parentheses")
		}
	}
	id := p.newNode(kind, start)
	n := p.nd(id)
	n.Op = op
	n.A, n.B = left, right
	return p.done(id)
}

// parseBinaryCascade is the BinaryCascade candidate: one recursion level
// per precedence level, from level up to ExponentiationExpression (12);
// floor is the lowest precedence the whole descent accepts (it decides
// whether `#x in y` is allowed at the bottom). It builds the same trees
// and reports the same diagnostics as parseBinary.
func (p *parser) parseBinaryCascade(level, floor int, noIn bool) NodeID {
	start := p.start()
	if level > 12 {
		return p.parseBinaryOperand(floor, noIn)
	}
	left := p.parseBinaryCascade(level+1, floor, noIn)
	if p.isBareArrow(left) {
		return left
	}
	added := 0
	for {
		prec, op := p.binaryPrec(noIn)
		if prec != level {
			break
		}
		if !p.binaryOperatorOK(left, op) {
			break
		}
		p.flushCover(start)
		p.next()
		var right NodeID
		if op == OpExp {
			right = p.parseBinaryCascade(12, 12, noIn) // right-associative
		} else {
			right = p.parseBinaryCascade(level+1, level+1, noIn)
		}
		left = p.binaryNode(start, left, op, right)
		added++
		if !p.enter() {
			break
		}
		if op == OpExp {
			break
		}
	}
	p.depth -= added
	return left
}

// parseBinaryOperand parses the operand at the bottom of the binary
// levels: a UnaryExpression, or `#x` when followed by `in`.
func (p *parser) parseBinaryOperand(minPrec int, noIn bool) NodeID {
	if !p.at(TPrivateName) {
		return p.parseUnary(noIn)
	}
	name, span := p.tok.Value, p.tok.Span()
	p.next()
	if !p.isKw(KwIn) || noIn || precRelational < minPrec {
		p.unexpected()
		return p.errNode()
	}
	id := p.newNode(KPrivateIdentifier, span.Start)
	p.nd(id).Str = p.str(name)
	p.done(id)
	p.usePrivate(name, span)
	return id
}

// mixesCoalesce reports whether operand (of an op expression) is an
// unparenthesized logical expression that op may not be mixed with.
func (p *parser) mixesCoalesce(op Op, operand NodeID) bool {
	n := p.nd(operand)
	if n.Kind != KLogicalExpression || n.Flags&NParenthesized != 0 {
		return false
	}
	if op == OpCoalesce {
		return n.Op == OpOr || n.Op == OpAnd
	}
	return n.Op == OpCoalesce
}

// parseUnary parses UnaryExpression and prefix UpdateExpression.
func (p *parser) parseUnary(noIn bool) NodeID {
	start := p.start()
	var op Op
	switch p.tok.Kind {
	case TBang:
		op = OpNot
	case TTilde:
		op = OpBitNot
	case TPlus:
		op = OpPos
	case TMinus:
		op = OpNeg
	case TPlusPlus, TMinusMinus:
		upd := OpInc
		if p.at(TMinusMinus) {
			upd = OpDec
		}
		p.next()
		if !p.enter() {
			p.leave()
			return p.errNode()
		}
		arg := p.parseUnary(noIn)
		p.leave()
		if p.isBareArrow(arg) {
			p.syntaxErrorAt(CodeUnexpectedToken, p.nd(arg).Span, "arrow function is not allowed here")
		}
		p.checkSimpleTarget(arg)
		id := p.newNode(KUpdateExpression, start)
		n := p.nd(id)
		n.Op = upd
		n.Flags |= NPrefix
		n.A = arg
		return p.done(id)
	case TIdentifier:
		if p.tok.Flags&FlagEscaped == 0 {
			switch p.tok.Kw {
			case KwTypeof:
				op = OpTypeof
			case KwVoid:
				op = OpVoid
			case KwDelete:
				op = OpDelete
			case KwAwait:
				if p.cur().async && !p.cur().awaitIdent {
					return p.parseAwait(noIn)
				}
			}
		}
	}
	if op == OpNone {
		return p.parsePostfix(noIn)
	}
	p.next()
	if !p.enter() {
		p.leave()
		return p.errNode()
	}
	argStart := p.start()
	arg := p.parseUnary(noIn)
	p.leave()
	if p.isBareArrow(arg) {
		p.syntaxErrorAt(CodeUnexpectedToken, p.nd(arg).Span, "arrow function is not allowed here")
	}
	p.flushCover(argStart)
	if op == OpDelete {
		p.checkDelete(arg)
	}
	id := p.newNode(KUnaryExpression, start)
	n := p.nd(id)
	n.Op = op
	n.A = arg
	return p.done(id)
}

// checkDelete applies the delete early errors (13.5.1.1).
func (p *parser) checkDelete(arg NodeID) {
	n := p.nd(arg)
	switch n.Kind {
	case KIdentifier:
		if p.strict() {
			p.early(CodeStrictDelete, n.Span, "delete of an unqualified identifier in strict mode")
		}
	case KMemberExpression:
		if p.nd(n.B).Kind == KPrivateIdentifier {
			p.early(CodePrivateName, n.Span, "private fields cannot be deleted")
		}
	case KChainExpression:
		// delete a?.#x
		if m := p.nd(n.A); m.Kind == KMemberExpression && p.nd(m.B).Kind == KPrivateIdentifier {
			p.early(CodePrivateName, n.Span, "private fields cannot be deleted")
		}
	}
}

// parseAwait parses AwaitExpression (15.8).
func (p *parser) parseAwait(noIn bool) NodeID {
	start := p.start()
	c := p.cur()
	if c.inParams {
		p.early(CodeYieldAwaitInParams, p.tok.Span(), "await expression in formal parameters")
	}
	if p.inStaticBlock() {
		p.early(CodeAwaitInStaticBlock, p.tok.Span(), "await in class static block")
	}
	c.awaitPos = start
	p.next()
	if !p.enter() {
		p.leave()
		return p.errNode()
	}
	arg := p.parseUnary(noIn)
	p.leave()
	if p.isBareArrow(arg) {
		p.syntaxErrorAt(CodeUnexpectedToken, p.nd(arg).Span, "arrow function is not allowed here")
	}
	id := p.newNode(KAwaitExpression, start)
	p.nd(id).A = arg
	return p.done(id)
}

// parsePostfix parses LeftHandSideExpression with an optional postfix
// ++/-- ([no LineTerminator here]).
func (p *parser) parsePostfix(noIn bool) NodeID {
	start := p.start()
	expr := p.parseLHS(noIn)
	if (p.at(TPlusPlus) || p.at(TMinusMinus)) && !p.newlineBefore() && !p.isBareArrow(expr) {
		upd := OpInc
		if p.at(TMinusMinus) {
			upd = OpDec
		}
		p.checkSimpleTarget(expr)
		p.next()
		id := p.newNode(KUpdateExpression, start)
		n := p.nd(id)
		n.Op = upd
		n.A = expr
		return p.done(id)
	}
	return expr
}

// parseLHS parses LeftHandSideExpression: NewExpression, CallExpression or
// OptionalExpression.
func (p *parser) parseLHS(noIn bool) NodeID {
	start := p.start()
	var expr NodeID
	switch {
	case p.isKw(KwNew):
		expr = p.parseNew()
	case p.isKw(KwSuper):
		expr = p.parseSuper()
	case p.isKw(KwImport):
		expr = p.parseImportExpr()
	default:
		expr = p.parsePrimary(noIn)
		if p.isBareArrow(expr) {
			return expr
		}
	}
	return p.parseSubscripts(expr, start, false)
}

// parseNew parses `new MemberExpression Arguments?` and new.target.
func (p *parser) parseNew() NodeID {
	start := p.start()
	newSpan := p.tok.Span()
	p.next()
	if p.at(TDot) {
		p.next()
		if !p.isKw(KwTarget) {
			p.unexpected()
			return p.errNode()
		}
		propSpan := p.tok.Span()
		p.next()
		if !p.newTargetAllowed() {
			p.early(CodeNewTarget, Span{start, propSpan.End}, "new.target outside a function")
		}
		id := p.newNode(KMetaProperty, start)
		meta := p.ident("new", newSpan)
		prop := p.ident("target", propSpan)
		n := p.nd(id)
		n.A, n.B = meta, prop
		return p.done(id)
	}
	if !p.enter() {
		p.leave()
		return p.errNode()
	}
	defer p.leave()
	cstart := p.start()
	var callee NodeID
	switch {
	case p.isKw(KwNew):
		callee = p.parseNew()
	case p.isKw(KwSuper):
		callee = p.parseSuper()
		if p.at(TLParen) {
			p.syntaxError(CodeUnexpectedToken, "super() cannot be used with new")
		}
	case p.isKw(KwImport):
		callee = p.parseImportExpr()
		if p.nd(callee).Kind == KImportExpression {
			p.syntaxErrorAt(CodeUnexpectedToken, p.nd(callee).Span, "import() cannot be used with new")
		}
	default:
		callee = p.parsePrimary(false)
		if p.isBareArrow(callee) {
			p.syntaxErrorAt(CodeUnexpectedToken, p.nd(callee).Span, "arrow function is not allowed here")
		}
	}
	callee = p.parseSubscripts(callee, cstart, true)
	id := p.newNode(KNewExpression, start)
	p.nd(id).A = callee
	if p.at(TLParen) {
		l := p.parseArguments()
		p.nd(id).List = l
	}
	return p.done(id)
}

func (p *parser) newTargetAllowed() bool {
	for i := len(p.fns) - 1; i >= 0; i-- {
		c := &p.fns[i]
		if c.arrow {
			continue
		}
		return !c.top
	}
	return false
}

// superFlags returns the method / superCall permissions of the innermost
// non-arrow context.
func (p *parser) superFlags() (method, call bool) {
	for i := len(p.fns) - 1; i >= 0; i-- {
		c := &p.fns[i]
		if c.arrow {
			continue
		}
		return c.method, c.superCall
	}
	return false, false
}

// parseSuper parses `super` which must be followed by arguments, `.` or
// `[` (SuperCall / SuperProperty).
func (p *parser) parseSuper() NodeID {
	start := p.start()
	span := p.tok.Span()
	p.next()
	method, call := p.superFlags()
	switch p.tok.Kind {
	case TLParen:
		if !call {
			p.early(CodeSuperCall, span, "super() is only valid in a derived class constructor")
		}
	case TDot, TLBracket:
		if !method {
			p.early(CodeSuperProperty, span, "super property access is only valid in methods")
		}
	default:
		p.unexpected()
	}
	id := p.newNode(KSuper, start)
	return p.done(id)
}

// parseImportExpr parses ImportCall and import.meta.
func (p *parser) parseImportExpr() NodeID {
	start := p.start()
	impSpan := p.tok.Span()
	p.next()
	if p.at(TDot) {
		p.next()
		if !p.isKw(KwMeta) {
			p.unexpected()
			return p.errNode()
		}
		propSpan := p.tok.Span()
		p.next()
		if !p.module {
			p.early(CodeImportMeta, Span{start, propSpan.End}, "import.meta is only valid in modules")
		}
		id := p.newNode(KMetaProperty, start)
		meta := p.ident("import", impSpan)
		prop := p.ident("meta", propSpan)
		n := p.nd(id)
		n.A, n.B = meta, prop
		return p.done(id)
	}
	if !p.at(TLParen) {
		p.unexpected()
		return p.errNode()
	}
	p.next()
	src := p.parseAssignment(false, false)
	opts := NoNode
	if p.eat(TComma) && !p.at(TRParen) {
		opts = p.parseAssignment(false, false)
		p.eat(TComma)
	}
	p.expect(TRParen)
	id := p.newNode(KImportExpression, start)
	n := p.nd(id)
	n.A, n.B = src, opts
	return p.done(id)
}

// parseSubscripts parses member accesses, calls, tagged templates and
// optional chains after expr. noCalls stops at arguments (new callee).
func (p *parser) parseSubscripts(expr NodeID, start int32, noCalls bool) NodeID {
	chain := false
	for !p.halted {
		switch p.tok.Kind {
		case TDot:
			p.flushCover(start)
			p.next()
			if p.at(TPrivateName) && p.nd(expr).Kind == KSuper {
				p.syntaxError(CodePrivateName, "private names cannot be accessed through super")
				return expr
			}
			prop := p.parseMemberName()
			expr = p.member(start, expr, prop, false, false)
		case TQuestionDot:
			if noCalls {
				p.syntaxError(CodeInvalidOptional, "optional chain is not allowed in new expression")
				return expr
			}
			if p.nd(expr).Kind == KSuper {
				p.unexpected()
				return expr
			}
			p.flushCover(start)
			p.next()
			chain = true
			switch p.tok.Kind {
			case TLParen:
				args := p.parseArguments()
				expr = p.call(start, expr, args, true)
			case TLBracket:
				p.next()
				prop := p.parseExpression(false)
				p.expect(TRBracket)
				expr = p.member(start, expr, prop, true, true)
			case TNoSubstitutionTemplate, TTemplateHead:
				p.syntaxError(CodeInvalidOptional, "tagged template in optional chain")
				return expr
			default:
				prop := p.parseMemberName()
				expr = p.member(start, expr, prop, false, true)
			}
		case TLBracket:
			p.flushCover(start)
			p.next()
			prop := p.parseExpression(false)
			p.expect(TRBracket)
			expr = p.member(start, expr, prop, true, false)
		case TNoSubstitutionTemplate, TTemplateHead:
			if chain {
				p.syntaxError(CodeInvalidOptional, "tagged template in optional chain")
				return expr
			}
			p.flushCover(start)
			quasi := p.parseTemplate(true)
			id := p.newNode(KTaggedTemplateExpression, start)
			n := p.nd(id)
			n.A, n.B = expr, quasi
			expr = p.done(id)
		case TLParen:
			if noCalls {
				goto out
			}
			p.flushCover(start)
			args := p.parseArguments()
			expr = p.call(start, expr, args, false)
		default:
			goto out
		}
	}
out:
	if chain {
		id := p.newNode(KChainExpression, start)
		p.nd(id).A = expr
		expr = p.done(id)
	}
	return expr
}

func (p *parser) member(start int32, obj, prop NodeID, computed, optional bool) NodeID {
	id := p.newNode(KMemberExpression, start)
	n := p.nd(id)
	n.A, n.B = obj, prop
	if computed {
		n.Flags |= NComputed
	}
	if optional {
		n.Flags |= NOptional
	}
	return p.done(id)
}

func (p *parser) call(start int32, callee NodeID, args NodeList, optional bool) NodeID {
	id := p.newNode(KCallExpression, start)
	n := p.nd(id)
	n.A = callee
	n.List = args
	if optional {
		n.Flags |= NOptional
	}
	return p.done(id)
}

// parseMemberName parses the IdentifierName or PrivateIdentifier after `.`.
func (p *parser) parseMemberName() NodeID {
	switch p.tok.Kind {
	case TIdentifier:
		id := p.ident(p.tok.Value, p.tok.Span())
		p.next()
		return id
	case TPrivateName:
		span := p.tok.Span()
		id := p.newNode(KPrivateIdentifier, span.Start)
		p.nd(id).Str = p.str(p.tok.Value)
		p.usePrivate(p.tok.Value, span)
		p.next()
		return p.done(id)
	}
	p.unexpected()
	return p.errNode()
}

// parseArguments parses Arguments: ( ArgumentList? ,? ).
func (p *parser) parseArguments() NodeList {
	l, _ := p.parseArgumentsCover(false)
	return l
}

// parseArgumentsCover parses Arguments; with cover the items may become
// async arrow parameters (CoverCallExpressionAndAsyncArrowHead).
func (p *parser) parseArgumentsCover(cover bool) (NodeList, bool) {
	p.expect(TLParen)
	mark := len(p.scratch)
	trailing := false
	for !p.at(TRParen) && !p.at(TEOF) {
		if p.at(TEllipsis) {
			s := p.start()
			p.next()
			arg := p.parseAssignment(false, cover)
			id := p.newNode(KSpreadElement, s)
			p.nd(id).A = arg
			p.push(p.done(id))
		} else {
			p.push(p.parseAssignment(false, cover))
		}
		if p.at(TRParen) {
			break
		}
		p.expect(TComma)
		if p.at(TRParen) {
			trailing = true
			if n := len(p.scratch); n > mark {
				p.nd(p.scratch[n-1]).Flags |= NTrailingComma
			}
		}
	}
	p.expect(TRParen)
	return p.list(mark), trailing
}

// parsePrimary parses PrimaryExpression, including arrow functions that
// start with an identifier or parenthesis (they are returned whole; the
// callers refuse to use them as operands).
func (p *parser) parsePrimary(noIn bool) NodeID {
	start := p.start()
	switch p.tok.Kind {
	case TIdentifier:
		if p.tok.Flags&FlagEscaped == 0 {
			switch p.tok.Kw {
			case KwThis:
				p.next()
				return p.done(p.newNode(KThisExpression, start))
			case KwNull:
				p.next()
				id := p.newNode(KLiteral, start)
				p.nd(id).Op = LitNull
				return p.done(id)
			case KwTrue, KwFalse:
				v := p.tok.Kw == KwTrue
				p.next()
				id := p.newNode(KLiteral, start)
				n := p.nd(id)
				n.Op = LitBoolean
				if v {
					n.Num = 1
				}
				return p.done(id)
			case KwFunction:
				return p.parseFunction(start, false, false, false)
			case KwClass:
				return p.parseClass(start, false, false)
			case KwAsync:
				return p.parseAsyncPrimary(noIn)
			case KwNew:
				return p.parseNew()
			case KwSuper:
				return p.parseSuper()
			case KwImport:
				return p.parseImportExpr()
			}
		}
		if !isIdentifierToken(&p.tok) {
			if p.tok.Flags&FlagEscaped != 0 {
				p.syntaxError(CodeEscapedKeyword, "keyword must not contain escapes")
			} else {
				p.unexpected()
			}
			return p.errNode()
		}
		tok := p.tok
		p.checkIdentifierReference(&tok)
		id := p.ident(tok.Value, tok.Span())
		p.next()
		if p.at(TArrow) && !p.newlineBefore() {
			return p.parseArrow(start, p.listOf(id), false, start, noIn)
		}
		return id
	case TNumeric, TString, TBigInt:
		return p.parseLiteral()
	case TSlash, TSlashEq:
		p.regoalRegExp()
		if p.at(TRegExp) {
			return p.parseLiteral()
		}
		p.unexpected()
		return p.errNode()
	case TRegExp:
		return p.parseLiteral()
	case TNoSubstitutionTemplate, TTemplateHead:
		return p.parseTemplate(false)
	case TLParen:
		return p.parseParenOrArrow(noIn)
	case TLBracket:
		return p.parseArrayLiteral()
	case TLBrace:
		return p.parseObjectLiteral()
	}
	p.unexpected()
	return p.errNode()
}

// parseAsyncPrimary handles `async` at the start of a primary expression:
// async function expressions, async arrows, a call of a function named
// async, or the identifier itself.
func (p *parser) parseAsyncPrimary(noIn bool) NodeID {
	start := p.start()
	asyncTok := p.tok
	pk := p.peek()
	pkNewline := pk.Flags&FlagNewlineBefore != 0
	switch {
	case isKwTok(pk, KwFunction) && !pkNewline:
		p.next()
		return p.parseFunction(start, true, false, false)
	case pk.Kind == TIdentifier && !pkNewline && isIdentifierToken(pk) && p.peek2().Kind == TArrow:
		// async BindingIdentifier => ConciseBody
		p.next()
		tok := p.tok
		p.checkIdentifierReference(&tok)
		if tok.Kw == KwAwait {
			p.early(CodeInvalidUseOfKeyword, tok.Span(), "await is reserved in async arrow parameters")
		}
		param := p.ident(tok.Value, tok.Span())
		p.next()
		if !p.at(TArrow) || p.newlineBefore() {
			p.unexpected()
			return param
		}
		return p.parseArrow(start, p.listOf(param), true, start, noIn)
	case pk.Kind == TLParen && !pkNewline:
		p.checkIdentifierReference(&asyncTok)
		p.next()
		c := p.cur()
		argsStart := p.start()
		awaitIdentBefore := p.lastAwaitIdent
		args, trailing := p.parseArgumentsCover(true)
		if p.at(TArrow) && !p.newlineBefore() {
			if c.yieldPos >= argsStart || c.awaitPos >= argsStart || (p.lastAwaitIdent >= argsStart && p.lastAwaitIdent != awaitIdentBefore) {
				p.early(CodeYieldAwaitInParams, Span{argsStart, p.prevEnd}, "yield or await in async arrow parameters")
			}
			for i, a := range p.ids(args) {
				if p.nd(a).Kind == KSpreadElement {
					if i != int(args.Len)-1 || trailing {
						p.syntaxErrorAt(CodeInvalidCover, p.nd(a).Span, "rest parameter must be last")
					}
					p.nd(a).Kind = KRestElement
				}
			}
			return p.parseArrow(start, args, true, argsStart, noIn)
		}
		p.flushCover(argsStart)
		// The callee node is created only now so that an async arrow leaves
		// no orphan in the arena (JS-INV-06).
		callee := p.ident("async", asyncTok.Span())
		return p.call(start, callee, args, false)
	}
	tok := p.tok
	p.checkIdentifierReference(&tok)
	id := p.ident("async", tok.Span())
	p.next()
	if p.at(TArrow) && !p.newlineBefore() {
		return p.parseArrow(start, p.listOf(id), false, start, noIn)
	}
	return id
}

// parseParenOrArrow parses CoverParenthesizedExpressionAndArrowParameterList
// and decides between a parenthesized expression and arrow parameters by
// the token after `)`.
func (p *parser) parseParenOrArrow(noIn bool) NodeID {
	start := p.start()
	p.next()
	c := p.cur()
	if p.at(TRParen) {
		p.next()
		if !p.at(TArrow) || p.newlineBefore() {
			p.unexpected()
			return p.errNode()
		}
		return p.parseArrow(start, NodeList{Start: int32(len(p.children))}, false, start, noIn)
	}
	mark := len(p.scratch)
	rest, trailing := false, false
	for !p.at(TEOF) {
		if p.at(TEllipsis) {
			s := p.start()
			p.next()
			arg := p.parseBindingTarget(bindNone)
			if p.at(TEq) {
				p.syntaxError(CodeInvalidPattern, "rest element cannot have an initializer")
			}
			id := p.newNode(KRestElement, s)
			p.nd(id).A = arg
			p.push(p.done(id))
			rest = true
			break
		}
		p.push(p.parseAssignment(false, true))
		if !p.at(TComma) {
			break
		}
		p.next()
		if p.at(TRParen) {
			trailing = true
			break
		}
	}
	p.expect(TRParen)
	if p.at(TArrow) && !p.newlineBefore() {
		if c.yieldPos >= start || c.awaitPos >= start {
			p.early(CodeYieldAwaitInParams, Span{start, p.prevEnd}, "yield or await expression in arrow parameters")
		}
		params := p.list(mark)
		return p.parseArrow(start, params, false, start, noIn)
	}
	items := p.scratch[mark:]
	if len(items) == 0 || rest || trailing {
		p.syntaxError(CodeInvalidCover, "invalid parenthesized expression")
		p.scratch = p.scratch[:mark]
		return p.errNode()
	}
	p.flushCover(start)
	var expr NodeID
	if len(items) == 1 {
		expr = items[0]
		p.scratch = p.scratch[:mark]
	} else {
		first, last := p.nd(items[0]).Span.Start, p.nd(items[len(items)-1]).Span.End
		l := p.list(mark)
		expr = p.newNode(KSequenceExpression, first)
		n := p.nd(expr)
		n.List = l
		n.Span.End = last
	}
	p.nd(expr).Flags |= NParenthesized
	return expr
}

// parseArrayLiteral parses ArrayLiteral (13.2.4); elements may later be
// reinterpreted as an ArrayAssignmentPattern.
func (p *parser) parseArrayLiteral() NodeID {
	start := p.start()
	p.next()
	mark := len(p.scratch)
	for !p.at(TRBracket) && !p.at(TEOF) {
		if p.at(TComma) {
			p.next()
			p.push(NoNode)
			continue
		}
		var el NodeID
		if p.at(TEllipsis) {
			s := p.start()
			p.next()
			arg := p.parseAssignment(false, true)
			el = p.newNode(KSpreadElement, s)
			p.nd(el).A = arg
			p.done(el)
		} else {
			el = p.parseAssignment(false, true)
		}
		p.push(el)
		if !p.at(TComma) {
			break
		}
		if p.nd(el).Kind == KSpreadElement {
			p.nd(el).Flags |= NTrailingComma
		}
		p.next()
	}
	p.expect(TRBracket)
	id := p.newNode(KArrayExpression, start)
	p.nd(id).List = p.list(mark)
	return p.done(id)
}

// parseObjectLiteral parses ObjectLiteral (13.2.5) including
// CoverInitializedName, whose error stays pending until the literal's
// fate is known.
func (p *parser) parseObjectLiteral() NodeID {
	start := p.start()
	p.next()
	mark := len(p.scratch)
	protoSeen := false
	for !p.at(TRBrace) && !p.at(TEOF) {
		if p.at(TEllipsis) {
			s := p.start()
			p.next()
			arg := p.parseAssignment(false, true)
			el := p.newNode(KSpreadElement, s)
			p.nd(el).A = arg
			p.done(el)
			p.push(el)
			if p.at(TComma) {
				p.nd(el).Flags |= NTrailingComma
			}
		} else {
			p.push(p.parseObjectMember(&protoSeen))
		}
		if p.at(TRBrace) {
			break
		}
		p.expect(TComma)
	}
	p.expect(TRBrace)
	id := p.newNode(KObjectExpression, start)
	p.nd(id).List = p.list(mark)
	return p.done(id)
}

// propKey is a parsed PropertyName / ClassElementName.
type propKey struct {
	id       NodeID
	computed bool
	private  bool
	name     string // StringValue / PropName for non-computed keys
	named    bool   // name is meaningful (identifier, string or numeric key)
	identTok Token  // the token when the key was an IdentifierName
	isIdent  bool
}

// parsePropertyKey parses PropertyName (and PrivateIdentifier when
// allowPrivate).
func (p *parser) parsePropertyKey(allowPrivate bool) propKey {
	var k propKey
	switch p.tok.Kind {
	case TIdentifier:
		k.identTok = p.tok
		k.isIdent = true
		k.name, k.named = p.tok.Value, true
		k.id = p.ident(p.tok.Value, p.tok.Span())
		p.next()
	case TString:
		k.name, k.named = p.tok.Value, true
		k.id = p.parseLiteral()
	case TNumeric:
		k.name, k.named = NumberToString(p.tok.Num), true
		k.id = p.parseLiteral()
	case TBigInt:
		k.id = p.parseLiteral()
	case TLBracket:
		p.next()
		k.computed = true
		k.id = p.parseAssignment(false, false)
		p.expect(TRBracket)
	case TPrivateName:
		if !allowPrivate {
			p.unexpected()
			k.id = p.errNode()
			return k
		}
		k.private = true
		k.name, k.named = p.tok.Value, true
		span := p.tok.Span()
		k.id = p.newNode(KPrivateIdentifier, span.Start)
		p.nd(k.id).Str = p.str(p.tok.Value)
		p.next()
		p.done(k.id)
	default:
		p.unexpected()
		k.id = p.errNode()
	}
	return k
}

// isModifierKeyEnd reports whether t ends a property after a get / set /
// async prefix word, making the word itself the key.
func isModifierKeyEnd(t *Token) bool {
	switch t.Kind {
	case TLParen, TColon, TComma, TRBrace, TEq, TSemicolon, TEOF:
		return true
	}
	return false
}

// parseObjectMember parses PropertyDefinition.
func (p *parser) parseObjectMember(protoSeen *bool) NodeID {
	start := p.start()
	isAsync, isGen := false, false
	kind := OpInit
	if p.isKw(KwAsync) {
		if pk := p.peek(); !isModifierKeyEnd(pk) && pk.Flags&FlagNewlineBefore == 0 {
			isAsync = true
			p.next()
		}
	}
	if p.at(TStar) {
		isGen = true
		p.next()
	}
	if !isAsync && !isGen && (p.isKw(KwGet) || p.isKw(KwSet)) {
		if pk := p.peek(); !isModifierKeyEnd(pk) {
			kind = OpGet
			if p.isKw(KwSet) {
				kind = OpSet
			}
			p.next()
		}
	}
	key := p.parsePropertyKey(false)
	id := p.newNode(KProperty, start)
	p.nd(id).A = key.id
	if key.computed {
		p.nd(id).Flags |= NComputed
	}
	if p.at(TLParen) {
		mk := OpMethod
		if kind != OpInit {
			mk = kind
		}
		fn := p.parseMethod(isAsync, isGen, mk, false)
		n := p.nd(id)
		n.B = fn
		n.Op = kind
		if kind == OpInit {
			n.Flags |= NMethod
		}
		return p.done(id)
	}
	if isAsync || isGen || kind != OpInit {
		p.unexpected()
		return p.done(id)
	}
	p.nd(id).Op = OpInit
	if p.at(TColon) {
		p.next()
		val := p.parseAssignment(false, true)
		p.nd(id).B = val
		if !key.computed && key.named && key.name == "__proto__" {
			if *protoSeen {
				p.cover = append(p.cover, coverErr{code: CodeDuplicateProto, span: p.nd(key.id).Span})
			}
			*protoSeen = true
		}
		return p.done(id)
	}
	// Shorthand: IdentifierReference or CoverInitializedName.
	if !key.isIdent || key.computed {
		p.unexpected()
		return p.done(id)
	}
	tok := key.identTok
	if !isIdentifierToken(&tok) {
		if tok.Flags&FlagEscaped != 0 {
			p.early(CodeEscapedKeyword, tok.Span(), "keyword must not contain escapes")
		} else {
			p.early(CodeReservedWord, tok.Span(), "unexpected reserved word")
		}
	}
	p.checkIdentifierReference(&tok)
	val := p.ident(tok.Value, tok.Span())
	p.nd(id).Flags |= NShorthand
	if p.at(TEq) {
		eqSpan := p.tok.Span()
		p.next()
		right := p.parseAssignment(false, false)
		ap := p.newNode(KAssignmentPattern, tok.Start)
		n := p.nd(ap)
		n.A, n.B = val, right
		p.done(ap)
		val = ap
		p.cover = append(p.cover, coverErr{code: CodeInvalidCover, span: Span{tok.Start, eqSpan.End}})
	}
	p.nd(id).B = val
	return p.done(id)
}

// parseLiteral turns the current literal token into a Literal node,
// applying the strict-mode octal and regular-expression early errors.
func (p *parser) parseLiteral() NodeID {
	t := p.tok
	id := p.newNode(KLiteral, t.Start)
	n := p.nd(id)
	switch t.Kind {
	case TString:
		n.Op = LitString
		n.Str = p.str(t.Value)
		if t.Flags&FlagLegacyOctal != 0 && p.strict() {
			p.early(CodeStrictOctal, t.Span(), "octal escape sequences are not allowed in strict mode")
		}
	case TNumeric:
		n.Op = LitNumber
		n.Num = t.Num
		if t.Flags&FlagLegacyOctal != 0 && p.strict() {
			p.early(CodeStrictOctal, t.Span(), "legacy octal literals are not allowed in strict mode")
		}
	case TBigInt:
		n.Op = LitBigInt
		n.Str = p.str(t.Value)
	case TRegExp:
		n.Op = LitRegExp
		n.Str = p.str(t.Value)
		n.Str2 = p.str(t.Raw)
		flags, at, ok := validateRegExpFlags(t.Raw)
		if !ok {
			flagStart := t.End - int32(len(t.Raw))
			p.early(CodeInvalidRegExpFlags, Span{flagStart + int32(at), flagStart + int32(at) + 1}, "invalid regular expression flags")
		} else if off, ok := validateRegExpPattern(t.Value, flags); !ok {
			p.early(CodeInvalidRegExp, Span{t.Start + 1 + int32(off), t.Start + 2 + int32(off)}, "invalid regular expression")
		}
	}
	p.next()
	return p.done(id)
}

// parseTemplate parses TemplateLiteral; untagged templates may not
// contain NotEscapeSequences (13.2.8.1).
func (p *parser) parseTemplate(tagged bool) NodeID {
	start := p.start()
	id := p.newNode(KTemplateLiteral, start)
	qmark := len(p.scratch)
	var exprs []NodeID
	for !p.halted {
		t := p.tok
		if t.Kind != TNoSubstitutionTemplate && t.Kind != TTemplateHead && t.Kind != TTemplateMiddle && t.Kind != TTemplateTail {
			p.unexpected()
			break
		}
		tail := t.Kind == TNoSubstitutionTemplate || t.Kind == TTemplateTail
		qs, qe := t.Start+1, t.End-1
		if !tail {
			qe = t.End - 2
		}
		if t.Err != CodeNone {
			qe = t.End
		}
		if qe < qs {
			qe = qs
		}
		q := p.newNode(KTemplateElement, qs)
		n := p.nd(q)
		n.Span.End = qe
		n.Str2 = p.str(t.Raw)
		if t.Flags&FlagInvalidEscape != 0 {
			n.Flags |= NCookedNull
			if !tagged {
				p.early(CodeInvalidTemplate, Span{t.ErrAt, t.ErrAt + 1}, "invalid escape sequence in template")
			}
		} else {
			n.Str = p.str(t.Value)
		}
		if tail {
			n.Flags |= NTail
		}
		p.push(q)
		p.next()
		if tail {
			break
		}
		p.tmplDepth++
		e := p.parseExpression(false)
		p.tmplDepth--
		exprs = append(exprs, e)
		if !p.at(TRBrace) {
			p.unexpectedExpected(TRBrace)
			break
		}
		p.regoalTemplateTail()
	}
	quasis := p.list(qmark)
	n := p.nd(id)
	n.List = quasis
	n.List2 = p.listOf(exprs...)
	return p.done(id)
}
